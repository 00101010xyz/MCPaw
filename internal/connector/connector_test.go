package connector

import (
	"strings"
	"testing"
)

const minimalManifest = `
apiVersion: mcpaw.dev/v1
kind: Connector
metadata:
  id: example
  name: Example
  version: 1.0.0
spec:
  baseUrl:
    default: https://api.example.com
  variables:
    - name: accountId
      default: "42"
      pattern: '^[0-9]+$'
  secrets:
    - name: apiKey
      required: true
  auth:
    type: bearer
    value: "{{secrets.apiKey}}"
  tools:
    - name: get_thing
      description: Fetch a thing.
      inputSchema:
        type: object
        additionalProperties: false
        required: [thingId]
        properties:
          thingId: {type: string}
          limit: {type: integer}
      request:
        method: GET
        path: /accounts/{{vars.accountId}}/things/{{input.thingId}}
        query:
          limit: "{{input.limit|default:10}}"
      response:
        successCodes: [200]
`

func compileString(t *testing.T, src string) (*Compiled, error) {
	t.Helper()
	m, err := ParseManifest([]byte(src))
	if err != nil {
		return nil, err
	}
	return Compile(m)
}

func mustCompile(t *testing.T, src string) *Compiled {
	t.Helper()
	c, err := compileString(t, src)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return c
}

func TestCompileMinimalManifest(t *testing.T) {
	c := mustCompile(t, minimalManifest)
	if len(c.Tools) != 1 {
		t.Fatalf("got %d tools, want 1", len(c.Tools))
	}
	tool, ok := c.Tool("get_thing")
	if !ok {
		t.Fatal("tool lookup by name failed")
	}
	if tool.InputSchema == nil || tool.Path == nil {
		t.Fatal("tool was not fully compiled")
	}
	if !tool.EnabledByDefault() {
		t.Fatal("a non-disabled tool should be enabled by default")
	}
	if c.Auth.Type != AuthBearer {
		t.Fatalf("auth type = %q", c.Auth.Type)
	}
}

// Every manifest shipped inside the binary must compile, because a broken one
// would only be discovered by an operator at their first tool call.
func TestBuiltinManifestsCompile(t *testing.T) {
	recs, err := Builtins()
	if err != nil {
		t.Fatalf("Builtins: %v", err)
	}
	if len(recs) == 0 {
		t.Fatal("no built-in connectors were embedded")
	}
	var foundZotero bool
	for _, rec := range recs {
		if rec.ID == "zotero-local" {
			foundZotero = true
		}
		if rec.Checksum == "" || rec.Source != "builtin" {
			t.Fatalf("built-in %s has bad provenance: %+v", rec.ID, rec)
		}
	}
	if !foundZotero {
		t.Fatal("the Zotero connector is missing from the built-ins")
	}
}

func TestZoteroConnectorShape(t *testing.T) {
	recs, err := Builtins()
	if err != nil {
		t.Fatalf("Builtins: %v", err)
	}
	var zotero *Compiled
	for _, rec := range recs {
		if rec.ID == "zotero-local" {
			m, err := ParseManifest(rec.Manifest)
			if err != nil {
				t.Fatalf("ParseManifest: %v", err)
			}
			if zotero, err = Compile(m); err != nil {
				t.Fatalf("Compile: %v", err)
			}
		}
	}
	if zotero == nil {
		t.Fatal("zotero connector not found")
	}

	if !zotero.Manifest.Spec.BaseURL.RequiresPrivateNetwork {
		t.Error("the Zotero connector must declare that it needs private-network egress")
	}
	for _, want := range []string{
		"zotero_search_items", "zotero_get_item", "zotero_list_collections",
		"zotero_list_collection_items", "zotero_list_tags", "zotero_export_items",
	} {
		tool, ok := zotero.Tool(want)
		if !ok {
			t.Errorf("missing tool %s", want)
			continue
		}
		// The local Zotero API is read-only; every shipped tool must say so and
		// must actually be a safe method.
		if tool.Def.Annotations == nil || tool.Def.Annotations.ReadOnlyHint == nil || !*tool.Def.Annotations.ReadOnlyHint {
			t.Errorf("tool %s is missing readOnlyHint", want)
		}
		if tool.Def.Request.Method != "GET" {
			t.Errorf("tool %s uses %s, want GET", want, tool.Def.Request.Method)
		}
	}

	// Every declared secret is optional: the connector must keep working,
	// unauthenticated, against the local API with nothing configured at all.
	for _, secret := range zotero.Secrets() {
		if secret.Required {
			t.Errorf("secret %s must be optional", secret.Name)
		}
	}
	if missing := zotero.MissingRequiredSecrets(nil); len(missing) != 0 {
		t.Errorf("Zotero should work with no secrets, missing=%v", missing)
	}
}

