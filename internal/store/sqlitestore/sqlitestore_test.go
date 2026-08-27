package sqlitestore

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/00101010xyz/mcpaw/internal/domain"
	"github.com/00101010xyz/mcpaw/internal/platform/id"
	"github.com/00101010xyz/mcpaw/internal/store"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedConnector(t *testing.T, s *Store) *domain.ConnectorRecord {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	c := &domain.ConnectorRecord{
		ID: "zotero-local", Name: "Zotero", Version: "1.0.0",
		Source: domain.SourceBuiltin, Manifest: []byte("kind: Connector"),
		Checksum: "abc", CreatedAt: now, UpdatedAt: now,
	}
	if err := s.Connectors().Upsert(context.Background(), c); err != nil {
		t.Fatalf("seed connector: %v", err)
	}
	return c
}

func seedInstance(t *testing.T, s *Store, slug string) *domain.Instance {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Millisecond)
	i := &domain.Instance{
		ID: id.New("inst"), Slug: slug, Name: "My Zotero", ConnectorID: "zotero-local",
		BaseURL: "http://host.docker.internal:23119", Variables: map[string]string{"userId": "0"},
		Enabled: true, AllowPrivateNetwork: true, TimeoutMS: 15000, RateLimitPerMin: 120,
		MaxConcurrent: 4, MaxResponseBytes: 1 << 20, CreatedAt: now, UpdatedAt: now, Version: 1,
	}
	if err := s.Instances().Create(context.Background(), i); err != nil {
		t.Fatalf("seed instance: %v", err)
	}
	return i
}

func TestMigrationsAreIdempotent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "idem.db")
	s1, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	_ = s1.Close()
	s2, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("reopening an already-migrated database: %v", err)
	}
	_ = s2.Close()
}

func TestUserLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)

	u := &domain.User{
		ID: id.New("usr"), Email: "Admin@Example.COM", PasswordHash: "$argon2id$fake",
		Role: domain.RoleAdmin, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.Users().Create(ctx, u); err != nil {
		t.Fatalf("Create: %v", err)
	}

	n, err := s.Users().Count(ctx)
	if err != nil || n != 1 {
		t.Fatalf("Count = %d, %v; want 1, nil", n, err)
	}

	// Email lookup must be case-insensitive, otherwise an operator who types
	// their address with different capitalisation is locked out.
	got, err := s.Users().GetByEmail(ctx, "admin@example.com")
	if err != nil {
		t.Fatalf("GetByEmail: %v", err)
	}
	if got.ID != u.ID || got.Role != domain.RoleAdmin {
		t.Fatalf("unexpected user %+v", got)
	}

	// Duplicate emails differing only in case must conflict.
	dup := *u
	dup.ID = id.New("usr")
	dup.Email = "ADMIN@example.com"
	if err := s.Users().Create(ctx, &dup); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate email: got %v, want ErrConflict", err)
	}

	login := now.Add(time.Minute)
	got.LastLoginAt = &login
	got.UpdatedAt = login
	if err := s.Users().Update(ctx, got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	reloaded, err := s.Users().GetByID(ctx, u.ID)
	if err != nil {
		t.Fatalf("GetByID: %v", err)
	}
	if reloaded.LastLoginAt == nil || !reloaded.LastLoginAt.Equal(login) {
		t.Fatalf("last login not persisted: %+v", reloaded.LastLoginAt)
	}
}

