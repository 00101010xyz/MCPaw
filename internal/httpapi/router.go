// Package httpapi assembles the HTTP surface of MCPaw: the MCP endpoints, the
// administrative web interface, and the operational probes.
//
// Routing lives here and only here, so the complete set of reachable URLs — and
// the middleware guarding each of them — can be read in one file.
package httpapi

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/00101010xyz/mcpaw/internal/httpx"
	"github.com/00101010xyz/mcpaw/internal/mcp"
	"github.com/00101010xyz/mcpaw/internal/platform/config"
	"github.com/00101010xyz/mcpaw/internal/platform/metrics"
	"github.com/00101010xyz/mcpaw/internal/service"
	"github.com/00101010xyz/mcpaw/internal/store"
	"github.com/00101010xyz/mcpaw/internal/webui"
)

// Deps are the collaborators the router needs. Everything is injected: the
// router constructs nothing, which keeps it a pure composition of parts that
// were already tested in isolation.
type Deps struct {
	Config     config.Config
	Logger     *slog.Logger
	Metrics    *metrics.Registry
	Repos      store.Repositories
	Sessions   *service.Sessions
	Tokens     *service.Tokens
	Instances  *service.Instances
	WebUI      *webui.Server
	MCPHandler *mcp.Handler
	Version    string
}

// New builds the root handler.
func New(deps Deps) (http.Handler, error) {
	if deps.Logger == nil {
		deps.Logger = slog.Default()
	}
	for name, present := range map[string]bool{
		"repositories": deps.Repos != nil,
		"sessions":     deps.Sessions != nil,
		"tokens":       deps.Tokens != nil,
		"instances":    deps.Instances != nil,
		"web ui":       deps.WebUI != nil,
		"mcp handler":  deps.MCPHandler != nil,
	} {
		if !present {
			return nil, fmt.Errorf("httpapi: %s dependency is required", name)
		}
	}

	mux := http.NewServeMux()
	mountProbes(mux, deps)
	mountMCP(mux, deps)
	mountWebUI(mux, deps)

	// The outermost chain applies to everything, including 404s, so no request
	// escapes logging, panic recovery or the security headers.
	root := httpx.Chain(
		httpx.RequestContext(deps.Logger, deps.Config.TrustProxyHeaders),
		httpx.Recover(),
		httpx.SecurityHeaders(deps.Config.SecureCookies),
	)
	return root(mux), nil
}

// mountProbes registers the operational endpoints.
//
// They are deliberately unauthenticated: a liveness probe that needs a
// credential is a liveness probe that fails during exactly the incident it
// exists to detect. Neither reveals configuration.
func mountProbes(mux *http.ServeMux, deps Deps) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeText(w, http.StatusOK, "ok")
	})

	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
		defer cancel()
		if err := deps.Repos.Ping(ctx); err != nil {
			deps.Logger.Warn("readiness probe failed", slog.String("error", err.Error()))
			writeText(w, http.StatusServiceUnavailable, "database unavailable")
			return
		}
		writeText(w, http.StatusOK, "ready")
	})

	mux.HandleFunc("GET /version", func(w http.ResponseWriter, r *http.Request) {
		writeText(w, http.StatusOK, deps.Version)
	})

	if deps.Config.MetricsEnabled && deps.Metrics != nil {
		mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
			if err := deps.Metrics.Write(w); err != nil {
				deps.Logger.Warn("could not render metrics", slog.String("error", err.Error()))
			}
		})
	}
}

// mountMCP registers the protocol endpoints.
func mountMCP(mux *http.ServeMux, deps Deps) {
	auth := httpx.BearerAuth{
		Tokens: deps.Tokens,
		Resolve: func(ctx context.Context, slug string) (string, bool, error) {
			resolved, err := deps.Instances.ResolveBySlug(ctx, slug)
			if err != nil {
				return "", false, err
			}
			return resolved.Instance.ID, resolved.Instance.Enabled, nil
		},
	}

	// attach carries the authenticated identity forward: the instance for the
	// protocol layer, the resolved configuration so the request path does not
	// look it up twice, and the caller identity for the audit trail.
	attach := func(r *http.Request, instanceID, tokenID string) *http.Request {
		ctx := mcp.WithInstanceID(r.Context(), instanceID)
		ctx = service.WithCaller(ctx, tokenID, httpx.ClientIPFrom(ctx))
		if resolved, err := deps.Instances.ResolveBySlug(ctx, r.PathValue("slug")); err == nil {
			ctx = service.WithResolved(ctx, resolved)
		}
		deps.Tokens.TouchLastUsed(ctx, tokenID)
		return r.WithContext(ctx)
	}

	chain := httpx.Chain(
		httpx.AccessLog(deps.Metrics, "mcp"),
		httpx.NoStore(),
		httpx.BodyLimit(deps.Config.MaxRequestBytes),
		auth.Middleware("slug", attach),
	)
	handler := chain(deps.MCPHandler)

	// One pattern per verb keeps the method-not-allowed behaviour explicit
	// rather than depending on the mux's fallthrough.
	for _, method := range []string{"POST", "GET", "DELETE"} {
		mux.Handle(method+" /mcp/{slug}", handler)
	}
	mux.HandleFunc("/mcp/{slug}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeText(w, http.StatusMethodNotAllowed, "method not allowed")
	})
}

