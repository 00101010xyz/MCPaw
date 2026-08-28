package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/00101010xyz/mcpaw/internal/domain"
)

// fakeZoteroServer serves the three endpoints the indexer crawls: one
// top-level item with one PDF attachment carrying extracted text.
func fakeZoteroServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/users/0/items/top", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"key":"ITEM0001","data":{"itemType":"journalArticle","title":"A Paper"}}]`)
	})
	mux.HandleFunc("/api/users/0/items/ITEM0001/children", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"key":"ATT00001","data":{"itemType":"attachment","contentType":"application/pdf"}}]`)
	})
	mux.HandleFunc("/api/users/0/items/ATT00001/fulltext", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"content":"the quick brown fox jumps over the lazy dog. `+
			strings.Repeat("more filler text to make a real chunk. ", 20)+`"}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// fakeEmbedderServer serves the Ollama-compatible /api/embed contract,
// returning one small deterministic vector per input text.
func fakeEmbedderServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		embeddings := make([][]float32, len(req.Input))
		for i, text := range req.Input {
			// A cheap, deterministic "embedding": length and a hash-ish value,
			// good enough to give distinct vectors without a real model.
			embeddings[i] = []float32{float32(len(text)), float32(i + 1)}
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"embeddings": embeddings})
	}))
	t.Cleanup(srv.Close)
	return srv
}

// createIndexableZoteroInstance creates an instance pointed at the fake
// Zotero server, with the fake embedder configured and every tool the
// indexer needs enabled — the common setup for the Reindex tests below.
func createIndexableZoteroInstance(t *testing.T, env *testEnv, zoteroURL, embedderURL string) *domain.Instance {
	t.Helper()
	ctx := context.Background()
	in := zoteroCreateInput("indexable")
	in.BaseURL = zoteroURL
	in.EmbedderURL = embedderURL
	inst, err := env.Instances.Create(ctx, systemActor(), in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, tool := range []string{"zotero_list_top_items", "zotero_get_item_children", "zotero_get_item_fulltext"} {
		if err := env.Instances.SetToolEnabled(ctx, systemActor(), inst.ID, tool, true); err != nil {
			t.Fatalf("SetToolEnabled(%s): %v", tool, err)
		}
	}
	return inst
}

func waitForIndexDone(t *testing.T, env *testEnv, instanceID string, timeout time.Duration) IndexStatus {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		st, _, err := env.Indexer.Status(context.Background(), instanceID)
		if err != nil {
			t.Fatalf("Status: %v", err)
		}
		if !st.Running && (st.FinishedAt != (time.Time{}) || st.LastError != "") {
			return st
		}
		if time.Now().After(deadline) {
			t.Fatalf("reindex did not finish within %v (status: %+v)", timeout, st)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestIndexerReindexEndToEnd(t *testing.T) {
	env := newTestEnv(t)
	zotero := fakeZoteroServer(t)
	embedder := fakeEmbedderServer(t)
	inst := createIndexableZoteroInstance(t, env, zotero.URL, embedder.URL)
	ctx := context.Background()

	if env.Indexer.Ready(ctx, inst.ID) {
		t.Error("Ready must be false before any reindex has run")
	}

	if err := env.Indexer.Reindex(ctx, systemActor(), inst.ID); err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	st := waitForIndexDone(t, env, inst.ID, 5*time.Second)

	if st.LastError != "" {
		t.Fatalf("reindex failed: %s", st.LastError)
	}
	if st.ItemsProcessed != 1 {
		t.Errorf("ItemsProcessed = %d, want 1", st.ItemsProcessed)
	}
	if st.ChunksIndexed == 0 {
		t.Error("ChunksIndexed = 0, want at least one chunk from the fake PDF text")
	}
	if !env.Indexer.Ready(ctx, inst.ID) {
		t.Error("Ready must be true once chunks exist")
	}

	hits, err := env.Indexer.Search(ctx, inst.ID, "quick brown fox", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("Search returned no hits for text known to be indexed")
	}
	if hits[0].ItemKey != "ITEM0001" || hits[0].AttachmentKey != "ATT00001" {
		t.Errorf("hit = %+v, want ItemKey=ITEM0001 AttachmentKey=ATT00001", hits[0])
	}
}

// A second Reindex call must not run concurrently with one still in flight —
// the crawl clears the index first, so two overlapping runs could interleave
// a clear from one with the inserts of another.
func TestIndexerReindexRejectsConcurrentRun(t *testing.T) {
	env := newTestEnv(t)
	zotero := fakeZoteroServer(t)
	embedder := fakeEmbedderServer(t)
	inst := createIndexableZoteroInstance(t, env, zotero.URL, embedder.URL)
	ctx := context.Background()

	if err := env.Indexer.Reindex(ctx, systemActor(), inst.ID); err != nil {
		t.Fatalf("first Reindex: %v", err)
	}
	err := env.Indexer.Reindex(ctx, systemActor(), inst.ID)
	waitForIndexDone(t, env, inst.ID, 5*time.Second)
	if err == nil {
		t.Error("a second Reindex while one is running must be rejected")
	}
}

// Supported() is keyed on the connector ID today; Phase 1 turns this into
// "does the connector declare an index: block" instead, but the contract —
// true for zotero-local, false for anything else — must hold either way.
func TestIndexerSupported(t *testing.T) {
	env := newTestEnv(t)
	if !env.Indexer.Supported("zotero-local") {
		t.Error("zotero-local must be supported")
	}
	if env.Indexer.Supported("some-other-connector") {
		t.Error("an unrelated connector ID must not be reported as supported")
	}
}

func TestIndexerReindexRejectsUnsupportedConnector(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()

	// Import a second, minimal connector that the indexer has no crawler for.
	manifest := []byte(`
