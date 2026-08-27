package webui

import (
	"sync"
	"time"
)

// FlashLevel classifies a one-shot message.
type FlashLevel string

// Flash levels.
const (
	FlashSuccess FlashLevel = "success"
	FlashError   FlashLevel = "error"
	FlashInfo    FlashLevel = "info"
)

// Flash is a message displayed once after a redirect.
//
// Secret carries a value shown exactly once — a freshly minted bearer token.
// It is deliberately a distinct field so the template can render it with the
// "copy this now, it will not be shown again" treatment rather than as ordinary
// prose.
type Flash struct {
	Level   FlashLevel
	Message string
	Secret  string
	Details []string
	Test    *TestOutcome
}

// TestOutcome is the render model for a connection test result.
type TestOutcome struct {
	OK         bool
	Tool       string
	StatusCode int
	DurationMS int64
	Message    string
	Hint       string
	Preview    string
}

// flashStore holds pending messages server-side, keyed by session.
//
// Keeping them in memory rather than in a cookie means the browser never
// carries a value the server will later render: a tampered cookie cannot inject
// text into the page, and a one-time token is never written to disk by the
// browser.
type flashStore struct {
	ttl   time.Duration
	now   func() time.Time
	mu    sync.Mutex
	items map[string]flashEntry
}

type flashEntry struct {
	flash   *Flash
	expires time.Time
}

func newFlashStore(ttl time.Duration) *flashStore {
	if ttl <= 0 {
		ttl = 2 * time.Minute
	}
	return &flashStore{ttl: ttl, now: time.Now, items: map[string]flashEntry{}}
}

func (s *flashStore) set(sessionID string, f *Flash) {
	if sessionID == "" || f == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked()
	s.items[sessionID] = flashEntry{flash: f, expires: s.now().Add(s.ttl)}
}

// take returns and removes the pending flash, so a refresh does not redisplay
// it — least of all a one-time secret.
func (s *flashStore) take(sessionID string) *Flash {
	if sessionID == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.items[sessionID]
	if !ok {
		return nil
	}
	delete(s.items, sessionID)
	if s.now().After(entry.expires) {
		return nil
	}
	return entry.flash
}

func (s *flashStore) pruneLocked() {
	now := s.now()
	for k, v := range s.items {
		if now.After(v.expires) {
			delete(s.items, k)
		}
	}
}
