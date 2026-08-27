package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/00101010xyz/mcpaw/internal/platform/logging"
)

// Transport headers defined by the Streamable HTTP transport.
const (
	HeaderSessionID       = "Mcp-Session-Id"
	HeaderProtocolVersion = "MCP-Protocol-Version"
)

type ctxKey int

const ctxKeyInstanceID ctxKey = iota

// WithInstanceID returns a context carrying the authenticated instance.
//
// Authentication happens in middleware, before this package is reached, so the
// protocol handler can assume the caller is already authorised for exactly one
// instance and never has to make an authorisation decision itself.
func WithInstanceID(ctx context.Context, instanceID string) context.Context {
	return context.WithValue(ctx, ctxKeyInstanceID, instanceID)
}

// InstanceIDFrom extracts the authenticated instance from a context.
func InstanceIDFrom(ctx context.Context) (string, bool) {
	id, ok := ctx.Value(ctxKeyInstanceID).(string)
	return id, ok && id != ""
}

// HandlerOptions tunes the transport.
type HandlerOptions struct {
	// MaxBodyBytes caps a single JSON-RPC request body.
	MaxBodyBytes int64
	// SSEKeepalive is the interval between comment frames on an open stream,
	// which keeps intermediaries from dropping an idle connection.
	SSEKeepalive time.Duration
	// SSEMaxLifetime bounds how long one stream may stay open, so a forgotten
	// client cannot hold a connection forever.
	SSEMaxLifetime time.Duration
}

func (o HandlerOptions) withDefaults() HandlerOptions {
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = 1 << 20
	}
	if o.SSEKeepalive <= 0 {
		o.SSEKeepalive = 15 * time.Second
	}
	if o.SSEMaxLifetime <= 0 {
		o.SSEMaxLifetime = 30 * time.Minute
	}
	return o
}

// Handler implements the MCP Streamable HTTP transport.
type Handler struct {
	server *Server
	opts   HandlerOptions
}

// NewHandler wraps a Server in the HTTP transport.
func NewHandler(server *Server, opts HandlerOptions) *Handler {
	return &Handler{server: server, opts: opts.withDefaults()}
}

