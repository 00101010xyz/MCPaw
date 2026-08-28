package linkding

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/00101010xyz/mcpaw/internal/connector"
	"github.com/00101010xyz/mcpaw/internal/engine"
	"github.com/00101010xyz/mcpaw/internal/index/source"
	"github.com/00101010xyz/mcpaw/internal/upstream"
)

// compiledLinkding compiles the real, shipped Linkding manifest, so this
// test exercises the crawler against the actual tool names and request
// templates an operator's instance would use.
func compiledLinkding(t *testing.T) *connector.Compiled {
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
	compiled := compiledLinkding(t)
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
			InstanceID: "test-instance", Slug: "linkding", BaseURL: base,
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

func TestCrawlFiltersToCompleteSnapshots(t *testing.T) {
	srv := jsonHandler(t, map[string]string{
		"/api/bookmarks/": `{"count":1,"next":null,"previous":null,"results":[{"id":1}]}`,
		"/api/bookmarks/1/assets/": `{"count":4,"next":null,"previous":null,"results":[
			{"id":10,"asset_type":"snapshot","status":"complete","content_type":"text/html"},
			{"id":11,"asset_type":"snapshot","status":"pending","content_type":"text/html"},
			{"id":12,"asset_type":"upload","status":"complete","content_type":"image/png"},
			{"id":13,"asset_type":"snapshot","status":"failure","content_type":"text/html"}
		]}`,
		"/api/bookmarks/1/assets/10/download/": `<p>Snapshot text.</p>`,
	})

	got := collect(t, runtimeFor(t, srv))
	if len(got) != 1 {
		t.Fatalf("got %d emits, want 1 (only asset 10 is a complete snapshot): %+v", len(got), got)
	}
	if got[0].Doc.ItemKey != "1" || got[0].Doc.AttachmentKey != "10" {
		t.Errorf("doc = %+v, want ItemKey=1 AttachmentKey=10", got[0].Doc)
	}
	if got[0].Text != "Snapshot text." {
		t.Errorf("text = %q, want the stripped HTML", got[0].Text)
	}
}

func TestCrawlEmitsEmptyDocumentForBookmarkWithNoSnapshot(t *testing.T) {
	srv := jsonHandler(t, map[string]string{
		"/api/bookmarks/":          `{"count":1,"next":null,"previous":null,"results":[{"id":1}]}`,
		"/api/bookmarks/1/assets/": `{"count":0,"next":null,"previous":null,"results":[]}`,
	})

	got := collect(t, runtimeFor(t, srv))
	if len(got) != 1 {
		t.Fatalf("got %d emits, want 1 (the empty-text visit)", len(got))
	}
	if got[0].Text != "" {
		t.Errorf("text = %q, want empty", got[0].Text)
	}
	if got[0].Doc.ItemKey != "1" || got[0].Doc.AttachmentKey != "1" {
		t.Errorf("doc = %+v, want ItemKey=AttachmentKey=1", got[0].Doc)
	}
}

// A bookmark with more than one complete snapshot must produce one document
// per snapshot, not just the first — the ordering the API returns them in
// is not documented, so guessing which is "current" would be unreliable.
func TestCrawlEmitsOneDocumentPerCompleteSnapshot(t *testing.T) {
	srv := jsonHandler(t, map[string]string{
		"/api/bookmarks/": `{"count":1,"next":null,"previous":null,"results":[{"id":1}]}`,
		"/api/bookmarks/1/assets/": `{"count":2,"next":null,"previous":null,"results":[
			{"id":10,"asset_type":"snapshot","status":"complete"},
			{"id":20,"asset_type":"snapshot","status":"complete"}
		]}`,
		"/api/bookmarks/1/assets/10/download/": `<p>First snapshot.</p>`,
		"/api/bookmarks/1/assets/20/download/": `<p>Second snapshot.</p>`,
	})

	got := collect(t, runtimeFor(t, srv))
	if len(got) != 2 {
		t.Fatalf("got %d emits, want 2: %+v", len(got), got)
	}
	byAttachment := map[string]string{}
	for _, g := range got {
		byAttachment[g.Doc.AttachmentKey] = g.Text
	}
	if byAttachment["10"] != "First snapshot." || byAttachment["20"] != "Second snapshot." {
		t.Errorf("got %+v", byAttachment)
	}
}

// A children-listing failure for one bookmark must not abort the whole
// crawl — it degrades to one empty-text visit and moves on.
func TestCrawlToleratesAssetListingFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/bookmarks/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"count":2,"next":null,"previous":null,"results":[{"id":1},{"id":2}]}`)
	})
	// bookmark 1's assets endpoint explicitly 404s — a trailing-slash
	// pattern like "/api/bookmarks/" would otherwise silently absorb an
	// unregistered "/api/bookmarks/1/assets/" request instead of actually
	// simulating a failure there.
	mux.HandleFunc("/api/bookmarks/1/assets/", http.NotFound)
	mux.HandleFunc("/api/bookmarks/2/assets/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"count":1,"next":null,"previous":null,"results":[{"id":20,"asset_type":"snapshot","status":"complete"}]}`)
	})
	mux.HandleFunc("/api/bookmarks/2/assets/20/download/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `<p>OK.</p>`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	got := collect(t, runtimeFor(t, srv))
	if len(got) != 2 {
		t.Fatalf("got %d emits, want 2 (one empty visit for bookmark 1, one real document for bookmark 2): %+v", len(got), got)
	}
	byItem := map[string]string{}
	for _, g := range got {
		byItem[g.Doc.ItemKey] = g.Text
	}
	if byItem["1"] != "" {
		t.Errorf("bookmark 1 text = %q, want empty (its assets listing 404s)", byItem["1"])
	}
	if byItem["2"] != "OK." {
		t.Errorf("bookmark 2 text = %q, want OK.", byItem["2"])
	}
}

func TestCrawlToleratesSnapshotFetchFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/bookmarks/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"count":1,"next":null,"previous":null,"results":[{"id":1}]}`)
	})
	mux.HandleFunc("/api/bookmarks/1/assets/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"count":1,"next":null,"previous":null,"results":[{"id":10,"asset_type":"snapshot","status":"complete"}]}`)
	})
	// The download path is a more specific match than the assets-list route
	// above (net/http.ServeMux prefers the longest registered pattern), so
	// registering it explicitly to 404 is what actually simulates "the
	// download failed" rather than silently falling through to the
	// assets-list handler above, which a trailing-slash pattern would do
	// for an unregistered path.
	mux.HandleFunc("/api/bookmarks/1/assets/10/download/", http.NotFound)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	got := collect(t, runtimeFor(t, srv))
	if len(got) != 1 {
		t.Fatalf("got %d emits, want 1", len(got))
	}
	if got[0].Text != "" {
		t.Errorf("text = %q, want empty on a fetch failure", got[0].Text)
	}
}

func TestCrawlPaginatesBookmarks(t *testing.T) {
	old := pageSize
	pageSize = 2
	t.Cleanup(func() { pageSize = old })

	mux := http.NewServeMux()
	var seenOffsets []string
	mux.HandleFunc("/api/bookmarks/", func(w http.ResponseWriter, r *http.Request) {
		offset := r.URL.Query().Get("offset")
		seenOffsets = append(seenOffsets, offset)
		w.Header().Set("Content-Type", "application/json")
		switch offset {
		case "0":
			fmt.Fprint(w, `{"count":3,"next":null,"previous":null,"results":[{"id":1},{"id":2}]}`)
		case "2":
			fmt.Fprint(w, `{"count":3,"next":null,"previous":null,"results":[{"id":3}]}`)
		default:
			fmt.Fprint(w, `{"count":3,"next":null,"previous":null,"results":[]}`)
		}
	})
	mux.HandleFunc("/api/bookmarks/1/assets/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"count":0,"next":null,"previous":null,"results":[]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	// bookmarks 2 and 3's assets endpoints aren't registered; a 404 there
	// just means an empty-text visit, irrelevant to what this test checks.

	got := collect(t, runtimeFor(t, srv))
	if len(got) != 3 {
		t.Fatalf("got %d emits, want 3 (a full page of 2 plus one more): %+v", len(got), got)
	}
	if len(seenOffsets) < 2 {
		t.Fatal("only one page was fetched; pagination did not continue past a full page")
	}
}

func TestCrawlReportsTruncatedWhenMaxBookmarksHit(t *testing.T) {
	oldPageSize, oldMax := pageSize, maxBookmarks
	pageSize, maxBookmarks = 2, 4
	t.Cleanup(func() { pageSize, maxBookmarks = oldPageSize, oldMax })

	mux := http.NewServeMux()
	mux.HandleFunc("/api/bookmarks/", func(w http.ResponseWriter, r *http.Request) {
		offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		w.Header().Set("Content-Type", "application/json")
		// Always answer with a full page: an endless bookmark list, from
		// the crawler's point of view, so the only way the loop stops is
		// by hitting maxBookmarks. IDs are derived from offset so they stay
		// distinct and, importantly, valid JSON integers (no leading zero).
		fmt.Fprintf(w, `{"count":999,"next":"x","previous":null,"results":[{"id":%d},{"id":%d}]}`,
			1000+offset, 1001+offset)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	var emitCount int
	truncated, err := Crawler{}.Crawl(context.Background(), runtimeFor(t, srv),
		func(context.Context, source.Document, string) error { emitCount++; return nil })
	if err != nil {
		t.Fatalf("Crawl: %v", err)
	}
	if !truncated {
		t.Error("truncated = false, want true: maxBookmarks was hit against an endless page sequence")
	}
	if emitCount < maxBookmarks {
		t.Errorf("emitCount = %d, want at least maxBookmarks (%d)", emitCount, maxBookmarks)
	}
}

func TestRequiredToolsMatchesTheToolsActuallyCalled(t *testing.T) {
	want := map[string]bool{
		"linkding_list_bookmarks":    true,
		"linkding_list_assets":       true,
		"linkding_get_asset_content": true,
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
