// Package webui serves the administrative web interface.
//
// The UI is server-rendered HTML with no client-side framework and no inline
// script. That is a security decision as much as a simplicity one: with a
// strict Content-Security-Policy and html/template's contextual escaping, the
// class of bugs where configuration data becomes executable script is closed by
// construction rather than by review.
package webui

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/00101010xyz/mcpaw/internal/domain"
	"github.com/00101010xyz/mcpaw/internal/httpx"
	"github.com/00101010xyz/mcpaw/internal/platform/logging"
	"github.com/00101010xyz/mcpaw/internal/service"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// Server renders the administrative interface.
type Server struct {
	users      *service.Users
	sessions   *service.Sessions
	instances  *service.Instances
	connectors *service.Connectors
	tokens     *service.Tokens
	audit      *service.Audit

	pages   map[string]*template.Template
	flashes *flashStore
	logger  *slog.Logger

	publicURL      string
	version        string
	secureCookies  bool
	sessionMaxAge  time.Duration
	loginLimiter   *httpx.AttemptLimiter
	staticFileTime time.Time
}

// Config wires the web UI.
type Config struct {
	Users         *service.Users
	Sessions      *service.Sessions
	Instances     *service.Instances
	Connectors    *service.Connectors
	Tokens        *service.Tokens
	Audit         *service.Audit
	Logger        *slog.Logger
	PublicURL     string
	Version       string
	SecureCookies bool
	SessionMaxAge time.Duration
	LoginLimiter  *httpx.AttemptLimiter
}

// New parses the templates and returns a ready Server.
//
// Templates are parsed once at construction: a broken template becomes a
// startup failure rather than a 500 that only appears when an operator visits
// the one page nobody tested.
func New(cfg Config) (*Server, error) {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.SessionMaxAge <= 0 {
		cfg.SessionMaxAge = 24 * time.Hour
	}
	if cfg.LoginLimiter == nil {
		cfg.LoginLimiter = httpx.NewAttemptLimiter(10, time.Minute)
	}

	pages, err := parsePages()
	if err != nil {
		return nil, err
	}

	return &Server{
		users: cfg.Users, sessions: cfg.Sessions, instances: cfg.Instances,
		connectors: cfg.Connectors, tokens: cfg.Tokens, audit: cfg.Audit,
		pages: pages, flashes: newFlashStore(2 * time.Minute), logger: cfg.Logger,
		publicURL: strings.TrimRight(cfg.PublicURL, "/"), version: cfg.Version,
		secureCookies: cfg.SecureCookies, sessionMaxAge: cfg.SessionMaxAge,
		loginLimiter: cfg.LoginLimiter, staticFileTime: time.Now(),
	}, nil
}

// pageNames are the content templates, each combined with the shared layout
// into its own template set.
//
// Go templates cannot select an overridden block by a runtime name, so a single
// parsed set cannot hold several definitions of "content". Building one set per
// page is the idiomatic way to get layout inheritance, and it also means a
// template referencing a block it does not have fails at startup.
var pageNames = []string{
	"login", "setup", "account", "instances", "instance_new",
	"instance_detail", "connectors", "tokens", "audit",
}

func parsePages() (map[string]*template.Template, error) {
	pages := make(map[string]*template.Template, len(pageNames))
	for _, name := range pageNames {
		tmpl, err := template.New(name).Funcs(templateFuncs()).
			ParseFS(templateFS, "templates/layout.html", "templates/"+name+".html")
		if err != nil {
			return nil, fmt.Errorf("webui: parsing template %s: %w", name, err)
		}
		pages[name] = tmpl
	}
	return pages, nil
}

