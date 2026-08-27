package service

import (
	"context"
	"errors"
	"time"

	"github.com/00101010xyz/mcpaw/internal/domain"
	"github.com/00101010xyz/mcpaw/internal/secrets"
	"github.com/00101010xyz/mcpaw/internal/store"
)

// Sessions manages authenticated web sessions.
type Sessions struct {
	repo    store.SessionRepository
	users   store.UserRepository
	keyring *secrets.Keyring
	audit   *Audit

	idleTimeout     time.Duration
	absoluteTimeout time.Duration
	now             func() time.Time
}

// SessionsConfig wires the session service.
type SessionsConfig struct {
	Repo    store.SessionRepository
	Users   store.UserRepository
	Keyring *secrets.Keyring
	Audit   *Audit
	// IdleTimeout extends a session on each request; AbsoluteTimeout is the
	// hard ceiling that no amount of activity can extend past, which bounds how
	// long a stolen cookie stays useful.
	IdleTimeout     time.Duration
	AbsoluteTimeout time.Duration
}

// NewSessions constructs the session service.
func NewSessions(cfg SessionsConfig) *Sessions {
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = 2 * time.Hour
	}
	if cfg.AbsoluteTimeout <= 0 {
		cfg.AbsoluteTimeout = 24 * time.Hour
	}
	return &Sessions{
		repo: cfg.Repo, users: cfg.Users, keyring: cfg.Keyring, audit: cfg.Audit,
		idleTimeout: cfg.IdleTimeout, absoluteTimeout: cfg.AbsoluteTimeout, now: time.Now,
	}
}

// Issue creates a session for a user and returns the cookie value plus the CSRF
// token bound to it.
func (s *Sessions) Issue(ctx context.Context, user *domain.User, ip, userAgent string) (cookieValue string, session *domain.Session, err error) {
	cookieValue, storedID, err := s.keyring.NewSessionID()
	if err != nil {
		return "", nil, err
	}
	csrf, err := secrets.NewCSRFToken()
	if err != nil {
		return "", nil, err
	}

	now := s.now().UTC()
	session = &domain.Session{
		ID: storedID, UserID: user.ID, CSRFToken: csrf,
		CreatedAt: now, LastSeenAt: now,
		// The initial expiry is the sooner of the two windows; Validate keeps
		// extending it up to the absolute ceiling.
		ExpiresAt: now.Add(minDuration(s.idleTimeout, s.absoluteTimeout)),
		IP:        truncateString(ip, 64), UserAgent: truncateString(userAgent, 256),
	}
	if err := s.repo.Create(ctx, session); err != nil {
		return "", nil, err
	}
	return cookieValue, session, nil
}

// Validate resolves a session cookie to its user, sliding the idle window.
//
// It returns ErrUnauthorized for every failure mode — unknown, expired,
// deleted user, disabled user — because the caller's only correct response to
// any of them is the same: clear the cookie and ask for credentials.
func (s *Sessions) Validate(ctx context.Context, cookieValue string) (*domain.User, *domain.Session, error) {
	if cookieValue == "" {
		return nil, nil, domain.ErrUnauthorized
	}
	session, err := s.repo.Get(ctx, s.keyring.SessionLookupKey(cookieValue))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, nil, domain.ErrUnauthorized
		}
		return nil, nil, err
	}

	now := s.now().UTC()
	if session.Expired(now) || now.Sub(session.CreatedAt) > s.absoluteTimeout {
		_ = s.repo.Delete(ctx, session.ID)
		return nil, nil, domain.ErrUnauthorized
	}

	user, err := s.users.GetByID(ctx, session.UserID)
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			_ = s.repo.Delete(ctx, session.ID)
			return nil, nil, domain.ErrUnauthorized
		}
		return nil, nil, err
	}
	if user.Disabled {
		_ = s.repo.Delete(ctx, session.ID)
		return nil, nil, domain.ErrUnauthorized
	}

	// Slide the idle window, but never past the absolute ceiling.
	newExpiry := now.Add(s.idleTimeout)
	if ceiling := session.CreatedAt.Add(s.absoluteTimeout); newExpiry.After(ceiling) {
		newExpiry = ceiling
	}
	if newExpiry.After(session.ExpiresAt) {
		if err := s.repo.Touch(ctx, session.ID, now, newExpiry); err != nil {
			return nil, nil, err
		}
		session.ExpiresAt = newExpiry
	}
	session.LastSeenAt = now
	return user, session, nil
}

// Revoke terminates one session.
func (s *Sessions) Revoke(ctx context.Context, cookieValue string) error {
	if cookieValue == "" {
		return nil
	}
	return s.repo.Delete(ctx, s.keyring.SessionLookupKey(cookieValue))
}

// PruneExpired deletes sessions past their expiry.
func (s *Sessions) PruneExpired(ctx context.Context) (int64, error) {
	return s.repo.DeleteExpired(ctx, s.now().UTC())
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func truncateString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max]
}
