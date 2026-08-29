// Package connector defines the declarative description of an upstream API and
// compiles it into an executable form.
//
// A connector manifest is *data*, never code: there is no scripting hook, no
// plugin ABI and no dynamic loading. Everything an API needs — its base URL,
// authentication scheme, configurable variables and the tools it exposes — is
// expressed in a schema this package validates exhaustively at import time. A
// manifest that would fail at call time is rejected before it is ever stored.
package connector

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"

	"gopkg.in/yaml.v3"
)

// Supported manifest envelope values.
const (
	APIVersion = "mcpaw.dev/v1"
	Kind       = "Connector"
)

// Manifest is the on-disk/on-wire representation of a connector.
type Manifest struct {
	APIVersion string   `yaml:"apiVersion" json:"apiVersion"`
	Kind       string   `yaml:"kind"       json:"kind"`
	Metadata   Metadata `yaml:"metadata"   json:"metadata"`
	Spec       Spec     `yaml:"spec"       json:"spec"`
}

// Metadata identifies and documents a connector.
type Metadata struct {
	ID            string   `yaml:"id"                      json:"id"`
	Name          string   `yaml:"name"                    json:"name"`
	Version       string   `yaml:"version"                 json:"version"`
	Description   string   `yaml:"description,omitempty"   json:"description,omitempty"`
	Documentation string   `yaml:"documentation,omitempty" json:"documentation,omitempty"`
	Tags          []string `yaml:"tags,omitempty"          json:"tags,omitempty"`
}

// Spec is the behavioural part of a connector.
type Spec struct {
	BaseURL   BaseURLSpec `yaml:"baseUrl"             json:"baseUrl"`
	Auth      AuthSpec    `yaml:"auth,omitempty"      json:"auth,omitempty"`
	Variables []Variable  `yaml:"variables,omitempty" json:"variables,omitempty"`
	Secrets   []Secret    `yaml:"secrets,omitempty"   json:"secrets,omitempty"`
	Defaults  Defaults    `yaml:"defaults,omitempty"  json:"defaults,omitempty"`
	Tools     []Tool      `yaml:"tools"               json:"tools"`
}

// BaseURLSpec describes the upstream origin and how much of it an operator may
// change.
type BaseURLSpec struct {
	Default     string `yaml:"default"               json:"default"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`

	// RequiresPrivateNetwork documents that this API normally lives on
	// loopback or a private range. It does not grant egress — it only lets the
	// UI explain to the operator why the private-network opt-in is needed.
	RequiresPrivateNetwork bool `yaml:"requiresPrivateNetwork,omitempty" json:"requiresPrivateNetwork,omitempty"`

	// Locked prevents an operator from pointing the instance at a different
	// origin, for connectors whose semantics only make sense against one host.
	Locked bool `yaml:"locked,omitempty" json:"locked,omitempty"`
}

// Authentication schemes a connector may declare.
const (
	AuthNone   = "none"
	AuthAPIKey = "apiKey"
	AuthBearer = "bearer"
	AuthBasic  = "basic"
)

// AuthSpec describes how credentials are attached to every upstream request.
// The credential itself is always a template reading the secrets namespace, so
// the manifest carries the *shape* of authentication and never a value.
type AuthSpec struct {
	Type string `yaml:"type,omitempty" json:"type,omitempty"`

	// In and Name apply to apiKey: where the key goes ("header" or "query")
	// and under what name.
	In   string `yaml:"in,omitempty"   json:"in,omitempty"`
	Name string `yaml:"name,omitempty" json:"name,omitempty"`

	// Value is the credential template for apiKey and bearer.
	Value string `yaml:"value,omitempty" json:"value,omitempty"`

	// Username and Password are the credential templates for basic auth.
	Username string `yaml:"username,omitempty" json:"username,omitempty"`
	Password string `yaml:"password,omitempty" json:"password,omitempty"`
}

// Variable is a non-secret, operator-supplied configuration value.
type Variable struct {
	Name        string   `yaml:"name"                  json:"name"`
	Description string   `yaml:"description,omitempty" json:"description,omitempty"`
	Default     string   `yaml:"default,omitempty"     json:"default,omitempty"`
	Required    bool     `yaml:"required,omitempty"    json:"required,omitempty"`
	Pattern     string   `yaml:"pattern,omitempty"     json:"pattern,omitempty"`
	Enum        []string `yaml:"enum,omitempty"        json:"enum,omitempty"`
	Example     string   `yaml:"example,omitempty"     json:"example,omitempty"`
	// Advanced hides this variable behind the instance form's Advanced
	// section rather than the main flow — for a variable most operators
	// should leave alone, such as an override that narrows an otherwise
	// automatic default.
	Advanced bool `yaml:"advanced,omitempty" json:"advanced,omitempty"`
}

