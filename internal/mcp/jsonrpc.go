// Package mcp implements the Model Context Protocol server surface: the
// JSON-RPC 2.0 vocabulary, the MCP method set, and the Streamable HTTP
// transport that carries them.
//
// The package is deliberately transport-complete but domain-empty: it knows how
// to speak MCP and nothing about connectors, instances or upstream APIs. All of
// that arrives through the Backend port, which keeps protocol concerns
// separable from platform concerns and makes the protocol testable on its own.
package mcp

import (
	"encoding/json"
	"fmt"
)

// jsonrpcVersion is the only JSON-RPC version MCP uses.
const jsonrpcVersion = "2.0"

// JSON-RPC 2.0 error codes.
const (
	CodeParseError     = -32700
	CodeInvalidRequest = -32600
	CodeMethodNotFound = -32601
	CodeInvalidParams  = -32602
	CodeInternalError  = -32603
)

// Request is an incoming JSON-RPC request or notification.
//
// ID is kept as raw JSON because JSON-RPC permits strings and numbers and
// requires the response to echo the value *exactly*; re-encoding through a
// typed field would turn the number 1 into 1.0 for some clients.
type Request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// IsNotification reports whether the message expects no response.
func (r *Request) IsNotification() bool { return len(r.ID) == 0 || string(r.ID) == "null" }

// Validate checks the JSON-RPC envelope.
func (r *Request) Validate() *Error {
	if r.JSONRPC != jsonrpcVersion {
		return &Error{Code: CodeInvalidRequest, Message: "jsonrpc must be \"2.0\""}
	}
	if r.Method == "" {
		return &Error{Code: CodeInvalidRequest, Message: "method must not be empty"}
	}
	return nil
}

// Response is an outgoing JSON-RPC response.
type Response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  any             `json:"result,omitempty"`
	Error   *Error          `json:"error,omitempty"`
}

// Error is a JSON-RPC error object.
type Error struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Error implements the error interface.
func (e *Error) Error() string { return fmt.Sprintf("jsonrpc error %d: %s", e.Code, e.Message) }

func newResult(id json.RawMessage, result any) *Response {
	return &Response{JSONRPC: jsonrpcVersion, ID: id, Result: result}
}

func newError(id json.RawMessage, code int, message string, data any) *Response {
	if len(id) == 0 {
		// A response to an unidentifiable request still needs an id member;
		// JSON-RPC specifies null for that case.
		id = json.RawMessage("null")
	}
	return &Response{JSONRPC: jsonrpcVersion, ID: id, Error: &Error{Code: code, Message: message, Data: data}}
}

// decodeParams unmarshals a request's params into v, mapping any failure onto
// the invalid-params error the protocol expects.
func decodeParams(raw json.RawMessage, v any) *Error {
	if len(raw) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return &Error{Code: CodeInvalidParams, Message: "params could not be decoded: " + err.Error()}
	}
	return nil
}
