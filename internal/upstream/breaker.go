package upstream

import (
	"errors"
	"sync"
	"time"
)

// Circuit breaker states.
const (
	StateClosed   = "closed"
	StateOpen     = "open"
	StateHalfOpen = "half-open"
)

// ErrCircuitOpen is returned when the breaker is shedding load for a key.
var ErrCircuitOpen = errors.New("upstream is unavailable (circuit breaker open)")

// BreakerConfig tunes failure detection.
type BreakerConfig struct {
	// FailureThreshold is the number of consecutive failures that opens the
	// circuit.
	FailureThreshold int
	// OpenDuration is how long the circuit stays open before a trial request is
	// allowed through.
	OpenDuration time.Duration
	// HalfOpenSuccesses is the number of consecutive successes in half-open
	// state required to close the circuit again.
	HalfOpenSuccesses int
}

func (c BreakerConfig) withDefaults() BreakerConfig {
	if c.FailureThreshold <= 0 {
		c.FailureThreshold = 5
	}
	if c.OpenDuration <= 0 {
		c.OpenDuration = 30 * time.Second
	}
	if c.HalfOpenSuccesses <= 0 {
		c.HalfOpenSuccesses = 2
	}
	return c
}

// Breaker is a per-key circuit breaker.
//
// Its job is to fail fast when an upstream is down. Without it, an unreachable
// Zotero desktop app turns every tool call into a full connection timeout, and
// the platform's concurrency slots fill with calls that were never going to
// succeed.
type Breaker struct {
	cfg   BreakerConfig
	now   func() time.Time
	mu    sync.Mutex
	state map[string]*breakerEntry
}

type breakerEntry struct {
	state        string
	failures     int
	successes    int
	openedAt     time.Time
	lastActivity time.Time
}

// NewBreaker creates a breaker with the given configuration.
func NewBreaker(cfg BreakerConfig) *Breaker {
	return &Breaker{cfg: cfg.withDefaults(), now: time.Now, state: map[string]*breakerEntry{}}
}

// Allow reports whether a call for key may proceed.
func (b *Breaker) Allow(key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	e := b.entry(key)
	now := b.now()
	e.lastActivity = now

	switch e.state {
	case StateOpen:
		if now.Sub(e.openedAt) < b.cfg.OpenDuration {
			return ErrCircuitOpen
		}
		// The cooldown has elapsed: let one request through to probe the
		// upstream rather than waiting for a human to notice.
		e.state = StateHalfOpen
		e.successes = 0
		return nil
	default:
		return nil
	}
}

// Record reports the outcome of a call.
func (b *Breaker) Record(key string, success bool) {
	b.mu.Lock()
	defer b.mu.Unlock()

	e := b.entry(key)
	now := b.now()
	e.lastActivity = now

	if success {
		switch e.state {
		case StateHalfOpen:
			e.successes++
			if e.successes >= b.cfg.HalfOpenSuccesses {
				e.state = StateClosed
				e.failures = 0
				e.successes = 0
			}
		default:
			e.state = StateClosed
			e.failures = 0
		}
		return
	}

	e.failures++
	e.successes = 0
	// A failure while probing means the upstream is still down: re-open
	// immediately instead of spending the whole threshold again.
	if e.state == StateHalfOpen || e.failures >= b.cfg.FailureThreshold {
		e.state = StateOpen
		e.openedAt = now
	}
}

// State reports the breaker state for a key, for metrics and the admin UI.
func (b *Breaker) State(key string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	e, ok := b.state[key]
	if !ok {
		return StateClosed
	}
	if e.state == StateOpen && b.now().Sub(e.openedAt) >= b.cfg.OpenDuration {
		return StateHalfOpen
	}
	return e.state
}

// Reset clears the breaker for a key, used when an instance is reconfigured:
// the operator has changed something, so previous failures no longer describe
// the current target.
func (b *Breaker) Reset(key string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.state, key)
}

// entry fetches or creates the state for a key. The caller must hold the lock.
func (b *Breaker) entry(key string) *breakerEntry {
	e, ok := b.state[key]
	if !ok {
		e = &breakerEntry{state: StateClosed}
		b.state[key] = e
		b.sweepLocked()
	}
	return e
}

// maxBreakerKeys bounds the state map. Keys are instance IDs, so growth is
// naturally bounded, but an unbounded map reachable from request handling is
// exactly the kind of thing that becomes a leak after a refactor.
const maxBreakerKeys = 4096

func (b *Breaker) sweepLocked() {
	if len(b.state) <= maxBreakerKeys {
		return
	}
	cutoff := b.now().Add(-time.Hour)
	for k, e := range b.state {
		if e.state == StateClosed && e.lastActivity.Before(cutoff) {
			delete(b.state, k)
		}
	}
}