// Secret is an operator-supplied credential, stored encrypted and never
// returned by any API.
type Secret struct {
	Name        string `yaml:"name"                  json:"name"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Required    bool   `yaml:"required,omitempty"    json:"required,omitempty"`
}

// Defaults supplies connector-wide request settings and the initial values for
// an instance's resource limits.
type Defaults struct {
	Headers          map[string]string `yaml:"headers,omitempty"          json:"headers,omitempty"`
	TimeoutMS        int               `yaml:"timeoutMs,omitempty"        json:"timeoutMs,omitempty"`
	MaxResponseBytes int64             `yaml:"maxResponseBytes,omitempty" json:"maxResponseBytes,omitempty"`
	RateLimitPerMin  int               `yaml:"rateLimitPerMin,omitempty"  json:"rateLimitPerMin,omitempty"`
	MaxConcurrent    int               `yaml:"maxConcurrent,omitempty"    json:"maxConcurrent,omitempty"`
}

// Tool is one MCP tool: a validated input contract plus the upstream request it
// maps onto.
type Tool struct {
	Name        string `yaml:"name"                  json:"name"`
	Title       string `yaml:"title,omitempty"       json:"title,omitempty"`
	Description string `yaml:"description"           json:"description"`

	Annotations  *Annotations   `yaml:"annotations,omitempty"  json:"annotations,omitempty"`
	InputSchema  map[string]any `yaml:"inputSchema"            json:"inputSchema"`
	OutputSchema map[string]any `yaml:"outputSchema,omitempty" json:"outputSchema,omitempty"`

	Request  RequestSpec  `yaml:"request"            json:"request"`
	Response ResponseSpec `yaml:"response,omitempty" json:"response,omitempty"`

	// Disabled marks a tool that exists in the manifest but is off unless an
	// operator turns it on — the right default for anything that writes.
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`
}

// Annotations are the MCP tool behaviour hints surfaced to clients.
type Annotations struct {
	Title           string `yaml:"title,omitempty"           json:"title,omitempty"`
	ReadOnlyHint    *bool  `yaml:"readOnlyHint,omitempty"    json:"readOnlyHint,omitempty"`
	DestructiveHint *bool  `yaml:"destructiveHint,omitempty" json:"destructiveHint,omitempty"`
	IdempotentHint  *bool  `yaml:"idempotentHint,omitempty"  json:"idempotentHint,omitempty"`
	OpenWorldHint   *bool  `yaml:"openWorldHint,omitempty"   json:"openWorldHint,omitempty"`
}

// RequestSpec is the template of the upstream HTTP request.
type RequestSpec struct {
	Method  string            `yaml:"method"            json:"method"`
	Path    string            `yaml:"path"              json:"path"`
	Query   map[string]string `yaml:"query,omitempty"   json:"query,omitempty"`
	Headers map[string]string `yaml:"headers,omitempty" json:"headers,omitempty"`
	Body    *BodySpec         `yaml:"body,omitempty"    json:"body,omitempty"`

	// Auth overrides the connector-level scheme for this tool. Only "none" is
	// meaningful; the empty string inherits.
	Auth string `yaml:"auth,omitempty" json:"auth,omitempty"`
}

// BodySpec describes the request body. Exactly one of JSON, From or Text may be
// set; ambiguity is a validation error rather than a precedence rule nobody
// remembers.
type BodySpec struct {
	ContentType string `yaml:"contentType,omitempty" json:"contentType,omitempty"`

	// JSON is a structure whose string leaves are templates. Leaves that fail
	// to resolve are omitted from the encoded object.
	JSON any `yaml:"json,omitempty" json:"json,omitempty"`

	// From is a namespaced path taken verbatim, e.g. "input.payload".
	From string `yaml:"from,omitempty" json:"from,omitempty"`

	// Text is a raw template sent as-is.
	Text string `yaml:"text,omitempty" json:"text,omitempty"`
}

// Response formats.
const (
	FormatAuto = "auto"
	FormatJSON = "json"
	FormatText = "text"
)

// ResponseSpec describes how an upstream response becomes an MCP tool result.
type ResponseSpec struct {
	// SuccessCodes lists the status codes treated as success. Empty means
	// "any 2xx".
	SuccessCodes []int `yaml:"successCodes,omitempty" json:"successCodes,omitempty"`

	// Select projects a dotted path out of a JSON response body.
	Select string `yaml:"select,omitempty" json:"select,omitempty"`

	// IncludeHeaders is an allowlist of response headers to surface as
	// metadata; an allowlist rather than a denylist so a chatty upstream cannot
	// leak Set-Cookie or internal routing headers to the model.
	IncludeHeaders []string `yaml:"includeHeaders,omitempty" json:"includeHeaders,omitempty"`

	Format string `yaml:"format,omitempty" json:"format,omitempty"`

	// Structured emits MCP structuredContent in addition to the text block.
	Structured bool `yaml:"structured,omitempty" json:"structured,omitempty"`

	// MaxBytes caps this tool's response body, overriding the instance limit
	// downward only.
	MaxBytes int64 `yaml:"maxBytes,omitempty" json:"maxBytes,omitempty"`
}

// maxManifestBytes bounds an uploaded manifest. Parsing untrusted YAML is the
// classic place to be handed a billion-laughs document.
const maxManifestBytes = 1 << 20

// ParseManifest decodes YAML (or JSON, which is a subset) into a Manifest.
//
// Decoding is strict: unknown fields are an error, because a silently ignored
// key is how "I configured the timeout" becomes "the timeout was never applied".
func ParseManifest(data []byte) (*Manifest, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("connector: manifest is empty")
	}
	if len(data) > maxManifestBytes {
		return nil, fmt.Errorf("connector: manifest exceeds %d bytes", maxManifestBytes)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)

	var m Manifest
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("connector: parsing manifest: %w", err)
	}
	// A second document would be silently ignored otherwise.
	var extra Manifest
	if err := dec.Decode(&extra); err == nil {
		return nil, fmt.Errorf("connector: manifest must contain exactly one document")
	}
	return &m, nil
}

// Checksum returns a stable digest of a manifest's bytes, used to detect
// whether a built-in manifest changed across an upgrade.
func Checksum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Marshal re-encodes a manifest as YAML, used when importing an OpenAPI
// document so the operator can review and store the generated result.
func (m *Manifest) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(m); err != nil {
		return nil, fmt.Errorf("connector: encoding manifest: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("connector: encoding manifest: %w", err)
	}
	return buf.Bytes(), nil
}
