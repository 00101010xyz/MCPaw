package httpapi

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/00101010xyz/mcpaw/internal/connector"
	"github.com/00101010xyz/mcpaw/internal/domain"
	"github.com/00101010xyz/mcpaw/internal/engine"
	"github.com/00101010xyz/mcpaw/internal/mcp"
	"github.com/00101010xyz/mcpaw/internal/platform/config"
	"github.com/00101010xyz/mcpaw/internal/platform/metrics"
	"github.com/00101010xyz/mcpaw/internal/secrets"
	"github.com/00101010xyz/mcpaw/internal/service"
	"github.com/00101010xyz/mcpaw/internal/store"
	"github.com/00101010xyz/mcpaw/internal/store/sqlitestore"
	"github.com/00101010xyz/mcpaw/internal/webui"
)

// app wires the full stack — real store, real services, the real webui and
// mcp handlers — behind the actual router this package builds. Exercising
// requests through httptest.NewServer (rather than calling handler.ServeHTTP
// directly) means cookies, redirects and headers behave exactly as they would
// for a real client, which is what the CSRF and bearer-scoping tests need.
type app struct {
	srv        *httptest.Server
	repos      store.Repositories
	users      *service.Users
	instances  *service.Instances
	tokens     *service.Tokens
	connectors *service.Connectors
}

// pingFailer wraps a real store.Repositories and forces Ping to fail, so
// /readyz can be tested against a database that is genuinely unreachable
// without having to sabotage the file on disk.
type pingFailer struct {
	store.Repositories
	err error
}

func (p pingFailer) Ping(context.Context) error { return p.err }

func newApp(t *testing.T, wrapRepos func(store.Repositories) store.Repositories) *app {
	t.Helper()
	ctx := context.Background()

	real, err := sqlitestore.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("sqlitestore.Open: %v", err)
	}
	t.Cleanup(func() { _ = real.Close() })

	var repos store.Repositories = real
	if wrapRepos != nil {
		repos = wrapRepos(real)
	}

	keyring, err := secrets.NewKeyring(bytes.Repeat([]byte{0x22}, 32))
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
	backend := service.NewMCPBackend(instances, nil, audit, "test", logger)

	mcpSessions := mcp.NewSessionStore(mcp.SessionConfig{})
	mcpServer, err := mcp.NewServer(mcp.ServerConfig{
		Backend: backend, Sessions: mcpSessions,
		Info: mcp.Implementation{Name: "mcpaw", Version: "test"},
	})
	if err != nil {
		t.Fatalf("mcp.NewServer: %v", err)
	}
	mcpHandler := mcp.NewHandler(mcpServer, mcp.HandlerOptions{})

	webUI, err := webui.New(webui.Config{
		Users: users, Sessions: sessions, Instances: instances, Connectors: connectors,
		Tokens: tokens, Audit: audit, Logger: logger, Version: "test",
	})
	if err != nil {
		t.Fatalf("webui.New: %v", err)
	}

	handler, err := New(Deps{
		Config: config.Config{MaxRequestBytes: 1 << 20},
		Logger: logger, Metrics: metrics.NewRegistry(), Repos: repos,
		Sessions: sessions, Tokens: tokens, Instances: instances,
		WebUI: webUI, MCPHandler: mcpHandler, Version: "test",
	})
	if err != nil {
		t.Fatalf("httpapi.New: %v", err)
	}

	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	return &app{srv: srv, repos: repos, users: users, instances: instances, tokens: tokens, connectors: connectors}
}

// browserClient does not auto-follow redirects, so a test can assert on the
// redirect itself (status + Location) instead of silently following it.
func browserClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	return &http.Client{
		Jar:           jar,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading response body: %v", err)
	}
	return string(b)
}

var csrfFieldRe = regexp.MustCompile(`name="csrf_token" value="([^"]*)"`)

// extractCSRF pulls the token the server embedded in a rendered page, the way
// a real browser's form submission would.
func extractCSRF(t *testing.T, body string) string {
	t.Helper()
	m := csrfFieldRe.FindStringSubmatch(body)
	if len(m) < 2 || m[1] == "" {
		t.Fatalf("no csrf_token field found in page:\n%.500s", body)
	}
	return m[1]
}

