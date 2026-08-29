package zotero

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/00101010xyz/mcpaw/internal/connector"
	"github.com/00101010xyz/mcpaw/internal/engine"
	"github.com/00101010xyz/mcpaw/internal/index/source"
	"github.com/00101010xyz/mcpaw/internal/upstream"
)

// compiledZotero compiles the real, shipped Zotero manifest, so this test
// exercises the crawler against the actual tool names and request templates
// an operator's instance would use — not a stand-in that could drift from
// the real manifest unnoticed.
func compiledZotero(t *testing.T) *connector.Compiled {
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
	compiled := compiledZotero(t)
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
			InstanceID: "test-instance", Slug: "zotero", BaseURL: base,
			Vars:    map[string]string{"userId": "0"},
			Policy:  upstream.EgressPolicy{AllowPrivateNetworks: true},
			Timeout: 5 * time.Second, MaxResponseBytes: 1 << 20,
			RateLimitPerMin: 120, MaxConcurrent: 4,
		},
		Connector: compiled, EnabledTools: enabled,
	}
}

// jsonHandler builds a mux serving one JSON body per exact path, so each
// test case only has to state which paths exist and what they return.
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

func collect(t *testing.T, rt source.Runtime) []struct {
	Doc  source.Document
	Text string
} {
	t.Helper()
	var got []struct {
		Doc  source.Document
		Text string
	}
	_, err := Crawler{}.Crawl(context.Background(), rt, func(_ context.Context, doc source.Document, text string) error {
		got = append(got, struct {
			Doc  source.Document
			Text string
		}{doc, text})
		return nil
	})
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	return got
}

func TestCrawlFiltersAttachmentTypesAndSkipsNonAttachments(t *testing.T) {
	srv := jsonHandler(t, map[string]string{
		"/api/users/0/items/top": `[{"key":"ITEM0001"}]`,
		"/api/users/0/items/ITEM0001/children": `[
			{"key":"NOTE0001","data":{"itemType":"note"}},
			{"key":"ATT00PDF","data":{"itemType":"attachment","contentType":"application/pdf"}},
			{"key":"ATT00SNP","data":{"itemType":"attachment","contentType":"text/html"}},
			{"key":"ATT00IMG","data":{"itemType":"attachment","contentType":"image/png"}},
			{"key":"","data":{"itemType":"attachment","contentType":"application/pdf"}}
		]`,
		"/api/users/0/items/ATT00PDF/fulltext": `{"content":"pdf text"}`,
		"/api/users/0/items/ATT00SNP/fulltext": `{"content":"snapshot text"}`,
	})

	got := collect(t, runtimeFor(t, srv))

	// Exactly the PDF and the HTML snapshot should be emitted: the note
	// (wrong itemType), the image (wrong contentType) and the attachment
	// with no key must all be excluded.
	byKey := map[string]string{}
	for _, g := range got {
		byKey[g.Doc.AttachmentKey] = g.Text
	}
	if len(got) != 2 {
		t.Fatalf("got %d emits, want 2 (pdf + snapshot): %+v", len(got), got)
	}
	if byKey["ATT00PDF"] != "pdf text" {
		t.Errorf("ATT00PDF text = %q", byKey["ATT00PDF"])
	}
	if byKey["ATT00SNP"] != "snapshot text" {
		t.Errorf("ATT00SNP text = %q", byKey["ATT00SNP"])
	}
	for _, g := range got {
		if g.Doc.ItemKey != "ITEM0001" {
			t.Errorf("ItemKey = %q, want ITEM0001", g.Doc.ItemKey)
		}
	}
}

func TestCrawlEmitsEmptyDocumentForItemWithNoIndexableAttachment(t *testing.T) {
	srv := jsonHandler(t, map[string]string{
		"/api/users/0/items/top":               `[{"key":"ITEM0001"}]`,
		"/api/users/0/items/ITEM0001/children": `[{"key":"NOTE0001","data":{"itemType":"note"}}]`,
	})

	got := collect(t, runtimeFor(t, srv))
	if len(got) != 1 {
		t.Fatalf("got %d emits, want 1 (the empty-text visit)", len(got))
	}
	if got[0].Text != "" {
		t.Errorf("text = %q, want empty", got[0].Text)
	}
	if got[0].Doc.ItemKey != "ITEM0001" || got[0].Doc.AttachmentKey != "ITEM0001" {
		t.Errorf("doc = %+v, want ItemKey=AttachmentKey=ITEM0001", got[0].Doc)
	}
}

// A children-listing failure for one item (Zotero 404ing, say) must not
// abort the whole crawl — it degrades to one empty-text visit and moves on.
func TestCrawlToleratesChildrenFetchFailure(t *testing.T) {
	srv := jsonHandler(t, map[string]string{
		"/api/users/0/items/top": `[{"key":"ITEM0001"},{"key":"ITEM0002"}]`,
		"/api/users/0/items/ITEM0002/children": `[
			{"key":"ATT00001","data":{"itemType":"attachment","contentType":"application/pdf"}}
		]`,
		"/api/users/0/items/ATT00001/fulltext": `{"content":"second item's text"}`,
		// ITEM0001/children is deliberately not registered -> 404.
	})

	got := collect(t, runtimeFor(t, srv))
	if len(got) != 2 {
		t.Fatalf("got %d emits, want 2 (one empty visit for ITEM0001, one real document for ITEM0002): %+v", len(got), got)
	}
}

