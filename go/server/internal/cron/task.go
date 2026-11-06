package cron

import (
	"context"
	"sync"
	"time"
)

// missedTolerance is node-cron's default missedExecutionTolerance: a slot
// whose timer fires at most this late still runs.
const missedTolerance = 1000 * time.Millisecond

// maxDelay is node-cron's cap on one timer; a longer wait is re-armed when
// the timer fires early.
const maxDelay = 24 * time.Hour

// Task runs a function at every time a Schedule matches, the way
// node-cron's schedule() did with its default options: a run is not skipped
// because the previous one is still going, and a slot the timer reaches more
// than a second late (or after the next slot) is missed, not run.
type Task struct {
	sched *Schedule
	fn    func(ctx context.Context)
	// Missed is told about each slot that was skipped; node-cron printed a
	// warning for it. Nil means nothing is reported.
	missed func(slot time.Time)
	// failed is told when the walk to the next slot fails, which stops the
	// task. node-cron threw from its timer callback there.
	failed func(err error)
	now    func() time.Time

	mu       sync.Mutex
	stopped  bool
	timer    *time.Timer
	expected time.Time

	running sync.WaitGroup
	ctx     context.Context
	cancel  context.CancelFunc
}

// Options are the optional hooks of a Task.
type Options struct {
	Missed func(slot time.Time)
	Failed func(err error)
	// Now replaces the clock, for tests.
	Now func() time.Time
}

// Start schedules fn. It returns the error node-cron's start threw when no
// first slot exists.
func Start(sched *Schedule, fn func(ctx context.Context), opts Options) (*Task, error) {
	t := &Task{sched: sched, fn: fn, missed: opts.Missed, failed: opts.Failed, now: opts.Now}
	if t.now == nil {
		t.now = time.Now
	}
	first, err := sched.Next(t.nowWithoutMs())
	if err != nil {
		return nil, err
	}
	t.ctx, t.cancel = context.WithCancel(context.Background())
	t.mu.Lock()
	t.expected = first
	t.arm()
	t.mu.Unlock()
	return t, nil
}

func (t *Task) nowWithoutMs() time.Time {
	return t.now().Truncate(time.Second)
}

// arm sets the timer for the expected slot. The caller holds mu.
func (t *Task) arm() {
	if t.stopped {
		return
	}
	delay := min(max(t.expected.Sub(t.now()), 0), maxDelay)
	t.timer = time.AfterFunc(delay, t.beat)
}

// beat is node-cron's heartBeat: decide which slots were missed and whether
// the current one runs, then arm for the next.
func (t *Task) beat() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return
	}
	missed, run, next, err := planBeat(t.expected, t.nowWithoutMs(), t.sched.Next)
	if err != nil {
		t.stopped = true
		if t.failed != nil {
			t.failed(err)
		}
		return
	}
	t.expected = next
	for _, slot := range missed {
		if t.missed != nil {
			t.missed(slot)
		}
	}
	if run {
		t.running.Add(1)
		go func() {
			defer t.running.Done()
			t.fn(t.ctx)
		}()
	}
	t.arm()
}

// planBeat is node-cron's: walk the slots from expected to now. A slot the
// timer reached within the tolerance and before the following slot runs;
// older slots are missed.
func planBeat(expected, now time.Time, next func(time.Time) (time.Time, error)) (missed []time.Time, run bool, nextSlot time.Time, err error) {
	slot := expected
	for {
		if now.Before(slot) {
			return missed, false, slot, nil
		}
		following, err := next(slot)
		if err != nil {
			return missed, false, time.Time{}, err
		}
		if !following.After(slot) {
			following, err = next(now)
			return missed, false, following, err
		}
		gap := following.Sub(slot)
		lateBy := now.Sub(slot)
		if lateBy <= missedTolerance && lateBy < gap {
			return missed, true, following, nil
		}
		missed = append(missed, slot)
		slot = following
	}
}

// Stop starts no further runs. A run in progress continues; Drain waits for
// it.
func (t *Task) Stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.stopped = true
	if t.timer != nil {
		t.timer.Stop()
	}
}

// Drain stops the task, waits up to timeout for running calls of fn,
// cancels their context, and waits up to timeout more. It reports whether
// they all returned.
func (t *Task) Drain(timeout time.Duration) bool {
	t.Stop()
	done := make(chan struct{})
	go func() {
		t.running.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
	}
	t.cancel()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
}
