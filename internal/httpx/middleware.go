package httpx

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/00101010xyz/mcpaw/internal/platform/id"
	"github.com/00101010xyz/mcpaw/internal/platform/logging"
	"github.com/00101010xyz/mcpaw/internal/platform/metrics"
)

// Middleware is the standard decorator signature used throughout the server.
type Middleware func(http.Handler) http.Handler

// Chain composes middleware so that the first argument is the outermost layer.
// Reading a chain top to bottom therefore matches the order a request passes
// through it.
func Chain(mw ...Middleware) Middleware {
	return func(next http.Handler) http.Handler {
		for i := len(mw) - 1; i >= 0; i-- {
			next = mw[i](next)
		}
		return next
	}
}

// responseRecorder captures the status and size for logging and metrics.
type responseRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *responseRecorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
		r.ResponseWriter.WriteHeader(status)
	}
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Flush forwards to the underlying writer so Server-Sent Events keep working
// through the middleware chain.
func (r *responseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		if r.status == 0 {
			r.status = http.StatusOK
		}
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the original writer.
func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// RequestContext installs the request ID, the resolved client IP and a
// request-scoped logger. It is the outermost middleware so everything after it
// can log with correlation.
func RequestContext(logger *slog.Logger, trustProxy bool) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			requestID := r.Header.Get("X-Request-Id")
			if requestID == "" || len(requestID) > 64 {
				requestID = id.New("req")
			}
			clientIP := resolveClientIP(r, trustProxy)

			ctx := logging.WithRequestID(r.Context(), requestID)
			ctx = WithClientIP(ctx, clientIP)
			ctx = logging.WithLogger(ctx, logger.With(
				slog.String("request_id", requestID),
				slog.String("client_ip", clientIP),
			))
			w.Header().Set("X-Request-Id", requestID)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Recover converts a panic into a 500 without taking the process down.
//
// A panic in one request must never be able to stop the platform serving every
// other instance, and the stack trace belongs in the log rather than in the
// response body.
func Recover() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					if rec == http.ErrAbortHandler {
						// The standard library uses this to abort a response
						// deliberately; re-panicking preserves that contract.
						panic(rec)
					}
					logging.FromContext(r.Context()).Error("panic recovered in handler",
						slog.String("panic", fmt.Sprint(rec)),
						slog.String("path", r.URL.Path),
						slog.String("stack", string(debug.Stack())))
					http.Error(w, "internal server error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// SecurityHeaders applies the browser hardening headers and mints the
// per-request CSP nonce.
func SecurityHeaders(enableHSTS bool) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			nonce, err := newNonce()
			if err != nil {
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			h := w.Header()
			// default-src 'none' means anything the page needs must be listed
			// explicitly, so a future template that pulls in a CDN fails
			// visibly during development instead of silently widening the
			// attack surface in production.
			h.Set("Content-Security-Policy", strings.Join([]string{
				"default-src 'none'",
				"base-uri 'none'",
				"form-action 'self'",
				"frame-ancestors 'none'",
				"img-src 'self' data:",
				"style-src 'self' 'nonce-" + nonce + "'",
				"script-src 'self' 'nonce-" + nonce + "'",
				"connect-src 'self'",
				"font-src 'self'",
			}, "; "))
			h.Set("X-Content-Type-Options", "nosniff")
			h.Set("X-Frame-Options", "DENY")
			h.Set("Referrer-Policy", "no-referrer")
			h.Set("Cross-Origin-Opener-Policy", "same-origin")
			h.Set("Cross-Origin-Resource-Policy", "same-origin")
			h.Set("Permissions-Policy", "geolocation=(), camera=(), microphone=(), payment=()")
			if enableHSTS {
				h.Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
			}
			next.ServeHTTP(w, r.WithContext(WithNonce(r.Context(), nonce)))
		})
	}
}

func newNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(b), nil
}

// AccessLog records one structured line per request and feeds the metrics
// registry.
func AccessLog(reg *metrics.Registry, routeLabel string) Middleware {
	var (
		requests *metrics.Counter
		latency  *metrics.Histogram
	)
	if reg != nil {
		latency = reg.NewHistogram("mcpaw_http_request_duration_seconds",
			"HTTP request latency in seconds.", nil, []string{"route"}, []string{routeLabel})
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &responseRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)

			elapsed := time.Since(start)
			if rec.status == 0 {
				rec.status = http.StatusOK
			}
			logging.FromContext(r.Context()).Info("http request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Int("status", rec.status),
				slog.Int("bytes", rec.bytes),
				slog.Int64("duration_ms", elapsed.Milliseconds()))

			if reg != nil {
				requests = reg.NewCounter("mcpaw_http_requests_total",
					"Total HTTP requests handled.",
					[]string{"route", "method", "status"},
					[]string{routeLabel, r.Method, statusClass(rec.status)})
				requests.Inc()
				latency.Observe(elapsed.Seconds())
			}
		})
	}
}

// statusClass buckets statuses so the metric's cardinality stays bounded no
// matter how many distinct codes an upstream invents.
func statusClass(status int) string {
	switch {
	case status < 200:
		return "1xx"
	case status < 300:
		return "2xx"
	case status < 400:
		return "3xx"
	case status < 500:
		return "4xx"
	default:
		return "5xx"
	}
}

// BodyLimit caps request bodies before a handler reads them.
func BodyLimit(max int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, max)
			next.ServeHTTP(w, r)
		})
	}
}

// NoStore prevents caches from retaining authenticated pages and API responses.
func NoStore() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store, max-age=0")
			w.Header().Set("Pragma", "no-cache")
			next.ServeHTTP(w, r)
		})
	}
}
