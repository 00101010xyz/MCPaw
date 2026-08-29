package webui

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/00101010xyz/mcpaw/internal/connector"
	"github.com/00101010xyz/mcpaw/internal/domain"
	"github.com/00101010xyz/mcpaw/internal/engine"
	"github.com/00101010xyz/mcpaw/internal/httpx"
	"github.com/00101010xyz/mcpaw/internal/index"
	_ "github.com/00101010xyz/mcpaw/internal/index/source/zotero" // registers the Zotero crawler
	"github.com/00101010xyz/mcpaw/internal/secrets"
	"github.com/00101010xyz/mcpaw/internal/service"
	"github.com/00101010xyz/mcpaw/internal/store/sqlitestore"
)

// harness wires a real Server against a real (temp-file) database and the
// real service layer — the same collaborators cmd/mcpaw/serve.go wires, minus
// the HTTP server itself. Rendering through the real template set is the
// point: it is what would have caught the .Palette regression this test file
// exists to guard against.
type harness struct {
	server     *Server
	users      *service.Users
	sessions   *service.Sessions
	instances  *service.Instances
	connectors *service.Connectors
	tokens     *service.Tokens
	audit      *service.Audit
	indexer    *service.Indexer
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	ctx := context.Background()

	repos, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("sqlitestore.Open: %v", err)
	}
	t.Cleanup(func() { _ = repos.Close() })

	keyring, err := secrets.NewKeyring(bytes.Repeat([]byte{0x11}, 32))
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	sealer, err := keyring.NewSealer()
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	audit := service.NewAudit(repos.Audit(), logger)

	registry := connector.NewRegistry()
	connectors := service.NewConnectors(repos.Connectors(), repos.Instances(), registry, audit, logger)
	if err := connectors.SyncBuiltins(ctx); err != nil {
		t.Fatalf("SyncBuiltins: %v", err)
	}
	if err := connectors.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	executor := engine.New(engine.Config{})
	users := service.NewUsers(repos.Users(), repos.Sessions(), audit)
	sessions := service.NewSessions(service.SessionsConfig{
		Repo: repos.Sessions(), Users: repos.Users(), Keyring: keyring, Audit: audit,
		IdleTimeout: time.Hour, AbsoluteTimeout: 24 * time.Hour,
	})
	tokens := service.NewTokens(repos.Tokens(), repos.Instances(), keyring, audit)
	instances := service.NewInstances(service.InstancesConfig{
		Repo: repos.Instances(), Connectors: connectors, Sealer: sealer, Executor: executor, Audit: audit,
	})
	indexer := service.NewIndexer(service.IndexerConfig{
		Repo: repos.SearchIndex(), Platform: repos.Platform(), Instances: instances, Audit: audit,
		Embedder: &index.Embedder{Client: executor.Client()}, Sealer: sealer, Logger: logger,
	})

	srv, err := New(Config{
		Users: users, Sessions: sessions, Instances: instances, Connectors: connectors,
		Tokens: tokens, Audit: audit, Indexer: indexer, Logger: logger, Version: "test",
	})
	if err != nil {
		t.Fatalf("webui.New: %v", err)
	}

	return &harness{
		server: srv, users: users, sessions: sessions,
		instances: instances, connectors: connectors, tokens: tokens, audit: audit, indexer: indexer,
	}
}

func (h *harness) createAdmin(t *testing.T, email string) *domain.User {
	t.Helper()
	u, err := h.users.Create(context.Background(), service.SystemActor(), email, "correct-horse-battery-staple", domain.RoleAdmin)
	if err != nil {
		t.Fatalf("create admin: %v", err)
	}
	return u
}

