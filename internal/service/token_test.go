package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/00101010xyz/mcpaw/internal/domain"
)

func TestTokensCreateAndAuthenticate(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	res, err := env.Tokens.Create(ctx, systemActor(), CreateTokenInput{Name: "ci"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if res.Plaintext == "" {
		t.Fatal("Create must return the plaintext exactly once")
	}

	got, err := env.Tokens.Authenticate(ctx, res.Plaintext)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if got.ID != res.Token.ID {
		t.Errorf("Authenticate resolved token %q, want %q", got.ID, res.Token.ID)
	}
}

func TestTokensCreateRejectsBlankName(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.Tokens.Create(context.Background(), systemActor(), CreateTokenInput{Name: "  "})
	if !errors.Is(err, domain.ErrInvalidInput) {
		t.Errorf("error = %v, want ErrInvalidInput", err)
	}
}

func TestTokensCreateValidatesScopedInstance(t *testing.T) {
	env := newTestEnv(t)
	_, err := env.Tokens.Create(context.Background(), systemActor(), CreateTokenInput{
		Name: "scoped", InstanceID: "does-not-exist",
	})
	if !errors.Is(err, domain.ErrNotFound) {
		t.Errorf("error = %v, want ErrNotFound for a token scoped to a nonexistent instance", err)
	}
}

// Every rejection reason collapses to the same sentinel with no detail, so a
// caller cannot distinguish "wrong token" from "expired" from "revoked" —
// that distinction is free reconnaissance for an attacker guessing tokens.
func TestTokensAuthenticateRejectionsAreUniform(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	cases := []struct {
		name      string
		presented func() string
	}{
		{"empty", func() string { return "" }},
		{"garbage", func() string { return "not-a-token-at-all" }},
		{"wrong prefix", func() string { return "wrongprefix_abc123" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := env.Tokens.Authenticate(ctx, tc.presented())
			if !errors.Is(err, domain.ErrUnauthorized) {
				t.Errorf("error = %v, want ErrUnauthorized", err)
			}
		})
	}
}

func TestTokensAuthenticateRejectsRevoked(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	res, err := env.Tokens.Create(ctx, systemActor(), CreateTokenInput{Name: "revoke-me"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := env.Tokens.Revoke(ctx, systemActor(), res.Token.ID); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if _, err := env.Tokens.Authenticate(ctx, res.Plaintext); !errors.Is(err, domain.ErrUnauthorized) {
		t.Errorf("error = %v, want ErrUnauthorized for a revoked token", err)
	}
}

func TestTokensAuthenticateRejectsExpired(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	res, err := env.Tokens.Create(ctx, systemActor(), CreateTokenInput{Name: "expiring", TTL: time.Hour})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Fast-forward the service's clock past expiry rather than sleeping.
	env.Tokens.now = func() time.Time { return time.Now().Add(2 * time.Hour) }

	if _, err := env.Tokens.Authenticate(ctx, res.Plaintext); !errors.Is(err, domain.ErrUnauthorized) {
		t.Errorf("error = %v, want ErrUnauthorized for an expired token", err)
	}
}

// This is the scoping contract BearerAuth's middleware relies on: a
// scoped token must not authorise any instance but its own; an unscoped
// token authorises every instance.
func TestTokenScopingContract(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	instA, err := env.Instances.Create(ctx, systemActor(), zoteroCreateInput("scope-a"))
	if err != nil {
		t.Fatalf("Create instance A: %v", err)
	}
	instB, err := env.Instances.Create(ctx, systemActor(), zoteroCreateInput("scope-b"))
	if err != nil {
		t.Fatalf("Create instance B: %v", err)
	}

	scoped, err := env.Tokens.Create(ctx, systemActor(), CreateTokenInput{Name: "scoped", InstanceID: instA.ID})
	if err != nil {
		t.Fatalf("Create scoped token: %v", err)
	}
	unscoped, err := env.Tokens.Create(ctx, systemActor(), CreateTokenInput{Name: "unscoped"})
	if err != nil {
		t.Fatalf("Create unscoped token: %v", err)
	}

	scopedToken, err := env.Tokens.Authenticate(ctx, scoped.Plaintext)
	if err != nil {
		t.Fatalf("Authenticate scoped: %v", err)
	}
	if !scopedToken.Scopes(instA.ID) {
		t.Error("a token scoped to instance A must scope to instance A")
	}
	if scopedToken.Scopes(instB.ID) {
		t.Fatal("a token scoped to instance A must NOT scope to instance B — this is the cross-instance leak the bearer middleware depends on being impossible")
	}

	unscopedToken, err := env.Tokens.Authenticate(ctx, unscoped.Plaintext)
	if err != nil {
		t.Fatalf("Authenticate unscoped: %v", err)
	}
	if !unscopedToken.Scopes(instA.ID) || !unscopedToken.Scopes(instB.ID) {
		t.Error("an unscoped token must authorise every instance")
	}
}

func TestTokensRevokeIsIdempotentAtServiceLevel(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	res, err := env.Tokens.Create(ctx, systemActor(), CreateTokenInput{Name: "double-revoke"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := env.Tokens.Revoke(ctx, systemActor(), res.Token.ID); err != nil {
		t.Fatalf("first Revoke: %v", err)
	}
	if err := env.Tokens.Revoke(ctx, systemActor(), res.Token.ID); err != nil {
		t.Errorf("second Revoke: %v, want no error re-revoking an already-revoked token", err)
	}
}
