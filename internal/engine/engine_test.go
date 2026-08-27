package engine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/00101010xyz/mcpaw/internal/connector"
	"github.com/00101010xyz/mcpaw/internal/upstream"
)

const testManifest = `
apiVersion: mcpaw.dev/v1
kind: Connector
metadata:
  id: testapi
  name: Test API
  version: 1.0.0
spec:
  baseUrl:
    default: http://example.invalid
  auth:
    type: apiKey
    in: header
    name: X-Api-Key
    value: "{{secrets.apiKey}}"
  variables:
    - name: accountId
      default: "0"
  secrets:
    - name: apiKey
  defaults:
    headers:
      Accept: application/json
  tools:
    - name: search
      description: Search things.
      inputSchema:
        type: object
        additionalProperties: false
        required: [query]
        properties:
          query: {type: string, maxLength: 50}
          limit: {type: integer, minimum: 1, maximum: 100}
          tag: {type: string}
      request:
        method: GET
        path: /accounts/{{vars.accountId}}/search
        query:
          q: "{{input.query}}"
          limit: "{{input.limit|default:10}}"
          tag: "{{input.tag}}"
      response:
        successCodes: [200]
        format: json
        includeHeaders: [X-Total-Count]
    - name: get_item
      description: Get an item by key.
      inputSchema:
        type: object
        additionalProperties: false
        required: [key]
        properties:
          key: {type: string}
      request:
        method: GET
        path: /items/{{input.key}}
      response:
        successCodes: [200]
        format: json
    - name: create_item
      description: Create an item.
      inputSchema:
        type: object
        additionalProperties: false
        required: [title]
        properties:
          title: {type: string}
          count: {type: integer}
          note: {type: string}
      request:
        method: POST
        path: /items
        body:
          json:
            title: "{{input.title}}"
            count: "{{input.count}}"
            note: "{{input.note}}"
            source: mcpaw
      response:
        successCodes: [201]
        format: json
    - name: envelope
      description: Returns a nested envelope.
      inputSchema:
        type: object
        additionalProperties: false
        properties: {}
      request:
        method: GET
        path: /envelope
      response:
        successCodes: [200]
        format: json
        select: data.items
    - name: plain
      description: Returns plain text.
      inputSchema:
        type: object
        additionalProperties: false
        properties: {}
      request:
        method: GET
        path: /plain
      response:
        successCodes: [200]
        format: text
`

func testConnector(t *testing.T) *connector.Compiled {
	t.Helper()
	m, err := connector.ParseManifest([]byte(testManifest))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	c, err := connector.Compile(m)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return c
}

// capture records the request an upstream server received so tests can assert
// on exactly what MCPaw sent.
type capture struct {
	Method string
	Path   string
	// RequestURI is the raw, still-escaped target as it arrived on the wire.
	// Assertions about escaping must use this: r.URL.Path is already decoded,
	// so it cannot distinguish "one segment containing a slash" from "two
	// segments".
	RequestURI string
	Query      url.Values
	Header     http.Header
	Body       string
}

func testTarget(t *testing.T, base string) *Target {
	t.Helper()
	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse base url: %v", err)
	}
	return &Target{
		InstanceID:       "inst_test",
		Slug:             "test",
		BaseURL:          u,
		Vars:             map[string]string{"accountId": "42"},
		Secrets:          map[string]string{},
		Policy:           upstream.EgressPolicy{AllowPrivateNetworks: true},
		Timeout:          5 * time.Second,
		MaxResponseBytes: 1 << 20,
		RateLimitPerMin:  1000,
		MaxConcurrent:    4,
	}
}

func newServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *capture) {
	t.Helper()
	got := &capture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got.Method, got.Path, got.RequestURI = r.Method, r.URL.Path, r.RequestURI
		got.Query, got.Header, got.Body = r.URL.Query(), r.Header.Clone(), string(body)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv, got
}

func exec(t *testing.T, target *Target, conn *connector.Compiled, toolName string, args map[string]any) (*Result, error) {
	t.Helper()
	tool, ok := conn.Tool(toolName)
	if !ok {
		t.Fatalf("unknown tool %s", toolName)
	}
	return New(Config{}).Execute(context.Background(), target, conn, tool, args)
}

