package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/00101010xyz/mcpaw/internal/domain"
	"github.com/00101010xyz/mcpaw/internal/platform/id"
	"github.com/00101010xyz/mcpaw/internal/secrets"
	"github.com/00101010xyz/mcpaw/internal/store"
)

// Tokens manages the bearer credentials MCP clients present.
type Tokens struct {
	repo      store.TokenRepository
	instances store.InstanceRepository
	keyring   *secrets.Keyring
	audit     *Audit
	now       func() time.Time
}

// NewTokens constructs the token service.
func NewTokens(repo store.TokenRepository, instances store.InstanceRepository, keyring *secrets.Keyring, audit *Audit) *Tokens {
	return &Tokens{repo: repo, instances: instances, keyring: keyring, audit: audit, now: time.Now}
}

// CreateTokenInput describes a token to mint.
type CreateTokenInput struct {
	Name string
	// InstanceID scopes the token to one instance. Empty grants access to every
	// enabled instance, which the UI marks clearly as the broader choice.
	InstanceID string
	// TTL bounds the token's life. Zero means no expiry.
	TTL time.Duration
}

// CreateTokenResult carries the one and only chance to see the plaintext.
type CreateTokenResult struct {
	Token     *domain.Token
	Plaintext string
}

// Create mints a bearer token.
func (s *Tokens) Create(ctx context.Context, actor Actor, in CreateTokenInput) (*CreateTokenResult, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: a token name is required", domain.ErrInvalidInput)
	}
	if len(name) > 128 {
		return nil, fmt.Errorf("%w: token name is too long", domain.ErrInvalidInput)
	}
	if in.InstanceID != "" {
		if _, err := s.instances.Get(ctx, in.InstanceID); err != nil {
			return nil, err
		}
	}

	plaintext, prefix, err := secrets.NewToken()
	if err != nil {
		return nil, err
	}
	now := s.now().UTC()
	token := &domain.Token{
		ID: id.New("tok"), Name: name,
		Hash:   s.keyring.TokenLookupKey(plaintext),
		Prefix: prefix, InstanceID: in.InstanceID,
		CreatedBy: actor.ID, CreatedAt: now,
	}
	if in.TTL > 0 {
		expiry := now.Add(in.TTL)
		token.ExpiresAt = &expiry
	}
	if err := s.repo.Create(ctx, token); err != nil {
		return nil, err
	}

	scope := "all instances"
	if in.InstanceID != "" {
		scope = in.InstanceID
	}
	s.audit.Success(ctx, actor, domain.ActionTokenCreate, "token", token.ID,
		map[string]any{"name": name, "scope": scope, "expires": token.ExpiresAt != nil})
	return &CreateTokenResult{Token: token, Plaintext: plaintext}, nil
}

// Authenticate resolves a presented bearer token.
//
// Every rejection returns ErrUnauthorized with no detail: telling a caller
// whether a token is unknown, expired or revoked is free reconnaissance.
func (s *Tokens) Authenticate(ctx context.Context, presented string) (*domain.Token, error) {
	presented = strings.TrimSpace(presented)
	if presented == "" || !strings.HasPrefix(presented, secrets.TokenPrefix) {
		return nil, domain.ErrUnauthorized
	}
	token, err := s.repo.GetByLookupKey(ctx, s.keyring.TokenLookupKey(presented))
	if err != nil {
		if errors.Is(err, domain.ErrNotFound) {
			return nil, domain.ErrUnauthorized
		}
		return nil, err
	}
	if !token.Usable(s.now().UTC()) {
		return nil, domain.ErrUnauthorized
	}
	return token, nil
}

// TouchLastUsed records that a token was used.
//
// This is best-effort and deliberately non-fatal: it is a usability feature
// ("is this token still in use?"), and a write failure must not reject an
// otherwise valid request.
func (s *Tokens) TouchLastUsed(ctx context.Context, tokenID string) {
	_ = s.repo.TouchLastUsed(ctx, tokenID, s.now().UTC())
}

// Revoke permanently disables a token.
func (s *Tokens) Revoke(ctx context.Context, actor Actor, tokenID string) error {
	if err := s.repo.Revoke(ctx, tokenID, s.now().UTC()); err != nil {
		return err
	}
	s.audit.Success(ctx, actor, domain.ActionTokenRevoke, "token", tokenID, nil)
	return nil
}

// List returns every token. Only digests are stored, so no plaintext can be
// recovered here.
func (s *Tokens) List(ctx context.Context) ([]*domain.Token, error) { return s.repo.List(ctx) }
