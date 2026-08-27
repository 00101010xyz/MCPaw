package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/00101010xyz/mcpaw/internal/domain"
)

// fakeBackend is a controllable Backend for protocol-level tests.
type fakeBackend struct {
	tools       []Tool
	callResult  *CallToolResult
	callErr     error
	describeErr error
	lastName    string
	lastArgs    map[string]any
	calls       int
}

func (f *fakeBackend) Describe(context.Context, string) (Implementation, string, error) {
	if f.describeErr != nil {
		return Implementation{}, "", f.describeErr
	}
	return Implementation{Name: "mcpaw/test", Version: "1.0.0"}, "Use these tools to read the library.", nil
}

func (f *fakeBackend) ListTools(context.Context, string) ([]Tool, error) { return f.tools, nil }

func (f *fakeBackend) CallTool(_ context.Context, _, name string, args map[string]any) (*CallToolResult, error) {
	f.calls++
	f.lastName, f.lastArgs = name, args
	if f.callErr != nil {
		return nil, f.callErr
	}
	if f.callResult != nil {
		return f.callResult, nil
	}
	return &CallToolResult{Content: []Content{TextContent("ok")}}, nil
}

func newTestHandler(t *testing.T, backend Backend) *Handler {
	t.Helper()
	srv, err := NewServer(ServerConfig{Backend: backend, Info: Implementation{Name: "mcpaw", Version: "test"}})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return NewHandler(srv, HandlerOptions{})
}

// post sends one JSON-RPC message through the transport as an authenticated
// client of instance "inst_1".
func post(t *testing.T, h *Handler, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp/test", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	req = req.WithContext(WithInstanceID(req.Context(), "inst_1"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeResponse(t *testing.T, rec *httptest.ResponseRecorder) Response {
	t.Helper()
	var resp Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decoding %q: %v", rec.Body.String(), err)
	}
	return resp
}

func TestInitializeHandshake(t *testing.T) {
	h := newTestHandler(t, &fakeBackend{})
	rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{
		"protocolVersion":"2025-06-18",
		"capabilities":{},
		"clientInfo":{"name":"test-client","version":"0.1"}}}`, nil)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body)
	}
	resp := decodeResponse(t, rec)
	if resp.Error != nil {
		t.Fatalf("initialize failed: %+v", resp.Error)
	}
	var result InitializeResult
	raw, _ := json.Marshal(resp.Result)
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatalf("decoding result: %v", err)
	}
	if result.ProtocolVersion != "2025-06-18" {
		t.Errorf("protocolVersion = %q, want the client's supported version echoed", result.ProtocolVersion)
	}
	if result.Capabilities.Tools == nil {
		t.Error("tools capability was not advertised")
	}
	if result.Instructions == "" {
		t.Error("instructions were not returned")
	}
	// A session must be minted and handed back for the client to echo.
	if rec.Header().Get(HeaderSessionID) == "" {
		t.Error("no session id was returned from initialize")
	}
}

func TestProtocolVersionNegotiationFallsBackToLatest(t *testing.T) {
	h := newTestHandler(t, &fakeBackend{})
	rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{
		"protocolVersion":"1999-01-01","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`, nil)

	resp := decodeResponse(t, rec)
	if resp.Error != nil {
		t.Fatalf("an unknown client version must not fail the handshake: %+v", resp.Error)
	}
	raw, _ := json.Marshal(resp.Result)
	var result InitializeResult
	_ = json.Unmarshal(raw, &result)
	if result.ProtocolVersion != LatestProtocolVersion {
		t.Fatalf("protocolVersion = %q, want %q", result.ProtocolVersion, LatestProtocolVersion)
	}
}

