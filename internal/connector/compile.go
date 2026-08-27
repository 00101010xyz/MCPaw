package connector

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/00101010xyz/mcpaw/internal/tmpl"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Platform-wide bounds. They are constants rather than configuration because
// they exist to keep a hostile or careless manifest from exhausting the
// process, and a limit an operator can raise arbitrarily is not a limit.
const (
	DefaultTimeoutMS        = 15_000
	MaxTimeoutMS            = 120_000
	DefaultMaxResponseBytes = int64(1 << 20)  // 1 MiB
	MaxResponseBytesCap     = int64(16 << 20) // 16 MiB
	DefaultRateLimitPerMin  = 120
	DefaultMaxConcurrent    = 4
	MaxToolsPerConnector    = 256
)

var (
	connectorIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,62}$`)
	// MCP tool names are exposed to models and to clients that key on them, so
	// they are restricted to a conservative identifier shape.
	toolNameRe   = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,63}$`)
	identifierRe = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_-]{0,63}$`)
	headerNameRe = regexp.MustCompile(`^[A-Za-z0-9!#$%&'*+.^_` + "`" + `|~-]+$`)
)

var allowedMethods = map[string]bool{
	"GET": true, "POST": true, "PUT": true, "PATCH": true, "DELETE": true, "HEAD": true,
}

// ValidationError aggregates every problem found in a manifest so an author
// fixes them in one pass instead of one error per re-upload.
type ValidationError struct{ Problems []string }

// Error implements error.
func (e *ValidationError) Error() string {
	if len(e.Problems) == 1 {
		return "connector: invalid manifest: " + e.Problems[0]
	}
	return fmt.Sprintf("connector: invalid manifest (%d problems):\n  - %s",
		len(e.Problems), strings.Join(e.Problems, "\n  - "))
}

type problems struct{ list []string }

func (p *problems) addf(format string, args ...any) {
	p.list = append(p.list, fmt.Sprintf(format, args...))
}

func (p *problems) err() error {
	if len(p.list) == 0 {
		return nil
	}
	return &ValidationError{Problems: p.list}
}

// Compiled is a manifest that has been validated and prepared for execution:
// every template is parsed, every JSON Schema is compiled, and every lookup is
// a map access. Compilation happens once at load time so the request path does
// no parsing at all.
type Compiled struct {
	Manifest *Manifest
	Auth     *CompiledAuth
	Headers  []Pair
	Tools    []*CompiledTool

	byName     map[string]*CompiledTool
	variables  map[string]Variable
	secretDefs map[string]Secret
}

// Pair is a rendered-at-request-time key/template pair.
type Pair struct {
	Key   string
	Value *tmpl.Template
}

// CompiledAuth is the prepared authentication scheme.
type CompiledAuth struct {
	Type     string
	In       string
	Name     string
	Value    *tmpl.Template
	Username *tmpl.Template
	Password *tmpl.Template
}

// CompiledTool is a validated, execution-ready tool.
type CompiledTool struct {
	Def *Tool

	InputSchema *jsonschema.Schema
	Path        *tmpl.Template
	Query       []Pair
	Headers     []Pair
	Body        *CompiledBody

	SuccessCodes   map[int]bool
	SelectPath     []string
	IncludeHeaders []string
}

// Name returns the tool's MCP name.
func (t *CompiledTool) Name() string { return t.Def.Name }

// EnabledByDefault reports whether a fresh instance should expose this tool.
func (t *CompiledTool) EnabledByDefault() bool { return !t.Def.Disabled }

// CompiledBody is the prepared request body.
type CompiledBody struct {
	ContentType string
	JSON        *BodyNode
	From        *tmpl.Template
	Text        *tmpl.Template
}

// BodyNode is one node of a JSON body template tree.
//
// The fields stay unexported and are read through accessors so that the
// execution engine can walk the tree without being able to mutate a compiled
// artifact that is shared across concurrent requests.
type BodyNode struct {
	tmpl   *tmpl.Template
	object map[string]*BodyNode
	array  []*BodyNode
	// literal holds a non-string scalar taken verbatim from the manifest.
	literal    any
	hasLiteral bool
}

// Template returns the node's template, or nil when the node is not a leaf
// template.
func (n *BodyNode) Template() *tmpl.Template { return n.tmpl }

