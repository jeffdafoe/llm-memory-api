package db

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// An unbounded job's context has no deadline; Background's has one. Runs
// before the drain test, which closes background work for the process.
func TestBackgroundUnboundedHasNoDeadline(t *testing.T) {
	deadlines := make(chan bool, 2)
	BackgroundUnbounded(func(ctx context.Context) {
		_, ok := ctx.Deadline()
		deadlines <- ok
	})
	if <-deadlines {
		t.Error("unbounded job has a deadline")
	}
	Background(func(ctx context.Context) {
		_, ok := ctx.Deadline()
		deadlines <- ok
	})
	if !<-deadlines {
		t.Error("bounded job has no deadline")
	}
}

func TestDrainBackground(t *testing.T) {
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
