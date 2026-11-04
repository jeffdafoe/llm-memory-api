// Package ttlcache is the in-memory cache behind the session, actor and
// permission caches. Each entry has its own expiry.
//
// Two rules the plain Node Maps did not have:
//
//   - Invalidation wins over a load in flight. A loader reads Generation()
//     before its database query and stores with SetIfCurrent; Delete and
//     Clear bump the generation, so a result read before the invalidation is
//     never published after it. Without this a logout (or a permission
//     change) racing a request could put the revoked entry back for a whole
//     TTL.
//   - Expired entries are removed. Writes sweep the map at most once a
//     minute, so tokens that are never presented again do not pile up.
package ttlcache

import (
	"sync"
	"time"
)

const sweepInterval = time.Minute

type entry[V any] struct {
	value   V
	expires time.Time
}

type Cache[K comparable, V any] struct {
	// Now is the clock; nil means time.Now. Tests replace it.
	Now func() time.Time

	mu        sync.Mutex
	entries   map[K]entry[V]
	gen       uint64
	lastSweep time.Time
}

func (c *Cache[K, V]) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// Get returns a live entry.
func (c *Cache[K, V]) Get(key K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if ok && e.expires.After(c.now()) {
		return e.value, true
	}
	var zero V
	return zero, false
}

// Generation is read by a loader before it queries the database.
func (c *Cache[K, V]) Generation() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gen
}

// SetIfCurrent stores the entry only when no invalidation happened since the
// loader read gen. It reports whether the entry was stored.
func (c *Cache[K, V]) SetIfCurrent(gen uint64, key K, value V, expires time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if gen != c.gen {
		return false
	}
	now := c.now()
	if c.entries == nil {
		c.entries = map[K]entry[V]{}
	}
	if now.Sub(c.lastSweep) >= sweepInterval {
		for k, e := range c.entries {
			if !e.expires.After(now) {
				delete(c.entries, k)
			}
		}
		c.lastSweep = now
	}
	c.entries[key] = entry[V]{value: value, expires: expires}
	return true
}

// Delete drops one entry and invalidates every load in flight.
func (c *Cache[K, V]) Delete(key K) {
	c.mu.Lock()
	delete(c.entries, key)
	c.gen++
	c.mu.Unlock()
}

// Clear drops every entry and invalidates every load in flight.
func (c *Cache[K, V]) Clear() {
	c.mu.Lock()
	c.entries = nil
	c.gen++
	c.mu.Unlock()
}

// Len counts stored entries, live or not yet swept. For tests.
func (c *Cache[K, V]) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