// Object returns the node's fields, or nil when the node is not an object.
func (n *BodyNode) Object() map[string]*BodyNode { return n.object }

// Array returns the node's elements, or nil when the node is not an array.
func (n *BodyNode) Array() []*BodyNode { return n.array }

// HasLiteral reports whether the node is a verbatim scalar from the manifest.
func (n *BodyNode) HasLiteral() bool { return n.hasLiteral }

// Literal returns the verbatim scalar value.
func (n *BodyNode) Literal() any { return n.literal }

// Compile validates a manifest and prepares it for execution.
func Compile(m *Manifest) (*Compiled, error) {
	if m == nil {
		return nil, errors.New("connector: manifest is nil")
	}
	var p problems

	if m.APIVersion != APIVersion {
		p.addf("apiVersion must be %q, got %q", APIVersion, m.APIVersion)
	}
	if m.Kind != Kind {
		p.addf("kind must be %q, got %q", Kind, m.Kind)
	}
	if !connectorIDRe.MatchString(m.Metadata.ID) {
		p.addf("metadata.id %q must match %s", m.Metadata.ID, connectorIDRe)
	}
	if strings.TrimSpace(m.Metadata.Name) == "" {
		p.addf("metadata.name must not be empty")
	}
	if strings.TrimSpace(m.Metadata.Version) == "" {
		p.addf("metadata.version must not be empty")
	}

	c := &Compiled{
		Manifest:   m,
		byName:     map[string]*CompiledTool{},
		variables:  map[string]Variable{},
		secretDefs: map[string]Secret{},
	}

	validateBaseURL(m.Spec.BaseURL, &p)
	compileVariables(m.Spec.Variables, c, &p)
	compileSecrets(m.Spec.Secrets, c, &p)
	validateDefaults(m.Spec.Defaults, &p)
	c.Headers = compilePairs("spec.defaults.headers", m.Spec.Defaults.Headers, c, &p, true)
	c.Auth = compileAuth(m.Spec.Auth, c, &p)
	compileTools(m.Spec.Tools, c, &p)

	if err := p.err(); err != nil {
		return nil, err
	}
	return c, nil
}

func validateBaseURL(b BaseURLSpec, p *problems) {
	if b.Default == "" {
		p.addf("spec.baseUrl.default must not be empty")
		return
	}
	if err := ValidateBaseURL(b.Default); err != nil {
		p.addf("spec.baseUrl.default: %v", err)
	}
}

// ValidateBaseURL enforces the shape of an upstream origin. It is exported
// because operators can override the base URL per instance and that override
// must satisfy exactly the same rules as the manifest default.
func ValidateBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("not a valid URL: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("must include a host")
	}
	if u.User != nil {
		return errors.New("must not embed credentials")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return errors.New("must not include a query string or fragment")
	}
	return nil
}

func compileVariables(vars []Variable, c *Compiled, p *problems) {
	for _, v := range vars {
		if !identifierRe.MatchString(v.Name) {
			p.addf("variable %q: name must match %s", v.Name, identifierRe)
			continue
		}
		if _, dup := c.variables[v.Name]; dup {
			p.addf("variable %q: declared more than once", v.Name)
			continue
		}
		if v.Pattern != "" {
			re, err := regexp.Compile(v.Pattern)
			if err != nil {
				p.addf("variable %q: pattern does not compile: %v", v.Name, err)
			} else if v.Default != "" && !re.MatchString(v.Default) {
				p.addf("variable %q: default %q does not match its own pattern", v.Name, v.Default)
			}
		}
		if len(v.Enum) > 0 && v.Default != "" && !contains(v.Enum, v.Default) {
			p.addf("variable %q: default %q is not one of its enum values", v.Name, v.Default)
		}
		c.variables[v.Name] = v
	}
}

func compileSecrets(secrets []Secret, c *Compiled, p *problems) {
	for _, s := range secrets {
		if !identifierRe.MatchString(s.Name) {
			p.addf("secret %q: name must match %s", s.Name, identifierRe)
			continue
		}
		if _, dup := c.secretDefs[s.Name]; dup {
			p.addf("secret %q: declared more than once", s.Name)
			continue
		}
		c.secretDefs[s.Name] = s
	}
}

