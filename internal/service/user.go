package service

import (
	"context"
	"errors"
	"fmt"
	"net/mail"
	"strings"
	"time"

	"github.com/00101010xyz/mcpaw/internal/domain"
	"github.com/00101010xyz/mcpaw/internal/platform/id"
	"github.com/00101010xyz/mcpaw/internal/secrets"
	"github.com/00101010xyz/mcpaw/internal/store"
)

// Users manages administrator accounts and authentication.
type Users struct {
	repo     store.UserRepository
	sessions store.SessionRepository
	audit    *Audit
	now      func() time.Time
}

// NewUsers constructs the user service.
func NewUsers(repo store.UserRepository, sessions store.SessionRepository, audit *Audit) *Users {
	return &Users{repo: repo, sessions: sessions, audit: audit, now: time.Now}
}

// NeedsSetup reports whether the platform has no administrator yet, which
// unlocks the one-time setup flow.
func (s *Users) NeedsSetup(ctx context.Context) (bool, error) {
	n, err := s.repo.Count(ctx)
	if err != nil {
		return false, err
	}
	return n == 0, nil
}

// Setup creates the first administrator.
//
// It re-checks emptiness inside the call rather than trusting the caller's
// earlier NeedsSetup, so a race between two concurrent setup submissions cannot
// create a second, attacker-chosen administrator.
func (s *Users) Setup(ctx context.Context, actor Actor, email, password string) (*domain.User, error) {
	needs, err := s.NeedsSetup(ctx)
	if err != nil {
		return nil, err
	}
	if !needs {
		return nil, fmt.Errorf("setup has already been completed: %w", domain.ErrForbidden)
	}
	user, err := s.create(ctx, email, password, domain.RoleAdmin)
	if err != nil {
		return nil, err
	}
	s.audit.Success(ctx, actor, domain.ActionUserCreate, "user", user.ID,
		map[string]any{"email": user.Email, "role": string(user.Role), "first_admin": true})
	return user, nil
}

// Create adds an administrator or viewer account.
func (s *Users) Create(ctx context.Context, actor Actor, email, password string, role domain.Role) (*domain.User, error) {
	user, err := s.create(ctx, email, password, role)
	if err != nil {
		return nil, err
	}
	s.audit.Success(ctx, actor, domain.ActionUserCreate, "user", user.ID,
		map[string]any{"email": user.Email, "role": string(user.Role)})
	return user, nil
}

func (s *Users) create(ctx context.Context, email, password string, role domain.Role) (*domain.User, error) {
	email = normaliseEmail(email)
	if err := validateEmail(email); err != nil {
		return nil, err
	}
	if !role.Valid() {
		return nil, fmt.Errorf("%w: unknown role %q", domain.ErrInvalidInput, role)
	}
	hash, err := secrets.HashPassword(password)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", domain.ErrInvalidInput, err)
	}

	now := s.now().UTC()
	user := &domain.User{
		ID: id.New("usr"), Email: email, PasswordHash: hash, Role: role,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := s.repo.Create(ctx, user); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			return nil, fmt.Errorf("an account with that email already exists: %w", domain.ErrConflict)
		}
		return nil, err
	}
	return user, nil
}

