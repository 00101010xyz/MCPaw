package gitea

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/00101010xyz/mcpaw/internal/connector"
	"github.com/00101010xyz/mcpaw/internal/engine"
	"github.com/00101010xyz/mcpaw/internal/index"
	"github.com/00101010xyz/mcpaw/internal/index/source"
	"github.com/00101010xyz/mcpaw/internal/upstream"
)

// compiledGitea compiles the real, shipped Gitea manifest, so this test
// exercises the crawler against the actual tool names and request
// templates an operator's instance would use — not a stand-in that could
// drift from the real manifest unnoticed.
func compiledGitea(t *testing.T) *connector.Compiled {
	t.Helper()
	records, err := connector.Builtins()
	if err != nil {
		t.Fatalf("connector.Builtins: %v", err)
	}
	for _, rec := range records {
		if rec.ID != connectorID {
			continue
		}
		manifest, err := connector.ParseManifest(rec.Manifest)
		if err != nil {
			t.Fatalf("ParseManifest: %v", err)
		}
		compiled, err := connector.Compile(manifest)
		if err != nil {
			t.Fatalf("Compile: %v", err)
		}
		return compiled
	}
	t.Fatalf("no built-in connector %q found", connectorID)
	return nil
}

func runtimeFor(t *testing.T, srv *httptest.Server) source.Runtime {
	t.Helper()
	compiled := compiledGitea(t)
	base, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parsing test server URL: %v", err)
	}
	enabled := map[string]bool{}
	for _, name := range requiredTools {
		enabled[name] = true
	}
	return source.Runtime{
		Executor: engine.New(engine.Config{}),
		Target: &engine.Target{
			InstanceID: "test-instance", Slug: "gitea", BaseURL: base,
			Vars:             map[string]string{"owner": "octocat", "repo": "thesis", "ref": "main"},
			Policy:           upstream.EgressPolicy{AllowPrivateNetworks: true},
			Timeout:          5 * time.Second,
			MaxResponseBytes: 1 << 20,
			RateLimitPerMin:  120,
			MaxConcurrent:    4,
		},
		Connector: compiled, EnabledTools: enabled,
	}
}

func jsonHandler(t *testing.T, routes map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for path, body := range routes {
		body := body
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, body)
		})
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

type emitted struct {
	Doc  source.Document
	Text string
}

func collect(t *testing.T, rt source.Runtime) []emitted {
	t.Helper()
	var got []emitted
	_, err := Crawler{}.Crawl(context.Background(), rt, func(_ context.Context, doc source.Document, text string) error {
		got = append(got, emitted{doc, text})
		return nil
	})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	return got
}

func blobBody(content string) string {
	return fmt.Sprintf(`{"content":%q,"encoding":"base64"}`, base64.StdEncoding.EncodeToString([]byte(content)))
}

func TestCrawlFiltersByExtension(t *testing.T) {
	srv := jsonHandler(t, map[string]string{
		"/api/v1/repos/octocat/thesis/git/trees/main": `{"tree":[
			{"path":"README.md","type":"blob","sha":"abc11111"},
			{"path":"chapters/intro.typ","type":"blob","sha":"abc22222"},
			{"path":"image.png","type":"blob","sha":"abc33333"},
			{"path":"docs","type":"tree","sha":"abc44444"},
			{"path":"noext","type":"blob","sha":"abc55555"}
		],"truncated":false}`,
		"/api/v1/repos/octocat/thesis/git/blobs/abc11111": blobBody("# Chapter\n\nText."),
		"/api/v1/repos/octocat/thesis/git/blobs/abc22222": blobBody("= Chapter\n\nText."),
	})

	got := collect(t, runtimeFor(t, srv))
	if len(got) != 2 {
		t.Fatalf("got %d emits, want 2 (only README.md and chapters/intro.typ): %+v", len(got), got)
	}
	byPath := map[string]emitted{}
	for _, g := range got {
		byPath[g.Doc.AttachmentKey] = g
	}
	if _, ok := byPath["image.png"]; ok {
		t.Error("image.png must not be fetched: it has no recognised extension")
	}
	if _, ok := byPath["docs"]; ok {
		t.Error("a tree entry of type \"tree\" (directory) must never be emitted as a document")
	}
	if _, ok := byPath["noext"]; ok {
		t.Error("a file with no extension must not be fetched")
	}
	if byPath["README.md"].Doc.HeadingDialect != index.DialectMarkdown {
		t.Errorf("README.md dialect = %q, want markdown", byPath["README.md"].Doc.HeadingDialect)
	}
	if byPath["chapters/intro.typ"].Doc.HeadingDialect != index.DialectTypst {
		t.Errorf("chapters/intro.typ dialect = %q, want typst", byPath["chapters/intro.typ"].Doc.HeadingDialect)
	}
	if byPath["README.md"].Text != "# Chapter\n\nText." {
		t.Errorf("README.md text = %q", byPath["README.md"].Text)
	}
	if byPath["chapters/intro.typ"].Doc.ItemKey != "chapters/intro.typ" {
		t.Errorf("ItemKey = %q, want the full path for this flat source", byPath["chapters/intro.typ"].Doc.ItemKey)
	}
}

