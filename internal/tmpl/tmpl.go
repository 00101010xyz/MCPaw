// Package tmpl implements the deliberately minimal template language used by
// connector manifests.
//
// Why not text/template? Because a connector manifest is untrusted-ish
// configuration that renders attacker-influenced values (LLM-supplied tool
// arguments) into URLs, headers and request bodies. A general template engine
// puts the burden of correct escaping on whoever writes the manifest, and one
// forgotten escape becomes an injection. This language instead supports exactly
// one construct — a namespaced lookup with an optional default — and leaves
// escaping to the *sink*, which knows the correct rules.
//
// Grammar:
//
//	text        := (literal | placeholder)*
//	placeholder := "{{" ws namespace "." path (ws "|" ws "default:" value)? ws "}}"
//	namespace   := "input" | "vars" | "secrets"
//	path        := ident ("." ident)*
//
// There are no function calls, no conditionals, no iteration and no arithmetic,
// so a manifest cannot express anything beyond substitution.
package tmpl

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// Namespaces recognised by the language.
const (
	NamespaceInput   = "input"
	NamespaceVars    = "vars"
	NamespaceSecrets = "secrets"
)

// Ref is one namespaced lookup appearing in a template.
type Ref struct {
	Namespace string
	Path      []string
}

// String renders the reference in source syntax, for error messages.
func (r Ref) String() string { return r.Namespace + "." + strings.Join(r.Path, ".") }

// Scope resolves references during rendering.
type Scope interface {
	// Lookup returns the value at path within namespace. The boolean reports
	// whether the value exists; a nil value that exists is distinct from a
	// value that does not.
	Lookup(namespace string, path []string) (any, bool)
}

// Template is a parsed template, safe for concurrent use.
type Template struct {
	src   string
	parts []part
}

type part struct {
	literal    string
	ref        *Ref
	defaultVal string
	hasDefault bool
}

// Escaper transforms a substituted value before it is written into the output.
// The sink chooses it, never the manifest author.
type Escaper func(string) string

// Identity is an Escaper that performs no transformation. Use it only where the
// sink applies its own encoding (for example url.Values, which percent-encodes
// on Encode).
func Identity(s string) string { return s }

// Parse compiles a template. It fails on unknown namespaces, unterminated
// placeholders and unsupported filters, so a malformed manifest is rejected at
// import time rather than at tool-call time.
func Parse(src string) (*Template, error) {
	t := &Template{src: src}
	rest := src
	for {
		open := strings.Index(rest, "{{")
		if open < 0 {
			if rest != "" {
				t.parts = append(t.parts, part{literal: rest})
			}
			return t, nil
		}
		if open > 0 {
			t.parts = append(t.parts, part{literal: rest[:open]})
		}
		rest = rest[open+2:]
		close := strings.Index(rest, "}}")
		if close < 0 {
			return nil, fmt.Errorf("tmpl: unterminated placeholder in %q", src)
		}
		p, err := parsePlaceholder(rest[:close])
		if err != nil {
			return nil, fmt.Errorf("tmpl: in %q: %w", src, err)
		}
		t.parts = append(t.parts, p)
		rest = rest[close+2:]
	}
}

// MustParse is Parse for compile-time-constant templates in tests and defaults.
func MustParse(src string) *Template {
	t, err := Parse(src)
	if err != nil {
		panic(err)
	}
	return t
}

func parsePlaceholder(body string) (part, error) {
	segments := strings.Split(body, "|")
	lookup := strings.TrimSpace(segments[0])
	if lookup == "" {
		return part{}, fmt.Errorf("empty placeholder")
	}

	namespace, path, ok := strings.Cut(lookup, ".")
	if !ok || path == "" {
		return part{}, fmt.Errorf("placeholder %q must be namespace.path", lookup)
	}
	switch namespace {
	case NamespaceInput, NamespaceVars, NamespaceSecrets:
	default:
		return part{}, fmt.Errorf("unknown namespace %q (want input, vars or secrets)", namespace)
	}
	elems := strings.Split(path, ".")
	for _, e := range elems {
		if !validIdent(e) {
			return part{}, fmt.Errorf("invalid path element %q in %q", e, lookup)
		}
	}

	p := part{ref: &Ref{Namespace: namespace, Path: elems}}
	for _, seg := range segments[1:] {
		filter := strings.TrimSpace(seg)
		val, ok := strings.CutPrefix(filter, "default:")
		if !ok {
			return part{}, fmt.Errorf("unsupported filter %q (only default: is available)", filter)
		}
		p.defaultVal = unquote(strings.TrimSpace(val))
		p.hasDefault = true
	}
	return p, nil
}

func unquote(s string) string {
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		if s[0] == '"' {
			if out, err := strconv.Unquote(s); err == nil {
				return out
			}
		}
		return s[1 : len(s)-1]
	}
	return s
}

func validIdent(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '_':
		case c >= '0' && c <= '9' && i > 0:
		case c == '-' && i > 0:
		default:
			return false
		}
	}
	return true
}