func TestGiteaConnectorShape(t *testing.T) {
	recs, err := Builtins()
	if err != nil {
		t.Fatalf("Builtins: %v", err)
	}
	var gitea *Compiled
	for _, rec := range recs {
		if rec.ID == "gitea" {
			m, err := ParseManifest(rec.Manifest)
			if err != nil {
				t.Fatalf("ParseManifest: %v", err)
			}
			if gitea, err = Compile(m); err != nil {
				t.Fatalf("Compile: %v", err)
			}
		}
	}
	if gitea == nil {
		t.Fatal("gitea connector not found")
	}

	for _, want := range []string{"gitea_list_repos", "gitea_get_repo", "gitea_list_tree", "gitea_get_file"} {
		tool, ok := gitea.Tool(want)
		if !ok {
			t.Errorf("missing tool %s", want)
			continue
		}
		if tool.Def.Annotations == nil || tool.Def.Annotations.ReadOnlyHint == nil || !*tool.Def.Annotations.ReadOnlyHint {
			t.Errorf("tool %s is missing readOnlyHint", want)
		}
		if tool.Def.Request.Method != "GET" {
			t.Errorf("tool %s uses %s, want GET", want, tool.Def.Request.Method)
		}
	}

	// owner, repo and ref are all optional: the default behaviour is to
	// discover and index every repository the configured token can see (see
	// gitea_list_repos), not one fixed repository — they exist only as an
	// advanced override to index a single one instead.
	for _, name := range []string{"owner", "repo", "ref"} {
		var found *Variable
		for _, v := range gitea.Variables() {
			if v.Name == name {
				v := v
				found = &v
			}
		}
		if found == nil {
			t.Errorf("missing variable %s", name)
			continue
		}
		if found.Required {
			t.Errorf("variable %s must be optional", name)
		}
	}

	// The token secret must stay optional so a public repository works with
	// nothing configured, the same posture Zotero's own optional secret takes.
	for _, secret := range gitea.Secrets() {
		if secret.Required {
			t.Errorf("secret %s must be optional", secret.Name)
		}
	}

	// gitea_get_file's path must never interpolate a value that can contain
	// a slash into a single path segment: PathEscape would turn "a/b" into
	// "a%2Fb", which upstream will not treat as two segments. sha (a git
	// blob hash) can't contain a slash; this pins that choice against a
	// future edit accidentally reintroducing a path-shaped input.
	getFile, ok := gitea.Tool("gitea_get_file")
	if !ok {
		t.Fatal("gitea_get_file tool not found")
	}
	if _, hasPath := getFile.Def.InputSchema["properties"].(map[string]any)["path"]; hasPath {
		t.Error(`gitea_get_file must not take a "path" input — see the blob-SHA-vs-path-escaping comment above`)
	}
	if _, hasSHA := getFile.Def.InputSchema["properties"].(map[string]any)["sha"]; !hasSHA {
		t.Error(`gitea_get_file must take a "sha" input`)
	}
}

