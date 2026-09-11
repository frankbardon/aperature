package rules

import (
	"sync"
	"sync/atomic"
	"time"
)

// Clock is the engine's SINGLE time source. It is read for two things:
//
//   - compiled-rule cache TTL expiry (this file), and
//   - the per-decision reference instant a rule's date comparisons resolve
//     against (now.go).
//
// One clock rather than two is deliberate. An engine holding two independent
// ideas of "now" can have them disagree — a cache entry expiring against one
// instant while a date rule decides against another — and that is a worse failure
// than the coupling it would avoid. The coupling's visible cost is that a test
// pinning the clock for a month-end date fixture also freezes cache expiry; see
// WithClock.
//
// It is injected so tests drive both behaviours deterministically instead of
// sleeping; production uses the real clock.
type Clock interface {
	Now() time.Time
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

// CacheStats reports the compiled-rule cache's counters. They expose the
// hit/miss/eviction behaviour the latency benchmark (E4-S4) tunes against.
type CacheStats struct {
	Hits      uint64
	Misses    uint64
	Evictions uint64
	Entries   int
}

// compiledCache is the concurrency-safe compiled-rule cache. It keys a *Compiled
// by the rule's canonical hash, so distinct rule references whose ASTs render to
// the same expression share one compiled program. An optional TTL (with the
// injected clock) bounds entry lifetime; TTL <= 0 keeps entries until explicitly
// invalidated. The cache is the NFR lever that keeps per-Check rule cost bounded:
// the expensive expr-lang compile happens once per canonical form.
//
// mu guards the entries map only. The counters are atomics, so the hot path — a
// hash that is present and unexpired — takes the READ lock and nothing else.
// They used to be plain fields guarded by mu, which meant a cache HIT had to
// acquire the WRITE lock purely to bump a number: every concurrent rule
// evaluation serialised on that bump even though none of them touched the map.
// That was tolerable while a decision meant one rule evaluation, but a
// rule-backed Enumerate evaluates the rule once per candidate (twice, in fact —
// once to gather the grant's members and once in the per-candidate decision), so
// the contention multiplies by the candidate count.
//
// The counters' meaning is unchanged: hits counts every get that returned a
// live entry, misses every get that did not, evictions every entry dropped for
// expiry or invalidation. Only the synchronisation changed.
type compiledCache struct {
	mu      sync.RWMutex
	ttl     time.Duration
	clock   Clock
	entries map[[32]byte]cacheEntry

	hits      atomic.Uint64
	misses    atomic.Uint64
	evictions atomic.Uint64
}

type cacheEntry struct {
	compiled  *Compiled
	expiresAt time.Time // zero when no TTL applies
}

func newCompiledCache(ttl time.Duration, clock Clock) *compiledCache {
	if clock == nil {
		clock = realClock{}
	}
	return &compiledCache{
		ttl:     ttl,
		clock:   clock,
		entries: make(map[[32]byte]cacheEntry),
	}
}

// get returns the cached program for hash when present and unexpired. An expired
// entry is dropped and counted as a miss (and an eviction).
//
// The hit path holds only the read lock; the counter bump is atomic. The write
// lock is taken solely when the map may have to change (an expired entry to
// delete) or when a second look is needed because the first read found nothing.
func (c *compiledCache) get(key [32]byte) (*Compiled, bool) {
	c.mu.RLock()
	e, ok := c.entries[key]
	c.mu.RUnlock()
	if ok && !c.expired(e) {
		c.hits.Add(1)
		return e.compiled, true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Re-read under the write lock: another goroutine may have refreshed it.
	if e, ok := c.entries[key]; ok {
		if !c.expired(e) {
			c.hits.Add(1)
			return e.compiled, true
		}
		delete(c.entries, key)
		c.evictions.Add(1)
	}
	c.misses.Add(1)
	return nil, false
}

// put stores compiled under its own hash, stamping an expiry when a TTL applies.
func (c *compiledCache) put(compiled *Compiled) {
	var expiresAt time.Time
	if c.ttl > 0 {
		expiresAt = c.clock.Now().Add(c.ttl)
	}
	c.mu.Lock()
	c.entries[compiled.key] = cacheEntry{compiled: compiled, expiresAt: expiresAt}
	c.mu.Unlock()
}

func (c *compiledCache) expired(e cacheEntry) bool {
	if e.expiresAt.IsZero() {
		return false
	}
	return !c.clock.Now().Before(e.expiresAt)
}

// invalidate drops a single cached entry by cache key, reporting whether one was
// present.
func (c *compiledCache) invalidate(key [32]byte) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.entries[key]; ok {
		delete(c.entries, key)
		c.evictions.Add(1)
		return true
	}
	return false
}

// clear drops every cached entry.
func (c *compiledCache) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.evictions.Add(uint64(len(c.entries)))
	c.entries = make(map[[32]byte]cacheEntry)
}

// stats samples the counters and the entry count. Each counter is read
// atomically; the four values are not a single instant's snapshot, because
// taking one would mean re-serialising the hit path on a lock that exists only
// for observability. Callers use it for observability and for sequential tests,
// where it is exact.
func (c *compiledCache) stats() CacheStats {
	c.mu.RLock()
	entries := len(c.entries)
	c.mu.RUnlock()
	return CacheStats{
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Evictions: c.evictions.Load(),
		Entries:   entries,
	}
}
