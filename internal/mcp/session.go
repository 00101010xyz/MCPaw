package mcp

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"
	"time"
)

// Session records a client's handshake so later requests can be correlated in
// logs and so a reconfigured server can invite a client to re-initialize.
//
// MCP sessions are advisory: the specification permits a fully stateless server
// and MCPaw's tool calls need no session state. Keeping them anyway costs one
// map entry per client and buys correlation, per-client rate-limit keys and a
// clean way to expire clients that negotiated a protocol version we later stop
// supporting.
type Session struct {
	ID              string
	InstanceID      string
	ProtocolVersion string
	ClientName      string
	ClientVersion   string
	CreatedAt       time.Time
	LastSeenAt      time.Time
}

// SessionStore holds live sessions in memory.
//
// In-memory is the right choice here precisely because sessions are advisory:
// losing them on restart costs a client one re-initialize, whereas persisting
// them would add a write to the hot path for no functional gain. A multi-replica
// deployment simply sees a re-initialize when a client lands on another replica.
type SessionStore struct {
	ttl      time.Duration
	max      int
	now      func() time.Time
	mu       sync.Mutex
	sessions map[string]*Session
}

// SessionConfig tunes the store.
type SessionConfig struct {
	// TTL is how long a session survives without activity.
	TTL time.Duration
	// Max bounds the number of live sessions, so a client that initializes in a
	// loop cannot exhaust memory.
	Max int
}

// NewSessionStore creates an empty store.
func NewSessionStore(cfg SessionConfig) *SessionStore {
	if cfg.TTL <= 0 {
		cfg.TTL = time.Hour
	}
	if cfg.Max <= 0 {
		cfg.Max = 10_000
	}
	return &SessionStore{ttl: cfg.TTL, max: cfg.Max, now: time.Now, sessions: map[string]*Session{}}
}

// Create registers a new session and returns it.
func (s *SessionStore) Create(instanceID, protocolVersion string, client Implementation) (*Session, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return nil, fmt.Errorf("mcp: generating session id: %w", err)
	}
	now := s.now()
	sess := &Session{
		ID:              base64.RawURLEncoding.EncodeToString(raw),
		InstanceID:      instanceID,
		ProtocolVersion: protocolVersion,
		ClientName:      client.Name,
		ClientVersion:   client.Version,
		CreatedAt:       now,
		LastSeenAt:      now,
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(now)
	if len(s.sessions) >= s.max {
		return nil, fmt.Errorf("mcp: too many active sessions")
	}
	s.sessions[sess.ID] = sess
	return sess, nil
}

// Get returns a live session and refreshes its activity timestamp.
func (s *SessionStore) Get(id string) (*Session, bool) {
	if id == "" {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	sess, ok := s.sessions[id]
	if !ok {
		return nil, false
	}
	now := s.now()
	if now.Sub(sess.LastSeenAt) > s.ttl {
		delete(s.sessions, id)
		return nil, false
	}
	sess.LastSeenAt = now
	return sess, true
}

// Delete terminates a session.
func (s *SessionStore) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

// DeleteByInstance drops every session belonging to an instance, used when the
// instance is reconfigured or removed.
func (s *SessionStore) DeleteByInstance(instanceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for id, sess := range s.sessions {
		if sess.InstanceID == instanceID {
			delete(s.sessions, id)
		}
	}
}

// Len reports the number of live sessions.
func (s *SessionStore) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneLocked(s.now())
	return len(s.sessions)
}

func (s *SessionStore) pruneLocked(now time.Time) {
	for id, sess := range s.sessions {
		if now.Sub(sess.LastSeenAt) > s.ttl {
			delete(s.sessions, id)
		}
	}
}
