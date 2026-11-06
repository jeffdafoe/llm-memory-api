// Package attempts is the in-memory attempt limiter (Node's
// services/attempt-limiter.js) that throttles password guessing: the
// connector login on /authorize, and later the dashboard login and the
// account-link route.
//
// It is deliberately in-process. The server runs as one process, so the
// counters are already global; losing them on a restart only resets the
// window early. Attempts are counted when they start (Acquire), not when they
// fail, so a burst of parallel requests cannot all pass the check before the
// first failure is recorded, and a success refunds nothing.
package attempts

import (
	"math"
	"sync"
	"time"
)

// sweepInterval is the minimum gap between full-map sweeps. Without it, once
// the map is past SweepAt, every check for a new key would scan the whole map,
// work an attacker controls by sending distinct keys.
const sweepInterval = time.Second

type entry struct {
	attempts    int
	windowStart time.Time
}

// Limiter allows MaxAttempts per key in each Window, counted from the key's
// first attempt.
type Limiter struct {
	maxAttempts int
	window      time.Duration
	maxKeys     int
	sweepAt     int
	now         func() time.Time

	mu        sync.Mutex
	entries   map[string]*entry
	lastSweep time.Time
	swept     bool
}

// Options configure a Limiter. MaxKeys defaults to 10000 and SweepAt to 1000,
// as in Node; Now defaults to time.Now.
type Options struct {
	MaxAttempts int
	Window      time.Duration
	MaxKeys     int
	SweepAt     int
	Now         func() time.Time
}

// New returns a Limiter.
func New(o Options) *Limiter {
	l := &Limiter{
		maxAttempts: o.MaxAttempts,
		window:      o.Window,
		maxKeys:     o.MaxKeys,
		sweepAt:     o.SweepAt,
		now:         o.Now,
		entries:     map[string]*entry{},
	}
	if l.maxKeys == 0 {
		l.maxKeys = 10000
	}
	if l.sweepAt == 0 {
		l.sweepAt = 1000
	}
	if l.now == nil {
		l.now = time.Now
	}
	return l
}

func (l *Limiter) expired(e *entry) bool {
	return l.now().Sub(e.windowStart) >= l.window
}

// current returns the key's live entry, dropping an expired one.
func (l *Limiter) current(key string) *entry {
	e := l.entries[key]
	if e != nil && l.expired(e) {
		delete(l.entries, key)
		return nil
	}
	return e
}

// sweep drops every expired entry. Expired keys are otherwise removed only
// when the same key comes back, and one-off keys (a guessed name) never do.
func (l *Limiter) sweep() {
	if l.swept && l.now().Sub(l.lastSweep) < sweepInterval {
		return
	}
	l.swept = true
	l.lastSweep = l.now()
	for key, e := range l.entries {
		if l.expired(e) {
			delete(l.entries, key)
		}
	}
}

// retryAfter rounds a remaining time up to whole seconds, at least 1.
func retryAfter(remaining time.Duration) int {
	seconds := math.Ceil(float64(remaining) / float64(time.Second))
	return int(math.Max(1, seconds))
}

// check reports whether key is blocked and, if so, the seconds until it is
// not. It changes no counts. The caller holds the lock.
func (l *Limiter) check(key string) (blocked bool, retryAfterSeconds int) {
	if e := l.current(key); e != nil {
		if e.attempts >= l.maxAttempts {
			return true, retryAfter(l.window - l.now().Sub(e.windowStart))
		}
		return false, 0
	}
	// A new key while the map is full (a flood of distinct keys the sweep
	// could not reclaim): refuse rather than grow without bound.
	if len(l.entries) >= l.sweepAt {
		l.sweep()
	}
	if len(l.entries) >= l.maxKeys {
		return true, retryAfter(l.window)
	}
	return false, 0
}

func (l *Limiter) consume(key string) {
	if e := l.current(key); e != nil {
		e.attempts++
		return
	}
	l.entries[key] = &entry{attempts: 1, windowStart: l.now()}
}

// Check is check under the lock.
func (l *Limiter) Check(key string) (blocked bool, retryAfterSeconds int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.check(key)
}

// Consume counts one attempt for key.
func (l *Limiter) Consume(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.consume(key)
}

// Size is the number of keys held.
func (l *Limiter) Size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

// Pair is one limiter and the key to count against it.
type Pair struct {
	Limiter *Limiter
	Key     string
}

// acquireMu serializes Acquire, so the check-all-then-consume-all step is one
// unit across limiters, as it was in Node's single thread: two concurrent
// requests cannot both pass on the same remaining quota.
var acquireMu sync.Mutex

// Acquire reserves one attempt against every pair at once. All are checked
// first and only then all consumed; nothing is consumed when any is blocked.
// retryAfterSeconds is the largest wait among the blocked ones.
func Acquire(pairs ...Pair) (allowed bool, retryAfterSeconds int) {
	acquireMu.Lock()
	defer acquireMu.Unlock()
	for _, p := range pairs {
		p.Limiter.mu.Lock()
		blocked, wait := p.Limiter.check(p.Key)
		p.Limiter.mu.Unlock()
		if blocked && wait > retryAfterSeconds {
			retryAfterSeconds = wait
		}
	}
	if retryAfterSeconds > 0 {
		return false, retryAfterSeconds
	}
	for _, p := range pairs {
		p.Limiter.Consume(p.Key)
	}
	return true, 0
}
