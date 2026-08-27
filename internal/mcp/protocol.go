package mcp

import "encoding/json"

// Protocol revisions this server understands, newest first. Negotiation echoes
// the client's version when it is supported and otherwise offers the newest,
// which is what the specification prescribes.
var supportedProtocolVersions = []string{
	"2025-06-18",
	"2025-03-26",
	"2024-11-05",
}

// LatestProtocolVersion is the revision offered when a client asks for one this
// server does not implement.
const LatestProtocolVersion = "2025-06-18"

func isSupportedVersion(v string) bool {
	for _, s := range supportedProtocolVersions {
		if s == v {
			return true
		}
	}
	return false
}

// MCP method names.
const (
	MethodInitialize  = "initialize"
	MethodPing        = "ping"
	MethodToolsList   = "tools/list"
	MethodToolsCall   = "tools/call"
	MethodInitialized = "notifications/initialized"
	MethodCancelled   = "notifications/cancelled"
)

// Implementation identifies a party in the protocol handshake.
type Implementation struct {
	Name    string `json:"name"`
	Title   string `json:"title,omitempty"`
	Version string `json:"version"`
}

// ClientCapabilities describes what the connecting client supports. MCPaw does
// not currently depend on any of it, but capturing it keeps the handshake
// faithful and gives the audit log something meaningful to record.
type ClientCapabilities struct {
	Roots        *struct{ ListChanged bool } `json:"roots,omitempty"`
	Sampling     map[string]any              `json:"sampling,omitempty"`
	Elicitation  map[string]any              `json:"elicitation,omitempty"`
	Experimental map[string]any              `json:"experimental,omitempty"`
}

// ToolsCapability advertises tool support.
type ToolsCapability struct {
	// ListChanged is false: an instance's tool list changes only when an
	// operator reconfigures it, and MCPaw does not hold a channel open to push
	// that. Advertising a capability we do not honour would be worse than not
	// advertising it.
	ListChanged bool `json:"listChanged"`
}

// ServerCapabilities describes what this server offers.
type ServerCapabilities struct {
	Tools *ToolsCapability `json:"tools,omitempty"`
}

// InitializeParams is the client's half of the handshake.
type InitializeParams struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ClientCapabilities `json:"capabilities"`
	ClientInfo      Implementation     `json:"clientInfo"`
}

// InitializeResult is the server's half of the handshake.
type InitializeResult struct {
	ProtocolVersion string             `json:"protocolVersion"`
	Capabilities    ServerCapabilities `json:"capabilities"`
	ServerInfo      Implementation     `json:"serverInfo"`
	Instructions    string             `json:"instructions,omitempty"`
}

// ToolAnnotations are behavioural hints a client may use to decide whether a
// call needs confirmation.
type ToolAnnotations struct {
	Title           string `json:"title,omitempty"`
	ReadOnlyHint    *bool  `json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `json:"openWorldHint,omitempty"`
}

// Tool is one advertised tool.
type Tool struct {
	Name         string           `json:"name"`
	Title        string           `json:"title,omitempty"`
	Description  string           `json:"description,omitempty"`
	InputSchema  map[string]any   `json:"inputSchema"`
	OutputSchema map[string]any   `json:"outputSchema,omitempty"`
	Annotations  *ToolAnnotations `json:"annotations,omitempty"`
}

// ListToolsParams carries the optional pagination cursor.
type ListToolsParams struct {
	Cursor string `json:"cursor,omitempty"`
}

// ListToolsResult is the response to tools/list.
type ListToolsResult struct {
	Tools []Tool `json:"tools"`
	// NextCursor is always empty: a connector is capped at a few hundred tools,
	// so the whole list fits in one response and paging would add a failure
	// mode for no benefit.
	NextCursor string `json:"nextCursor,omitempty"`
}

// CallToolParams is the request to invoke a tool.
type CallToolParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// Content block types.
const ContentTypeText = "text"

// Content is one block of a tool result.
type Content struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

// TextContent builds a text content block.
func TextContent(text string) Content { return Content{Type: ContentTypeText, Text: text} }

// CallToolResult is the response to tools/call.
//
// A failing tool is reported here with IsError set, not as a JSON-RPC error:
// the protocol reserves transport errors for protocol-level problems, and a
// model needs to *see* that its call failed in order to correct itself.
type CallToolResult struct {
	Content           []Content `json:"content"`
	StructuredContent any       `json:"structuredContent,omitempty"`
	IsError           bool      `json:"isError,omitempty"`
}

// ErrorResult builds a failed tool result.
func ErrorResult(message string) *CallToolResult {
	return &CallToolResult{Content: []Content{TextContent(message)}, IsError: true}
}

// EmptyResult is the response body for methods that return nothing, such as
// ping. An empty JSON object is required — a null result is not valid MCP.
type EmptyResult struct{}

// MarshalJSON renders the empty result as {}.
func (EmptyResult) MarshalJSON() ([]byte, error) { return []byte("{}"), nil }

var _ json.Marshaler = EmptyResult{}