func TestOlderProtocolVersionsAreSupported(t *testing.T) {
	h := newTestHandler(t, &fakeBackend{})
	for _, version := range supportedProtocolVersions {
		body := fmt.Sprintf(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{
			"protocolVersion":%q,"capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`, version)
		resp := decodeResponse(t, post(t, h, body, nil))
		raw, _ := json.Marshal(resp.Result)
		var result InitializeResult
		_ = json.Unmarshal(raw, &result)
		if result.ProtocolVersion != version {
			t.Errorf("version %q was not echoed (got %q)", version, result.ProtocolVersion)
		}
	}
}

func TestToolsListAndCall(t *testing.T) {
	backend := &fakeBackend{tools: []Tool{{
		Name:        "zotero_search_items",
		Description: "Search",
		InputSchema: map[string]any{"type": "object"},
	}}}
	h := newTestHandler(t, backend)

	resp := decodeResponse(t, post(t, h, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`, nil))
	if resp.Error != nil {
		t.Fatalf("tools/list failed: %+v", resp.Error)
	}
	raw, _ := json.Marshal(resp.Result)
	var list ListToolsResult
	_ = json.Unmarshal(raw, &list)
	if len(list.Tools) != 1 || list.Tools[0].Name != "zotero_search_items" {
		t.Fatalf("unexpected tool list %+v", list.Tools)
	}

	resp = decodeResponse(t, post(t, h,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"zotero_search_items","arguments":{"query":"x"}}}`, nil))
	if resp.Error != nil {
		t.Fatalf("tools/call failed: %+v", resp.Error)
	}
	if backend.lastName != "zotero_search_items" || backend.lastArgs["query"] != "x" {
		t.Fatalf("backend received name=%q args=%v", backend.lastName, backend.lastArgs)
	}
}

func TestEmptyToolListIsAnArrayNotNull(t *testing.T) {
	h := newTestHandler(t, &fakeBackend{tools: nil})
	rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	if !strings.Contains(rec.Body.String(), `"tools":[]`) {
		t.Fatalf("empty tool list was not serialised as []: %s", rec.Body)
	}
}

// A failing tool must be reported inside the result, not as a JSON-RPC error:
// the model needs to see the failure in order to correct its next call.
func TestToolFailureIsAResultNotATransportError(t *testing.T) {
	backend := &fakeBackend{callResult: ErrorResult("the upstream API returned 404 Not Found")}
	h := newTestHandler(t, backend)

	resp := decodeResponse(t, post(t, h,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"t","arguments":{}}}`, nil))
	if resp.Error != nil {
		t.Fatalf("tool failure surfaced as a JSON-RPC error: %+v", resp.Error)
	}
	raw, _ := json.Marshal(resp.Result)
	var result CallToolResult
	_ = json.Unmarshal(raw, &result)
	if !result.IsError || len(result.Content) == 0 {
		t.Fatalf("unexpected result %+v", result)
	}
}

// An unknown tool is a protocol-level mistake and does belong in the error
// channel.
func TestUnknownToolIsInvalidParams(t *testing.T) {
	h := newTestHandler(t, &fakeBackend{callErr: domain.ErrNotFound})
	resp := decodeResponse(t, post(t, h,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"nope","arguments":{}}}`, nil))
	if resp.Error == nil || resp.Error.Code != CodeInvalidParams {
		t.Fatalf("got %+v, want invalid params", resp.Error)
	}
}

func TestBackendErrorsDoNotLeakInternals(t *testing.T) {
	h := newTestHandler(t, &fakeBackend{callErr: fmt.Errorf("dial tcp 10.1.2.3:5432: connection refused")})
	resp := decodeResponse(t, post(t, h,
		`{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{"name":"t"}}`, nil))
	if resp.Error == nil || resp.Error.Code != CodeInternalError {
		t.Fatalf("got %+v, want an internal error", resp.Error)
	}
	if strings.Contains(resp.Error.Message, "10.1.2.3") {
		t.Fatalf("internal detail leaked to the client: %q", resp.Error.Message)
	}
}

func TestPingAndUnknownMethod(t *testing.T) {
	h := newTestHandler(t, &fakeBackend{})

	rec := post(t, h, `{"jsonrpc":"2.0","id":7,"method":"ping"}`, nil)
	if !strings.Contains(rec.Body.String(), `"result":{}`) {
		t.Fatalf("ping result = %s, want {}", rec.Body)
	}

	resp := decodeResponse(t, post(t, h, `{"jsonrpc":"2.0","id":8,"method":"resources/list"}`, nil))
	if resp.Error == nil || resp.Error.Code != CodeMethodNotFound {
		t.Fatalf("got %+v, want method not found for an unadvertised capability", resp.Error)
	}
}