func TestExecuteRendersPathQueryAndHeaders(t *testing.T) {
	srv, got := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Total-Count", "3")
		w.Header().Set("Set-Cookie", "session=leak")
		_, _ = w.Write([]byte(`{"items":[1,2,3]}`))
	})
	conn := testConnector(t)

	res, err := exec(t, testTarget(t, srv.URL), conn, "search", map[string]any{"query": "hello world"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if got.Path != "/accounts/42/search" {
		t.Errorf("path = %q, want /accounts/42/search", got.Path)
	}
	if got.Query.Get("q") != "hello world" {
		t.Errorf("q = %q", got.Query.Get("q"))
	}
	// The default applies when the caller omits the value.
	if got.Query.Get("limit") != "10" {
		t.Errorf("limit = %q, want the default 10", got.Query.Get("limit"))
	}
	// An unsupplied optional parameter must be absent, not empty.
	if _, present := got.Query["tag"]; present {
		t.Error("an unsupplied optional query parameter was sent anyway")
	}
	if got.Header.Get("Accept") != "application/json" {
		t.Error("connector default header was not applied")
	}
	// The apiKey secret is unset, so no credential header should be sent.
	if got.Header.Get("X-Api-Key") != "" {
		t.Error("an authentication header was sent with no configured secret")
	}

	if !res.IsJSON || res.StatusCode != 200 {
		t.Fatalf("unexpected result %+v", res)
	}
	if res.Headers["X-Total-Count"] != "3" {
		t.Errorf("allowlisted header missing: %v", res.Headers)
	}
	// Headers outside the manifest's allowlist must never reach the client.
	if _, leaked := res.Headers["Set-Cookie"]; leaked {
		t.Error("a non-allowlisted response header leaked to the caller")
	}
}

func TestSecretIsSentWhenConfigured(t *testing.T) {
	srv, got := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	target := testTarget(t, srv.URL)
	target.Secrets["apiKey"] = "sk-secret-value"

	if _, err := exec(t, target, testConnector(t), "search", map[string]any{"query": "x"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got.Header.Get("X-Api-Key") != "sk-secret-value" {
		t.Fatalf("credential header = %q", got.Header.Get("X-Api-Key"))
	}
}

func TestArgumentsAreValidatedAgainstTheSchema(t *testing.T) {
	srv, got := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{}`))
	})
	conn := testConnector(t)
	target := testTarget(t, srv.URL)

	cases := []struct {
		name string
		args map[string]any
	}{
		{"missing required", map[string]any{}},
		{"wrong type", map[string]any{"query": 42}},
		{"too long", map[string]any{"query": strings.Repeat("x", 100)}},
		{"out of range", map[string]any{"query": "x", "limit": 500}},
		{"unknown property", map[string]any{"query": "x", "surprise": true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := exec(t, target, conn, "search", tc.args)
			if err == nil {
				t.Fatal("invalid arguments were accepted")
			}
			e, ok := AsError(err)
			if !ok || e.Kind != KindInvalidArguments {
				t.Fatalf("got %v, want an invalid_arguments error", err)
			}
			if len(e.Details) == 0 {
				t.Error("validation failure carried no detail for the caller")
			}
		})
	}
	// Nothing invalid should ever have reached the upstream.
	if got.Method != "" {
		t.Fatalf("an invalid call still reached the upstream: %+v", got)
	}
}

// A tool argument must never be able to break out of its path segment.
func TestPathInjectionIsEscaped(t *testing.T) {
	srv, got := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	conn := testConnector(t)

	for _, malicious := range []string{
		"../../admin",
		"abc/../../etc/passwd",
		"x?admin=true",
		"x#frag",
		"x y",
	} {
		if _, err := exec(t, testTarget(t, srv.URL), conn, "get_item", map[string]any{"key": malicious}); err != nil {
			t.Fatalf("Execute(%q): %v", malicious, err)
		}
		// The value must occupy exactly one path segment on the wire, however
		// many slashes, question marks or spaces it contained.
		target := got.RequestURI
		if !strings.HasPrefix(target, "/items/") {
			t.Fatalf("argument %q escaped its path segment: %q", malicious, target)
		}
		if segments := strings.Split(strings.TrimPrefix(target, "/"), "/"); len(segments) != 2 {
			t.Fatalf("argument %q produced %d path segments (%q)", malicious, len(segments), target)
		}
		if strings.ContainsAny(target, "?# ") {
			t.Fatalf("argument %q injected an unescaped delimiter: %q", malicious, target)
		}
		if got.Query.Get("admin") != "" {
			t.Fatalf("argument %q injected a query parameter", malicious)
		}
	}
}

func TestJSONBodyRenderingDropsUnresolvedFields(t *testing.T) {
	srv, got := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	if _, err := exec(t, testTarget(t, srv.URL), testConnector(t), "create_item",
		map[string]any{"title": "A title", "count": 7}); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	var body map[string]any
	if err := json.Unmarshal([]byte(got.Body), &body); err != nil {
		t.Fatalf("body is not JSON: %q", got.Body)
	}
	if body["title"] != "A title" {
		t.Errorf("title = %v", body["title"])
	}
	// A number must survive as a number, not become the string "7".
	if n, ok := body["count"].(float64); !ok || n != 7 {
		t.Errorf("count = %#v, want the number 7", body["count"])
	}
	// A literal from the manifest is sent verbatim.
	if body["source"] != "mcpaw" {
		t.Errorf("manifest literal missing: %v", body["source"])
	}
	// An optional field the caller omitted must be absent, not null.
	if _, present := body["note"]; present {
		t.Errorf("unsupplied optional field was sent: %v", body["note"])
	}
	if ct := got.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("Content-Type = %q", ct)
	}
}

func TestResponseProjection(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"items":[{"id":1},{"id":2}]},"meta":{"secret":"x"}}`))
	})

	res, err := exec(t, testTarget(t, srv.URL), testConnector(t), "envelope", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	items, ok := res.Data.([]any)
	if !ok || len(items) != 2 {
		t.Fatalf("projection failed: %#v", res.Data)
	}
	if strings.Contains(res.Text, "secret") {
		t.Error("the projection did not drop the rest of the envelope")
	}
}

