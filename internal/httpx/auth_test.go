package httpx

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/00101010xyz/mcpaw/internal/domain"
)

// --- fakes -------------------------------------------------------------

type fakeSessionAuth struct {
	user    *domain.User
	session *domain.Session
	err     error
}

func (f fakeSessionAuth) Validate(context.Context, string) (*domain.User, *domain.Session, error) {
	return f.user, f.session, f.err
}

type fakeTokenAuth struct {
	token *domain.Token
	err   error
}

func (f fakeTokenAuth) Authenticate(context.Context, string) (*domain.Token, error) {
	return f.token, f.err
}

// okHandler records that the request reached the end of the chain.
func okHandler(reached *bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*reached = true
		w.WriteHeader(http.StatusOK)
	})
}

// --- session cookie ----------------------------------------------------

func TestSetSessionCookieHardening(t *testing.T) {
	rec := httptest.NewRecorder()
	SetSessionCookie(rec, "abc123", true, time.Hour)

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies, want 1", len(cookies))
	}
	c := cookies[0]
	if c.Name != SessionCookieName || c.Value != "abc123" {
		t.Errorf("cookie = %s=%s", c.Name, c.Value)
	}
	if !c.HttpOnly {
		t.Error("HttpOnly must be set: without it an XSS in the admin UI can exfiltrate the session")
	}
	if !c.Secure {
		t.Error("Secure must be set when requested")
	}
	if c.SameSite != http.SameSiteLaxMode {
		t.Errorf("SameSite = %v, want Lax so cross-site form posts do not carry the session", c.SameSite)
	}
	if c.Path != "/" {
		t.Errorf("Path = %q, want /", c.Path)
	}
}

func TestClearSessionCookieExpires(t *testing.T) {
	rec := httptest.NewRecorder()
	ClearSessionCookie(rec, false)

	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("got %d cookies, want 1", len(cookies))
	}
	if cookies[0].MaxAge >= 0 {
		t.Errorf("MaxAge = %d, want negative so the browser drops the cookie", cookies[0].MaxAge)
	}
	if cookies[0].Value != "" {
		t.Errorf("Value = %q, want empty", cookies[0].Value)
	}
}

// --- OptionalSession ---------------------------------------------------

func TestOptionalSessionNoCookiePassesThrough(t *testing.T) {
	var reached bool
	h := OptionalSession(fakeSessionAuth{})(okHandler(&reached))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if !reached {
		t.Fatal("a request without a cookie must pass through unauthenticated")
	}
	if _, ok := UserFrom(context.Background()); ok {
		t.Error("no user should be attached")
	}
}

func TestOptionalSessionAttachesUser(t *testing.T) {
	user := &domain.User{ID: "u1", Role: domain.RoleAdmin}
	session := &domain.Session{ID: "s1", CSRFToken: "tok"}

	var gotUser *domain.User
	var gotSession *domain.Session
	h := OptionalSession(fakeSessionAuth{user: user, session: session})(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotUser, _ = UserFrom(r.Context())
			gotSession, _ = SessionFrom(r.Context())
		}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "cookie"})
	h.ServeHTTP(httptest.NewRecorder(), req)

	if gotUser != user {
		t.Error("the authenticated user must be attached to the context")
	}
	if gotSession != session {
		t.Error("the session must be attached to the context")
	}
}

// A stale cookie is cleared so the browser stops re-presenting it on every
// subsequent request.
func TestOptionalSessionClearsStaleCookie(t *testing.T) {
	var reached bool
	h := OptionalSession(fakeSessionAuth{err: domain.ErrUnauthorized})(okHandler(&reached))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "stale"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !reached {
		t.Error("a stale cookie must not block the request, only de-authenticate it")
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].MaxAge >= 0 {
		t.Error("a stale session cookie must be cleared")
	}
}

