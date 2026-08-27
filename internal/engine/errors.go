// Package engine turns a validated tool call into a guarded upstream HTTP
// request and maps the response back into an MCP-shaped result.
//
// It is the only place that composes the connector description, the instance
// configuration and the caller's arguments, and it applies every runtime
// control — schema validation, rate limiting, concurrency, circuit breaking,
// timeouts and response caps — in a fixed order that no call site can skip.
package engine

import (
	"errors"
	"fmt"
	"strings"

	"github.com/00101010xyz/mcpaw/internal/domain"
)

// Failure kinds. They are stable strings because they surface in metrics
// labels, the audit log and MCP error payloads, where a renamed constant would
// silently break a dashboard.
const (
	KindInvalidArguments = "invalid_arguments"
	KindNotConfigured    = "not_configured"
	KindRateLimited      = "rate_limited"
	KindCircuitOpen      = "circuit_open"
	KindEgressBlocked    = "egress_blocked"
	KindTimeout          = "timeout"
	KindUpstreamStatus   = "upstream_status"
	KindUpstreamFailure  = "upstream_failure"
	KindResponseTooLarge = "response_too_large"
	KindInternal         = "internal"
)

// Error is a tool-call failure with a kind that callers can branch on and a
// message that is safe to return to an MCP client.
//
// The distinction matters: err carries the operational detail for the log,
// Message carries only what the caller is entitled to know. Upstream error
// bodies and internal failures never reach a model through Message.
type Error struct {
	Kind       string
	Message    string
	StatusCode int
	// Details carries structured, non-sensitive context (for example the list
	// of schema violations) for inclusion in the tool result.
	Details []string
	err     error
}

// Error implements error.
func (e *Error) Error() string {
	if e.err != nil {
		return fmt.Sprintf("%s: %s: %v", e.Kind, e.Message, e.err)
	}
	return fmt.Sprintf("%s: %s", e.Kind, e.Message)
}

// Unwrap exposes the underlying cause.
func (e *Error) Unwrap() error { return e.err }

// Retryable reports whether the caller could reasonably try again later.
func (e *Error) Retryable() bool {
	switch e.Kind {
	case KindRateLimited, KindCircuitOpen, KindTimeout, KindUpstreamFailure:
		return true
	case KindUpstreamStatus:
		return e.StatusCode == 429 || e.StatusCode >= 500
	default:
		return false
	}
}

// FullMessage renders the message plus any details as one human-readable
// string, which is what an MCP client shows a model.
func (e *Error) FullMessage() string {
	if len(e.Details) == 0 {
		return e.Message
	}
	return e.Message + ": " + strings.Join(e.Details, "; ")
}

func newError(kind, message string, cause error) *Error {
	return &Error{Kind: kind, Message: message, err: cause}
}

// DomainError maps an engine failure onto the domain sentinel that best
// describes it, so HTTP adapters can pick a status code without knowing about
// engine kinds.
func (e *Error) DomainError() error {
	switch e.Kind {
	case KindInvalidArguments, KindNotConfigured:
		return domain.ErrInvalidInput
	case KindRateLimited:
		return domain.ErrRateLimited
	case KindEgressBlocked:
		return domain.ErrEgressBlocked
	default:
		return domain.ErrUpstream
	}
}

// AsError extracts an *Error from an error chain.
func AsError(err error) (*Error, bool) {
	var e *Error
	ok := errors.As(err, &e)
	return e, ok
}