func TestProjectionFailureIsReported(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"unexpected":true}`))
	})
	_, err := exec(t, testTarget(t, srv.URL), testConnector(t), "envelope", nil)
	e, ok := AsError(err)
	if !ok || e.Kind != KindUpstreamFailure {
		t.Fatalf("got %v, want an upstream_failure error", err)
	}
}

func TestTextResponsesArePassedThrough(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("@article{key, title={A}}"))
	})
	res, err := exec(t, testTarget(t, srv.URL), testConnector(t), "plain", nil)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if res.IsJSON {
		t.Error("a text response was reported as JSON")
	}
	if res.Text != "@article{key, title={A}}" {
		t.Errorf("text = %q", res.Text)
	}
}

func TestUpstreamErrorStatusIsMapped(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"item not found"}`))
	})
	_, err := exec(t, testTarget(t, srv.URL), testConnector(t), "get_item", map[string]any{"key": "ABC"})
	e, ok := AsError(err)
	if !ok || e.Kind != KindUpstreamStatus || e.StatusCode != 404 {
		t.Fatalf("got %v, want a 404 upstream_status error", err)
	}
	// The upstream's own explanation is genuinely useful and is included, but
	// bounded.
	if !strings.Contains(e.FullMessage(), "item not found") {
		t.Errorf("upstream explanation was dropped: %q", e.FullMessage())
	}
	if e.Retryable() {
		t.Error("a 404 must not be reported as retryable")
	}
}

func TestServerErrorsAreRetryable(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	_, err := exec(t, testTarget(t, srv.URL), testConnector(t), "get_item", map[string]any{"key": "ABC"})
	e, _ := AsError(err)
	if e == nil || !e.Retryable() {
		t.Fatalf("got %v, want a retryable error", err)
	}
}

// The egress policy must be enforced for tool calls, not only in unit tests of
// the guard.
func TestEgressPolicyBlocksLoopbackWithoutOptIn(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {})
	target := testTarget(t, srv.URL)
	target.Policy = upstream.EgressPolicy{}

	_, err := exec(t, target, testConnector(t), "get_item", map[string]any{"key": "ABC"})
	e, ok := AsError(err)
	if !ok || e.Kind != KindEgressBlocked {
		t.Fatalf("got %v, want an egress_blocked error", err)
	}
	// The message must tell the operator what to do about it.
	if !strings.Contains(e.Message, "private-network egress") {
		t.Errorf("unhelpful message: %q", e.Message)
	}
}

