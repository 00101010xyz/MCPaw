package upstream

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrRateLimited is returned when a caller has exhausted its token bucket.
var ErrRateLimited = errors.New("rate limit exceeded")

// Limiter decides whether a call may proceed under a per-key rate budget.
//
// It is an interface so a multi-replica deployment can substitute a shared
// implementation (Redis, for example) without touching call sites. The
// in-process implementation below is correct for a single container and is
// documented as such.
type Limiter interface {
	// Allow consumes one token for key, given a budget of ratePerMin. A
	// non-positive budget means unlimited.
	Allow(key string, ratePerMin int) bool
}

// MemoryLimiter is an in-process token bucket limiter.
type MemoryLimiter struct {
	now     func() time.Time
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens   float64
	capacity float64
	lastFill time.Time
}

// NewMemoryLimiter creates an empty limiter.
func NewMemoryLimiter() *MemoryLimiter {
	return &MemoryLimiter{now: time.Now, buckets: map[string]*bucket{}}
}

// Allow implements Limiter.
func (l *MemoryLimiter) Allow(key string, ratePerMin int) bool {
	if ratePerMin <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	capacity := float64(ratePerMin)
	b, ok := l.buckets[key]
	if !ok {
		// A new bucket starts full so the first burst after a restart is not
		// penalised.
		b = &bucket{tokens: capacity, capacity: capacity, lastFill: now}
		l.buckets[key] = b
		l.sweepLocked(now)
	}
	if b.capacity != capacity {
		// The operator changed the budget; rescale rather than discard, so a
		// configuration change cannot be used to reset an exhausted bucket.
		if b.tokens > capacity {
			b.tokens = capacity
		}
		b.capacity = capacity
	}

	elapsed := now.Sub(b.lastFill).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * capacity / 60.0
		if b.tokens > capacity {
			b.tokens = capacity
		}
		b.lastFill = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

const maxLimiterKeys = 8192

func (l *MemoryLimiter) sweepLocked(now time.Time) {
	if len(l.buckets) <= maxLimiterKeys {
		return
	}
	cutoff := now.Add(-10 * time.Minute)
	for k, b := range l.buckets {
		if b.lastFill.Before(cutoff) {
			delete(l.buckets, k)
		}
	}
}

// Gate bounds how many calls may be in flight per key at once.
//
// Rate limiting alone does not bound memory: sixty calls per minute against an
// upstream that takes a minute each means sixty concurrent buffered responses.
// The gate is what makes worst-case memory a product of two numbers the
// operator can see.
type Gate struct {
	mu   sync.Mutex
	sems map[string]chan struct{}
}

// NewGate creates an empty gate.
func NewGate() *Gate { return &Gate{sems: map[string]chan struct{}{}} }

// Acquire blocks until a slot is free, the context is done, or the key has no
// limit. The returned release function is always safe to call.
func (g *Gate) Acquire(ctx context.Context, key string, max int) (func(), error) {
	if max <= 0 {
		return func() {}, nil
	}
	sem := g.sem(key, max)
	select {
	case sem <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-sem }) }, nil
	case <-ctx.Done():
		return func() {}, ctx.Err()
	}
}

func (g *Gate) sem(key string, max int) chan struct{} {
	g.mu.Lock()
	defer g.mu.Unlock()
	s, ok := g.sems[key]
	if !ok || cap(s) != max {
		// Replacing the channel when the limit changes leaks at most the
		// in-flight holders of the old one, which drain on their own.
		s = make(chan struct{}, max)
		g.sems[key] = s
	}
	return s
}

// Forget drops the semaphore for a key, used when an instance is deleted.
func (g *Gate) Forget(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.sems, key)
}