// An infrastructure failure must not be mistaken for "not logged in": failing
// open here would serve unauthenticated requests during a database outage.
func TestOptionalSessionFailsClosedOnInternalError(t *testing.T) {
	var reached bool
	h := OptionalSession(fakeSessionAuth{err: errors.New("database is down")})(okHandler(&reached))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: "cookie"})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if reached {
		t.Error("an internal validation error must not fall through as unauthenticated")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// --- RequireSession / RequireAdmin -------------------------------------

func TestRequireSession(t *testing.T) {
	var reached, redirected bool
	h := RequireSession(func(w http.ResponseWriter, r *http.Request) {
		redirected = true
		w.WriteHeader(http.StatusFound)
	})(okHandler(&reached))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if reached || !redirected {
		t.Error("an unauthenticated request must hit the onUnauthenticated handler")
	}

	reached, redirected = false, false
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req = req.WithContext(WithUser(req.Context(), &domain.User{ID: "u1"}))
	h.ServeHTTP(httptest.NewRecorder(), req)
	if !reached || redirected {
		t.Error("an authenticated request must pass through")
	}
}

func TestRequireAdminBlocksViewer(t *testing.T) {
	var reached, forbidden bool
	h := RequireAdmin(func(w http.ResponseWriter, r *http.Request) {
		forbidden = true
		w.WriteHeader(http.StatusForbidden)
	})(okHandler(&reached))

	for _, tc := range []struct {
		name      string
		user      *domain.User
		wantAllow bool
	}{
		{"no user", nil, false},
		{"viewer", &domain.User{ID: "u1", Role: domain.RoleViewer}, false},
		{"admin", &domain.User{ID: "u2", Role: domain.RoleAdmin}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reached, forbidden = false, false
			req := httptest.NewRequest(http.MethodPost, "/", nil)
			if tc.user != nil {
				req = req.WithContext(WithUser(req.Context(), tc.user))
			}
			h.ServeHTTP(httptest.NewRecorder(), req)

			if reached != tc.wantAllow {
				t.Errorf("reached handler = %v, want %v", reached, tc.wantAllow)
			}
			if forbidden == tc.wantAllow {
				t.Errorf("forbidden = %v, want %v", forbidden, !tc.wantAllow)
			}
		})
	}
}

// --- CSRF --------------------------------------------------------------

func csrfChain(onFailure *bool, reached *bool, session *domain.Session) http.Handler {
	h := CSRF(func(w http.ResponseWriter, r *http.Request) {
		*onFailure = true
		w.WriteHeader(http.StatusForbidden)
	})(okHandler(reached))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if session != nil {
			r = r.WithContext(WithSession(r.Context(), session))
		}
		h.ServeHTTP(w, r)
	})
}

func TestCSRFSafeMethodsExempt(t *testing.T) {
	session := &domain.Session{ID: "s1", CSRFToken: "expected"}
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		var reached, failed bool
		h := csrfChain(&failed, &reached, session)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(m, "/", nil))
		if !reached || failed {
			t.Errorf("%s must be exempt from CSRF: safe methods do not change state", m)
		}
	}
}

// The bearer-authenticated MCP endpoint relies on this: with no ambient cookie
// there is nothing for an attacker's page to ride on, so requiring a token
// there would break every MCP client for no security gain.
func TestCSRFNoSessionExempt(t *testing.T) {
	var reached, failed bool
	h := csrfChain(&failed, &reached, nil)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/", nil))
	if !reached || failed {
		t.Error("a request with no session must be exempt from CSRF")
	}
}

func TestCSRFRejectsMissingAndWrongToken(t *testing.T) {
	session := &domain.Session{ID: "s1", CSRFToken: "expected-token"}
	for _, tc := range []struct {
		name      string
		header    string
		form      string
		wantAllow bool
	}{
		{"missing entirely", "", "", false},
		{"wrong header", "wrong-token", "", false},
		{"wrong form field", "", "wrong-token", false},
		{"empty header", "", "", false},
		{"prefix of the real token", "expected", "", false},
		{"correct header", "expected-token", "", true},
		{"correct form field", "", "expected-token", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reached, failed bool
			h := csrfChain(&failed, &reached, session)

			var req *http.Request
			if tc.form != "" {
				body := url.Values{CSRFFieldName: {tc.form}}.Encode()
				req = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
				req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			} else {
				req = httptest.NewRequest(http.MethodPost, "/", nil)
			}
			if tc.header != "" {
				req.Header.Set(CSRFHeaderName, tc.header)
			}

			h.ServeHTTP(httptest.NewRecorder(), req)
			if reached != tc.wantAllow {
				t.Errorf("reached = %v, want %v", reached, tc.wantAllow)
			}
			if failed == tc.wantAllow {
				t.Errorf("failed = %v, want %v", failed, !tc.wantAllow)
			}
		})
	}
}

// --- BearerAuth --------------------------------------------------------

func bearerHandler(t *testing.T, auth BearerAuth, reached *bool) http.Handler {
	t.Helper()
	mux := http.NewServeMux()
	mw := auth.Middleware("slug", func(r *http.Request, instanceID, tokenID string) *http.Request { return r })
	mux.Handle("/mcp/{slug}", mw(okHandler(reached)))
	return mux
}