func validateDefaults(d Defaults, p *problems) {
	if d.TimeoutMS < 0 || d.TimeoutMS > MaxTimeoutMS {
		p.addf("spec.defaults.timeoutMs must be between 0 and %d", MaxTimeoutMS)
	}
	if d.MaxResponseBytes < 0 || d.MaxResponseBytes > MaxResponseBytesCap {
		p.addf("spec.defaults.maxResponseBytes must be between 0 and %d", MaxResponseBytesCap)
	}
	if d.RateLimitPerMin < 0 {
		p.addf("spec.defaults.rateLimitPerMin must not be negative")
	}
	if d.MaxConcurrent < 0 {
		p.addf("spec.defaults.maxConcurrent must not be negative")
	}
}

func compileAuth(a AuthSpec, c *Compiled, p *problems) *CompiledAuth {
	typ := a.Type
	if typ == "" {
		typ = AuthNone
	}
	out := &CompiledAuth{Type: typ, In: a.In, Name: a.Name}

	parse := func(field, src string) *tmpl.Template {
		if src == "" {
			p.addf("spec.auth.%s is required for auth type %q", field, typ)
			return nil
		}
		t, err := tmpl.Parse(src)
		if err != nil {
			p.addf("spec.auth.%s: %v", field, err)
			return nil
		}
		checkRefs("spec.auth."+field, t, c, nil, p)
		return t
	}

	switch typ {
	case AuthNone:
	case AuthAPIKey:
		switch a.In {
		case "header":
			if !headerNameRe.MatchString(a.Name) {
				p.addf("spec.auth.name %q is not a valid header name", a.Name)
			}
		case "query":
			if a.Name == "" {
				p.addf("spec.auth.name is required for apiKey in query")
			}
		default:
			p.addf("spec.auth.in must be header or query for apiKey, got %q", a.In)
		}
		out.Value = parse("value", a.Value)
	case AuthBearer:
		out.Value = parse("value", a.Value)
	case AuthBasic:
		out.Username = parse("username", a.Username)
		out.Password = parse("password", a.Password)
	default:
		p.addf("spec.auth.type must be one of none, apiKey, bearer, basic; got %q", typ)
	}
	return out
}

func compileTools(tools []Tool, c *Compiled, p *problems) {
	if len(tools) == 0 {
		p.addf("spec.tools must declare at least one tool")
		return
	}
	if len(tools) > MaxToolsPerConnector {
		p.addf("spec.tools declares %d tools, the maximum is %d", len(tools), MaxToolsPerConnector)
		return
	}
	for i := range tools {
		t := &tools[i]
		ct := compileTool(t, c, p)
		if ct == nil {
			continue
		}
		if _, dup := c.byName[t.Name]; dup {
			p.addf("tool %q: declared more than once", t.Name)
			continue
		}
		c.byName[t.Name] = ct
		c.Tools = append(c.Tools, ct)
	}
}

func compileTool(t *Tool, c *Compiled, p *problems) *CompiledTool {
	where := "tool " + quote(t.Name)
	if !toolNameRe.MatchString(t.Name) {
		p.addf("%s: name must match %s", where, toolNameRe)
		return nil
	}
	if strings.TrimSpace(t.Description) == "" {
		// The description is the only thing a model has to decide whether to
		// call the tool. An undescribed tool is a misfire waiting to happen.
		p.addf("%s: description must not be empty", where)
	}

	ct := &CompiledTool{Def: t, SuccessCodes: map[int]bool{}}

	schema, inputProps := compileInputSchema(where, t.InputSchema, p)
	ct.InputSchema = schema
	if t.OutputSchema != nil {
		if _, _, err := compileSchema(t.OutputSchema); err != nil {
			p.addf("%s: outputSchema: %v", where, err)
		}
	}

	method := strings.ToUpper(strings.TrimSpace(t.Request.Method))
	if !allowedMethods[method] {
		p.addf("%s: request.method %q is not permitted", where, t.Request.Method)
	}
	t.Request.Method = method

	ct.Path = compilePath(where, t.Request.Path, c, inputProps, p)
	ct.Query = compilePairs(where+".request.query", t.Request.Query, c, p, false)
	ct.Headers = compilePairs(where+".request.headers", t.Request.Headers, c, p, true)
	for _, pair := range append(append([]Pair{}, ct.Query...), ct.Headers...) {
		checkRefs(where, pair.Value, c, inputProps, p)
	}

	if t.Request.Auth != "" && t.Request.Auth != AuthNone {
		p.addf("%s: request.auth may only be %q or omitted", where, AuthNone)
	}
	ct.Body = compileBody(where, t.Request.Body, method, c, inputProps, p)

	compileResponse(where, &t.Response, ct, p)
	return ct
}

