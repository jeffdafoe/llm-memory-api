package attempts

import (
	"strconv"
	"testing"
	"time"
)

// Ported from node/api/src/services/attempt-limiter.test.js.

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }
func newClock() *clock                   { return &clock{t: time.Unix(1, 0)} }
func ms(n int) time.Duration             { return time.Duration(n) * time.Millisecond }
func limiter(c *clock, max int) *Limiter {
	return New(Options{MaxAttempts: max, Window: time.Minute, Now: c.now})
}
func limiterWith(c *clock, max, sweepAt, maxKeys int) *Limiter {
	return New(Options{MaxAttempts: max, Window: time.Minute, Now: c.now, SweepAt: sweepAt, MaxKeys: maxKeys})
}

func TestBurstCappedAtMaxAttempts(t *testing.T) {
	c := newClock()
	caller, target := limiter(c, 5), limiter(c, 10)
	allowed := 0
	for i := 0; i < 50; i++ {
		if ok, _ := Acquire(Pair{caller, "7"}, Pair{target, "bob"}); ok {
			allowed++
		}
	}
	if allowed != 5 {
		t.Fatalf("allowed %d, want 5", allowed)
	}
}

func TestPerTargetCapAcrossCallers(t *testing.T) {
	c := newClock()
	caller, target := limiter(c, 5), limiter(c, 10)
	allowed := 0
	for id := 1; id <= 30; id++ {
		if ok, _ := Acquire(Pair{caller, strconv.Itoa(id)}, Pair{target, "bob"}); ok {
			allowed++
		}
	}
	if allowed != 10 {
		t.Fatalf("allowed %d, want 10", allowed)
	}
}

func TestBlockedAcquireConsumesNothing(t *testing.T) {
	c := newClock()
	caller, target := limiter(c, 5), limiter(c, 1)
	if ok, _ := Acquire(Pair{caller, "7"}, Pair{target, "bob"}); !ok {
		t.Fatal("first acquire refused")
	}
	for i := 0; i < 10; i++ {
		if ok, _ := Acquire(Pair{caller, "7"}, Pair{target, "bob"}); ok {
			t.Fatal("exhausted target allowed")
		}
	}
	allowed := 0
	for i := 0; i < 10; i++ {
		if ok, _ := Acquire(Pair{caller, "7"}, Pair{target, "other-" + strconv.Itoa(i)}); ok {
			allowed++
		}
	}
	if allowed != 4 {
		t.Fatalf("allowed %d, want 4 (caller used 1 of 5 before the target block)", allowed)
	}
}

func TestBlockLiftsAfterWindow(t *testing.T) {
	c := newClock()
	l := limiter(c, 2)
	l.Consume("7")
	l.Consume("7")
	if blocked, _ := l.Check("7"); !blocked {
		t.Fatal("not blocked")
	}
	c.advance(ms(59999))
	if _, wait := l.Check("7"); wait != 1 {
		t.Fatalf("retry after %d, want 1", wait)
	}
	c.advance(ms(1))
	if blocked, _ := l.Check("7"); blocked {
		t.Fatal("still blocked after the window")
	}
}

func TestSuccessDoesNotRefund(t *testing.T) {
	c := newClock()
	caller, target := limiter(c, 5), limiter(c, 10)
	for i := 0; i < 4; i++ {
		if ok, _ := Acquire(Pair{caller, "7"}, Pair{target, "victim-" + strconv.Itoa(i)}); !ok {
			t.Fatal("refused early")
		}
	}
	if ok, _ := Acquire(Pair{caller, "7"}, Pair{target, "my-own-account"}); !ok {
		t.Fatal("fifth refused")
	}
	if ok, _ := Acquire(Pair{caller, "7"}, Pair{target, "victim-5"}); ok {
		t.Fatal("sixth allowed")
	}
}

func TestSweepAtMostOncePerSecond(t *testing.T) {
	c := newClock()
	l := limiterWith(c, 5, 2, 2)
	l.Consume("a")
	l.Consume("b")
	c.advance(ms(59500))
	if blocked, _ := l.Check("c"); !blocked {
		t.Fatal("c allowed with a full map")
	}
	c.advance(ms(500))
	if blocked, _ := l.Check("c"); !blocked {
		t.Fatal("sweep ran again within a second")
	}
	if l.Size() != 2 {
		t.Fatalf("size %d", l.Size())
	}
	c.advance(ms(500))
	if blocked, _ := l.Check("c"); blocked {
		t.Fatal("sweep did not free the map")
	}
	if l.Size() != 0 {
		t.Fatalf("size %d", l.Size())
	}
}

func TestExpiredOneOffKeysSwept(t *testing.T) {
	c := newClock()
	l := limiterWith(c, 5, 10, 1000)
	for i := 0; i < 10; i++ {
		l.Consume("guess-" + strconv.Itoa(i))
	}
	if l.Size() != 10 {
		t.Fatalf("size %d", l.Size())
	}
	c.advance(time.Minute)
	if blocked, _ := l.Check("new"); blocked {
		t.Fatal("new key blocked")
	}
	if l.Size() != 0 {
		t.Fatalf("size %d", l.Size())
	}
}

func TestFullMapRefusesNewKeys(t *testing.T) {
	c := newClock()
	l := limiterWith(c, 5, 3, 3)
	l.Consume("a")
	l.Consume("b")
	l.Consume("c")
	if blocked, _ := l.Check("d"); !blocked {
		t.Fatal("d allowed with no room")
	}
	if blocked, _ := l.Check("a"); blocked {
		t.Fatal("existing key blocked")
	}
	c.advance(time.Minute)
	if blocked, _ := l.Check("d"); blocked {
		t.Fatal("d blocked after the window")
	}
}