func TestLinkdingConnectorShape(t *testing.T) {
	recs, err := Builtins()
	if err != nil {
		t.Fatalf("Builtins: %v", err)
	}
	var linkding *Compiled
	for _, rec := range recs {
		if rec.ID == "linkding" {
			m, err := ParseManifest(rec.Manifest)
			if err != nil {
				t.Fatalf("ParseManifest: %v", err)
			}
			if linkding, err = Compile(m); err != nil {
				t.Fatalf("Compile: %v", err)
			}
		}
	}
	if linkding == nil {
		t.Fatal("linkding connector not found")
	}

	for _, want := range []string{
		"linkding_list_bookmarks", "linkding_list_archived_bookmarks",
		"linkding_get_bookmark", "linkding_list_assets", "linkding_get_asset_content",
	} {
		tool, ok := linkding.Tool(want)
		if !ok {
			t.Errorf("missing tool %s", want)
			continue
		}
		if tool.Def.Annotations == nil || tool.Def.Annotations.ReadOnlyHint == nil || !*tool.Def.Annotations.ReadOnlyHint {
			t.Errorf("tool %s is missing readOnlyHint", want)
		}
		if tool.Def.Request.Method != "GET" {
			t.Errorf("tool %s uses %s, want GET", want, tool.Def.Request.Method)
		}
	}

	// Unlike Zotero's and Gitea's optional credential, every Linkding API
	// call needs a token — there is no anonymous mode to fall back to.
	for _, secret := range linkding.Secrets() {
		if !secret.Required {
			t.Errorf("secret %s must be required: Linkding has no unauthenticated API mode", secret.Name)
		}
	}
}

func TestParseManifestRejectsUnknownFields(t *testing.T) {
	src := strings.Replace(minimalManifest, "  version: 1.0.0", "  version: 1.0.0\n  typoedField: oops", 1)
	if _, err := ParseManifest([]byte(src)); err == nil {
		t.Fatal("unknown field was silently ignored")
	}
}

func TestParseManifestRejectsMultipleDocuments(t *testing.T) {
	if _, err := ParseManifest([]byte(minimalManifest + "\n---\n" + minimalManifest)); err == nil {
		t.Fatal("multi-document manifest accepted")
	}
}

func TestParseManifestRejectsEmptyAndOversized(t *testing.T) {
	if _, err := ParseManifest(nil); err == nil {
		t.Fatal("empty manifest accepted")
	}
	if _, err := ParseManifest([]byte(strings.Repeat("x", maxManifestBytes+1))); err == nil {
		t.Fatal("oversized manifest accepted")
	}
}

func TestValidationCatchesManifestMistakes(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(string) string
		wantSub string
	}{
		{"wrong apiVersion", func(s string) string {
			return strings.Replace(s, "apiVersion: mcpaw.dev/v1", "apiVersion: v2", 1)
		}, "apiVersion"},
		{"undeclared variable", func(s string) string {
			return strings.Replace(s, "{{vars.accountId}}", "{{vars.nope}}", 1)
		}, "undeclared variable"},
		{"undeclared secret", func(s string) string {
			return strings.Replace(s, "{{secrets.apiKey}}", "{{secrets.other}}", 1)
		}, "undeclared secret"},
		{"input not in schema", func(s string) string {
			return strings.Replace(s, "{{input.thingId}}", "{{input.thingID}}", 1)
		}, "not a property of inputSchema"},
		{"relative path", func(s string) string {
			return strings.Replace(s, "path: /accounts", "path: accounts", 1)
		}, "must start with /"},
		{"absolute url as path", func(s string) string {
			return strings.Replace(s, "path: /accounts", "path: /x\n        # \n", 1)
		}, ""},
		{"bad method", func(s string) string {
			return strings.Replace(s, "method: GET", "method: TRACE", 1)
		}, "not permitted"},
		{"non-object input schema", func(s string) string {
			return strings.Replace(s, "        type: object\n        additionalProperties: false", "        type: array", 1)
		}, "must be \"object\""},
		{"bad tool name", func(s string) string {
			return strings.Replace(s, "name: get_thing", "name: 9invalid!", 1)
		}, "must match"},
		{"missing description", func(s string) string {
			return strings.Replace(s, "      description: Fetch a thing.", "      description: \"\"", 1)
		}, "description must not be empty"},
		{"bad base url", func(s string) string {
			return strings.Replace(s, "default: https://api.example.com", "default: ftp://api.example.com", 1)
		}, "scheme must be http or https"},
		{"credentials in base url", func(s string) string {
			return strings.Replace(s, "default: https://api.example.com", "default: https://user:pw@api.example.com", 1)
		}, "must not embed credentials"},
		{"default violates own pattern", func(s string) string {
			return strings.Replace(s, `      default: "42"`, `      default: "abc"`, 1)
		}, "does not match its own pattern"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := compileString(t, tc.mutate(minimalManifest))
			if err == nil {
				t.Fatal("expected a validation error")
			}
			if tc.wantSub != "" && !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("error %q does not mention %q", err, tc.wantSub)
			}
		})
	}
}

