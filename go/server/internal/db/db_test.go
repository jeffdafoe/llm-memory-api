package db

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestDrainBackground(t *testing.T) {
	var finished atomic.Int32
	Background(func(ctx context.Context) {
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
	if finished.Load() != 1 {
		t.Fatal("cancellable job was not cancelled")
	}

	// After the drain started, new jobs are dropped.
	var ran atomic.Bool
	Background(func(ctx context.Context) { ran.Store(true) })
	close(release)
	time.Sleep(20 * time.Millisecond)
	if ran.Load() {
		t.Fatal("job accepted after drain")
	}
}
