package webui

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/00101010xyz/mcpaw/internal/connector"
	"github.com/00101010xyz/mcpaw/internal/domain"
	"github.com/00101010xyz/mcpaw/internal/engine"
	"github.com/00101010xyz/mcpaw/internal/httpx"
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

	srv, err := New(Config{
		Users: users, Sessions: sessions, Instances: instances, Connectors: connectors,
		Tokens: tokens, Audit: audit, Logger: logger, Version: "test",
	})
	if err != nil {
		t.Fatalf("webui.New: %v", err)
	}

	return &harness{
		server: srv, users: users, sessions: sessions,
		instances: instances, connectors: connectors, tokens: tokens, audit: audit,
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