func TestValidationReportsEveryProblemAtOnce(t *testing.T) {
	src := strings.Replace(minimalManifest, "{{vars.accountId}}", "{{vars.nope}}", 1)
	src = strings.Replace(src, "method: GET", "method: TRACE", 1)
	_, err := compileString(t, src)
	var ve *ValidationError
	if err == nil {
		t.Fatal("expected an error")
	}
	if !asValidationError(err, &ve) || len(ve.Problems) < 2 {
		t.Fatalf("expected several problems, got %v", err)
	}
}

func asValidationError(err error, target **ValidationError) bool {
	ve, ok := err.(*ValidationError)
	if ok {
		*target = ve
	}
	return ok
}

// A manifest must not be able to set headers the transport owns; smuggling a
// Host or Transfer-Encoding header is a request-splitting primitive.
func TestForbiddenHeadersAreRejected(t *testing.T) {
	src := strings.Replace(minimalManifest,
		"        query:\n          limit: \"{{input.limit|default:10}}\"",
		"        headers:\n          Host: evil.example.com",
		1)
	_, err := compileString(t, src)
	if err == nil || !strings.Contains(err.Error(), "controlled by the platform") {
		t.Fatalf("got %v, want a forbidden-header error", err)
	}
}

// A schema that points at a remote document would make the import step perform
// an outbound fetch to an attacker-chosen URL.
func TestRemoteSchemaReferencesAreRefused(t *testing.T) {
	src := strings.Replace(minimalManifest,
		"          thingId: {type: string}",
		"          thingId: {$ref: 'https://evil.example.com/schema.json'}",
		1)
	_, err := compileString(t, src)
	if err == nil || !strings.Contains(err.Error(), "not permitted") {
		t.Fatalf("got %v, want a refusal to load a remote schema", err)
	}
}

func TestBodyValidation(t *testing.T) {
	withBody := func(body string) string {
		s := strings.Replace(minimalManifest, "method: GET", "method: POST", 1)
		return strings.Replace(s,
			"        query:\n          limit: \"{{input.limit|default:10}}\"",
			body, 1)
	}
	// Exactly one of json/from/text.
	if _, err := compileString(t, withBody("        body:\n          json: {a: \"1\"}\n          text: \"x\"")); err == nil {
		t.Fatal("ambiguous body accepted")
	}
	// An explicitly empty object is a legitimate body for APIs that require
	// one, and is distinguishable from an omitted body.
	if _, err := compileString(t, withBody("        body:\n          json: {}")); err != nil {
		t.Fatalf("explicit empty json body rejected: %v", err)
	}
	if _, err := compileString(t, withBody("        body:\n          contentType: application/json")); err == nil {
		t.Fatal("body with no content accepted")
	}
	// A body on GET is a manifest bug.
	src := strings.Replace(minimalManifest,
		"        query:\n          limit: \"{{input.limit|default:10}}\"",
		"        body:\n          text: \"hello\"", 1)
	if _, err := compileString(t, src); err == nil {
		t.Fatal("body on a GET request accepted")
	}
}

