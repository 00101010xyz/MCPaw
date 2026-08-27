package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"github.com/00101010xyz/mcpaw/internal/domain"
	"github.com/00101010xyz/mcpaw/internal/platform/logging"
)

// Backend is the port through which the protocol layer reaches the platform.
//
// Everything domain-specific lives behind this interface, which is what lets
// the MCP implementation be tested with a fake backend and lets the platform
// evolve without touching protocol code.
type Backend interface {
	// Describe returns the server identity and usage instructions for an
	// instance, used to answer initialize.
	Describe(ctx context.Context, instanceID string) (Implementation, string, error)
	// ListTools returns the tools currently enabled on an instance.
	ListTools(ctx context.Context, instanceID string) ([]Tool, error)
	// CallTool invokes one tool. Tool-level failures are returned as a result
	// with IsError set; only protocol-level problems (unknown tool, instance
	// gone) are returned as errors.
	CallTool(ctx context.Context, instanceID, name string, args map[string]any) (*CallToolResult, error)
}

// Server dispatches MCP methods against a Backend.
type Server struct {
	backend  Backend
	sessions *SessionStore
	info     Implementation
}

// ServerConfig wires a Server.
type ServerConfig struct {
	Backend  Backend
	Sessions *SessionStore
	// Info identifies this platform build to clients.
	Info Implementation
}

// NewServer constructs a Server. The backend is required; everything else has a
// safe default.
func NewServer(cfg ServerConfig) (*Server, error) {
	if cfg.Backend == nil {
		return nil, errors.New("mcp: a backend is required")
	}
	if cfg.Sessions == nil {
		cfg.Sessions = NewSessionStore(SessionConfig{})
	}
	if cfg.Info.Name == "" {
		cfg.Info = Implementation{Name: "mcpaw", Version: "dev"}
	}
	return &Server{backend: cfg.Backend, sessions: cfg.Sessions, info: cfg.Info}, nil
}

// Sessions exposes the session store so the platform can invalidate sessions
// when an instance changes.
func (s *Server) Sessions() *SessionStore { return s.sessions }

// Handle dispatches one JSON-RPC request and returns the response, or nil for a
// notification.
//
// The session argument may be nil: MCPaw accepts requests from clients that do
// not echo the session header, because the tool path needs no session state and
// refusing them would break otherwise-conformant clients for no security gain
// (authentication is carried by the bearer token, not the session).
func (s *Server) Handle(ctx context.Context, instanceID string, sess *Session, req *Request) *Response {
	if rpcErr := req.Validate(); rpcErr != nil {
		if req.IsNotification() {
			return nil
		}
		return newError(req.ID, rpcErr.Code, rpcErr.Message, nil)
	}

	log := logging.FromContext(ctx).With(
		slog.String("mcp_method", req.Method),
		slog.String("instance_id", instanceID),
	)

	switch req.Method {
	case MethodInitialize:
		return s.handleInitialize(ctx, instanceID, req)
	case MethodPing:
		return s.reply(req, EmptyResult{})
	case MethodToolsList:
		return s.handleToolsList(ctx, instanceID, req)
	case MethodToolsCall:
		return s.handleToolsCall(ctx, instanceID, req)
	case MethodInitialized, MethodCancelled:
		// Notifications with nothing to do. Acknowledged by returning nothing.
		return nil
	default:
		if req.IsNotification() {
			// An unknown notification is not an error: the specification allows
			// a peer to send notifications the other side does not implement.
			log.Debug("ignoring unknown notification")
			return nil
		}
		log.Debug("unknown method")
		return newError(req.ID, CodeMethodNotFound, "unknown method: "+req.Method, nil)
	}
}

// reply builds a success response, or nil when the request was a notification.
func (s *Server) reply(req *Request, result any) *Response {
	if req.IsNotification() {
		return nil
	}
	return newResult(req.ID, result)
}