// templateFuncs exposes a deliberately small set of helpers. Anything more
// elaborate belongs in a view model, where it can be tested.
func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"formatTime": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.UTC().Format("2006-01-02 15:04 UTC")
		},
		"formatTimePtr": func(t *time.Time) string {
			if t == nil {
				return "never"
			}
			return t.UTC().Format("2006-01-02 15:04 UTC")
		},
		"humanBytes": func(n int64) string {
			switch {
			case n >= 1<<20:
				return fmt.Sprintf("%.1f MiB", float64(n)/float64(1<<20))
			case n >= 1<<10:
				return fmt.Sprintf("%.0f KiB", float64(n)/float64(1<<10))
			default:
				return fmt.Sprintf("%d B", n)
			}
		},
		"join":  strings.Join,
		"lower": strings.ToLower,
		"boolPtr": func(b *bool) bool {
			return b != nil && *b
		},
		"pluralise": func(n int, singular, plural string) string {
			if n == 1 {
				return singular
			}
			return plural
		},
	}
}

// pageData is the root render model shared by every template.
type pageData struct {
	Title     string
	Nonce     string
	User      *domain.User
	CSRF      string
	Flash     *Flash
	Nav       string
	PublicURL string
	Version   string
	Palette   []paletteItem
	Data      any
}

// paletteItem is one row of the command palette (⌘K). Group headers are
// rendered by the template whenever First is true for the row that opens a
// new group, so buildPalette is responsible for setting it correctly rather
// than leaving the template to guess where a group starts.
type paletteItem struct {
	First  bool
	Group  string
	Search string
	Href   string
	Label  string
	Hint   string
}

func (s *Server) page(r *http.Request, title, nav string, data any) pageData {
	ctx := r.Context()
	p := pageData{
		Title: title, Nonce: httpx.NonceFrom(ctx), Nav: nav,
		PublicURL: s.baseURL(r), Version: s.version, Data: data,
	}
	if user, ok := httpx.UserFrom(ctx); ok {
		p.User = user
		// The palette is only ever rendered inside the {{if .User}} nav block
		// in layout.html, so it is only worth building — and only worth the
		// extra instance list read — once someone is actually signed in.
		p.Palette = s.buildPalette(ctx)
	}
	if session, ok := httpx.SessionFrom(ctx); ok {
		p.CSRF = session.CSRFToken
		p.Flash = s.flashes.take(session.ID)
	}
	return p
}

// buildPalette assembles the ⌘K command palette: the fixed set of sections
// every operator can jump to, plus one row per configured instance so a large
// deployment stays a keystroke away from any endpoint.
//
// A failure to list instances is logged and the instances group is simply
// omitted — the palette staying incomplete is a much smaller problem than the
// page failing to render because of it.
func (s *Server) buildPalette(ctx context.Context) []paletteItem {
	items := []paletteItem{
		{First: true, Group: "Sections", Label: "Instances", Search: "instances home", Href: "/"},
		{Group: "Sections", Label: "New instance", Search: "new instance create", Href: "/instances/new"},
		{Group: "Sections", Label: "Connectors", Search: "connectors import manifest", Href: "/connectors"},
		{Group: "Sections", Label: "Tokens", Search: "tokens access bearer", Href: "/tokens"},
		{Group: "Sections", Label: "Audit log", Search: "audit log events", Href: "/audit"},
		{Group: "Sections", Label: "Account", Search: "account password", Href: "/account"},
	}

	instances, err := s.instances.List(ctx)
	if err != nil {
		logging.FromContext(ctx).Warn("could not list instances for the command palette",
			slog.String("error", err.Error()))
		return items
	}
	for i, inst := range instances {
		items = append(items, paletteItem{
			First:  i == 0,
			Group:  "Instances",
			Label:  inst.Instance.Name,
			Search: inst.Instance.Name + " " + inst.Instance.Slug + " " + inst.ConnectorName,
			Href:   "/instances/" + inst.Instance.ID,
			Hint:   "/mcp/" + inst.Instance.Slug,
		})
	}
	return items
}

