package httpx

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/00101010xyz/mcpaw/internal/platform/metrics"
)

func TestChainOrderOutermostFirst(t *testing.T) {
	var order []string
	mark := func(name string) Middleware {
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				order = append(order, name)
				next.ServeHTTP(w, r)
			})
		}
	}
	h := Chain(mark("a"), mark("b"), mark("c"))(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		order = append(order, "handler")
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	want := "a,b,c,handler"
	if got := strings.Join(order, ","); got != want {
		t.Errorf("execution order = %q, want %q — the first Chain argument must run first", got, want)
	}
}

func TestChainEmptyIsPassthrough(t *testing.T) {
	var reached bool
	h := Chain()(okHandler(&reached))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if !reached {
		t.Error("an empty chain must still reach the handler")
	}
}

// --- RequestContext ------------------------------------------------------

func TestRequestContextGeneratesRequestID(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var gotIP string
	h := RequestContext(logger, false)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIP = ClientIPFrom(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.5:54321"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Header().Get("X-Request-Id") == "" {
		t.Error("X-Request-Id must be set on the response")
	}
	if gotIP != "203.0.113.5" {
		t.Errorf("ClientIPFrom = %q, want the RemoteAddr host without the port", gotIP)
	}
}

// Trusting X-Forwarded-For without a real proxy in front lets any client
// spoof its own address in the audit log — this is the load-bearing test for
// that default.
func TestRequestContextIgnoresForwardedHeaderUntrusted(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var gotIP string
	h := RequestContext(logger, false)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIP = ClientIPFrom(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.5:54321"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if gotIP != "203.0.113.5" {
		t.Errorf("ClientIPFrom = %q, want the spoofable X-Forwarded-For header ignored", gotIP)
	}
}

func TestRequestContextHonoursForwardedHeaderWhenTrusted(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var gotIP string
	h := RequestContext(logger, true)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIP = ClientIPFrom(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.5:54321"
	req.Header.Set("X-Forwarded-For", "1.2.3.4, 203.0.113.5")
	h.ServeHTTP(httptest.NewRecorder(), req)

	if gotIP != "1.2.3.4" {
		t.Errorf("ClientIPFrom = %q, want the first hop of X-Forwarded-For when the proxy is trusted", gotIP)
	}
}

// A malicious or malformed inbound request ID must not be reflected verbatim
// into logs.
func TestRequestContextRejectsOversizedRequestID(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := RequestContext(logger, false)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-Id", strings.Repeat("a", 200))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Request-Id"); got == strings.Repeat("a", 200) {
		t.Error("an oversized inbound request ID must be replaced with a generated one")
	}
}

func TestRequestContextPreservesReasonableRequestID(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := RequestContext(logger, false)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-Request-Id", "client-supplied-id")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if got := rec.Header().Get("X-Request-Id"); got != "client-supplied-id" {
		t.Errorf("X-Request-Id = %q, want the caller-supplied id preserved for tracing", got)
	}
}

// --- Recover ---------------------------------------------------------------

func TestRecoverConvertsPanicTo500(t *testing.T) {
	h := Recover()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic("boom")
	}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)

	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic escaped Recover: %v", r)
			}
		}()
		h.ServeHTTP(rec, req)
	}()

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

func TestRecoverDoesNotSwallowOtherRequests(t *testing.T) {
	// A panic in one request must not prevent handling the next — verified by
	// simply calling the wrapped handler again after a panic.
	var calls int
	h := Recover()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			panic("first request explodes")
		}
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h.ServeHTTP(httptest.NewRecorder(), req)
	h.ServeHTTP(httptest.NewRecorder(), req)

	if calls != 2 {
		t.Errorf("calls = %d, want 2 — the second request must still be served", calls)
	}
}

func TestRecoverRepanicsErrAbortHandler(t *testing.T) {
	h := Recover()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		panic(http.ErrAbortHandler)
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)

	defer func() {
		if r := recover(); r != http.ErrAbortHandler {
			t.Errorf("recovered %v, want http.ErrAbortHandler re-panicked so the stdlib's own contract holds", r)
		}
	}()
	h.ServeHTTP(httptest.NewRecorder(), req)
	t.Fatal("expected a re-panic that never reached here")
}

// --- SecurityHeaders -------------------------------------------------------