func TestBearerAuthRequiresToken(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
	}{
		{"no header", ""},
		{"wrong scheme", "Basic dXNlcjpwYXNz"},
		{"bearer with no value", "Bearer "},
		{"scheme only", "Bearer"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var reached bool
			h := bearerHandler(t, BearerAuth{Tokens: fakeTokenAuth{}}, &reached)

			req := httptest.NewRequest(http.MethodPost, "/mcp/zotero", nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if reached {
				t.Error("handler must not be reached without a bearer token")
			}
			if rec.Code != http.StatusUnauthorized {
				t.Errorf("status = %d, want 401", rec.Code)
			}
			if got := rec.Header().Get("WWW-Authenticate"); !strings.Contains(got, "Bearer") {
				t.Errorf("WWW-Authenticate = %q, want it to name the Bearer scheme", got)
			}
		})
	}
}

func TestBearerAuthInvalidToken(t *testing.T) {
	var reached bool
	h := bearerHandler(t, BearerAuth{Tokens: fakeTokenAuth{err: domain.ErrUnauthorized}}, &reached)

	req := httptest.NewRequest(http.MethodPost, "/mcp/zotero", nil)
	req.Header.Set("Authorization", "Bearer mcpaw_bogus")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if reached || rec.Code != http.StatusUnauthorized {
		t.Errorf("invalid token: reached=%v status=%d, want false/401", reached, rec.Code)
	}
}

// A token scoped to another instance must not be able to tell a real endpoint
// from an imaginary one — both answer 404, never 403.
func TestBearerAuthOutOfScopeIs404NotForbidden(t *testing.T) {
	var reached bool
	auth := BearerAuth{
		Tokens: fakeTokenAuth{token: &domain.Token{ID: "t1", InstanceID: "other-instance"}},
		Resolve: func(context.Context, string) (string, bool, error) {
			return "this-instance", true, nil
		},
	}
	h := bearerHandler(t, auth, &reached)

	req := httptest.NewRequest(http.MethodPost, "/mcp/zotero", nil)
	req.Header.Set("Authorization", "Bearer mcpaw_valid")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if reached {
		t.Fatal("an out-of-scope token must not reach the handler")
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 — a 403 would confirm the endpoint exists", rec.Code)
	}
}

// An unknown slug answers identically to an out-of-scope one, so the two are
// indistinguishable to a caller probing for endpoints.
func TestBearerAuthUnknownSlugIs404(t *testing.T) {
	var reached bool
	auth := BearerAuth{
		Tokens: fakeTokenAuth{token: &domain.Token{ID: "t1"}},
		Resolve: func(context.Context, string) (string, bool, error) {
			return "", false, domain.ErrNotFound
		},
	}
	h := bearerHandler(t, auth, &reached)

	req := httptest.NewRequest(http.MethodPost, "/mcp/nope", nil)
	req.Header.Set("Authorization", "Bearer mcpaw_valid")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if reached || rec.Code != http.StatusNotFound {
		t.Errorf("unknown slug: reached=%v status=%d, want false/404", reached, rec.Code)
	}
}

// An infrastructure failure resolving the slug must not be mistaken for
// "unknown endpoint": failing open to 404 here would mask a database outage
// as a routine client error.
func TestBearerAuthResolveInternalErrorIs500(t *testing.T) {
	var reached bool
	auth := BearerAuth{
		Tokens: fakeTokenAuth{token: &domain.Token{ID: "t1"}},
		Resolve: func(context.Context, string) (string, bool, error) {
			return "", false, errors.New("database is down")
		},
	}
	h := bearerHandler(t, auth, &reached)

	req := httptest.NewRequest(http.MethodPost, "/mcp/zotero", nil)
	req.Header.Set("Authorization", "Bearer mcpaw_valid")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if reached {
		t.Error("handler must not be reached on an internal resolution error")
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500, not a 404 that would misreport a real outage as a missing endpoint", rec.Code)
	}
}

func TestBearerAuthDisabledInstance(t *testing.T) {
	var reached bool
	auth := BearerAuth{
		Tokens: fakeTokenAuth{token: &domain.Token{ID: "t1", InstanceID: "inst"}},
		Resolve: func(context.Context, string) (string, bool, error) {
			return "inst", false, nil
		},
	}
	h := bearerHandler(t, auth, &reached)

	req := httptest.NewRequest(http.MethodPost, "/mcp/zotero", nil)
	req.Header.Set("Authorization", "Bearer mcpaw_valid")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if reached {
		t.Error("a disabled instance must not serve requests")
	}
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
}