apiVersion: mcpaw.dev/v1
kind: Connector
metadata:
  id: not-indexable
  name: Not Indexable
  version: 1.0.0
spec:
  baseUrl:
    default: https://example.invalid
  auth:
    type: none
  tools:
    - name: ping
      description: Ping the service.
      inputSchema: {type: object}
      request: {method: GET, path: /ping}
      response: {successCodes: [200], format: json}
`)
	if _, err := env.Connector.ImportManifest(ctx, systemActor(), manifest, domain.SourceManifest); err != nil {
		t.Fatalf("ImportManifest: %v", err)
	}
	inst, err := env.Instances.Create(ctx, systemActor(), CreateInput{
		Name: "Not Indexable", Slug: "not-indexable", ConnectorID: "not-indexable",
		Enabled: true,
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	err = env.Indexer.Reindex(ctx, systemActor(), inst.ID)
	if !errors.Is(err, domain.ErrInvalidInput) {
		t.Errorf("Reindex on an unsupported connector: error = %v, want ErrInvalidInput", err)
	}
}

func TestIndexerReindexFailsWithoutEmbedderConfigured(t *testing.T) {
	env := newTestEnv(t)
	zotero := fakeZoteroServer(t)
	ctx := context.Background()

	in := zoteroCreateInput("no-embedder")
	in.BaseURL = zotero.URL
	inst, err := env.Instances.Create(ctx, systemActor(), in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, tool := range []string{"zotero_list_top_items", "zotero_get_item_children", "zotero_get_item_fulltext"} {
		if err := env.Instances.SetToolEnabled(ctx, systemActor(), inst.ID, tool, true); err != nil {
			t.Fatalf("SetToolEnabled(%s): %v", tool, err)
		}
	}

	if err := env.Indexer.Reindex(ctx, systemActor(), inst.ID); err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	st := waitForIndexDone(t, env, inst.ID, 5*time.Second)
	if st.LastError == "" {
		t.Error("reindex without an embedder URL configured must fail, not silently produce an empty index")
	}
	if env.Indexer.Ready(ctx, inst.ID) {
		t.Error("an instance with no embedder configured must never report Ready")
	}
}

// Indexing must respect the same per-tool enable/disable switch an operator
// uses for MCP clients — it must not reach upstream behind their back for a
// tool they turned off.
func TestIndexerReindexRequiresEnabledTools(t *testing.T) {
	env := newTestEnv(t)
	zotero := fakeZoteroServer(t)
	embedder := fakeEmbedderServer(t)
	ctx := context.Background()

	in := zoteroCreateInput("tools-disabled")
	in.BaseURL = zotero.URL
	in.EmbedderURL = embedder.URL
	inst, err := env.Instances.Create(ctx, systemActor(), in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	// Every tool is enabled by default; explicitly disable the one the
	// crawl needs first, so indexing must not reach upstream for it anyway.
	if err := env.Instances.SetToolEnabled(ctx, systemActor(), inst.ID, "zotero_list_top_items", false); err != nil {
		t.Fatalf("SetToolEnabled: %v", err)
	}

	if err := env.Indexer.Reindex(ctx, systemActor(), inst.ID); err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	st := waitForIndexDone(t, env, inst.ID, 5*time.Second)
	if st.LastError == "" {
		t.Error("reindex must fail when a required crawl tool is not enabled on this instance")
	}
}

func TestIndexerSearchRejectsEmptyQuery(t *testing.T) {
	env := newTestEnv(t)
	ctx := context.Background()
	inst, err := env.Instances.Create(ctx, systemActor(), zoteroCreateInput("search-empty-query"))
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = env.Indexer.Search(ctx, inst.ID, "   ", 5)
	if err == nil {
		t.Error("Search with a blank query must be rejected")
	}
}

func TestIndexerSearchOnEmptyIndexReturnsNoHitsNotError(t *testing.T) {
	env := newTestEnv(t)
	embedder := fakeEmbedderServer(t)
	ctx := context.Background()

	in := zoteroCreateInput("search-empty-index")
	in.EmbedderURL = embedder.URL
	inst, err := env.Instances.Create(ctx, systemActor(), in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	hits, err := env.Indexer.Search(ctx, inst.ID, "anything", 5)
	if err != nil {
		t.Fatalf("Search on an empty index: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("got %d hits from an empty index, want 0", len(hits))
	}
}