func TestNotificationsGetAcceptedWithNoBody(t *testing.T) {
	h := newTestHandler(t, &fakeBackend{})
	rec := post(t, h, `{"jsonrpc":"2.0","method":"notifications/initialized"}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != "" {
		t.Fatalf("a notification produced a body: %q", rec.Body)
	}
	// An unknown notification must also be tolerated rather than erroring.
	rec = post(t, h, `{"jsonrpc":"2.0","method":"notifications/somethingNew"}`, nil)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("unknown notification status = %d, want 202", rec.Code)
	}
}

func TestBatchRequests(t *testing.T) {
	h := newTestHandler(t, &fakeBackend{tools: []Tool{}})
	rec := post(t, h, `[
		{"jsonrpc":"2.0","id":1,"method":"ping"},
		{"jsonrpc":"2.0","method":"notifications/initialized"},
		{"jsonrpc":"2.0","id":2,"method":"tools/list"}
	]`, nil)

	var responses []Response
	if err := json.Unmarshal(rec.Body.Bytes(), &responses); err != nil {
		t.Fatalf("batch response is not an array: %s", rec.Body)
	}
	// The notification contributes no response.
	if len(responses) != 2 {
		t.Fatalf("got %d responses, want 2", len(responses))
	}
}

func TestMalformedRequests(t *testing.T) {
	h := newTestHandler(t, &fakeBackend{})

	rec := post(t, h, `{not json`, nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if resp := decodeResponse(t, rec); resp.Error == nil || resp.Error.Code != CodeParseError {
		t.Fatalf("got %+v, want a parse error", resp.Error)
	}

	// Wrong JSON-RPC version.
	resp := decodeResponse(t, post(t, h, `{"jsonrpc":"1.0","id":1,"method":"ping"}`, nil))
	if resp.Error == nil || resp.Error.Code != CodeInvalidRequest {
		t.Fatalf("got %+v, want invalid request", resp.Error)
	}

	// Empty body and empty batch.
	if rec := post(t, h, ``, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty body status = %d", rec.Code)
	}
	if rec := post(t, h, `[]`, nil); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty batch status = %d", rec.Code)
	}
}

func TestRequestIDIsEchoedExactly(t *testing.T) {
	h := newTestHandler(t, &fakeBackend{})
	for _, id := range []string{`42`, `"abc-123"`} {
		rec := post(t, h, fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"method":"ping"}`, id), nil)
		if !strings.Contains(rec.Body.String(), `"id":`+id) {
			t.Fatalf("id %s was not echoed verbatim: %s", id, rec.Body)
		}
	}
}

func TestOversizedBodyIsRejected(t *testing.T) {
	srv, _ := NewServer(ServerConfig{Backend: &fakeBackend{}})
	h := NewHandler(srv, HandlerOptions{MaxBodyBytes: 128})
	rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"ping","params":{"pad":"`+strings.Repeat("x", 500)+`"}}`, nil)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", rec.Code)
	}
}

func TestUnknownSessionIsRejected(t *testing.T) {
	h := newTestHandler(t, &fakeBackend{})
	rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		map[string]string{HeaderSessionID: "not-a-real-session"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 so the client re-initializes", rec.Code)
	}
}

// A session minted for one instance must not be usable against another, even
// though the bearer token is what actually authorises the call.
func TestSessionIsBoundToItsInstance(t *testing.T) {
	h := newTestHandler(t, &fakeBackend{})
	rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{
		"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`, nil)
	sessionID := rec.Header().Get(HeaderSessionID)
	if sessionID == "" {
		t.Fatal("no session was created")
	}

	req := httptest.NewRequest(http.MethodPost, "/mcp/other", strings.NewReader(`{"jsonrpc":"2.0","id":2,"method":"ping"}`))
	req.Header.Set(HeaderSessionID, sessionID)
	req = req.WithContext(WithInstanceID(req.Context(), "inst_2"))
	other := httptest.NewRecorder()
	h.ServeHTTP(other, req)
	if other.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 for a session from another instance", other.Code)
	}
}

func TestSessionIsAcceptedWhenEchoed(t *testing.T) {
	h := newTestHandler(t, &fakeBackend{})
	rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{
		"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`, nil)
	sessionID := rec.Header().Get(HeaderSessionID)

	rec = post(t, h, `{"jsonrpc":"2.0","id":2,"method":"ping"}`, map[string]string{HeaderSessionID: sessionID})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d for a valid session", rec.Code)
	}
}