func compileInputSchema(where string, raw map[string]any, p *problems) (*jsonschema.Schema, map[string]bool) {
	if raw == nil {
		p.addf("%s: inputSchema is required (use {type: object, properties: {}} for a no-argument tool)", where)
		return nil, nil
	}
	if typ, _ := raw["type"].(string); typ != "object" {
		// MCP tool arguments are always a JSON object; anything else would be
		// rejected by clients rather than by us, and much later.
		p.addf("%s: inputSchema.type must be \"object\"", where)
	}
	schema, normalised, err := compileSchema(raw)
	if err != nil {
		p.addf("%s: inputSchema: %v", where, err)
		return nil, nil
	}
	props := map[string]bool{}
	if m, ok := normalised["properties"].(map[string]any); ok {
		for k := range m {
			props[k] = true
		}
	}
	if len(props) == 0 {
		// No declared properties means we cannot check input references, so we
		// signal "unknown" rather than "empty" by returning nil.
		return schema, nil
	}
	return schema, props
}

// noRemoteLoader refuses every schema reference the manifest does not carry
// itself. Without it, importing a manifest containing `$ref: https://…` would
// make the platform fetch an attacker-chosen URL at import time.
type noRemoteLoader struct{}

func (noRemoteLoader) Load(url string) (any, error) {
	return nil, fmt.Errorf("remote schema reference %q is not permitted", url)
}