func TestBearerAuthAllowsScopedToken(t *testing.T) {
	var reached bool
	var gotInstance, gotToken string
	auth := BearerAuth{
		Tokens:  fakeTokenAuth{token: &domain.Token{ID: "t1", InstanceID: "inst"}},
		Resolve: func(context.Context, string) (string, bool, error) { return "inst", true, nil },
		OnResult: func(_ http.ResponseWriter, _ *http.Request, instanceID, tokenID string) {
			gotInstance, gotToken = instanceID, tokenID
		},
	}
	h := bearerHandler(t, auth, &reached)

	req := httptest.NewRequest(http.MethodPost, "/mcp/zotero", nil)
	req.Header.Set("Authorization", "Bearer mcpaw_valid")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !reached || rec.Code != http.StatusOK {
		t.Fatalf("a correctly scoped token must be served: reached=%v status=%d", reached, rec.Code)
	}
	if gotInstance != "inst" || gotToken != "t1" {
		t.Errorf("OnResult got %q/%q, want inst/t1", gotInstance, gotToken)
	}
}

// The scheme is case-insensitive per RFC 7235; rejecting "bearer" would break
// conformant clients.
func TestBearerAuthSchemeIsCaseInsensitive(t *testing.T) {
	var reached bool
	auth := BearerAuth{
		Tokens:  fakeTokenAuth{token: &domain.Token{ID: "t1", InstanceID: "inst"}},
		Resolve: func(context.Context, string) (string, bool, error) { return "inst", true, nil },
	}
	h := bearerHandler(t, auth, &reached)

	req := httptest.NewRequest(http.MethodPost, "/mcp/zotero", nil)
	req.Header.Set("Authorization", "bearer mcpaw_valid")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if !reached {
		t.Error(`"bearer" must be accepted as well as "Bearer"`)
	}
}

// --- AttemptLimiter ----------------------------------------------------

func TestAttemptLimiterBlocksAfterLimit(t *testing.T) {
	l := NewAttemptLimiter(3, time.Minute)

	for i := range 3 {
		if !l.Allow("1.2.3.4") {
			t.Fatalf("attempt %d should be allowed, the limit is 3", i+1)
		}
		l.RecordFailure("1.2.3.4")
	}
	if l.Allow("1.2.3.4") {
		t.Error("a fourth attempt must be blocked")
	}
	if !l.Allow("5.6.7.8") {
		t.Error("the limit must be per key: another address is unaffected")
	}
}

func TestAttemptLimiterWindowExpiry(t *testing.T) {
	l := NewAttemptLimiter(1, time.Minute)
	now := time.Now()
	l.now = func() time.Time { return now }

	l.RecordFailure("k")
	if l.Allow("k") {
		t.Fatal("should be blocked inside the window")
	}

	now = now.Add(2 * time.Minute)
	if !l.Allow("k") {
		t.Error("the counter must reset once the window has passed")
	}
}

// Someone who mistypes twice then succeeds must not stay throttled.
func TestAttemptLimiterSuccessClearsCounter(t *testing.T) {
	l := NewAttemptLimiter(2, time.Minute)
	l.RecordFailure("k")
	l.RecordFailure("k")
	if l.Allow("k") {
		t.Fatal("should be blocked after reaching the limit")
	}

	l.RecordSuccess("k")
	if !l.Allow("k") {
		t.Error("a successful attempt must clear the failure counter")
	}
}

// Without pruning, an attacker cycling through distinct keys (e.g. spoofed
// IPs on an untrusted-proxy deployment) grows counts unboundedly.
func TestAttemptLimiterPrunesExpiredEntriesUnderPressure(t *testing.T) {
	l := NewAttemptLimiter(5, time.Minute)
	now := time.Now()
	l.now = func() time.Time { return now }

	for i := range maxAttemptKeys {
		l.RecordFailure(strconv.Itoa(i))
	}
	// Every prior window has now elapsed.
	now = now.Add(2 * time.Minute)
	l.RecordFailure("trigger-a-prune")

	l.mu.Lock()
	n := len(l.counts)
	l.mu.Unlock()
	if n >= maxAttemptKeys {
		t.Errorf("counts len = %d, want pruning to have reclaimed expired entries once the map grew large", n)
	}
}

func TestNewAttemptLimiterRejectsNonsenseBounds(t *testing.T) {
	l := NewAttemptLimiter(0, 0)
	if l.limit <= 0 || l.window <= 0 {
		t.Fatalf("limit/window = %d/%v, want positive defaults rather than a limiter that blocks everything",
			l.limit, l.window)
	}
	if !l.Allow("k") {
		t.Error("a limiter built with nonsense bounds must not block the first attempt")
	}
}