// Clients that do not track sessions must still work: the bearer token is the
// authentication, not the session id.
func TestRequestsWithoutASessionAreAccepted(t *testing.T) {
	h := newTestHandler(t, &fakeBackend{})
	if rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for a stateless client", rec.Code)
	}
}

func TestUnsupportedProtocolVersionHeaderIsRejected(t *testing.T) {
	h := newTestHandler(t, &fakeBackend{})
	rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		map[string]string{HeaderProtocolVersion: "1999-01-01"})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	// A supported version passes.
	rec = post(t, h, `{"jsonrpc":"2.0","id":1,"method":"ping"}`,
		map[string]string{HeaderProtocolVersion: LatestProtocolVersion})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d for a supported version header", rec.Code)
	}
}

func TestMissingAuthenticationContextFailsClosed(t *testing.T) {
	h := newTestHandler(t, &fakeBackend{})
	req := httptest.NewRequest(http.MethodPost, "/mcp/test", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req) // no instance in context

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d; an unauthenticated request must never be served", rec.Code)
	}
}

func TestSSEResponseWhenClientOnlyAcceptsEventStream(t *testing.T) {
	h := newTestHandler(t, &fakeBackend{})
	rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"ping"}`, map[string]string{"Accept": "text/event-stream"})

	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if !strings.Contains(rec.Body.String(), "event: message\ndata: {") {
		t.Fatalf("body is not an SSE frame: %q", rec.Body)
	}
}

func TestDeleteTerminatesSession(t *testing.T) {
	h := newTestHandler(t, &fakeBackend{})
	rec := post(t, h, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{
		"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`, nil)
	sessionID := rec.Header().Get(HeaderSessionID)

	req := httptest.NewRequest(http.MethodDelete, "/mcp/test", nil)
	req.Header.Set(HeaderSessionID, sessionID)
	req = req.WithContext(WithInstanceID(req.Context(), "inst_1"))
	del := httptest.NewRecorder()
	h.ServeHTTP(del, req)
	if del.Code != http.StatusNoContent {
		t.Fatalf("DELETE status = %d, want 204", del.Code)
	}

	rec = post(t, h, `{"jsonrpc":"2.0","id":2,"method":"ping"}`, map[string]string{HeaderSessionID: sessionID})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("a terminated session was still accepted (status %d)", rec.Code)
	}
}

func TestUnsupportedMethodReturns405(t *testing.T) {
	h := newTestHandler(t, &fakeBackend{})
	req := httptest.NewRequest(http.MethodPut, "/mcp/test", nil)
	req = req.WithContext(WithInstanceID(req.Context(), "inst_1"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); !strings.Contains(allow, "POST") {
		t.Fatalf("Allow = %q", allow)
	}
}

func TestSessionStoreExpiryAndLimits(t *testing.T) {
	clock := time.Now()
	store := NewSessionStore(SessionConfig{TTL: time.Minute, Max: 2})
	store.now = func() time.Time { return clock }

	s1, err := store.Create("inst_1", "2025-06-18", Implementation{Name: "c"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.Create("inst_1", "2025-06-18", Implementation{Name: "c"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := store.Create("inst_1", "2025-06-18", Implementation{Name: "c"}); err == nil {
		t.Fatal("the session cap was not enforced")
	}

	// Expired sessions are pruned, freeing capacity.
	clock = clock.Add(2 * time.Minute)
	if _, ok := store.Get(s1.ID); ok {
		t.Fatal("an expired session was returned")
	}
	if _, err := store.Create("inst_1", "2025-06-18", Implementation{Name: "c"}); err != nil {
		t.Fatalf("capacity was not reclaimed after expiry: %v", err)
	}
}

func TestSessionStoreDeleteByInstance(t *testing.T) {
	store := NewSessionStore(SessionConfig{})
	a, _ := store.Create("inst_a", "2025-06-18", Implementation{Name: "c"})
	b, _ := store.Create("inst_b", "2025-06-18", Implementation{Name: "c"})

	store.DeleteByInstance("inst_a")
	if _, ok := store.Get(a.ID); ok {
		t.Error("session for the reconfigured instance survived")
	}
	if _, ok := store.Get(b.ID); !ok {
		t.Error("session for an unrelated instance was dropped")
	}
}

func TestNewServerRequiresABackend(t *testing.T) {
	if _, err := NewServer(ServerConfig{}); err == nil {
		t.Fatal("a server was constructed with no backend")
	}
}
