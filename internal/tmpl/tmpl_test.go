package tmpl

import (
	"net/url"
	"strings"
	"testing"
)

func scope() MapScope {
	return MapScope{
		Input: map[string]any{
			"query": "machine learning",
			"limit": float64(25),
			"flag":  true,
			"nested": map[string]any{
				"key": "deep",
			},
			"list": []any{1, 2},
		},
		Vars:    map[string]string{"userId": "0"},
		Secrets: map[string]string{"apiKey": "sk-live-abc"},
	}
}

func render(t *testing.T, src string, esc Escaper) (string, bool) {
	t.Helper()
	tpl, err := Parse(src)
	if err != nil {
		t.Fatalf("Parse(%q): %v", src, err)
	}
	out, ok, err := tpl.Render(scope(), esc)
	if err != nil {
		t.Fatalf("Render(%q): %v", src, err)
	}
	return out, ok
}

func TestRenderBasics(t *testing.T) {
	cases := []struct{ src, want string }{
		{"/api/users/{{vars.userId}}/items", "/api/users/0/items"},
		{"{{input.query}}", "machine learning"},
		{"{{input.limit}}", "25"},
		{"{{input.flag}}", "true"},
		{"{{input.nested.key}}", "deep"},
		{"{{ vars.userId }}", "0"},
		{"no placeholders", "no placeholders"},
		{"a{{vars.userId}}b{{vars.userId}}c", "a0b0c"},
	}
	for _, c := range cases {
		got, ok := render(t, c.src, Identity)
		if !ok || got != c.want {
			t.Errorf("Render(%q) = %q, %v; want %q, true", c.src, got, ok, c.want)
		}
	}
}

// An integral JSON number must not acquire a ".0" suffix: many APIs reject
// "limit=25.0" outright.
func TestIntegralNumbersRenderWithoutDecimalPoint(t *testing.T) {
	got, _ := render(t, "{{input.limit}}", Identity)
	if strings.Contains(got, ".") {
		t.Fatalf("got %q, want an integer rendering", got)
	}
}

func TestUnresolvedPlaceholderReportsNotOK(t *testing.T) {
	got, ok := render(t, "{{input.missing}}", Identity)
	if ok {
		t.Fatalf("missing value reported as resolved (%q)", got)
	}
	// A partially resolvable template must be entirely unresolved, never
	// half-rendered.
	if got, ok := render(t, "q={{input.query}}&x={{input.missing}}", Identity); ok || got != "" {
		t.Fatalf("partial render leaked %q (ok=%v)", got, ok)
	}
}

func TestDefaults(t *testing.T) {
	got, ok := render(t, `{{input.missing|default:"fallback"}}`, Identity)
	if !ok || got != "fallback" {
		t.Fatalf("got %q, %v; want fallback, true", got, ok)
	}
	got, ok = render(t, "{{input.missing|default:25}}", Identity)
	if !ok || got != "25" {
		t.Fatalf("got %q, %v; want 25, true", got, ok)
	}
	// A present value wins over the default.
	got, _ = render(t, `{{input.query|default:"unused"}}`, Identity)
	if got != "machine learning" {
		t.Fatalf("default overrode a present value: %q", got)
	}
}

// The escaper applies to substituted values only. If it applied to literal
// text, a path template's own slashes would be encoded and every request would
// 404.
func TestEscapingAppliesOnlyToValues(t *testing.T) {
	s := MapScope{Vars: map[string]string{"userId": "a/b c"}}
	tpl := MustParse("/api/users/{{vars.userId}}/items")
	got, ok, err := tpl.Render(s, url.PathEscape)
	if err != nil || !ok {
		t.Fatalf("Render: %v %v", ok, err)
	}
	if got != "/api/users/a%2Fb%20c/items" {
		t.Fatalf("got %q", got)
	}
}

// Path traversal supplied as a tool argument must not escape the intended path.
func TestPathTraversalIsNeutralised(t *testing.T) {
	s := MapScope{Input: map[string]any{"key": "../../admin"}}
	tpl := MustParse("/api/items/{{input.key}}")
	got, _, _ := tpl.Render(s, url.PathEscape)
	if strings.Contains(got, "../") {
		t.Fatalf("traversal survived escaping: %q", got)
	}
}

