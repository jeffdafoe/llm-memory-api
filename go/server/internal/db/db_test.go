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