// mountWebUI registers the administrative interface.
func mountWebUI(mux *http.ServeMux, deps Deps) {
	ui := deps.WebUI

	// Static assets need no session and no CSRF, and must not be no-stored.
	mux.Handle("GET /static/", http.StripPrefix("/static/", ui.StaticHandler()))

	base := httpx.Chain(
		httpx.AccessLog(deps.Metrics, "web"),
		httpx.NoStore(),
		httpx.BodyLimit(deps.Config.MaxRequestBytes),
		httpx.OptionalSession(deps.Sessions),
		httpx.CSRF(csrfFailed),
	)
	// Pages reachable without a session: the sign-in and first-run flows.
	public := func(h http.HandlerFunc) http.Handler { return base(h) }
	// Everything else requires a session.
	private := func(h http.HandlerFunc) http.Handler {
		return base(httpx.RequireSession(redirectToLogin)(h))
	}
	// Mutations additionally require the admin role.
	admin := func(h http.HandlerFunc) http.Handler {
		return base(httpx.RequireSession(redirectToLogin)(httpx.RequireAdmin(forbidden)(h)))
	}

	mux.Handle("GET /setup", public(ui.GetSetup))
	mux.Handle("POST /setup", public(ui.PostSetup))
	mux.Handle("GET /login", public(ui.GetLogin))
	mux.Handle("POST /login", public(ui.PostLogin))
	mux.Handle("POST /logout", private(ui.PostLogout))

	mux.Handle("GET /{$}", private(ui.GetInstances))
	mux.Handle("GET /instances/new", private(ui.GetNewInstance))
	mux.Handle("POST /instances", admin(ui.PostInstances))
	mux.Handle("GET /instances/{id}", private(ui.GetInstance))
	mux.Handle("POST /instances/{id}", admin(ui.PostInstance))
	mux.Handle("POST /instances/{id}/secrets", admin(ui.PostInstanceSecret))
	mux.Handle("POST /instances/{id}/secrets/delete", admin(ui.PostInstanceSecretDelete))
	mux.Handle("POST /instances/{id}/tools", admin(ui.PostInstanceTool))
	mux.Handle("POST /instances/{id}/test", admin(ui.PostInstanceTest))
	mux.Handle("POST /instances/{id}/reindex", admin(ui.PostInstanceReindex))
	mux.Handle("POST /instances/{id}/reindex/rebuild", admin(ui.PostInstanceReindexRebuild))
	mux.Handle("POST /instances/{id}/delete", admin(ui.PostInstanceDelete))

	mux.Handle("GET /settings/semantic-search", private(ui.GetSettingsSearch))
	mux.Handle("POST /settings/semantic-search", admin(ui.PostSettingsSearch))
	mux.Handle("POST /settings/semantic-search/secret", admin(ui.PostSettingsSearchSecret))
	mux.Handle("POST /settings/semantic-search/secret/delete", admin(ui.PostSettingsSearchSecretDelete))

	mux.Handle("GET /connectors", private(ui.GetConnectors))
	mux.Handle("POST /connectors/import", admin(ui.PostImportConnector))
	mux.Handle("POST /connectors/{id}/delete", admin(ui.PostDeleteConnector))

	mux.Handle("GET /tokens", private(ui.GetTokens))
	mux.Handle("POST /tokens", admin(ui.PostTokens))
	mux.Handle("POST /tokens/{id}/revoke", admin(ui.PostRevokeToken))

	mux.Handle("GET /usage", private(ui.GetUsage))
	mux.Handle("POST /usage/clear", admin(ui.PostUsageClear))
	mux.Handle("GET /settings/usage", private(ui.GetSettingsUsage))
	mux.Handle("POST /settings/usage", admin(ui.PostSettingsUsage))

	mux.Handle("GET /audit", private(ui.GetAudit))
	mux.Handle("GET /account", private(ui.GetAccount))
	mux.Handle("POST /account/password", private(ui.PostChangePassword))
}

func redirectToLogin(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

func forbidden(w http.ResponseWriter, r *http.Request) {
	writeText(w, http.StatusForbidden, "your account does not have permission to change configuration")
}

// csrfFailed reports a rejected token without hinting at how to satisfy it.
func csrfFailed(w http.ResponseWriter, r *http.Request) {
	writeText(w, http.StatusForbidden,
		"the request could not be verified; reload the page and try again")
}

func writeText(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body + "\n"))
}