func (h *harness) createInstance(t *testing.T, name, slug string) *domain.Instance {
	t.Helper()
	inst, err := h.instances.Create(context.Background(), service.SystemActor(), service.CreateInput{
		Name: name, Slug: slug, ConnectorID: "zotero-local", Enabled: true, AllowPrivateNetwork: true,
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}
	return inst
}

// authedRequest builds a request carrying an authenticated user in context,
// the way httpx.OptionalSession would after validating a real cookie. Handler
// methods are exercised directly here, bypassing the middleware chain, so
// this file tests rendering; internal/httpapi/router_test.go tests the
// middleware (CSRF, RequireAdmin, bearer scoping) that sits in front of it.
func authedRequest(method, target string, user *domain.User) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	return r.WithContext(httpx.WithUser(r.Context(), user))
}

// TestEveryPageRendersForAnAuthenticatedAdmin is the regression test for the
// class of bug this file exists to catch: a template referencing a pageData
// field the Go code never populates fails template execution and turns every
// page into a 500. This walks every page template with a realistic Data
// payload and confirms each one actually renders.
func TestEveryPageRendersForAnAuthenticatedAdmin(t *testing.T) {
	h := newHarness(t)
	admin := h.createAdmin(t, "admin@example.com")
	inst := h.createInstance(t, "My Zotero", "zotero")

	cases := []struct {
		name string
		req  *http.Request
		call func(http.ResponseWriter, *http.Request)
	}{
		{"instances", authedRequest(http.MethodGet, "/", admin), h.server.GetInstances},
		{"new instance", authedRequest(http.MethodGet, "/instances/new", admin), h.server.GetNewInstance},
		{"instance detail", instanceDetailRequest(admin, inst.ID), h.server.GetInstance},
		{"connectors", authedRequest(http.MethodGet, "/connectors", admin), h.server.GetConnectors},
		{"tokens", authedRequest(http.MethodGet, "/tokens", admin), h.server.GetTokens},
		{"audit", authedRequest(http.MethodGet, "/audit", admin), h.server.GetAudit},
		{"account", authedRequest(http.MethodGet, "/account", admin), h.server.GetAccount},
		{"settings search", authedRequest(http.MethodGet, "/settings/semantic-search", admin), h.server.GetSettingsSearch},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			tc.call(rec, tc.req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "<html") {
				t.Fatalf("response does not look like a rendered page: %.200s", rec.Body.String())
			}
		})
	}
}

// The embedder API key row renders differently once a key is set (a
// "set" tag and a Remove button appear) — the branch the base
// TestEveryPageRendersForAnAuthenticatedAdmin case above never exercises,
// since it never sets one.
// The shared embedder API key is platform-wide configuration now, rendered
// on the Search settings page rather than any one instance's page — see
// domain.EmbedderSettings for why it moved out of the instance.
func TestSettingsSearchRendersWithAPIKeySet(t *testing.T) {
	h := newHarness(t)
	admin := h.createAdmin(t, "admin@example.com")

	if err := h.indexer.SetEmbedderAPIKey(context.Background(), service.SystemActor(), "sk-test-value"); err != nil {
		t.Fatalf("SetEmbedderAPIKey: %v", err)
	}

	rec := httptest.NewRecorder()
	h.server.GetSettingsSearch(rec, authedRequest(http.MethodGet, "/settings/semantic-search", admin))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "tag--ok") {
		t.Fatal("response does not show the API key as set")
	}
}