func TestVariableValidation(t *testing.T) {
	c := mustCompile(t, minimalManifest)

	if err := c.ValidateVariables(map[string]string{"accountId": "123"}); err != nil {
		t.Fatalf("valid variables rejected: %v", err)
	}
	if err := c.ValidateVariables(map[string]string{"accountId": "not-a-number"}); err == nil {
		t.Fatal("pattern violation accepted")
	}
	if err := c.ValidateVariables(map[string]string{"unknown": "x"}); err == nil {
		t.Fatal("unknown variable accepted")
	}

	resolved := c.ResolveVariables(nil)
	if resolved["accountId"] != "42" {
		t.Fatalf("default not applied: %v", resolved)
	}
	resolved = c.ResolveVariables(map[string]string{"accountId": "7"})
	if resolved["accountId"] != "7" {
		t.Fatalf("override not applied: %v", resolved)
	}
}

func TestRequiredVariableEnforcement(t *testing.T) {
	src := strings.Replace(minimalManifest,
		"    - name: accountId\n      default: \"42\"\n      pattern: '^[0-9]+$'",
		"    - name: accountId\n      required: true\n      pattern: '^[0-9]+$'", 1)
	c := mustCompile(t, src)
	if err := c.ValidateVariables(nil); err == nil {
		t.Fatal("missing required variable accepted")
	}
	if err := c.ValidateVariables(map[string]string{"accountId": "9"}); err != nil {
		t.Fatalf("supplied required variable rejected: %v", err)
	}
}

func TestSecretValidation(t *testing.T) {
	c := mustCompile(t, minimalManifest)
	if err := c.ValidateSecretNames([]string{"apiKey"}); err != nil {
		t.Fatalf("declared secret rejected: %v", err)
	}
	if err := c.ValidateSecretNames([]string{"apiKey", "sneaky"}); err == nil {
		t.Fatal("undeclared secret accepted")
	}
	if missing := c.MissingRequiredSecrets(nil); len(missing) != 1 || missing[0] != "apiKey" {
		t.Fatalf("MissingRequiredSecrets = %v", missing)
	}
	if missing := c.MissingRequiredSecrets(map[string][]byte{"apiKey": {1}}); len(missing) != 0 {
		t.Fatalf("MissingRequiredSecrets = %v, want none", missing)
	}
}

func TestValidateBaseURL(t *testing.T) {
	good := []string{"http://localhost:23119", "https://api.zotero.org", "http://host.docker.internal:23119/base"}
	for _, u := range good {
		if err := ValidateBaseURL(u); err != nil {
			t.Errorf("ValidateBaseURL(%q) = %v", u, err)
		}
	}
	bad := []string{"", "file:///etc/passwd", "https://", "https://x/?a=b", "https://x#frag", "not a url"}
	for _, u := range bad {
		if err := ValidateBaseURL(u); err == nil {
			t.Errorf("ValidateBaseURL(%q) accepted", u)
		}
	}
}

func TestRegistryPutGetList(t *testing.T) {
	r := NewRegistry()
	recs, err := Builtins()
	if err != nil {
		t.Fatalf("Builtins: %v", err)
	}
	for _, rec := range recs {
		if _, err := r.Put(rec); err != nil {
			t.Fatalf("Put(%s): %v", rec.ID, err)
		}
	}
	if r.Len() != len(recs) {
		t.Fatalf("Len = %d, want %d", r.Len(), len(recs))
	}
	if _, ok := r.Get("zotero-local"); !ok {
		t.Fatal("Get(zotero-local) failed")
	}
	if got := r.List(); len(got) != len(recs) {
		t.Fatalf("List returned %d entries", len(got))
	}
	r.Remove("zotero-local")
	if _, ok := r.Get("zotero-local"); ok {
		t.Fatal("Remove did not evict the entry")
	}
}