func (s *Server) handleInitialize(ctx context.Context, instanceID string, req *Request) *Response {
	var params InitializeParams
	if rpcErr := decodeParams(req.Params, &params); rpcErr != nil {
		return newError(req.ID, rpcErr.Code, rpcErr.Message, nil)
	}

	// Version negotiation: honour the client's choice when we implement it,
	// otherwise propose our newest and let the client decide whether to
	// continue. Never fail the handshake outright over a version.
	version := params.ProtocolVersion
	if !isSupportedVersion(version) {
		version = LatestProtocolVersion
	}

	info, instructions, err := s.backend.Describe(ctx, instanceID)
	if err != nil {
		return s.backendError(ctx, req, err)
	}

	result := InitializeResult{
		ProtocolVersion: version,
		Capabilities:    ServerCapabilities{Tools: &ToolsCapability{ListChanged: false}},
		ServerInfo:      info,
		Instructions:    instructions,
	}
	return s.reply(req, result)
}

func (s *Server) handleToolsList(ctx context.Context, instanceID string, req *Request) *Response {
	var params ListToolsParams
	if rpcErr := decodeParams(req.Params, &params); rpcErr != nil {
		return newError(req.ID, rpcErr.Code, rpcErr.Message, nil)
	}
	tools, err := s.backend.ListTools(ctx, instanceID)
	if err != nil {
		return s.backendError(ctx, req, err)
	}
	if tools == nil {
		// The field is required, and `null` is not an empty list.
		tools = []Tool{}
	}
	return s.reply(req, ListToolsResult{Tools: tools})
}

func (s *Server) handleToolsCall(ctx context.Context, instanceID string, req *Request) *Response {
	var params CallToolParams
	if rpcErr := decodeParams(req.Params, &params); rpcErr != nil {
		return newError(req.ID, rpcErr.Code, rpcErr.Message, nil)
	}
	if params.Name == "" {
		return newError(req.ID, CodeInvalidParams, "tool name must not be empty", nil)
	}

	result, err := s.backend.CallTool(ctx, instanceID, params.Name, params.Arguments)
	if err != nil {
		// An unknown or disabled tool is a caller mistake about the protocol
		// surface, so it is a JSON-RPC error rather than a tool result.
		if errors.Is(err, domain.ErrNotFound) {
			return newError(req.ID, CodeInvalidParams, "unknown tool: "+params.Name, nil)
		}
		return s.backendError(ctx, req, err)
	}
	return s.reply(req, result)
}

// backendError converts a backend failure into a JSON-RPC error without
// leaking internal detail to the client. The full error goes to the log, where
// an operator can see it; the client gets a stable, generic message.
func (s *Server) backendError(ctx context.Context, req *Request, err error) *Response {
	log := logging.FromContext(ctx)
	switch {
	case errors.Is(err, domain.ErrNotFound):
		return newError(req.ID, CodeInvalidParams, "the requested resource does not exist", nil)
	case errors.Is(err, domain.ErrDisabled):
		return newError(req.ID, CodeInvalidRequest, "this MCP instance is disabled", nil)
	case errors.Is(err, domain.ErrRateLimited):
		return newError(req.ID, CodeInternalError, "rate limit exceeded, retry shortly", nil)
	default:
		log.Error("mcp backend failure", slog.String("error", err.Error()))
		return newError(req.ID, CodeInternalError, "the server could not complete the request", nil)
	}
}

// HandleBatch dispatches a JSON-RPC batch, returning only the responses that
// are actually required (notifications produce none).
func (s *Server) HandleBatch(ctx context.Context, instanceID string, sess *Session, reqs []*Request) []*Response {
	out := make([]*Response, 0, len(reqs))
	for _, req := range reqs {
		if resp := s.Handle(ctx, instanceID, sess, req); resp != nil {
			out = append(out, resp)
		}
	}
	return out
}

// decodeRequests parses a request body that may hold either a single JSON-RPC
// message or a batch, normalising both into a slice.
func decodeRequests(body []byte) ([]*Request, bool, error) {
	trimmed := skipSpace(body)
	if len(trimmed) == 0 {
		return nil, false, errors.New("empty request body")
	}
	if trimmed[0] == '[' {
		var batch []*Request
		if err := json.Unmarshal(body, &batch); err != nil {
			return nil, true, err
		}
		if len(batch) == 0 {
			return nil, true, errors.New("batch must not be empty")
		}
		return batch, true, nil
	}
	var single Request
	if err := json.Unmarshal(body, &single); err != nil {
		return nil, false, err
	}
	return []*Request{&single}, false, nil
}

func skipSpace(b []byte) []byte {
	for i, c := range b {
		switch c {
		case ' ', '\t', '\r', '\n':
			continue
		default:
			return b[i:]
		}
	}
	return nil
}