func TestUserNotFound(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.Users().GetByID(context.Background(), "missing"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestInvalidRoleIsRejectedByTheSchema(t *testing.T) {
	s := newTestStore(t)
	now := time.Now().UTC()
	u := &domain.User{ID: id.New("usr"), Email: "x@example.com", PasswordHash: "h",
		Role: domain.Role("superuser"), CreatedAt: now, UpdatedAt: now}
	if err := s.Users().Create(context.Background(), u); !errors.Is(err, domain.ErrInvalidInput) {
		t.Fatalf("got %v, want ErrInvalidInput from the CHECK constraint", err)
	}
}

func TestInstanceCRUDAndVersionBump(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedConnector(t, s)
	i := seedInstance(t, s, "zotero")

	got, err := s.Instances().GetBySlug(ctx, "zotero")
	if err != nil {
		t.Fatalf("GetBySlug: %v", err)
	}
	if got.Variables["userId"] != "0" || !got.AllowPrivateNetwork {
		t.Fatalf("configuration did not round trip: %+v", got)
	}

	before := got.Version
	got.Name = "Renamed"
	got.UpdatedAt = time.Now().UTC()
	if err := s.Instances().Update(ctx, got); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.Version != before+1 {
		t.Fatalf("in-memory version = %d, want %d", got.Version, before+1)
	}
	reloaded, _ := s.Instances().Get(ctx, i.ID)
	if reloaded.Version != before+1 || reloaded.Name != "Renamed" {
		t.Fatalf("persisted version/name wrong: %+v", reloaded)
	}

	if err := s.Instances().Create(ctx, i); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("duplicate slug: got %v, want ErrConflict", err)
	}
}

// Deleting a connector that still has instances must be refused, not silently
// orphan the instances.
func TestConnectorDeleteIsRestrictedByInstances(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedConnector(t, s)
	i := seedInstance(t, s, "zotero")

	if err := s.Connectors().Delete(ctx, "zotero-local"); !errors.Is(err, domain.ErrConflict) {
		t.Fatalf("got %v, want ErrConflict", err)
	}
	if err := s.Instances().Delete(ctx, i.ID); err != nil {
		t.Fatalf("Delete instance: %v", err)
	}
	if err := s.Connectors().Delete(ctx, "zotero-local"); err != nil {
		t.Fatalf("Delete connector after instance removal: %v", err)
	}
}

func TestSecretsAndToolBindingsCascade(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	seedConnector(t, s)
	i := seedInstance(t, s, "zotero")
	repo := s.Instances()

	ct := []byte{1, 2, 3, 4}
	if err := repo.SetSecret(ctx, &domain.InstanceSecret{
		InstanceID: i.ID, Name: "apiKey", Ciphertext: ct, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	if err := repo.SetToolBinding(ctx, &domain.ToolBinding{
		InstanceID: i.ID, ToolName: "zotero_search_items", Enabled: false}); err != nil {
		t.Fatalf("SetToolBinding: %v", err)
	}

	names, err := repo.ListSecretNames(ctx, i.ID)
	if err != nil || len(names) != 1 || names[0] != "apiKey" {
		t.Fatalf("ListSecretNames = %v, %v", names, err)
	}
	secrets, err := repo.LoadSecrets(ctx, i.ID)
	if err != nil || string(secrets["apiKey"]) != string(ct) {
		t.Fatalf("LoadSecrets = %v, %v", secrets, err)
	}

	// Overwriting a secret must replace, not duplicate.
	if err := repo.SetSecret(ctx, &domain.InstanceSecret{
		InstanceID: i.ID, Name: "apiKey", Ciphertext: []byte{9}, UpdatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("SetSecret overwrite: %v", err)
	}
	secrets, _ = repo.LoadSecrets(ctx, i.ID)
	if len(secrets) != 1 || secrets["apiKey"][0] != 9 {
		t.Fatalf("overwrite failed: %v", secrets)
	}

	if err := repo.Delete(ctx, i.ID); err != nil {
		t.Fatalf("Delete instance: %v", err)
	}
	if names, _ := repo.ListSecretNames(ctx, i.ID); len(names) != 0 {
		t.Fatalf("secrets survived instance deletion: %v", names)
	}
	if bs, _ := repo.ListToolBindings(ctx, i.ID); len(bs) != 0 {
		t.Fatalf("tool bindings survived instance deletion: %v", bs)
	}
}

func TestTokenLifecycle(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	repo := s.Tokens()
	now := time.Now().UTC().Truncate(time.Millisecond)

	tok := &domain.Token{ID: id.New("tok"), Name: "laptop", Hash: "lookup-key-1",
		Prefix: "abcd1234", CreatedAt: now}
	if err := repo.Create(ctx, tok); err != nil {
		t.Fatalf("Create: %v", err)
	}

	got, err := repo.GetByLookupKey(ctx, "lookup-key-1")
	if err != nil {
		t.Fatalf("GetByLookupKey: %v", err)
	}
	if !got.Usable(now) {
		t.Fatal("freshly created token is not usable")
	}
	if !got.Scopes("any-instance") {
		t.Fatal("unscoped token should cover every instance")
	}

	if err := repo.Revoke(ctx, tok.ID, now.Add(time.Minute)); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	got, _ = repo.GetByLookupKey(ctx, "lookup-key-1")
	if got.Usable(now.Add(2 * time.Minute)) {
		t.Fatal("revoked token is still usable")
	}

	// Revocation must be idempotent and must not move the original timestamp.
	first := *got.RevokedAt
	if err := repo.Revoke(ctx, tok.ID, now.Add(time.Hour)); err != nil {
		t.Fatalf("second Revoke: %v", err)
	}
	got, _ = repo.GetByLookupKey(ctx, "lookup-key-1")
	if !got.RevokedAt.Equal(first) {
		t.Fatalf("revocation timestamp moved: %v -> %v", first, *got.RevokedAt)
	}

	if _, err := repo.GetByLookupKey(ctx, "no-such-key"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestTokenExpiryAndScope(t *testing.T) {
	now := time.Now().UTC()
	past := now.Add(-time.Hour)
	expired := &domain.Token{ExpiresAt: &past}
	if expired.Usable(now) {
		t.Fatal("expired token reported usable")
	}
	scoped := &domain.Token{InstanceID: "inst_a"}
	if !scoped.Scopes("inst_a") || scoped.Scopes("inst_b") {
		t.Fatal("token scoping is wrong")
	}
}

func TestSessionExpiryPruning(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now().UTC().Truncate(time.Millisecond)
	u := &domain.User{ID: id.New("usr"), Email: "a@example.com", PasswordHash: "h",
		Role: domain.RoleAdmin, CreatedAt: now, UpdatedAt: now}
	if err := s.Users().Create(ctx, u); err != nil {
		t.Fatalf("create user: %v", err)
	}

	live := &domain.Session{ID: "live", UserID: u.ID, CSRFToken: "c1",
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.Add(time.Hour)}
	dead := &domain.Session{ID: "dead", UserID: u.ID, CSRFToken: "c2",
		CreatedAt: now.Add(-2 * time.Hour), LastSeenAt: now.Add(-2 * time.Hour), ExpiresAt: now.Add(-time.Hour)}
	for _, sess := range []*domain.Session{live, dead} {
		if err := s.Sessions().Create(ctx, sess); err != nil {
			t.Fatalf("create session: %v", err)
		}
	}

	n, err := s.Sessions().DeleteExpired(ctx, now)
	if err != nil || n != 1 {
		t.Fatalf("DeleteExpired = %d, %v; want 1, nil", n, err)
	}
	if _, err := s.Sessions().Get(ctx, "dead"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("expired session survived: %v", err)
	}
	if _, err := s.Sessions().Get(ctx, "live"); err != nil {
		t.Fatalf("live session was pruned: %v", err)
	}

	// Deleting the user must cascade to their sessions.
	if _, err := s.write.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, u.ID); err != nil {
		t.Fatalf("delete user: %v", err)
	}
	if _, err := s.Sessions().Get(ctx, "live"); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("session outlived its user (foreign keys off?): %v", err)
	}
}

func TestAuditAppendListAndPrune(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	repo := s.Audit()
	base := time.Now().UTC().Truncate(time.Millisecond)

	for i, action := range []string{domain.ActionLogin, domain.ActionInstanceCreate, domain.ActionLogin} {
		e := &domain.AuditEvent{ID: id.New("aud"), At: base.Add(time.Duration(i) * time.Second),
			ActorType: "user", ActorID: "usr_1", Action: action, Result: "ok",
			Detail: map[string]any{"seq": i}}
		if err := repo.Append(ctx, e); err != nil {
			t.Fatalf("Append: %v", err)
		}
	}

	all, err := repo.List(ctx, store.AuditFilter{})
	if err != nil || len(all) != 3 {
		t.Fatalf("List = %d events, %v; want 3", len(all), err)
	}
	// Newest first.
	if !all[0].At.After(all[1].At) {
		t.Fatal("audit events are not ordered newest first")
	}
	logins, err := repo.List(ctx, store.AuditFilter{Action: domain.ActionLogin})
	if err != nil || len(logins) != 2 {
		t.Fatalf("filtered List = %d, %v; want 2", len(logins), err)
	}
	if logins[0].Detail["seq"] == nil {
		t.Fatal("audit detail did not round trip")
	}

	n, err := repo.Prune(ctx, base.Add(2*time.Second))
	if err != nil || n != 2 {
		t.Fatalf("Prune = %d, %v; want 2", n, err)
	}
}

func TestAuditListLimitIsBounded(t *testing.T) {
	ctx := context.Background()
	s := newTestStore(t)
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		_ = s.Audit().Append(ctx, &domain.AuditEvent{ID: id.New("aud"),
			At: now.Add(time.Duration(i) * time.Second), Action: "x"})
	}
	got, err := s.Audit().List(ctx, store.AuditFilter{Limit: 2})
	if err != nil || len(got) != 2 {
		t.Fatalf("List with limit = %d, %v; want 2", len(got), err)
	}
	// An absurd limit is clamped rather than honoured.
	got, err = s.Audit().List(ctx, store.AuditFilter{Limit: 1 << 30})
	if err != nil || len(got) != 5 {
		t.Fatalf("List with huge limit = %d, %v; want 5", len(got), err)
	}
}
