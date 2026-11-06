package ttlcache

import (
	"testing"
	"time"
)

func TestInvalidationBeatsLoadInFlight(t *testing.T) {
	c := &Cache[string, int]{}
	gen := c.Generation() // a loader starts its query
	c.Delete("token")     // a logout lands meanwhile
	if c.SetIfCurrent(gen, "token", 1, time.Now().Add(time.Minute)) {
		t.Fatal("stale load was published after Delete")
	}
	if _, ok := c.Get("token"); ok {
		t.Fatal("stale entry visible")
	}

	gen = c.Generation()
	c.Clear()
	if c.SetIfCurrent(gen, "other", 1, time.Now().Add(time.Minute)) {
		t.Fatal("stale load was published after Clear")
	}

	gen = c.Generation()
	if !c.SetIfCurrent(gen, "token", 2, time.Now().Add(time.Minute)) {
		t.Fatal("current load refused")
	}
	if v, ok := c.Get("token"); !ok || v != 2 {
		t.Fatalf("Get = %v, %v", v, ok)
	}
}

func TestExpiryAndSweep(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	c := &Cache[string, int]{Now: func() time.Time { return now }}
	c.SetIfCurrent(c.Generation(), "old", 1, now.Add(time.Second))
	c.SetIfCurrent(c.Generation(), "keep", 2, now.Add(time.Hour))

	now = now.Add(2 * time.Second)
	if _, ok := c.Get("old"); ok {
		t.Fatal("expired entry returned")
	}
	// The last sweep was at the first write; a write within the minute does
	// not sweep.
	c.SetIfCurrent(c.Generation(), "new", 3, now.Add(time.Hour))
	if c.Len() != 3 {
		t.Fatalf("Len = %d before the sweep interval", c.Len())
	}

	now = now.Add(sweepInterval)
	c.SetIfCurrent(c.Generation(), "newer", 4, now.Add(time.Hour))
	if c.Len() != 3 {
		t.Fatalf("Len = %d after sweep, want 3 (old removed)", c.Len())
	}
	if _, ok := c.Get("keep"); !ok {
		t.Fatal("live entry swept")
	}
}

func TestDeleteFuncInvalidatesLoads(t *testing.T) {
	var c Cache[string, int]
	far := time.Now().Add(time.Hour)
	c.SetIfCurrent(c.Generation(), "a", 1, far)
	c.SetIfCurrent(c.Generation(), "b", 2, far)
	gen := c.Generation()
	c.DeleteFunc(func(_ string, v int) bool { return v == 1 })
	if _, ok := c.Get("a"); ok {
		t.Error("matched entry kept")
	}
	if v, ok := c.Get("b"); !ok || v != 2 {
		t.Error("other entry dropped")
	}
	if c.SetIfCurrent(gen, "a", 1, far) {
		t.Error("a load from before DeleteFunc was stored")
	}
}
