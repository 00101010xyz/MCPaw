// Package logging configures structured logging and provides request-scoped
// helpers.
//
// All logs are structured (slog) so they can be queried rather than grepped.
// The package also defines the redaction rules that keep credentials out of
// log output regardless of which component does the logging.
package logging

import (
	"context"
	"io"
	"log/slog"
	"strings"
)

type ctxKey int

const (
	ctxKeyLogger ctxKey = iota
	ctxKeyRequestID
)

// New builds the root logger from a level and format string.
func New(w io.Writer, level, format string) *slog.Logger {
	var lv slog.Level
	switch strings.ToLower(level) {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: lv, ReplaceAttr: redact}
	var h slog.Handler
	if strings.ToLower(format) == "text" {
		h = slog.NewTextHandler(w, opts)
	} else {
		h = slog.NewJSONHandler(w, opts)
	}
	return slog.New(h)
}

// sensitiveKeys are attribute names whose values must never be written to logs.
// The match is case-insensitive and substring-based, which is deliberately
// broad: a false positive costs a redacted debug line, a false negative leaks a
// credential.
var sensitiveKeys = []string{
	"password", "secret", "token", "authorization", "apikey", "api_key",
	"cookie", "credential", "key", "csrf",
}

func redact(_ []string, a slog.Attr) slog.Attr {
	lower := strings.ToLower(a.Key)
	for _, k := range sensitiveKeys {
		if strings.Contains(lower, k) {
			return slog.String(a.Key, "[redacted]")
		}
	}
	return a
}

// WithLogger returns a context carrying the given logger.
func WithLogger(ctx context.Context, l *slog.Logger) context.Context {
	return context.WithValue(ctx, ctxKeyLogger, l)
}

// FromContext returns the request-scoped logger, or a no-op logger when none
// was installed. It never returns nil, so callers need no nil checks.
func FromContext(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxKeyLogger).(*slog.Logger); ok && l != nil {
		return l
	}
	return slog.New(discardHandler{})
}

// WithRequestID returns a context carrying a request correlation ID.
func WithRequestID(ctx context.Context, rid string) context.Context {
	return context.WithValue(ctx, ctxKeyRequestID, rid)
}

// RequestID returns the request correlation ID, or "" when absent.
func RequestID(ctx context.Context) string {
	if s, ok := ctx.Value(ctxKeyRequestID).(string); ok {
		return s
	}
	return ""
}

type discardHandler struct{}

func (discardHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardHandler) Handle(context.Context, slog.Record) error { return nil }
func (h discardHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h discardHandler) WithGroup(string) slog.Handler           { return h }