// Source returns the original template text.
func (t *Template) Source() string { return t.src }

// Static reports whether the template contains no placeholders, in which case
// rendering is guaranteed to succeed and produce Source().
func (t *Template) Static() bool {
	for _, p := range t.parts {
		if p.ref != nil {
			return false
		}
	}
	return true
}

// Refs returns every reference the template makes, used by manifest validation
// to reject templates that read undeclared variables or secrets.
func (t *Template) Refs() []Ref {
	var out []Ref
	for _, p := range t.parts {
		if p.ref != nil {
			out = append(out, *p.ref)
		}
	}
	return out
}

// UsesSecrets reports whether the template reads the secrets namespace. Callers
// use it to decide whether a rendered value must be kept out of logs.
func (t *Template) UsesSecrets() bool {
	for _, p := range t.parts {
		if p.ref != nil && p.ref.Namespace == NamespaceSecrets {
			return true
		}
	}
	return false
}

// Render substitutes values from scope, applying escape to each substituted
// value but never to literal text.
//
// The boolean result reports whether every placeholder resolved. When it is
// false the caller must drop the whole field: emitting a half-rendered URL or
// an empty query parameter is how "optional filter" silently becomes "match
// everything".
func (t *Template) Render(scope Scope, escape Escaper) (string, bool, error) {
	if escape == nil {
		escape = Identity
	}
	var sb strings.Builder
	sb.Grow(len(t.src))
	for _, p := range t.parts {
		if p.ref == nil {
			sb.WriteString(p.literal)
			continue
		}
		raw, ok := resolve(scope, p)
		if !ok {
			return "", false, nil
		}
		s, err := stringify(raw)
		if err != nil {
			return "", false, fmt.Errorf("tmpl: %s: %w", p.ref, err)
		}
		sb.WriteString(escape(s))
	}
	return sb.String(), true, nil
}

// RenderValue renders a template that consists of exactly one placeholder as
// the underlying typed value, preserving numbers, booleans, objects and arrays.
// Any other template renders as a string. This is what lets a JSON body
// template carry a real integer rather than the string "25".
func (t *Template) RenderValue(scope Scope) (any, bool, error) {
	if len(t.parts) == 1 && t.parts[0].ref != nil {
		p := t.parts[0]
		raw, ok := resolve(scope, p)
		if !ok {
			return nil, false, nil
		}
		if p.hasDefault {
			if s, isString := raw.(string); isString && s == p.defaultVal {
				return coerceDefault(p.defaultVal), true, nil
			}
		}
		return raw, true, nil
	}
	s, ok, err := t.Render(scope, Identity)
	return s, ok, err
}

func resolve(scope Scope, p part) (any, bool) {
	if scope != nil {
		if v, ok := scope.Lookup(p.ref.Namespace, p.ref.Path); ok && v != nil {
			return v, true
		}
	}
	if p.hasDefault {
		return p.defaultVal, true
	}
	return nil, false
}

// coerceDefault gives a default literal the most specific JSON type it can
// carry, so `default:25` in a body template is the number 25.
func coerceDefault(s string) any {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err == nil {
		switch v.(type) {
		case float64, bool, nil:
			return v
		}
	}
	return s
}

// stringify converts a resolved value to text. Composite values are refused
// rather than rendered as Go syntax, which would be both meaningless to the
// upstream API and a subtle information leak.
func stringify(v any) (string, error) {
	switch x := v.(type) {
	case string:
		return x, nil
	case bool:
		return strconv.FormatBool(x), nil
	case float64:
		// JSON numbers arrive as float64; render integral values without a
		// trailing ".0" so that limit=25 does not become limit=25.0.
		if x == float64(int64(x)) {
			return strconv.FormatInt(int64(x), 10), nil
		}
		return strconv.FormatFloat(x, 'f', -1, 64), nil
	case int:
		return strconv.Itoa(x), nil
	case int64:
		return strconv.FormatInt(x, 10), nil
	case json.Number:
		return x.String(), nil
	case nil:
		return "", fmt.Errorf("value is null")
	default:
		return "", fmt.Errorf("cannot interpolate %T into a string", v)
	}
}

// MapScope is the standard Scope backed by three maps.
type MapScope struct {
	Input   map[string]any
	Vars    map[string]string
	Secrets map[string]string
}

// Lookup implements Scope.
func (m MapScope) Lookup(namespace string, path []string) (any, bool) {
	switch namespace {
	case NamespaceInput:
		return lookupNested(m.Input, path)
	case NamespaceVars:
		if len(path) != 1 {
			return nil, false
		}
		v, ok := m.Vars[path[0]]
		return v, ok
	case NamespaceSecrets:
		if len(path) != 1 {
			return nil, false
		}
		v, ok := m.Secrets[path[0]]
		return v, ok
	default:
		return nil, false
	}
}

func lookupNested(root map[string]any, path []string) (any, bool) {
	var cur any = root
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[key]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}