func TestRateLimitIsEnforced(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	conn := testConnector(t)
	target := testTarget(t, srv.URL)
	target.RateLimitPerMin = 2

	e := New(Config{})
	tool, _ := conn.Tool("get_item")
	args := map[string]any{"key": "ABC"}
	for i := 0; i < 2; i++ {
		if _, err := e.Execute(context.Background(), target, conn, tool, args); err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
	}
	_, err := e.Execute(context.Background(), target, conn, tool, args)
	ee, ok := AsError(err)
	if !ok || ee.Kind != KindRateLimited {
		t.Fatalf("got %v, want a rate_limited error", err)
	}
	if !ee.Retryable() {
		t.Error("rate limiting should be retryable")
	}
}

func TestTimeoutIsEnforced(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	})
	target := testTarget(t, srv.URL)
	target.Timeout = 50 * time.Millisecond

	start := time.Now()
	_, err := exec(t, target, testConnector(t), "get_item", map[string]any{"key": "ABC"})
	e, ok := AsError(err)
	if !ok || e.Kind != KindTimeout {
		t.Fatalf("got %v, want a timeout error", err)
	}
	if time.Since(start) > time.Second {
		t.Fatal("the per-instance timeout was not applied")
	}
}

func TestOversizedJSONResponseIsRefusedRatherThanTruncated(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"padding":"` + strings.Repeat("a", 5000) + `"}`))
	})
	target := testTarget(t, srv.URL)
	target.MaxResponseBytes = 100

	_, err := exec(t, target, testConnector(t), "get_item", map[string]any{"key": "ABC"})
	e, ok := AsError(err)
	if !ok || e.Kind != KindResponseTooLarge {
		t.Fatalf("got %v, want a response_too_large error", err)
	}
}

// Repeated failures must open the breaker so a dead upstream stops consuming
// request slots.
func TestBreakerOpensAfterRepeatedUpstreamFailures(t *testing.T) {
	conn := testConnector(t)
	// A port nothing listens on, reached through the permissive policy.
	target := testTarget(t, "http://127.0.0.1:1")
	target.Timeout = 200 * time.Millisecond

	e := New(Config{Breaker: upstream.NewBreaker(upstream.BreakerConfig{FailureThreshold: 2, OpenDuration: time.Minute})})
	tool, _ := conn.Tool("get_item")
	args := map[string]any{"key": "ABC"}

	for i := 0; i < 2; i++ {
		if _, err := e.Execute(context.Background(), target, conn, tool, args); err == nil {
			t.Fatalf("call %d unexpectedly succeeded", i)
		}
	}
	_, err := e.Execute(context.Background(), target, conn, tool, args)
	ee, ok := AsError(err)
	if !ok || ee.Kind != KindCircuitOpen {
		t.Fatalf("got %v, want a circuit_open error", err)
	}
}

// A 4xx says the call was wrong, not that the upstream is unhealthy; it must
// not trip the breaker for every other caller.
func TestClientErrorsDoNotOpenTheBreaker(t *testing.T) {
	srv, _ := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	conn := testConnector(t)
	target := testTarget(t, srv.URL)
	e := New(Config{Breaker: upstream.NewBreaker(upstream.BreakerConfig{FailureThreshold: 2})})
	tool, _ := conn.Tool("get_item")

	for i := 0; i < 5; i++ {
		_, err := e.Execute(context.Background(), target, conn, tool, map[string]any{"key": "ABC"})
		ee, _ := AsError(err)
		if ee == nil || ee.Kind != KindUpstreamStatus {
			t.Fatalf("call %d: got %v, want upstream_status", i, err)
		}
	}
}

func TestExecuteRejectsIncompleteConfiguration(t *testing.T) {
	conn := testConnector(t)
	tool, _ := conn.Tool("get_item")
	e := New(Config{})

	if _, err := e.Execute(context.Background(), nil, conn, tool, nil); err == nil {
		t.Error("nil target accepted")
	}
	if _, err := e.Execute(context.Background(), &Target{}, conn, tool, nil); err == nil {
		t.Error("target with no base URL accepted")
	}
}

// A base URL carrying a path prefix must be preserved, not replaced.
func TestBaseURLPathPrefixIsPreserved(t *testing.T) {
	srv, got := newServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	})
	target := testTarget(t, srv.URL+"/api/v2")

	if _, err := exec(t, target, testConnector(t), "get_item", map[string]any{"key": "ABC"}); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if got.Path != "/api/v2/items/ABC" {
		t.Fatalf("path = %q, want the base prefix preserved", got.Path)
	}
}