func TestCompositeValuesCannotBeInterpolatedIntoStrings(t *testing.T) {
	tpl := MustParse("x={{input.list}}")
	if _, _, err := tpl.Render(scope(), Identity); err == nil {
		t.Fatal("expected an error interpolating an array into a string")
	}
}

func TestRenderValuePreservesTypes(t *testing.T) {
	cases := []struct {
		src  string
		want any
	}{
		{"{{input.limit}}", float64(25)},
		{"{{input.flag}}", true},
		{"{{input.query}}", "machine learning"},
	}
	for _, c := range cases {
		v, ok, err := MustParse(c.src).RenderValue(scope())
		if err != nil || !ok || v != c.want {
			t.Errorf("RenderValue(%q) = %#v, %v, %v; want %#v", c.src, v, ok, err, c.want)
		}
	}
	// A numeric default keeps its JSON type rather than becoming a string.
	v, ok, err := MustParse("{{input.missing|default:7}}").RenderValue(scope())
	if err != nil || !ok || v != float64(7) {
		t.Fatalf("RenderValue with numeric default = %#v (%T), %v, %v", v, v, ok, err)
	}
	// Composite values survive intact.
	v, _, _ = MustParse("{{input.nested}}").RenderValue(scope())
	if _, isMap := v.(map[string]any); !isMap {
		t.Fatalf("nested object was flattened: %#v", v)
	}
}

func TestParseRejectsMalformedTemplates(t *testing.T) {
	bad := []string{
		"{{input.query",           // unterminated
		"{{}}",                    // empty
		"{{env.PATH}}",            // unknown namespace
		"{{input}}",               // no path
		"{{input.}}",              // empty path element
		"{{input.a b}}",           // invalid identifier
		"{{input.query|upper}}",   // unsupported filter
		"{{input.query|reverse}}", // unsupported filter
	}
	for _, src := range bad {
		if _, err := Parse(src); err == nil {
			t.Errorf("Parse(%q) succeeded; want an error", src)
		}
	}
}

func TestRefsAndSecretDetection(t *testing.T) {
	tpl := MustParse("{{vars.userId}}/{{secrets.apiKey}}")
	refs := tpl.Refs()
	if len(refs) != 2 {
		t.Fatalf("Refs() = %v", refs)
	}
	if refs[0].String() != "vars.userId" || refs[1].String() != "secrets.apiKey" {
		t.Fatalf("unexpected refs %v", refs)
	}
	if !tpl.UsesSecrets() {
		t.Fatal("UsesSecrets() = false for a template reading secrets")
	}
	if MustParse("{{vars.userId}}").UsesSecrets() {
		t.Fatal("UsesSecrets() = true for a template with no secrets")
	}
	if !MustParse("static").Static() || MustParse("{{vars.userId}}").Static() {
		t.Fatal("Static() is wrong")
	}
}

func TestNilScopeFallsBackToDefaults(t *testing.T) {
	got, ok, err := MustParse(`{{input.x|default:"d"}}`).Render(nil, Identity)
	if err != nil || !ok || got != "d" {
		t.Fatalf("got %q, %v, %v", got, ok, err)
	}
	if _, ok, _ := MustParse("{{input.x}}").Render(nil, Identity); ok {
		t.Fatal("nil scope resolved a reference with no default")
	}
}

func TestLookupRejectsNestedVarsAndSecrets(t *testing.T) {
	s := scope()
	if _, ok := s.Lookup(NamespaceVars, []string{"a", "b"}); ok {
		t.Fatal("vars namespace should be flat")
	}
	if _, ok := s.Lookup(NamespaceSecrets, []string{"a", "b"}); ok {
		t.Fatal("secrets namespace should be flat")
	}
	if _, ok := s.Lookup("unknown", []string{"x"}); ok {
		t.Fatal("unknown namespace resolved")
	}
}