// A fulltext fetch failure (Zotero has not extracted text yet, most
// commonly a 404) degrades to an empty-text emit rather than aborting.
func TestCrawlToleratesFulltextFetchFailure(t *testing.T) {
	srv := jsonHandler(t, map[string]string{
		"/api/users/0/items/top": `[{"key":"ITEM0001"}]`,
		"/api/users/0/items/ITEM0001/children": `[
			{"key":"ATT00001","data":{"itemType":"attachment","contentType":"application/pdf"}}
		]`,
		// ATT00001/fulltext deliberately not registered -> 404.
	})

	got := collect(t, runtimeFor(t, srv))
	if len(got) != 1 {
		t.Fatalf("got %d emits, want 1", len(got))
	}
	if got[0].Text != "" {
		t.Errorf("text = %q, want empty on a fetch failure", got[0].Text)
	}
	if got[0].Doc.AttachmentKey != "ATT00001" {
		t.Errorf("AttachmentKey = %q, want ATT00001", got[0].Doc.AttachmentKey)
	}
}

// The crawl must page through results rather than stopping at the first
// page, and must stop once a short page signals there is nothing more.
func TestCrawlPaginatesTopItems(t *testing.T) {
	mux := http.NewServeMux()
	var seenStarts []string
	mux.HandleFunc("/api/users/0/items/top", func(w http.ResponseWriter, r *http.Request) {
		seenStarts = append(seenStarts, r.URL.Query().Get("start"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("start") {
		case "0":
			items := make([]string, pageSize)
			for i := range items {
				items[i] = fmt.Sprintf(`{"key":"ITEM%04d"}`, i)
			}
			fmt.Fprint(w, "["+joinJSON(items)+"]")
		case fmt.Sprint(pageSize):
			fmt.Fprint(w, `[{"key":"ITEMLAST"}]`)
		default:
			fmt.Fprint(w, `[]`)
		}
	})
	mux.HandleFunc("/api/users/0/items/ITEM0000/children", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[]`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	// children/fulltext for every item besides ITEM0000 aren't registered;
	// that's fine — a 404 there just means an empty-text visit, which is
	// exactly what this test is checking (that pagination itself works,
	// not the per-item crawl already covered above).
	rt := runtimeFor(t, srv)
	got := collect(t, rt)

	if len(got) != pageSize+1 {
		t.Fatalf("got %d emits, want %d (a full page plus one more)", len(got), pageSize+1)
	}
	if len(seenStarts) < 2 {
		t.Fatalf("only one page was fetched; pagination did not continue past a full page")
	}
}

func joinJSON(items []string) string {
	out := ""
	for i, s := range items {
		if i > 0 {
			out += ","
		}
		out += s
	}
	return out
}

// The indexer's pruning logic trusts "truncated" to mean "this run did not
// see the whole library" — it must be true whenever maxItems is hit while a
// full page is still coming back, since there may be more beyond it.
func TestCrawlReportsTruncatedWhenMaxItemsHit(t *testing.T) {
	oldPageSize, oldMaxItems := pageSize, maxItems
	pageSize, maxItems = 2, 4
	t.Cleanup(func() { pageSize, maxItems = oldPageSize, oldMaxItems })

	mux := http.NewServeMux()
	mux.HandleFunc("/api/users/0/items/top", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		start := r.URL.Query().Get("start")
		// Always answer with a full page: an endless library, from the
		// crawler's point of view, so the only way the loop stops is by
		// hitting maxItems.
		fmt.Fprintf(w, `[{"key":"ITEM%s0"},{"key":"ITEM%s1"}]`, start, start)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	// children/fulltext are deliberately unregistered; a 404 there just
	// means an empty-text visit, irrelevant to what this test checks.

	var emitCount int
	truncated, err := Crawler{}.Crawl(context.Background(), runtimeFor(t, srv),
		func(context.Context, source.Document, string) error { emitCount++; return nil })
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if !truncated {
		t.Error("truncated = false, want true: maxItems was hit against an endless page sequence")
	}
	if emitCount < maxItems {
		t.Errorf("emitCount = %d, want at least maxItems (%d)", emitCount, maxItems)
	}
}

func TestRequiredToolsMatchesTheToolsActuallyCalled(t *testing.T) {
	// This is a documentation-consistency check: RequiredTools is what the
	// indexer validates before starting a crawl, so it must name every tool
	// Crawl can actually call — no more, no less.
	want := map[string]bool{
		"zotero_list_top_items":    true,
		"zotero_get_item_children": true,
		"zotero_get_item_fulltext": true,
	}
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
