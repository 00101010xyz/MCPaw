// Package httpx holds the HTTP building blocks shared by the admin JSON API and
// the web UI: the middleware chain, request-scoped identity, and the helpers
// that turn domain errors into responses.
//
// Keeping them here means the security controls are written once and applied
// identically to both surfaces, rather than being re-implemented (and
// eventually diverging) in each.
package httpx

import (
	"context"
	"net"
	"net/http"
	"strings"

	"github.com/00101010xyz/mcpaw/internal/domain"
)

type ctxKey int

const (
	ctxKeyUser ctxKey = iota
	ctxKeySession
	ctxKeyNonce
	ctxKeyClientIP
)

// WithUser attaches the authenticated administrator to a context.
func WithUser(ctx context.Context, u *domain.User) context.Context {
	return context.WithValue(ctx, ctxKeyUser, u)
}

// UserFrom returns the authenticated administrator, if any.
func UserFrom(ctx context.Context) (*domain.User, bool) {
	u, ok := ctx.Value(ctxKeyUser).(*domain.User)
	return u, ok && u != nil
}

// WithSession attaches the web session to a context.
func WithSession(ctx context.Context, s *domain.Session) context.Context {
	return context.WithValue(ctx, ctxKeySession, s)
}

// SessionFrom returns the web session, if any.
func SessionFrom(ctx context.Context) (*domain.Session, bool) {
	s, ok := ctx.Value(ctxKeySession).(*domain.Session)
	return s, ok && s != nil
}

// WithNonce attaches the per-request Content-Security-Policy nonce.
func WithNonce(ctx context.Context, nonce string) context.Context {
	return context.WithValue(ctx, ctxKeyNonce, nonce)
}

// NonceFrom returns the CSP nonce for this request.
func NonceFrom(ctx context.Context) string {
	n, _ := ctx.Value(ctxKeyNonce).(string)
	return n
}

// WithClientIP attaches the resolved client address.
func WithClientIP(ctx context.Context, ip string) context.Context {
	return context.WithValue(ctx, ctxKeyClientIP, ip)
}

// ClientIPFrom returns the resolved client address.
func ClientIPFrom(ctx context.Context) string {
	ip, _ := ctx.Value(ctxKeyClientIP).(string)
	return ip
}

// resolveClientIP determines the caller's address.
//
// X-Forwarded-For is honoured only when the operator has declared that a
// trusted proxy sits in front of the process. Trusting it unconditionally would
// let any client forge the address that lands in the audit log and in the
// login rate limiter's key — turning both controls into decoration.
func resolveClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first, _, _ := strings.Cut(xff, ",")
			if ip := strings.TrimSpace(first); ip != "" {
				return ip
			}
		}
		if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
			return real
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