func compileSchema(raw map[string]any) (*jsonschema.Schema, map[string]any, error) {
	// Round-tripping through JSON normalises YAML's integer types and rejects
	// non-string map keys, which the schema compiler cannot represent.
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, nil, fmt.Errorf("cannot be encoded as JSON: %w", err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(data))
	if err != nil {
		return nil, nil, fmt.Errorf("cannot be decoded as JSON: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	compiler.UseLoader(noRemoteLoader{})
	compiler.AssertFormat()
	const loc = "mcpaw://tool-schema"
	if err := compiler.AddResource(loc, doc); err != nil {
		return nil, nil, err
	}
	schema, err := compiler.Compile(loc)
	if err != nil {
		return nil, nil, err
	}
	var normalised map[string]any
	if err := json.Unmarshal(data, &normalised); err != nil {
		return nil, nil, err
	}
	return schema, normalised, nil
}

func compilePath(where, path string, c *Compiled, inputProps map[string]bool, p *problems) *tmpl.Template {
	if !strings.HasPrefix(path, "/") {
		p.addf("%s: request.path must start with /", where)
		return nil
	}
	// A path that reintroduces an origin would let a manifest silently retarget
	// the request away from the configured base URL.
	if strings.Contains(path, "://") || strings.HasPrefix(path, "//") {
		p.addf("%s: request.path must be a path, not a URL", where)
		return nil
	}
	if strings.Contains(path, "?") || strings.Contains(path, "#") {
		p.addf("%s: request.path must not contain a query string or fragment; use request.query", where)
		return nil
	}
	t, err := tmpl.Parse(path)
	if err != nil {
		p.addf("%s: request.path: %v", where, err)
		return nil
	}
	checkRefs(where, t, c, inputProps, p)
	return t
}

func compilePairs(where string, in map[string]string, c *Compiled, p *problems, headerNames bool) []Pair {
	if len(in) == 0 {
		return nil
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	// Deterministic ordering keeps rendered requests reproducible, which makes
	// them comparable in tests and in logs.
	sort.Strings(keys)

	out := make([]Pair, 0, len(keys))
	for _, k := range keys {
		if headerNames && !headerNameRe.MatchString(k) {
			p.addf("%s: %q is not a valid header name", where, k)
			continue
		}
		if headerNames && isForbiddenHeader(k) {
			p.addf("%s: header %q is controlled by the platform and cannot be set by a manifest", where, k)
			continue
		}
		t, err := tmpl.Parse(in[k])
		if err != nil {
			p.addf("%s[%s]: %v", where, k, err)
			continue
		}
		out = append(out, Pair{Key: k, Value: t})
	}
	return out
}

// forbiddenHeaders may not be set from a manifest: they are either owned by the
// transport, or they would let a manifest smuggle a second request.
var forbiddenHeaders = map[string]bool{
	"host": true, "content-length": true, "transfer-encoding": true,
	"connection": true, "upgrade": true, "proxy-authorization": true,
	"proxy-connection": true, "te": true, "trailer": true,
}

func isForbiddenHeader(name string) bool { return forbiddenHeaders[strings.ToLower(name)] }

func compileBody(where string, b *BodySpec, method string, c *Compiled, inputProps map[string]bool, p *problems) *CompiledBody {
	if b == nil {
		return nil
	}
	if method == "GET" || method == "HEAD" {
		p.addf("%s: request.body is not allowed with %s", where, method)
		return nil
	}
	set := 0
	if b.JSON != nil {
		set++
	}
	if b.From != "" {
		set++
	}
	if b.Text != "" {
		set++
	}
	if set != 1 {
		p.addf("%s: request.body must set exactly one of json, from or text", where)
		return nil
	}

	out := &CompiledBody{ContentType: b.ContentType}
	switch {
	case b.JSON != nil:
		if out.ContentType == "" {
			out.ContentType = "application/json"
		}
		out.JSON = compileBodyNode(where, b.JSON, c, inputProps, p)
	case b.From != "":
		if out.ContentType == "" {
			out.ContentType = "application/json"
		}
		t, err := tmpl.Parse("{{" + b.From + "}}")
		if err != nil {
			p.addf("%s: request.body.from: %v", where, err)
			return nil
		}
		checkRefs(where, t, c, inputProps, p)
		out.From = t
	case b.Text != "":
		if out.ContentType == "" {
			out.ContentType = "text/plain; charset=utf-8"
		}
		t, err := tmpl.Parse(b.Text)
		if err != nil {
			p.addf("%s: request.body.text: %v", where, err)
			return nil
		}
		checkRefs(where, t, c, inputProps, p)
		out.Text = t
	}
	return out
}

func compileBodyNode(where string, v any, c *Compiled, inputProps map[string]bool, p *problems) *BodyNode {
	switch x := v.(type) {
	case string:
		t, err := tmpl.Parse(x)
		if err != nil {
			p.addf("%s: request.body.json: %v", where, err)
			return nil
		}
		checkRefs(where, t, c, inputProps, p)
		return &BodyNode{tmpl: t}
	case map[string]any:
		node := &BodyNode{object: map[string]*BodyNode{}}
		for k, val := range x {
			if child := compileBodyNode(where, val, c, inputProps, p); child != nil {
				node.object[k] = child
			}
		}
		return node
	case []any:
		node := &BodyNode{}
		for _, val := range x {
			if child := compileBodyNode(where, val, c, inputProps, p); child != nil {
				node.array = append(node.array, child)
			}
		}
		if node.array == nil {
			node.array = []*BodyNode{}
		}
		return node
	default:
		return &BodyNode{literal: v, hasLiteral: true}
	}
}

func compileResponse(where string, r *ResponseSpec, ct *CompiledTool, p *problems) {
	for _, code := range r.SuccessCodes {
		if code < 100 || code > 599 {
			p.addf("%s: response.successCodes contains invalid status %d", where, code)
			continue
		}
		ct.SuccessCodes[code] = true
	}
	switch r.Format {
	case "", FormatAuto, FormatJSON, FormatText:
	default:
		p.addf("%s: response.format must be auto, json or text; got %q", where, r.Format)
	}
	if r.MaxBytes < 0 || r.MaxBytes > MaxResponseBytesCap {
		p.addf("%s: response.maxBytes must be between 0 and %d", where, MaxResponseBytesCap)
	}
	if r.Select != "" {
		for _, seg := range strings.Split(r.Select, ".") {
			if seg == "" {
				p.addf("%s: response.select %q has an empty path segment", where, r.Select)
				break
			}
		}
		ct.SelectPath = strings.Split(r.Select, ".")
	}
	for _, h := range r.IncludeHeaders {
		if !headerNameRe.MatchString(h) {
			p.addf("%s: response.includeHeaders contains invalid header name %q", where, h)
			continue
		}
		ct.IncludeHeaders = append(ct.IncludeHeaders, h)
	}
}

// checkRefs verifies that every variable and secret a template reads is
// declared, and that top-level input references correspond to a declared schema
// property. This turns a class of runtime "why is this parameter empty?"
// mysteries into an import-time error message.
func checkRefs(where string, t *tmpl.Template, c *Compiled, inputProps map[string]bool, p *problems) {
	if t == nil {
		return
	}
	for _, ref := range t.Refs() {
		switch ref.Namespace {
		case tmpl.NamespaceVars:
			if _, ok := c.variables[ref.Path[0]]; !ok {
				p.addf("%s: template references undeclared variable %q", where, ref.Path[0])
			}
		case tmpl.NamespaceSecrets:
			if _, ok := c.secretDefs[ref.Path[0]]; !ok {
				p.addf("%s: template references undeclared secret %q", where, ref.Path[0])
			}
		case tmpl.NamespaceInput:
			if inputProps != nil && !inputProps[ref.Path[0]] {
				p.addf("%s: template references input %q, which is not a property of inputSchema", where, ref.Path[0])
			}
		}
	}
}

// Tool returns the compiled tool with the given name.
func (c *Compiled) Tool(name string) (*CompiledTool, bool) {
	t, ok := c.byName[name]
	return t, ok
}

// Variables returns the declared variables in manifest order.
func (c *Compiled) Variables() []Variable { return c.Manifest.Spec.Variables }

// Secrets returns the declared secrets in manifest order.
func (c *Compiled) Secrets() []Secret { return c.Manifest.Spec.Secrets }

// Variable looks up a declared variable.
func (c *Compiled) Variable(name string) (Variable, bool) {
	v, ok := c.variables[name]
	return v, ok
}

// ValidateVariables checks operator-supplied values against the manifest's
// declarations, returning one error describing every problem.
func (c *Compiled) ValidateVariables(values map[string]string) error {
	var p problems
	for _, v := range c.Manifest.Spec.Variables {
		val, present := values[v.Name]
		if !present || val == "" {
			if v.Required && v.Default == "" {
				p.addf("variable %q is required", v.Name)
			}
			continue
		}
		if v.Pattern != "" {
			// The pattern compiled during Compile, so a failure here is
			// impossible; ignoring the error keeps the happy path readable.
			if re, err := regexp.Compile(v.Pattern); err == nil && !re.MatchString(val) {
				p.addf("variable %q does not match the required pattern %s", v.Name, v.Pattern)
			}
		}
		if len(v.Enum) > 0 && !contains(v.Enum, val) {
			p.addf("variable %q must be one of %s", v.Name, strings.Join(v.Enum, ", "))
		}
	}
	for name := range values {
		if _, ok := c.variables[name]; !ok {
			p.addf("unknown variable %q", name)
		}
	}
	return p.err()
}

// ValidateSecretNames rejects secrets an instance sets that the connector never
// declared, so a typo surfaces immediately instead of silently doing nothing.
func (c *Compiled) ValidateSecretNames(names []string) error {
	var p problems
	for _, n := range names {
		if _, ok := c.secretDefs[n]; !ok {
			p.addf("unknown secret %q", n)
		}
	}
	return p.err()
}

// MissingRequiredSecrets returns the names of declared secrets that are
// required but absent from the supplied set.
func (c *Compiled) MissingRequiredSecrets(present map[string][]byte) []string {
	var missing []string
	for _, s := range c.Manifest.Spec.Secrets {
		if !s.Required {
			continue
		}
		if _, ok := present[s.Name]; !ok {
			missing = append(missing, s.Name)
		}
	}
	sort.Strings(missing)
	return missing
}

// ResolveVariables merges operator values over manifest defaults.
func (c *Compiled) ResolveVariables(values map[string]string) map[string]string {
	out := make(map[string]string, len(c.variables))
	for _, v := range c.Manifest.Spec.Variables {
		if val, ok := values[v.Name]; ok && val != "" {
			out[v.Name] = val
			continue
		}
		if v.Default != "" {
			out[v.Name] = v.Default
		}
	}
	return out
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

// quote wraps a name in double quotes for error messages.
func quote(s string) string { return "\"" + s + "\"" }