func TestHealthzIsAlwaysOK(t *testing.T) {
	a := newApp(t, nil)
	resp, err := http.Get(a.srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestReadyzReflectsDatabaseHealth(t *testing.T) {
	a := newApp(t, nil)
	resp, err := http.Get(a.srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthy db: status = %d, want 200", resp.StatusCode)
	}
	_ = resp.Body.Close()

	broken := newApp(t, func(r store.Repositories) store.Repositories {
		return pingFailer{Repositories: r, err: errors.New("database is unreachable")}
	})
	resp, err = http.Get(broken.srv.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("unhealthy db: status = %d, want 503", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// TestSetupLoginAndCSRF walks the real first-run flow — GET /setup, POST it to
// create the administrator, then use the session it grants to prove the CSRF
// middleware actually rejects a mutating POST with no token and accepts one
// with the token the page itself rendered.
func TestSetupLoginAndCSRF(t *testing.T) {
	a := newApp(t, nil)
	client := browserClient(t)

	resp, err := client.PostForm(a.srv.URL+"/setup", url.Values{
		"email": {"admin@example.com"}, "password": {"correct-horse-battery-staple"},
		"password_confirm": {"correct-horse-battery-staple"},
	})
	if err != nil {
		t.Fatalf("POST /setup: %v", err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /setup: status = %d, body = %s", resp.StatusCode, readBody(t, resp))
	}
	_ = resp.Body.Close()

	resp, err = client.Get(a.srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / after setup: status = %d", resp.StatusCode)
	}
	csrf := extractCSRF(t, readBody(t, resp))

	// Missing token: the CSRF middleware must reject this before the handler
	// ever runs.
	resp, err = client.PostForm(a.srv.URL+"/instances", url.Values{
		"connector_id": {"zotero-local"}, "action": {"choose"},
	})
	if err != nil {
		t.Fatalf("POST /instances (no csrf): %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("missing csrf token: status = %d, want 403", resp.StatusCode)
	}
	_ = resp.Body.Close()

	// The token the page actually rendered must be accepted.
	resp, err = client.PostForm(a.srv.URL+"/instances", url.Values{
		"connector_id": {"zotero-local"}, "action": {"choose"}, "csrf_token": {csrf},
	})
	if err != nil {
		t.Fatalf("POST /instances (valid csrf): %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("valid csrf token: status = %d, body = %s", resp.StatusCode, readBody(t, resp))
	}
	_ = resp.Body.Close()
}

// TestViewerCannotMutate proves RequireAdmin actually gates the mutating
// routes rather than merely being wired and never exercised.
func TestViewerCannotMutate(t *testing.T) {
	a := newApp(t, nil)
	ctx := context.Background()

	admin, err := a.users.Setup(ctx, service.SystemActor(), "admin@example.com", "correct-horse-battery-staple")
	if err != nil {
		t.Fatalf("Setup: %v", err)
	}
	actor := service.Actor{Type: service.ActorUser, ID: admin.ID}
	if _, err := a.users.Create(ctx, actor, "viewer@example.com", "correct-horse-battery-staple", domain.RoleViewer); err != nil {
		t.Fatalf("create viewer: %v", err)
	}

	client := browserClient(t)
	resp, err := client.PostForm(a.srv.URL+"/login", url.Values{
		"email": {"viewer@example.com"}, "password": {"correct-horse-battery-staple"},
	})
	if err != nil {
		t.Fatalf("POST /login: %v", err)
	}
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("viewer login: status = %d, body = %s", resp.StatusCode, readBody(t, resp))
	}
	_ = resp.Body.Close()

	resp, err = client.Get(a.srv.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	csrf := extractCSRF(t, readBody(t, resp))

	resp, err = client.PostForm(a.srv.URL+"/instances", url.Values{
		"connector_id": {"zotero-local"}, "action": {"choose"}, "csrf_token": {csrf},
	})
	if err != nil {
		t.Fatalf("POST /instances as viewer: %v", err)
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("viewer mutating request: status = %d, want 403", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

// TestMCPBearerAuthScoping is the security-relevant behaviour the package
// comments promise and nothing previously verified: a missing token is
// rejected with a WWW-Authenticate challenge, a token scoped to one instance
// 404s (not 403s) against a different instance's endpoint so it cannot be used
// to enumerate other endpoints, and it works normally against its own.
func TestMCPBearerAuthScoping(t *testing.T) {
	a := newApp(t, nil)
	ctx := context.Background()

	if _, err := a.users.Setup(ctx, service.SystemActor(), "admin@example.com", "correct-horse-battery-staple"); err != nil {
		t.Fatalf("Setup: %v", err)
	}
	instA, err := a.instances.Create(ctx, service.SystemActor(), service.CreateInput{
		Name: "A", Slug: "inst-a", ConnectorID: "zotero-local", Enabled: true,
	})
	if err != nil {
		t.Fatalf("create instance a: %v", err)
	}
	if _, err := a.instances.Create(ctx, service.SystemActor(), service.CreateInput{
		Name: "B", Slug: "inst-b", ConnectorID: "zotero-local", Enabled: true,
	}); err != nil {
		t.Fatalf("create instance b: %v", err)
	}

	result, err := a.tokens.Create(ctx, service.SystemActor(), service.CreateTokenInput{
		Name: "scoped", InstanceID: instA.ID,
	})
	if err != nil {
		t.Fatalf("create token: %v", err)
	}

	mcpRequest := func(t *testing.T, slug, bearer string) *http.Response {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, a.srv.URL+"/mcp/"+slug,
			strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
		if err != nil {
			t.Fatalf("building request: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		return resp
	}

	resp := mcpRequest(t, "inst-a", "")
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: status = %d, want 401", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") == "" {
		t.Fatal("no token: missing WWW-Authenticate challenge")
	}
	_ = resp.Body.Close()

	resp = mcpRequest(t, "inst-b", result.Plaintext)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("token scoped to a different instance: status = %d, want 404", resp.StatusCode)
	}
	_ = resp.Body.Close()

	resp = mcpRequest(t, "inst-a", result.Plaintext)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("correctly scoped token: status = %d, body = %s", resp.StatusCode, readBody(t, resp))
	}
	_ = resp.Body.Close()
}