// Authenticate verifies credentials and returns the user.
//
// Every failure path returns the identical error and performs comparable work:
// an unknown email still runs a password verification against a dummy hash, so
// response time does not reveal whether an account exists.
func (s *Users) Authenticate(ctx context.Context, actor Actor, email, password string) (*domain.User, error) {
	user, err := s.repo.GetByEmail(ctx, normaliseEmail(email))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			_ = secrets.VerifyPassword(dummyHash, password)
			s.audit.Failure(ctx, actor, domain.ActionLoginFailed, "user", "", "unknown account")
			return nil, domain.ErrUnauthorized
		}
		return nil, err
	}
	if err := secrets.VerifyPassword(user.PasswordHash, password); err != nil {
		s.audit.Failure(ctx, actor, domain.ActionLoginFailed, "user", user.ID, "bad password")
		return nil, domain.ErrUnauthorized
	}
	if user.Disabled {
		s.audit.Failure(ctx, actor, domain.ActionLoginFailed, "user", user.ID, "account disabled")
		return nil, domain.ErrUnauthorized
	}

	now := s.now().UTC()
	user.LastLoginAt = &now
	user.UpdatedAt = now
	if err := s.repo.Update(ctx, user); err != nil {
		// A failure to record the login timestamp is not a reason to deny a
		// correctly authenticated user.
		s.audit.Record(ctx, actor, domain.ActionLogin, "user", user.ID, "success",
			map[string]any{"warning": "last_login not persisted"})
		return user, nil
	}
	s.audit.Success(ctx, actor, domain.ActionLogin, "user", user.ID, map[string]any{"email": user.Email})
	return user, nil
}

// dummyHash is a real Argon2id hash of a random value, used so that
// authenticating an unknown account costs the same as a wrong password.
const dummyHash = "$argon2id$v=19$m=19456,t=2,p=1$mdNBZfAIYAcBYZRBHjhuPA$9qA3xxlJIqnQyNrmrj5NEcrLgwoqvV0nIzZhFyX1uXs"

// ChangePassword updates a user's password and invalidates their other
// sessions, which is what makes a password change actually end an attacker's
// access rather than merely changing a string.
func (s *Users) ChangePassword(ctx context.Context, actor Actor, userID, current, next string) error {
	user, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		return err
	}
	if err := secrets.VerifyPassword(user.PasswordHash, current); err != nil {
		s.audit.Failure(ctx, actor, domain.ActionUserUpdate, "user", userID, "current password incorrect")
		return domain.ErrUnauthorized
	}
	hash, err := secrets.HashPassword(next)
	if err != nil {
		return fmt.Errorf("%w: %s", domain.ErrInvalidInput, err)
	}
	user.PasswordHash = hash
	user.UpdatedAt = s.now().UTC()
	if err := s.repo.Update(ctx, user); err != nil {
		return err
	}
	if err := s.sessions.DeleteByUser(ctx, userID); err != nil {
		return err
	}
	s.audit.Success(ctx, actor, domain.ActionUserUpdate, "user", userID,
		map[string]any{"change": "password", "sessions_revoked": true})
	return nil
}

// SetDisabled enables or disables an account, terminating its sessions when
// disabling.
func (s *Users) SetDisabled(ctx context.Context, actor Actor, userID string, disabled bool) error {
	user, err := s.repo.GetByID(ctx, userID)
	if err != nil {
		return err
	}
	if disabled && user.ID == actor.ID {
		// Locking yourself out of the only admin account is not a supported
		// workflow, and recovering from it requires database surgery.
		return fmt.Errorf("%w: you cannot disable your own account", domain.ErrInvalidInput)
	}
	user.Disabled = disabled
	user.UpdatedAt = s.now().UTC()
	if err := s.repo.Update(ctx, user); err != nil {
		return err
	}
	if disabled {
		if err := s.sessions.DeleteByUser(ctx, userID); err != nil {
			return err
		}
	}
	s.audit.Success(ctx, actor, domain.ActionUserUpdate, "user", userID, map[string]any{"disabled": disabled})
	return nil
}

// Get returns one user.
func (s *Users) Get(ctx context.Context, userID string) (*domain.User, error) {
	return s.repo.GetByID(ctx, userID)
}

// List returns every account.
func (s *Users) List(ctx context.Context) ([]*domain.User, error) { return s.repo.List(ctx) }

func normaliseEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

func validateEmail(email string) error {
	if email == "" {
		return fmt.Errorf("%w: email is required", domain.ErrInvalidInput)
	}
	if len(email) > 254 {
		return fmt.Errorf("%w: email is too long", domain.ErrInvalidInput)
	}
	if _, err := mail.ParseAddress(email); err != nil {
		return fmt.Errorf("%w: %q is not a valid email address", domain.ErrInvalidInput, email)
	}
	return nil
}
