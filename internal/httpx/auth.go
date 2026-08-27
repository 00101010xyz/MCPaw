package httpx

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/00101010xyz/mcpaw/internal/domain"
	"github.com/00101010xyz/mcpaw/internal/platform/logging"
)

// SessionCookieName is the name of the admin session cookie. The __Host- prefix
// is not used because MCPaw is frequently run over plain HTTP on a private
// network, and that prefix would make the cookie silently unusable there.
const SessionCookieName = "mcpaw_session"

// CSRFHeaderName carries the CSRF token for fetch-style requests.
const CSRFHeaderName = "X-CSRF-Token"

// CSRFFieldName carries the CSRF token in HTML form posts.
const CSRFFieldName = "csrf_token"

// SessionAuthenticator resolves a session cookie to a user.
type SessionAuthenticator interface {
	Validate(ctx context.Context, cookieValue string) (*domain.User, *domain.Session, error)
}

// TokenAuthenticator resolves a bearer token and the instance it may reach.
type TokenAuthenticator interface {
	Authenticate(ctx context.Context, presented string) (*domain.Token, error)
}

// SetSessionCookie writes the session cookie.
func SetSessionCookie(w http.ResponseWriter, value string, secure bool, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:  SessionCookieName,
		Value: value,
		Path:  "/",
		// HttpOnly keeps the cookie out of reach of any script, so an XSS in
		// the admin UI cannot exfiltrate the session.
		HttpOnly: true,
		Secure:   secure,
		// Lax still sends the cookie on top-level navigations, which keeps
		// bookmarks working, while blocking it on cross-site form posts — the
		// shape a CSRF attack takes.
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int(maxAge.Seconds()),
	})
}

// ClearSessionCookie expires the session cookie.
func ClearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name: SessionCookieName, Value: "", Path: "/",
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode, MaxAge: -1,
	})
}

// SessionCookieValue reads the raw cookie value.
func SessionCookieValue(r *http.Request) string {
	c, err := r.Cookie(SessionCookieName)
	if err != nil {
		return ""
	}
	return c.Value
}