func TestSecurityHeadersSetAndNonceUnique(t *testing.T) {
	h := SecurityHeaders(false)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(NonceFrom(r.Context())))
	}))

	rec1 := httptest.NewRecorder()
	h.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/", nil))
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/", nil))

	for _, name := range []string{
		"Content-Security-Policy", "X-Content-Type-Options", "X-Frame-Options",
		"Referrer-Policy", "Cross-Origin-Opener-Policy", "Cross-Origin-Resource-Policy",
		"Permissions-Policy",
	} {
		if rec1.Header().Get(name) == "" {
			t.Errorf("%s must be set", name)
		}
	}
	if rec1.Header().Get("Strict-Transport-Security") != "" {
		t.Error("HSTS must not be set when enableHSTS is false — it would break plain-HTTP deployments")
	}
	csp := rec1.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'none'") {
		t.Errorf("CSP = %q, want a default-deny baseline", csp)
	}
	n1, n2 := rec1.Body.String(), rec2.Body.String()
	if n1 == "" || n1 == n2 {
		t.Errorf("nonce reused across requests: %q vs %q — this defeats the point of a nonce", n1, n2)
	}
	if !strings.Contains(csp, n1) {
		t.Error("the CSP header must carry the same nonce exposed to the handler")
	}
}

func TestSecurityHeadersHSTSWhenEnabled(t *testing.T) {
	h := SecurityHeaders(true)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Header().Get("Strict-Transport-Security") == "" {
		t.Error("HSTS must be set when enableHSTS is true")
	}
}

// --- BodyLimit / NoStore -----------------------------------------------

func TestBodyLimitRejectsOversizedBody(t *testing.T) {
	h := BodyLimit(8)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, err := io.ReadAll(r.Body)
		if err == nil {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusRequestEntityTooLarge)
	}))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("this body is far longer than 8 bytes"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want the oversized body to be rejected while reading", rec.Code)
	}
}

func TestBodyLimitAllowsWithinLimit(t *testing.T) {
	h := BodyLimit(1024)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.ReadAll(r.Body); err != nil {
			t.Errorf("unexpected read error: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("small body"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 for a body within the limit", rec.Code)
	}
}

// --- AccessLog -----------------------------------------------------------

func TestAccessLogRecordsStatusAndPassesThrough(t *testing.T) {
	h := AccessLog(nil, "test-route")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		w.Write([]byte("short and stout"))
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusTeapot {
		t.Errorf("AccessLog must not alter the response: status = %d, want %d", rec.Code, http.StatusTeapot)
	}
	if rec.Body.String() != "short and stout" {
		t.Errorf("AccessLog must not alter the body: got %q", rec.Body.String())
	}
}

// A handler that never calls WriteHeader implicitly answers 200 — the
// recorder must report that rather than 0, since 0 would be logged as a
// broken response.
func TestResponseRecorderDefaultsStatusOnWriteWithoutHeader(t *testing.T) {
	rec := &responseRecorder{ResponseWriter: httptest.NewRecorder()}
	rec.Write([]byte("hello"))
	if rec.status != http.StatusOK {
		t.Errorf("status = %d, want %d when Write is called before WriteHeader", rec.status, http.StatusOK)
	}
	if rec.bytes != len("hello") {
		t.Errorf("bytes = %d, want %d", rec.bytes, len("hello"))
	}
}

func TestResponseRecorderWriteHeaderIsSticky(t *testing.T) {
	inner := httptest.NewRecorder()
	rec := &responseRecorder{ResponseWriter: inner}
	rec.WriteHeader(http.StatusCreated)
	rec.WriteHeader(http.StatusInternalServerError)
	if rec.status != http.StatusCreated {
		t.Errorf("status = %d, want the first WriteHeader call to win, matching net/http semantics", rec.status)
	}
	if inner.Code != http.StatusCreated {
		t.Errorf("underlying recorder got %d, want %d", inner.Code, http.StatusCreated)
	}
}

func TestResponseRecorderUnwrap(t *testing.T) {
	inner := httptest.NewRecorder()
	rec := &responseRecorder{ResponseWriter: inner}
	if rec.Unwrap() != inner {
		t.Error("Unwrap must return the original writer so http.ResponseController can reach it")
	}
}

func TestAccessLogWithRegistryRecordsMetrics(t *testing.T) {
	reg := metrics.NewRegistry()
	h := AccessLog(reg, "zotero")(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))

	var buf strings.Builder
	if err := reg.Write(&buf); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, `route="zotero"`) {
		t.Errorf("metrics output missing route label:\n%s", out)
	}
	if !strings.Contains(out, `status="4xx"`) {
		t.Errorf("metrics output missing bucketed status label:\n%s", out)
	}
}

// httptest always gives RemoteAddr a port; a raw address with none (as some
// non-TCP listeners produce) must still resolve to something rather than
// erroring the request.
func TestRequestContextRemoteAddrWithoutPort(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	var gotIP string
	h := RequestContext(logger, false)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotIP = ClientIPFrom(r.Context())
	}))

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "no-port-here"
	h.ServeHTTP(httptest.NewRecorder(), req)

	if gotIP != "no-port-here" {
		t.Errorf("ClientIPFrom = %q, want the raw RemoteAddr as a fallback when it has no port", gotIP)
	}
}

func TestNoStoreHeaders(t *testing.T) {
	h := NoStore()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if got := rec.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Errorf("Cache-Control = %q, want no-store — an authenticated page must never be cached", got)
	}
}