// Gitea wraps long base64 payloads across multiple lines; the crawler must
// tolerate that rather than failing to decode.
func TestCrawlDecodesMultilineBase64(t *testing.T) {
	content := "# Title\n\n" + strings.Repeat("word ", 200)
	encoded := base64.StdEncoding.EncodeToString([]byte(content))
	wrapped := wrapBase64(encoded, 76)

	srv := jsonHandler(t, map[string]string{
		"/api/v1/repos/octocat/thesis/git/trees/main":     `{"tree":[{"path":"big.md","type":"blob","sha":"abc99999"}],"truncated":false}`,
		"/api/v1/repos/octocat/thesis/git/blobs/abc99999": fmt.Sprintf(`{"content":%q,"encoding":"base64"}`, wrapped),
	})

	got := collect(t, runtimeFor(t, srv))
	if len(got) != 1 {
		t.Fatalf("got %d emits, want 1", len(got))
	}
	if got[0].Text != content {
		t.Errorf("decoded text does not match: got %d bytes, want %d", len(got[0].Text), len(content))
	}
}

func wrapBase64(s string, width int) string {
	var out strings.Builder
	for len(s) > width {
		out.WriteString(s[:width])
		out.WriteByte('\n')
		s = s[width:]
	}
	out.WriteString(s)
	return out.String()
}

// A fetch failure for one file must not abort the rest of the crawl.
func TestCrawlToleratesBlobFetchFailure(t *testing.T) {
	srv := jsonHandler(t, map[string]string{
		"/api/v1/repos/octocat/thesis/git/trees/main": `{"tree":[
			{"path":"broken.md","type":"blob","sha":"abc66666"},
			{"path":"ok.md","type":"blob","sha":"abc77777"}
		],"truncated":false}`,
		"/api/v1/repos/octocat/thesis/git/blobs/abc77777": blobBody("# OK\n\nFine."),
		// abc66666 deliberately not registered -> 404.
	})

	got := collect(t, runtimeFor(t, srv))
	if len(got) != 2 {
		t.Fatalf("got %d emits, want 2 (one empty-text visit, one real document): %+v", len(got), got)
	}
	byPath := map[string]emitted{}
	for _, g := range got {
		byPath[g.Doc.AttachmentKey] = g
	}
	if byPath["broken.md"].Text != "" {
		t.Errorf("broken.md text = %q, want empty on fetch failure", byPath["broken.md"].Text)
	}
	if byPath["ok.md"].Text != "# OK\n\nFine." {
		t.Errorf("ok.md text = %q", byPath["ok.md"].Text)
	}
}

// Malformed base64 must degrade to an empty-text emit, not a crawl abort.
func TestCrawlToleratesMalformedBase64(t *testing.T) {
	srv := jsonHandler(t, map[string]string{
		"/api/v1/repos/octocat/thesis/git/trees/main":     `{"tree":[{"path":"bad.md","type":"blob","sha":"abc88888"}],"truncated":false}`,
		"/api/v1/repos/octocat/thesis/git/blobs/abc88888": `{"content":"not valid base64!!!","encoding":"base64"}`,
	})

	got := collect(t, runtimeFor(t, srv))
	if len(got) != 1 {
		t.Fatalf("got %d emits, want 1", len(got))
	}
	if got[0].Text != "" {
		t.Errorf("text = %q, want empty on a decode failure", got[0].Text)
	}
}

// The indexer's pruning logic trusts "truncated" to mean "this run did not
// see the whole repository" — it must be true whenever the Gitea server
// itself truncated the tree listing, independent of our own maxFiles cap.
func TestCrawlReportsTruncatedFromServerFlag(t *testing.T) {
	srv := jsonHandler(t, map[string]string{
		"/api/v1/repos/octocat/thesis/git/trees/main":     `{"tree":[{"path":"a.md","type":"blob","sha":"abc11111"}],"truncated":true}`,
		"/api/v1/repos/octocat/thesis/git/blobs/abc11111": blobBody("# A\n\nBody."),
	})

	truncated, err := Crawler{}.Crawl(context.Background(), runtimeFor(t, srv), func(context.Context, source.Document, string) error { return nil })
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if !truncated {
		t.Error("truncated = false, want true: the server's own truncated flag was set")
	}
}

func TestCrawlReportsTruncatedWhenMaxFilesHit(t *testing.T) {
	old := maxFiles
	maxFiles = 2
	t.Cleanup(func() { maxFiles = old })

	tree := `{"tree":[
		{"path":"a.md","type":"blob","sha":"aaaaaaaa"},
		{"path":"b.md","type":"blob","sha":"bbbbbbbb"},
		{"path":"c.md","type":"blob","sha":"cccccccc"}
	],"truncated":false}`
	srv := jsonHandler(t, map[string]string{
		"/api/v1/repos/octocat/thesis/git/trees/main":     tree,
		"/api/v1/repos/octocat/thesis/git/blobs/aaaaaaaa": blobBody("# A\n\nBody."),
		"/api/v1/repos/octocat/thesis/git/blobs/bbbbbbbb": blobBody("# B\n\nBody."),
	})

	var emitCount int
	truncated, err := Crawler{}.Crawl(context.Background(), runtimeFor(t, srv), func(context.Context, source.Document, string) error {
		emitCount++
		return nil
	})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if !truncated {
		t.Error("truncated = false, want true: maxFiles was lowered to 2 against a 3-file tree")
	}
	if emitCount != 2 {
		t.Errorf("emitCount = %d, want 2 (maxFiles)", emitCount)
	}
}

func TestRequiredToolsMatchesTheToolsActuallyCalled(t *testing.T) {
	want := map[string]bool{"gitea_list_tree": true, "gitea_get_file": true}
	got := Crawler{}.RequiredTools()
	if len(got) != len(want) {
		t.Fatalf("RequiredTools = %v, want exactly %v", got, want)
	}
	for _, name := range got {
		if !want[name] {
			t.Errorf("RequiredTools names %q, which Crawl never calls", name)
		}
	}
}