// OptionalSession attaches the user and session when a valid cookie is present,
// and otherwise passes the request through unauthenticated.
//
// Separating "attach if present" from "require" lets pages such as the login
// screen redirect an already-authenticated visitor without duplicating cookie
// handling.
func OptionalSession(auth SessionAuthenticator) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			value := SessionCookieValue(r)
			if value == "" {
				next.ServeHTTP(w, r)
				return
			}
			user, session, err := auth.Validate(r.Context(), value)
			if err != nil {
				if errors.Is(err, domain.ErrUnauthorized) {
					// The cookie is stale; removing it stops the browser
					// re-presenting it on every subsequent request.
					ClearSessionCookie(w, r.TLS != nil)
					next.ServeHTTP(w, r)
					return
				}
				logging.FromContext(r.Context()).Error("session validation failed",
					slog.String("error", err.Error()))
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			ctx := WithUser(r.Context(), user)
			ctx = WithSession(ctx, session)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// RequireSession rejects unauthenticated requests. onUnauthenticated lets the
// web UI redirect to the login page while the JSON API returns 401.
func RequireSession(onUnauthenticated http.HandlerFunc) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, ok := UserFrom(r.Context()); !ok {
				onUnauthenticated(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// RequireAdmin rejects viewers from mutating endpoints.
func RequireAdmin(onForbidden http.HandlerFunc) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, ok := UserFrom(r.Context())
			if !ok || user.Role != domain.RoleAdmin {
				onForbidden(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// CSRF enforces the double-submit token on state-changing, cookie-authenticated
// requests.
//
// Safe methods are exempt because they must not change state anyway. Requests
// with no session are exempt because there is no ambient credential for an
// attacker's page to ride on — which is exactly why the bearer-authenticated
// MCP endpoint needs no CSRF token.
func CSRF(onFailure http.HandlerFunc) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
				next.ServeHTTP(w, r)
				return
			}
			session, ok := SessionFrom(r.Context())
			if !ok {
				next.ServeHTTP(w, r)
				return
			}

			presented := r.Header.Get(CSRFHeaderName)
			if presented == "" {
				// ParseForm is safe to call here: the body limit middleware has
				// already bounded it.
				if err := r.ParseForm(); err == nil {
					presented = r.PostFormValue(CSRFFieldName)
				}
			}
			if subtle.ConstantTimeCompare([]byte(presented), []byte(session.CSRFToken)) != 1 {
				logging.FromContext(r.Context()).Warn("csrf token rejected",
					slog.String("path", r.URL.Path), slog.String("method", r.Method))
				onFailure(w, r)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// InstanceResolver loads an instance by its endpoint slug.
type InstanceResolver func(ctx context.Context, slug string) (instanceID string, enabled bool, err error)

// BearerAuth authenticates an MCP client and authorises it for the instance
// named in the path.
//
// Everything downstream may assume the caller holds a live token scoped to that
// exact instance, which is what lets the protocol layer contain no
// authorisation logic at all.
type BearerAuth struct {
	Tokens   TokenAuthenticator
	Resolve  InstanceResolver
	OnResult func(w http.ResponseWriter, r *http.Request, instanceID, tokenID string)
}

// Middleware returns the authenticating middleware.
//
// slugParam names the path wildcard carrying the endpoint slug.
func (b BearerAuth) Middleware(slugParam string, attach func(r *http.Request, instanceID, tokenID string) *http.Request) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			log := logging.FromContext(r.Context())

			presented, ok := bearerToken(r)
			if !ok {
				unauthorized(w, "a bearer token is required")
				return
			}
			token, err := b.Tokens.Authenticate(r.Context(), presented)
			if err != nil {
				if errors.Is(err, domain.ErrUnauthorized) {
					unauthorized(w, "invalid or expired token")
					return
				}
				log.Error("token authentication failed", slog.String("error", err.Error()))
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}

			slug := r.PathValue(slugParam)
			instanceID, enabled, err := b.Resolve(r.Context(), slug)
			if err != nil {
				if errors.Is(err, domain.ErrNotFound) {
					// A caller holding a valid token still learns nothing about
					// which slugs exist beyond its own scope.
					http.Error(w, "no such MCP endpoint", http.StatusNotFound)
					return
				}
				log.Error("instance resolution failed", slog.String("error", err.Error()))
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			if !token.Scopes(instanceID) {
				// Deliberately 404, not 403: a token scoped elsewhere should not
				// be able to enumerate which endpoints exist.
				http.Error(w, "no such MCP endpoint", http.StatusNotFound)
				return
			}
			if !enabled {
				http.Error(w, "this MCP endpoint is disabled", http.StatusServiceUnavailable)
				return
			}

			if b.OnResult != nil {
				b.OnResult(w, r, instanceID, token.ID)
			}
			next.ServeHTTP(w, attach(r, instanceID, token.ID))
		})
	}
}

func bearerToken(r *http.Request) (string, bool) {
	header := r.Header.Get("Authorization")
	if header == "" {
		return "", false
	}
	scheme, value, ok := strings.Cut(header, " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	value = strings.TrimSpace(value)
	return value, value != ""
}

func unauthorized(w http.ResponseWriter, message string) {
	// The WWW-Authenticate header tells a conformant client how to
	// authenticate rather than leaving it to guess.
	w.Header().Set("WWW-Authenticate", `Bearer realm="mcpaw"`)
	http.Error(w, message, http.StatusUnauthorized)
}

// AttemptLimiter throttles repeated failures per key, used for login and other
// credential-guessing surfaces.
//
// It is a fixed-window counter rather than a token bucket because the desired
// behaviour is "N tries then a cooling-off period", which is easier for an
// operator to reason about than a refill rate.
type AttemptLimiter struct {
	limit  int
	window time.Duration
	now    func() time.Time
	mu     sync.Mutex
	counts map[string]*attemptWindow
}

type attemptWindow struct {
	count int
	until time.Time
}

// NewAttemptLimiter creates a limiter allowing limit failures per window.
func NewAttemptLimiter(limit int, window time.Duration) *AttemptLimiter {
	if limit <= 0 {
		limit = 10
	}
	if window <= 0 {
		window = time.Minute
	}
	return &AttemptLimiter{limit: limit, window: window, now: time.Now, counts: map[string]*attemptWindow{}}
}

// Allow reports whether another attempt may be made for key.
func (l *AttemptLimiter) Allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	w, ok := l.counts[key]
	if !ok || now.After(w.until) {
		return true
	}
	return w.count < l.limit
}

// RecordFailure counts one failed attempt.
func (l *AttemptLimiter) RecordFailure(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	w, ok := l.counts[key]
	if !ok || now.After(w.until) {
		l.pruneLocked(now)
		l.counts[key] = &attemptWindow{count: 1, until: now.Add(l.window)}
		return
	}
	w.count++
}

// RecordSuccess clears the counter, so a legitimate user who mistyped twice is
// not penalised afterwards.
func (l *AttemptLimiter) RecordSuccess(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.counts, key)
}

const maxAttemptKeys = 10_000

func (l *AttemptLimiter) pruneLocked(now time.Time) {
	if len(l.counts) < maxAttemptKeys {
		return
	}
	for k, w := range l.counts {
		if now.After(w.until) {
			delete(l.counts, k)
		}
	}
}