// ServeHTTP routes the three verbs the transport defines.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		h.handlePost(w, r)
	case http.MethodGet:
		h.handleGet(w, r)
	case http.MethodDelete:
		h.handleDelete(w, r)
	default:
		w.Header().Set("Allow", "GET, POST, DELETE")
		writeTransportError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (h *Handler) handlePost(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	log := logging.FromContext(ctx)

	instanceID, ok := InstanceIDFrom(ctx)
	if !ok {
		// Reaching here means the handler was mounted without the
		// authentication middleware. Fail closed and loudly rather than
		// serving an unauthenticated request.
		log.Error("mcp handler reached without an authenticated instance")
		writeTransportError(w, http.StatusInternalServerError, "server misconfiguration")
		return
	}

	if err := checkProtocolVersionHeader(r); err != nil {
		writeTransportError(w, http.StatusBadRequest, err.Error())
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, h.opts.MaxBodyBytes))
	if err != nil {
		writeTransportError(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("request body exceeds the %d byte limit", h.opts.MaxBodyBytes))
		return
	}

	requests, isBatch, err := decodeRequests(body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest,
			newError(nil, CodeParseError, "could not parse JSON-RPC request: "+err.Error(), nil))
		return
	}

	// A session is looked up when the client supplies one. An unknown id gets a
	// 404, which the specification defines as the signal for the client to
	// start a fresh session rather than retrying forever.
	var session *Session
	if id := r.Header.Get(HeaderSessionID); id != "" {
		session, ok = h.server.sessions.Get(id)
		if !ok {
			writeTransportError(w, http.StatusNotFound, "unknown or expired session; re-initialize")
			return
		}
		if session.InstanceID != instanceID {
			// The token authorises one instance; a session id from another must
			// never be usable here.
			writeTransportError(w, http.StatusNotFound, "unknown or expired session; re-initialize")
			return
		}
	}

	responses := make([]*Response, 0, len(requests))
	for _, req := range requests {
		if req == nil {
			responses = append(responses, newError(nil, CodeInvalidRequest, "null request in batch", nil))
			continue
		}
		resp := h.server.Handle(ctx, instanceID, session, req)

		// A successful initialize mints the session that later requests echo.
		if req.Method == MethodInitialize && resp != nil && resp.Error == nil && session == nil {
			var params InitializeParams
			_ = json.Unmarshal(req.Params, &params)
			created, err := h.server.sessions.Create(instanceID, params.ProtocolVersion, params.ClientInfo)
			if err != nil {
				log.Warn("could not create mcp session", slog.String("error", err.Error()))
			} else {
				session = created
				w.Header().Set(HeaderSessionID, created.ID)
				log.Info("mcp session initialized",
					slog.String("session_id", created.ID),
					slog.String("client", params.ClientInfo.Name),
					slog.String("protocol_version", params.ProtocolVersion))
			}
		}
		if resp != nil {
			responses = append(responses, resp)
		}
	}

	if len(responses) == 0 {
		// Every message was a notification: acknowledged, nothing to return.
		w.WriteHeader(http.StatusAccepted)
		return
	}

	var payload any = responses[0]
	if isBatch {
		payload = responses
	}

	if prefersSSE(r) {
		h.writeSSEResponse(w, payload)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

// handleGet opens the server-to-client stream.
//
// MCPaw never initiates a message, so this stream carries only keepalives. The
// specification permits answering GET with 405 instead, but many clients open
// the stream during startup and treat a failure as fatal, so serving a
// well-behaved idle stream is the more compatible choice.
func (h *Handler) handleGet(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if _, ok := InstanceIDFrom(ctx); !ok {
		writeTransportError(w, http.StatusInternalServerError, "server misconfiguration")
		return
	}
	if id := r.Header.Get(HeaderSessionID); id != "" {
		if _, ok := h.server.sessions.Get(id); !ok {
			writeTransportError(w, http.StatusNotFound, "unknown or expired session; re-initialize")
			return
		}
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeTransportError(w, http.StatusInternalServerError, "streaming is not supported by this server")
		return
	}

	setSSEHeaders(w)
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	ticker := time.NewTicker(h.opts.SSEKeepalive)
	defer ticker.Stop()
	deadline := time.NewTimer(h.opts.SSEMaxLifetime)
	defer deadline.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-deadline.C:
			return
		case <-ticker.C:
			// A comment frame is a no-op for the client but keeps proxies and
			// load balancers from reaping the connection.
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func (h *Handler) handleDelete(w http.ResponseWriter, r *http.Request) {
	if id := r.Header.Get(HeaderSessionID); id != "" {
		h.server.sessions.Delete(id)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) writeSSEResponse(w http.ResponseWriter, payload any) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusOK, payload)
		return
	}
	data, err := json.Marshal(payload)
	if err != nil {
		writeTransportError(w, http.StatusInternalServerError, "could not encode response")
		return
	}

	setSSEHeaders(w)
	w.WriteHeader(http.StatusOK)
	// One event carrying the response, then the stream ends: there is nothing
	// further for this request.
	fmt.Fprintf(w, "event: message\ndata: %s\n\n", data)
	flusher.Flush()
}

func setSSEHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-store")
	h.Set("Connection", "keep-alive")
	// Nginx buffers proxied responses by default, which would hold keepalives
	// until the buffer fills and defeat the point of the stream.
	h.Set("X-Accel-Buffering", "no")
}

// prefersSSE reports whether the client asked for an event stream and did not
// also accept plain JSON.
func prefersSSE(r *http.Request) bool {
	accept := strings.ToLower(r.Header.Get("Accept"))
	if accept == "" {
		return false
	}
	wantsSSE := strings.Contains(accept, "text/event-stream")
	wantsJSON := strings.Contains(accept, "application/json") || strings.Contains(accept, "*/*")
	return wantsSSE && !wantsJSON
}

// checkProtocolVersionHeader validates the negotiated-version echo.
//
// A conformant client sends back the version the server returned from
// initialize, so an unsupported value here means the client is out of step and
// deserves a clear 400 rather than a confusing downstream failure.
func checkProtocolVersionHeader(r *http.Request) error {
	v := r.Header.Get(HeaderProtocolVersion)
	if v == "" {
		return nil
	}
	if isSupportedVersion(v) {
		return nil
	}
	return fmt.Errorf("unsupported MCP protocol version %q; this server supports %s",
		v, strings.Join(supportedProtocolVersions, ", "))
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(payload); err != nil {
		// The status line is already written, so the only remaining option is
		// to drop the connection; the client sees a truncated body and retries.
		return
	}
}

// writeTransportError reports a transport-level failure as a JSON-RPC error
// object, so a client that only knows how to parse JSON-RPC still gets a
// structured, actionable message.
func writeTransportError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, newError(nil, CodeInvalidRequest, message, nil))
}
