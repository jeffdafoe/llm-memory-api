package db

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestDrainBackground(t *testing.T) {
	// Before the drain: an unbounded job's context has no deadline, a
	// bounded one's has. (The drain closes background work for the
	// process, so these checks live in this test.)
	deadlines := make(chan bool, 2)
	hasDeadline := func() bool {
		select {
		case ok := <-deadlines:
			return ok
		case <-time.After(5 * time.Second):
			t.Fatal("background job did not run")
			return false
		}
	}
	BackgroundUnbounded(func(ctx context.Context) {
		_, ok := ctx.Deadline()
		deadlines <- ok
	})
	if hasDeadline() {
		t.Error("unbounded job has a deadline")
	}
	Background(func(ctx context.Context) {
		_, ok := ctx.Deadline()
		deadlines <- ok
	})
	if !hasDeadline() {
		t.Error("bounded job has no deadline")
	}

	var finished atomic.Int32
	Background(func(ctx context.Context) {
		<-ctx.Done()
		finished.Add(1)
	})
	BackgroundUnbounded(func(ctx context.Context) {
		<-ctx.Done()
		finished.Add(1)
	})
	// A job that ignores cancellation for longer than both drain windows.
	release := make(chan struct{})
	Background(func(ctx context.Context) {
		<-release
	})

	if DrainBackground(20 * time.Millisecond) {
		t.Fatal("drain reported done with a stuck job")
	}
	if finished.Load() != 2 {
		t.Fatal("cancellable jobs were not cancelled")
	}

	// After the drain started, new jobs are dropped.
	var ran atomic.Bool
	Background(func(ctx context.Context) { ran.Store(true) })
	BackgroundUnbounded(func(ctx context.Context) { ran.Store(true) })
	close(release)
	time.Sleep(20 * time.Millisecond)
	if ran.Load() {
		t.Fatal("job accepted after drain")
	}
}

// A job queued behind another before the drain starts is waited for, and
// runs with a deadline of its own once the first one ends.
func TestDrainWaitsForQueuedJob(t *testing.T) {
	r := newRunner()
	release := make(chan struct{})
	first := make(chan struct{})
	var secondRan atomic.Bool
	r.after(nil, func(ctx context.Context) {
		defer close(first)
		<-release
	})
	r.after(first, func(ctx context.Context) {
		if err := ctx.Err(); err != nil {
			t.Errorf("second job's context: %v", err)
		}
		secondRan.Store(true)
	})
	drained := make(chan bool)
	go func() { drained <- r.drain(2 * time.Second) }()
	select {
	case <-drained:
		t.Fatal("drain finished while the first job was blocked")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if !<-drained {
		t.Fatal("drain did not finish")
	}
	if !secondRan.Load() {
		t.Fatal("queued job did not run before the drain finished")
	}
	if r.after(nil, func(context.Context) {}) {
		t.Fatal("job accepted after drain")
	}
}

// An unbounded job's context has no deadline and ends at the drain.
func TestUnboundedJobEndsAtDrain(t *testing.T) {
	r := newRunner()
	deadline := make(chan bool, 1)
	done := make(chan struct{})
	r.start(nil, func(ctx context.Context) {
		_, ok := ctx.Deadline()
		deadline <- ok
		<-ctx.Done()
		close(done)
	}, false)
	if <-deadline {
		t.Error("unbounded job has a deadline")
	}
	if !r.drain(10 * time.Millisecond) {
		t.Error("drain did not end the unbounded job")
	}
	<-done
}