// The record ID is what the URL and every foreign key use; a manifest stored
// under a different ID than it declares would be served under a false name.
func TestRegistryRejectsIDMismatch(t *testing.T) {
	recs, _ := Builtins()
	rec := *recs[0]
	rec.ID = "something-else"
	if _, err := NewRegistry().Put(&rec); err == nil {
		t.Fatal("record/manifest id mismatch accepted")
	}
}

func TestManifestMarshalRoundTrip(t *testing.T) {
	m, err := ParseManifest([]byte(minimalManifest))
	if err != nil {
		t.Fatalf("ParseManifest: %v", err)
	}
	out, err := m.Marshal()
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	again, err := ParseManifest(out)
	if err != nil {
		t.Fatalf("re-parsing a marshalled manifest: %v", err)
	}
	if again.Metadata.ID != m.Metadata.ID || len(again.Spec.Tools) != len(m.Spec.Tools) {
		t.Fatal("manifest did not survive a marshal/parse round trip")
	}
	if _, err := Compile(again); err != nil {
		t.Fatalf("round-tripped manifest no longer compiles: %v", err)
	}
}

// pagingExemptTools lists tools that intentionally take no paging argument,
// each for a reason spelled out in docs/ARCHITECTURE.md § "Every tool must
// page": a single-item lookup, a small fixed-shape response, or a raw
// whole-content tool a source crawler depends on verbatim (a paginated
// companion tool exists alongside each of those instead).
var pagingExemptTools = map[string]string{
	"gitea_get_repo":               "single-item lookup",
	"gitea_get_file":               "crawler-shared raw blob; see gitea_read_file",
	"zotero_get_item":              "single-item lookup",
	"zotero_get_item_fulltext":     "crawler-shared raw text; see zotero_read_item_fulltext",
	"zotero_get_item_bibliography": "one small, fixed-shape citation",
	"linkding_get_bookmark":        "single-item lookup",
	"linkding_get_asset_content":   "crawler-shared raw content; see linkding_read_asset_content",
}

// nativePagingParamPairs are the argument names this codebase recognises as
// "the upstream paginates this for us" — every builtin list tool forwards
// one of these pairs to the upstream API. App-level pagination
// (CompiledTool.Paginate) is checked separately, since those tools also
// declare offset/limit in their schema but the engine — not a forwarded
// query parameter — is what does the paging.
var nativePagingParamPairs = [][2]string{
	{"limit", "offset"},
	{"limit", "start"},
	{"page", "per_page"},
	{"page", "limit"},
}

// TestEveryToolSupportsPaging enforces the architecture principle in
// docs/ARCHITECTURE.md § "Every tool must page": any tool not explicitly
// exempted must either set response.paginate (the engine pages a large text
// blob itself) or declare one of the recognised native-pagination argument
// pairs, so a future tool added without paging fails here instead of
// shipping silently unbounded.
func TestEveryToolSupportsPaging(t *testing.T) {
	recs, err := Builtins()
	if err != nil {
		t.Fatalf("Builtins: %v", err)
	}
	for _, rec := range recs {
		m, err := ParseManifest(rec.Manifest)
		if err != nil {
			t.Fatalf("ParseManifest(%s): %v", rec.ID, err)
		}
		c, err := Compile(m)
		if err != nil {
			t.Fatalf("Compile(%s): %v", rec.ID, err)
		}
		for _, tool := range c.Tools {
			name := tool.Name()
			if _, exempt := pagingExemptTools[name]; exempt {
				continue
			}
			if tool.Paginate {
				continue
			}
			props, _ := tool.Def.InputSchema["properties"].(map[string]any)
			paged := false
			for _, pair := range nativePagingParamPairs {
				_, hasA := props[pair[0]]
				_, hasB := props[pair[1]]
				if hasA && hasB {
					paged = true
					break
				}
			}
			if !paged {
				t.Errorf("%s: no paging support (neither response.paginate nor a recognised "+
					"limit/offset, limit/start or page/per_page argument pair) — either add one "+
					"or add it to pagingExemptTools with a reason", name)
			}
		}
	}
}