// Once an index actually holds chunks, the "Semantic search" panel grows a
// second button (Rebuild from scratch) and a last-run stats line — neither
// exercised by the empty-index case above, so this drives a real reindex
// through the harness's real Indexer to hit that branch.
func TestInstanceDetailRendersAfterAnIndexIsBuilt(t *testing.T) {
	h := newHarness(t)
	admin := h.createAdmin(t, "admin@example.com")

	mux := http.NewServeMux()
	mux.HandleFunc("/api/users/0/items/top", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"key":"ITEM0001"}]`)
	})
	mux.HandleFunc("/api/users/0/items/ITEM0001/children", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"key":"ATT00001","data":{"itemType":"attachment","contentType":"application/pdf"}}]`)
	})
	mux.HandleFunc("/api/users/0/items/ATT00001/fulltext", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"content":"some real extracted text, long enough to form a chunk."}`)
	})
	zotero := httptest.NewServer(mux)
	t.Cleanup(zotero.Close)

	embedder := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		vectors := make([][]float32, len(req.Input))
		for i := range vectors {
			vectors[i] = []float32{1, 2, 3}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"embeddings": vectors})
	}))
	t.Cleanup(embedder.Close)

	ctx := context.Background()
	if err := h.indexer.UpdateEmbedderSettings(ctx, service.SystemActor(), embedder.URL, "", 0); err != nil {
		t.Fatalf("UpdateEmbedderSettings: %v", err)
	}
	inst, err := h.instances.Create(ctx, service.SystemActor(), service.CreateInput{
		Name: "Built Zotero", Slug: "built-zotero", ConnectorID: "zotero-local",
		BaseURL: zotero.URL, Enabled: true, AllowPrivateNetwork: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, tool := range []string{"zotero_list_top_items", "zotero_get_item_children", "zotero_get_item_fulltext"} {
		if err := h.instances.SetToolEnabled(ctx, service.SystemActor(), inst.ID, tool, true); err != nil {
			t.Fatalf("SetToolEnabled(%s): %v", tool, err)
		}
	}
	if err := h.indexer.Reindex(ctx, service.SystemActor(), inst.ID, service.ReindexUpdate); err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		status, count, err := h.indexer.Status(ctx, inst.ID)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if !status.Running && count > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("reindex did not finish in time: %+v", status)
		}
		time.Sleep(5 * time.Millisecond)
	}

	rec := httptest.NewRecorder()
	h.server.GetInstance(rec, instanceDetailRequest(admin, inst.ID))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Update index") {
		t.Error("response does not offer Update index once the index has chunks")
	}
	if !strings.Contains(body, "Rebuild from scratch") {
		t.Error("response does not offer Rebuild from scratch once the index has chunks")
	}
	if !strings.Contains(body, "chunks written") {
		t.Error("response does not show the last-run stats line")
	}
}

// PostSettingsSearch saves the platform-wide embedder settings shared by
// every instance — it must not touch any instance's own configuration, since
// the whole point of moving this setting out of the instance is that it no
// longer lives there.
func TestPostSettingsSearchSavesGlobalSettings(t *testing.T) {
	h := newHarness(t)
	admin := h.createAdmin(t, "admin@example.com")
	inst := h.createInstance(t, "My Zotero", "settings-search-instance")
	originalName := inst.Name
	originalBaseURL := inst.BaseURL

	form := url.Values{
		"embedder_url": {"http://host.docker.internal:11434"}, "embedder_model": {"nomic-embed-text"},
		"rate_limit_per_min": {"30"},
	}
	req := authedRequest(http.MethodPost, "/settings/semantic-search", admin)
	req.Body = io.NopCloser(strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	h.server.PostSettingsSearch(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want a redirect", rec.Code)
	}

	settings, _, err := h.indexer.EmbedderSettings(context.Background())
	if err != nil {
		t.Fatalf("EmbedderSettings: %v", err)
	}
	if settings.URL != "http://host.docker.internal:11434" {
		t.Errorf("URL = %q", settings.URL)
	}
	if settings.Model != "nomic-embed-text" {
		t.Errorf("Model = %q", settings.Model)
	}
	if settings.RateLimitPerMin != 30 {
		t.Errorf("RateLimitPerMin = %d, want 30", settings.RateLimitPerMin)
	}

	updated, err := h.instances.Get(context.Background(), inst.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if updated.Name != originalName {
		t.Errorf("Name changed to %q, want untouched %q", updated.Name, originalName)
	}
	if updated.BaseURL != originalBaseURL {
		t.Errorf("BaseURL changed to %q, want untouched %q", updated.BaseURL, originalBaseURL)
	}
}

// PostInstances must save a connector-declared secret submitted on the
// creation form in the same request that creates the instance, rather than
// requiring a separate trip to the detail page afterward.
func TestPostInstancesSavesSecretsSubmittedOnCreate(t *testing.T) {
	h := newHarness(t)
	admin := h.createAdmin(t, "admin@example.com")

	form := url.Values{
		"connector_id": {"linkding"}, "action": {"create"},
		"name": {"My Bookmarks"}, "slug": {"my-bookmarks"},
		"base_url":     {"http://host.docker.internal:9090"},
		"secret_token": {"test-token-value"},
	}
	req := authedRequest(http.MethodPost, "/instances", admin)
	req.Body = io.NopCloser(strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	rec := httptest.NewRecorder()
	h.server.PostInstances(rec, req)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}

	instanceID := strings.TrimPrefix(rec.Header().Get("Location"), "/instances/")
	detail, err := h.instances.Detail(context.Background(), instanceID)
	if err != nil {
		t.Fatalf("Detail: %v", err)
	}
	var tokenSet bool
	for _, sec := range detail.Secrets {
		if sec.Def.Name == "token" {
			tokenSet = sec.Set
		}
	}
	if !tokenSet {
		t.Error("secret_token submitted on the creation form was not saved")
	}
}

func instanceDetailRequest(user *domain.User, instanceID string) *http.Request {
	r := authedRequest(http.MethodGet, "/instances/"+instanceID, user)
	r.SetPathValue("id", instanceID)
	return r
}

func TestLoginRedirectsPreSetupAndWhenAlreadyAuthenticated(t *testing.T) {
	h := newHarness(t)

	// No administrator exists yet: /login must hand off to /setup.
	rec := httptest.NewRecorder()
	h.server.GetLogin(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/setup" {
		t.Fatalf("pre-setup: status=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}

	admin := h.createAdmin(t, "admin@example.com")
	rec = httptest.NewRecorder()
	h.server.GetLogin(rec, authedRequest(http.MethodGet, "/login", admin))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/" {
		t.Fatalf("already authenticated: status=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}
}

func TestSetupRedirectsToLoginOnceAnAdminExists(t *testing.T) {
	h := newHarness(t)
	h.createAdmin(t, "admin@example.com")

	rec := httptest.NewRecorder()
	h.server.GetSetup(rec, httptest.NewRequest(http.MethodGet, "/setup", nil))
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login" {
		t.Fatalf("status=%d location=%q", rec.Code, rec.Header().Get("Location"))
	}
}

// TestBuildPaletteListsSectionsAndInstances exercises the fix directly: the
// static section rows come first with the group's first row flagged, and a
// created instance shows up in its own group with the MCP endpoint as the
// hint text.
func TestBuildPaletteListsSectionsAndInstances(t *testing.T) {
	h := newHarness(t)
	h.createInstance(t, "My Zotero", "zotero")

	items := h.server.buildPalette(context.Background())
	if len(items) == 0 {
		t.Fatal("buildPalette returned no items")
	}
	if !items[0].First || items[0].Group != "Sections" {
		t.Fatalf("first item = %+v, want the start of the Sections group", items[0])
	}

	var found bool
	for _, it := range items {
		if it.Group == "Instances" && it.Label == "My Zotero" {
			found = true
			if it.Hint != "/mcp/zotero" || it.Href == "" {
				t.Fatalf("instance row malformed: %+v", it)
			}
		}
	}
	if !found {
		t.Fatalf("created instance missing from the palette: %+v", items)
	}
}

// TestPageOnlyBuildsPaletteForAnAuthenticatedUser guards the other half of the
// fix: the palette does an instance-list read, and layout.html only ever
// renders it inside {{if .User}}, so an anonymous request must not pay for a
// query whose result the template will never use.
func TestPageOnlyBuildsPaletteForAnAuthenticatedUser(t *testing.T) {
	h := newHarness(t)
	admin := h.createAdmin(t, "admin@example.com")

	anon := h.server.page(httptest.NewRequest(http.MethodGet, "/", nil), "T", "instances", nil)
	if anon.Palette != nil {
		t.Fatalf("anonymous request built a palette: %+v", anon.Palette)
	}

	authed := h.server.page(authedRequest(http.MethodGet, "/", admin), "T", "instances", nil)
	if len(authed.Palette) == 0 {
		t.Fatal("authenticated request did not build a palette")
	}
}
