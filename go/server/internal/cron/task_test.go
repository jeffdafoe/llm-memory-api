package cron

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func mustParse(t *testing.T, pattern string) *Schedule {
	t.Helper()
	s, err := ParseIn(pattern, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func at(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// The cases follow node-cron's planBeat: a slot runs when the timer reaches
// it at most a second late and before the following slot; older slots are
// missed.
func TestPlanBeat(t *testing.T) {
	minutely := mustParse(t, "* * * * *")
	secondly := mustParse(t, "* * * * * *")
	cases := []struct {
		name     string
		sched    *Schedule
		expected string
		now      string
		missed   int
		run      bool
		next     string
	}{
		{"early wake re-arms", minutely, "2026-11-06T10:00:00Z", "2026-11-06T09:59:59Z", 0, false, "2026-11-06T10:00:00Z"},
		{"on time", minutely, "2026-11-06T10:00:00Z", "2026-11-06T10:00:00Z", 0, true, "2026-11-06T10:01:00Z"},
		{"one second late", minutely, "2026-11-06T10:00:00Z", "2026-11-06T10:00:01Z", 0, true, "2026-11-06T10:01:00Z"},
		{"two seconds late", minutely, "2026-11-06T10:00:00Z", "2026-11-06T10:00:02Z", 1, false, "2026-11-06T10:01:00Z"},
		{"two slots behind", minutely, "2026-11-06T10:00:00Z", "2026-11-06T10:02:00Z", 2, true, "2026-11-06T10:03:00Z"},
		{"late by the whole gap", secondly, "2026-11-06T10:00:00Z", "2026-11-06T10:00:01Z", 1, true, "2026-11-06T10:00:02Z"},
	}
	for _, c := range cases {
		missed, run, next, err := planBeat(at(t, c.expected), at(t, c.now), c.sched.Next)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(missed) != c.missed || run != c.run || !next.Equal(at(t, c.next)) {
			t.Errorf("%s: missed %d run %v next %s; want %d %v %s", c.name, len(missed), run, next, c.missed, c.run, c.next)
		}
	}
}

func TestPlanBeatWalkError(t *testing.T) {
	fail := errors.New("walk failed")
	_, _, _, err := planBeat(at(t, "2026-11-06T10:00:00Z"), at(t, "2026-11-06T10:00:00Z"), func(time.Time) (time.Time, error) {
		return time.Time{}, fail
	})
	if !errors.Is(err, fail) {
		t.Fatalf("err %v", err)
	}
}

func TestStartRefusesAScheduleWithNoSlot(t *testing.T) {
	_, err := Start(mustParse(t, "0 0 30W 2 *"), func(context.Context) {}, Options{})
	if !errors.Is(err, ErrNoMatch) {
		t.Fatalf("err %v", err)
	}
}

func TestTaskRunsAndDrains(t *testing.T) {
	var runs atomic.Int32
	started := make(chan struct{}, 10)
	task, err := Start(mustParse(t, "* * * * * *"), func(ctx context.Context) {
		runs.Add(1)
		started <- struct{}{}
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("no run within 3 s")
	}
	if !task.Drain(time.Second) {
		t.Fatal("drain timed out")
	}
	n := runs.Load()
	time.Sleep(1200 * time.Millisecond)
	if runs.Load() != n {
		t.Fatal("ran after Drain")
	}
}

func TestDrainCancelsARunningCall(t *testing.T) {
	cancelled := make(chan struct{})
	started := make(chan struct{})
	var once atomic.Bool
	task, err := Start(mustParse(t, "* * * * * *"), func(ctx context.Context) {
		if !once.CompareAndSwap(false, true) {
			return
		}
		close(started)
		<-ctx.Done()
		close(cancelled)
	}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("no run within 3 s")
	}
	if !task.Drain(50 * time.Millisecond) {
		t.Fatal("drain did not finish after cancelling")
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("running call was not cancelled")
	}
}

func TestMissedSlotsAreReported(t *testing.T) {
	// The clock jumps ahead by five seconds after Start, as a stalled process
	// would see it.
	var offset atomic.Int64
	now := func() time.Time { return time.Now().Add(time.Duration(offset.Load())) }
	var missed atomic.Int32
	var runs atomic.Int32
	task, err := Start(mustParse(t, "* * * * * *"), func(context.Context) { runs.Add(1) }, Options{
		Now:    now,
		Missed: func(time.Time) { missed.Add(1) },
	})
	if err != nil {
		t.Fatal(err)
	}
	defer task.Drain(time.Second)
	offset.Store(int64(5 * time.Second))
	deadline := time.Now().Add(3 * time.Second)
	for missed.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if missed.Load() < 4 {
		t.Fatalf("missed %d slots, want at least 4", missed.Load())
	}
}