// baseURL is the origin an operator should hand to an MCP client. It prefers
// the configured public URL, because behind a reverse proxy the request's own
// Host is frequently the internal one.
func (s *Server) baseURL(r *http.Request) string {
	if s.publicURL != "" {
		return s.publicURL
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func (s *Server) render(w http.ResponseWriter, r *http.Request, status int, name string, data pageData) {
	tmpl, ok := s.pages[name]
	if !ok {
		logging.FromContext(r.Context()).Error("unknown template requested", slog.String("template", name))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	// Rendering into a buffer first means a template error produces a clean
	// 500 rather than a half-written page with a 200 already committed.
	var buf strings.Builder
	if err := tmpl.ExecuteTemplate(&buf, "layout", data); err != nil {
		logging.FromContext(r.Context()).Error("template execution failed",
			slog.String("template", name), slog.String("error", err.Error()))
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, buf.String())
}

func (s *Server) flash(r *http.Request, f *Flash) {
	if session, ok := httpx.SessionFrom(r.Context()); ok {
		s.flashes.set(session.ID, f)
	}
}

func (s *Server) flashError(r *http.Request, format string, args ...any) {
	s.flash(r, &Flash{Level: FlashError, Message: fmt.Sprintf(format, args...)})
}

func (s *Server) flashSuccess(r *http.Request, format string, args ...any) {
	s.flash(r, &Flash{Level: FlashSuccess, Message: fmt.Sprintf(format, args...)})
}

// redirect issues a POST-redirect-GET so a refresh does not resubmit a form.
func redirect(w http.ResponseWriter, r *http.Request, path string) {
	http.Redirect(w, r, path, http.StatusSeeOther)
}

// errorMessage renders a domain error as text safe to show an operator.
//
// Only known sentinel errors produce their own message; anything else becomes a
// generic line, because an unexpected error's text can contain internal
// addresses and query fragments.
func errorMessage(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, domain.ErrNotFound):
		return "That item no longer exists."
	case errors.Is(err, domain.ErrConflict),
		errors.Is(err, domain.ErrInvalidInput),
		errors.Is(err, domain.ErrForbidden):
		// These carry a validation message written for a human by the service
		// layer, so the wrapped text is deliberately shown.
		return cleanupError(err)
	case errors.Is(err, domain.ErrUnauthorized):
		return "You are not authorised to do that."
	default:
		return "Something went wrong. Check the server logs for details."
	}
}

// cleanupError strips the wrapping sentinel from a message so the operator sees
// "the endpoint slug is already in use" rather than "conflict: the endpoint …".
func cleanupError(err error) string {
	msg := err.Error()
	for _, prefix := range []string{"invalid input: ", "conflict: ", "forbidden: "} {
		msg = strings.TrimPrefix(msg, prefix)
	}
	if idx := strings.Index(msg, ": invalid input"); idx > 0 {
		msg = msg[:idx]
	}
	if msg == "" {
		return "The request was rejected."
	}
	return strings.ToUpper(msg[:1]) + msg[1:]
}

func parseIntField(r *http.Request, name string, fallback int) int {
	v := strings.TrimSpace(r.PostFormValue(name))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return n
}

func parseInt64Field(r *http.Request, name string, fallback int64) int64 {
	v := strings.TrimSpace(r.PostFormValue(name))
	if v == "" {
		return fallback
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fallback
	}
	return n
}

func checkbox(r *http.Request, name string) bool {
	v := r.PostFormValue(name)
	return v == "on" || v == "true" || v == "1"
}

// StaticHandler serves the embedded stylesheet and script.
func (s *Server) StaticHandler() http.Handler {
	sub, err := fsSub(staticFS, "static")
	if err != nil {
		// The files are embedded at build time, so this cannot fail in a built
		// binary; failing loudly beats serving a UI with no styling.
		panic(err)
	}
	fileServer := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Static assets are embedded and versioned with the binary, so they may
		// be cached, but only for long enough to help a page load.
		w.Header().Set("Cache-Control", "public, max-age=300")
		fileServer.ServeHTTP(w, r)
	})
}
