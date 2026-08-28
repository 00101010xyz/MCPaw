package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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

// fakeGiteaServer serves the two endpoints the Gitea indexer crawls: a tree
// listing with one markdown file, and that file's blob content.
func fakeGiteaServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/octocat/thesis/git/trees/main", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"tree":[{"path":"chapters/intro.md","type":"blob","sha":"abc12345"}],"truncated":false}`)
	})
	mux.HandleFunc("/api/v1/repos/octocat/thesis/git/blobs/abc12345", func(w http.ResponseWriter, r *http.Request) {
		content := "# Introduction\n\nthe quick brown fox jumps over the lazy dog. " +
			strings.Repeat("more filler text to make a real chunk. ", 20)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"content":%q,"encoding":"base64"}`, base64.StdEncoding.EncodeToString([]byte(content)))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// mutableGiteaServer is fakeGiteaServer's counterpart for tests that need
// the upstream repository's content to change between two Reindex calls —
// proving a document was skipped, replaced or pruned means controlling
// exactly what the second crawl sees.
type mutableGiteaServer struct {
	mu    sync.Mutex
	tree  string
	blobs map[string]string
	srv   *httptest.Server
}

func newMutableGiteaServer(t *testing.T) *mutableGiteaServer {
	t.Helper()
	m := &mutableGiteaServer{tree: `{"tree":[],"truncated":false}`, blobs: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/repos/octocat/thesis/git/trees/main", func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		defer m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, m.tree)
	})
	mux.HandleFunc("/api/v1/repos/octocat/thesis/git/blobs/", func(w http.ResponseWriter, r *http.Request) {
		sha := strings.TrimPrefix(r.URL.Path, "/api/v1/repos/octocat/thesis/git/blobs/")
		m.mu.Lock()
		content, ok := m.blobs[sha]
		m.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"content":%q,"encoding":"base64"}`, base64.StdEncoding.EncodeToString([]byte(content)))
	})
	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

// setFiles replaces the whole tree listing and blob content in one call, so
// a test can move from "these documents exist" to "these other ones do" —
// the exact scenario a document skip, replace or prune test needs.
func (m *mutableGiteaServer) setFiles(files map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var entries []string
	m.blobs = map[string]string{}
	for path, content := range files {
		sha := fmt.Sprintf("%08x", len(entries)+1)
		m.blobs[sha] = content
		entries = append(entries, fmt.Sprintf(`{"path":%q,"type":"blob","sha":%q}`, path, sha))
	}
	m.tree = fmt.Sprintf(`{"tree":[%s],"truncated":false}`, strings.Join(entries, ","))
}

// setFilesTruncated is setFiles, but with the tree listing's own truncated
// flag forced true — simulating the Gitea server itself having cut the
// listing short, independent of anything this crawler's own caps would do.
func (m *mutableGiteaServer) setFilesTruncated(files map[string]string) {
	m.setFiles(files)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tree = strings.Replace(m.tree, `"truncated":false`, `"truncated":true`, 1)
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

	if err := env.Indexer.Reindex(ctx, systemActor(), inst.ID, ReindexUpdate); err != nil {
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

// This is the proof the Source seam actually holds: a second, independently
// registered crawler works end to end (crawl, heading-chunk, embed, store,
// search) through the exact same generic Indexer code path Zotero uses,
// with no branch anywhere in internal/service naming "gitea".
func TestIndexerReindexEndToEndGitea(t *testing.T) {
	env := newTestEnv(t)
	gitea := fakeGiteaServer(t)
	embedder := fakeEmbedderServer(t)
	ctx := context.Background()

	in := CreateInput{
		Name: "My Thesis", Slug: "my-thesis", ConnectorID: "gitea",
		BaseURL:             gitea.URL,
		Variables:           map[string]string{"owner": "octocat", "repo": "thesis", "ref": "main"},
		Enabled:             true,
		AllowPrivateNetwork: true,
		EmbedderURL:         embedder.URL,
	}
	inst, err := env.Instances.Create(ctx, systemActor(), in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, tool := range []string{"gitea_list_tree", "gitea_get_file"} {
		if err := env.Instances.SetToolEnabled(ctx, systemActor(), inst.ID, tool, true); err != nil {
			t.Fatalf("SetToolEnabled(%s): %v", tool, err)
		}
	}

	if !env.Indexer.Supported("gitea") {
		t.Fatal("Supported(\"gitea\") = false, want true once source/gitea is registered")
	}

	if err := env.Indexer.Reindex(ctx, systemActor(), inst.ID, ReindexUpdate); err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	st := waitForIndexDone(t, env, inst.ID, 5*time.Second)
	if st.LastError != "" {
		t.Fatalf("reindex failed: %s", st.LastError)
	}
	if st.ChunksIndexed == 0 {
		t.Fatal("ChunksIndexed = 0, want at least one chunk from chapters/intro.md")
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
	if hits[0].ItemKey != "chapters/intro.md" || hits[0].AttachmentKey != "chapters/intro.md" {
		t.Errorf("hit = %+v, want ItemKey=AttachmentKey=chapters/intro.md", hits[0])
	}
	if !strings.HasPrefix(hits[0].Text, "Introduction\n\n") {
		t.Errorf("hit text = %q, want it to carry the heading breadcrumb from ChunkHeading", hits[0].Text)
	}
}

// fakeLinkdingServer serves the three endpoints the Linkding indexer
// crawls: one bookmark with one complete HTML snapshot.
func fakeLinkdingServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/bookmarks/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"count":1,"next":null,"previous":null,"results":[{"id":1,"url":"https://example.com/article"}]}`)
	})
	mux.HandleFunc("/api/bookmarks/1/assets/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"count":1,"next":null,"previous":null,"results":[{"id":100,"asset_type":"snapshot","status":"complete","content_type":"text/html"}]}`)
	})
	mux.HandleFunc("/api/bookmarks/1/assets/100/download/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		fmt.Fprint(w, `<html><body><article><p>the quick brown fox jumps over the lazy dog. `+
			strings.Repeat("more filler text to make a real chunk. ", 20)+`</p></article></body></html>`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// This is the same Source-seam proof as TestIndexerReindexEndToEndGitea, for
// a third, independently registered crawler with a shape all its own —
// paginated bookmarks, a nested assets list, and raw (non-JSON) HTML that
// index.StripHTML has to turn into indexable text — through the identical
// generic Indexer code path, with no branch anywhere in internal/service
// naming "linkding".
func TestIndexerReindexEndToEndLinkding(t *testing.T) {
	env := newTestEnv(t)
	linkding := fakeLinkdingServer(t)
	embedder := fakeEmbedderServer(t)
	ctx := context.Background()

	in := CreateInput{
		Name: "My Bookmarks", Slug: "my-bookmarks", ConnectorID: "linkding",
		BaseURL:             linkding.URL,
		Enabled:             true,
		AllowPrivateNetwork: true,
		EmbedderURL:         embedder.URL,
	}
	inst, err := env.Instances.Create(ctx, systemActor(), in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := env.Instances.SetSecret(ctx, systemActor(), inst.ID, "token", "test-token"); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}
	for _, tool := range []string{"linkding_list_bookmarks", "linkding_list_assets", "linkding_get_asset_content"} {
		if err := env.Instances.SetToolEnabled(ctx, systemActor(), inst.ID, tool, true); err != nil {
			t.Fatalf("SetToolEnabled(%s): %v", tool, err)
		}
	}

	if !env.Indexer.Supported("linkding") {
		t.Fatal("Supported(\"linkding\") = false, want true once source/linkding is registered")
	}

	if err := env.Indexer.Reindex(ctx, systemActor(), inst.ID, ReindexUpdate); err != nil {
		t.Fatalf("Reindex: %v", err)
	}
	st := waitForIndexDone(t, env, inst.ID, 5*time.Second)
	if st.LastError != "" {
		t.Fatalf("reindex failed: %s", st.LastError)
	}
	if st.ChunksIndexed == 0 {
		t.Fatal("ChunksIndexed = 0, want at least one chunk from the fake snapshot")
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
	if hits[0].ItemKey != "1" || hits[0].AttachmentKey != "100" {
		t.Errorf("hit = %+v, want ItemKey=1 AttachmentKey=100", hits[0])
	}
	if strings.Contains(hits[0].Text, "<p>") || strings.Contains(hits[0].Text, "<article>") {
		t.Errorf("hit text = %q, want HTML tags stripped by index.StripHTML before chunking", hits[0].Text)
	}
}

// createIndexableGiteaInstance creates an instance pointed at a mutable fake
// Gitea server, with the fake embedder configured and both crawl tools
// enabled — the common setup for the incremental-update tests below, which
// all need to change what the server serves between two Reindex calls.
func createIndexableGiteaInstance(t *testing.T, env *testEnv, giteaURL, embedderURL string) *domain.Instance {
	t.Helper()
	ctx := context.Background()
	in := CreateInput{
		// Each test gets its own isolated env/database (see newTestEnv), so
		// a constant slug is fine — nothing else in the same test process
		// shares this instance.
		Name: "My Thesis", Slug: "my-thesis-incremental", ConnectorID: "gitea",
		BaseURL:             giteaURL,
		Variables:           map[string]string{"owner": "octocat", "repo": "thesis", "ref": "main"},
		Enabled:             true,
		AllowPrivateNetwork: true,
		EmbedderURL:         embedderURL,
	}
	inst, err := env.Instances.Create(ctx, systemActor(), in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	for _, tool := range []string{"gitea_list_tree", "gitea_get_file"} {
		if err := env.Instances.SetToolEnabled(ctx, systemActor(), inst.ID, tool, true); err != nil {
			t.Fatalf("SetToolEnabled(%s): %v", tool, err)
		}
	}
	return inst
}

func TestIndexerUpdateSkipsUnchangedDocument(t *testing.T) {
	env := newTestEnv(t)
	gitea := newMutableGiteaServer(t)
	embedder := fakeEmbedderServer(t)
	ctx := context.Background()
	inst := createIndexableGiteaInstance(t, env, gitea.srv.URL, embedder.URL)

	gitea.setFiles(map[string]string{"a.md": "# A\n\nSame content both runs."})
	if err := env.Indexer.Reindex(ctx, systemActor(), inst.ID, ReindexUpdate); err != nil {
		t.Fatalf("first Reindex: %v", err)
	}
	first := waitForIndexDone(t, env, inst.ID, 5*time.Second)
	if first.LastError != "" {
		t.Fatalf("first reindex failed: %s", first.LastError)
	}
	if first.ChunksIndexed == 0 {
		t.Fatal("first run indexed no chunks")
	}
	countAfterFirst, err := env.Store.SearchIndex().CountChunks(ctx, inst.ID)
	if err != nil {
		t.Fatalf("CountChunks: %v", err)
	}

	// Same file, same content: a second Update run must recognise it as
	// unchanged and do no embedding work for it at all.
	if err := env.Indexer.Reindex(ctx, systemActor(), inst.ID, ReindexUpdate); err != nil {
		t.Fatalf("second Reindex: %v", err)
	}
	second := waitForIndexDone(t, env, inst.ID, 5*time.Second)
	if second.LastError != "" {
		t.Fatalf("second reindex failed: %s", second.LastError)
	}
	if second.DocumentsSkipped != 1 {
		t.Errorf("DocumentsSkipped = %d, want 1", second.DocumentsSkipped)
	}
	if second.ChunksIndexed != 0 {
		t.Errorf("ChunksIndexed = %d, want 0: an unchanged document must not be re-embedded", second.ChunksIndexed)
	}
	countAfterSecond, err := env.Store.SearchIndex().CountChunks(ctx, inst.ID)
	if err != nil {
		t.Fatalf("CountChunks: %v", err)
	}
	if countAfterSecond != countAfterFirst {
		t.Errorf("chunk count changed from %d to %d for an unchanged document", countAfterFirst, countAfterSecond)
	}
}

func TestIndexerUpdateReplacesChangedDocument(t *testing.T) {
	env := newTestEnv(t)
	gitea := newMutableGiteaServer(t)
	embedder := fakeEmbedderServer(t)
	ctx := context.Background()
	inst := createIndexableGiteaInstance(t, env, gitea.srv.URL, embedder.URL)

	gitea.setFiles(map[string]string{"a.md": "# A\n\nOriginal content."})
	if err := env.Indexer.Reindex(ctx, systemActor(), inst.ID, ReindexUpdate); err != nil {
		t.Fatalf("first Reindex: %v", err)
	}
	waitForIndexDone(t, env, inst.ID, 5*time.Second)

	gitea.setFiles(map[string]string{"a.md": "# A\n\nCompletely different content now."})
	if err := env.Indexer.Reindex(ctx, systemActor(), inst.ID, ReindexUpdate); err != nil {
		t.Fatalf("second Reindex: %v", err)
	}
	second := waitForIndexDone(t, env, inst.ID, 5*time.Second)
	if second.LastError != "" {
		t.Fatalf("second reindex failed: %s", second.LastError)
	}
	if second.DocumentsSkipped != 0 {
		t.Errorf("DocumentsSkipped = %d, want 0: the document's content changed", second.DocumentsSkipped)
	}
	if second.ChunksIndexed == 0 {
		t.Error("ChunksIndexed = 0, want the changed document to have been re-embedded")
	}

	hits, err := env.Indexer.Search(ctx, inst.ID, "completely different", 5)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("Search found no hits for the new content")
	}
	oldHits, err := env.Indexer.Search(ctx, inst.ID, "original content", 5)
	if err != nil {
		t.Fatalf("Search (old content): %v", err)
	}
	for _, h := range oldHits {
		if strings.Contains(h.Text, "Original content") {
			t.Error("a chunk from the document's old content is still in the index after it changed")
		}
	}
}

func TestIndexerUpdatePrunesDocumentDeletedUpstream(t *testing.T) {
	env := newTestEnv(t)
	gitea := newMutableGiteaServer(t)
	embedder := fakeEmbedderServer(t)
	ctx := context.Background()
	inst := createIndexableGiteaInstance(t, env, gitea.srv.URL, embedder.URL)

	gitea.setFiles(map[string]string{
		"keep.md":   "# Keep\n\nThis file stays.",
		"delete.md": "# Delete\n\nThis file goes away.",
	})
	if err := env.Indexer.Reindex(ctx, systemActor(), inst.ID, ReindexUpdate); err != nil {
		t.Fatalf("first Reindex: %v", err)
	}
	waitForIndexDone(t, env, inst.ID, 5*time.Second)

	// delete.md no longer appears in the tree listing at all — exactly what
	// an upstream deletion (or rename) looks like from the crawler's side.
	gitea.setFiles(map[string]string{"keep.md": "# Keep\n\nThis file stays."})
	if err := env.Indexer.Reindex(ctx, systemActor(), inst.ID, ReindexUpdate); err != nil {
		t.Fatalf("second Reindex: %v", err)
	}
	second := waitForIndexDone(t, env, inst.ID, 5*time.Second)
	if second.LastError != "" {
		t.Fatalf("second reindex failed: %s", second.LastError)
	}
	if second.DocumentsPruned != 1 {
		t.Errorf("DocumentsPruned = %d, want 1", second.DocumentsPruned)
	}
	if second.DocumentsSkipped != 1 {
		t.Errorf("DocumentsSkipped = %d, want 1 (keep.md, unchanged)", second.DocumentsSkipped)
	}

	all, err := env.Store.SearchIndex().LoadAll(ctx, inst.ID)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	for _, c := range all {
		if c.AttachmentKey == "delete.md" {
			t.Error("delete.md's chunks are still in the index after being pruned")
		}
	}
	docs, err := env.Store.SearchIndex().ListDocuments(ctx, inst.ID)
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	for _, d := range docs {
		if d.AttachmentKey == "delete.md" {
			t.Error("delete.md's bookkeeping row is still present after being pruned")
		}
	}
}

// A truncated crawl does not know the full current state of the library, so
// pruning must sit out entirely rather than risk treating a document it
// simply never reached as one deleted upstream.
func TestIndexerUpdateSkipsPruningWhenCrawlWasTruncated(t *testing.T) {
	env := newTestEnv(t)
	gitea := newMutableGiteaServer(t)
	embedder := fakeEmbedderServer(t)
	ctx := context.Background()
	inst := createIndexableGiteaInstance(t, env, gitea.srv.URL, embedder.URL)

	gitea.setFiles(map[string]string{
		"keep.md":  "# Keep\n\nStays.",
		"maybe.md": "# Maybe\n\nMight look deleted, but the next crawl is truncated.",
	})
	if err := env.Indexer.Reindex(ctx, systemActor(), inst.ID, ReindexUpdate); err != nil {
		t.Fatalf("first Reindex: %v", err)
	}
	waitForIndexDone(t, env, inst.ID, 5*time.Second)

	// maybe.md is gone from this listing, but the listing is marked
	// truncated: the crawl might simply not have reached it, so it must not
	// be treated as deleted.
	gitea.setFilesTruncated(map[string]string{"keep.md": "# Keep\n\nStays."})
	if err := env.Indexer.Reindex(ctx, systemActor(), inst.ID, ReindexUpdate); err != nil {
		t.Fatalf("second Reindex: %v", err)
	}
	second := waitForIndexDone(t, env, inst.ID, 5*time.Second)
	if second.LastError != "" {
		t.Fatalf("second reindex failed: %s", second.LastError)
	}
	if second.DocumentsPruned != 0 {
		t.Errorf("DocumentsPruned = %d, want 0: a truncated crawl must never prune", second.DocumentsPruned)
	}

	docs, err := env.Store.SearchIndex().ListDocuments(ctx, inst.ID)
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	found := false
	for _, d := range docs {
		if d.AttachmentKey == "maybe.md" {
			found = true
		}
	}
	if !found {
		t.Error("maybe.md's bookkeeping row was removed despite the crawl being truncated")
	}
}

// A model change must not silently mix vectors from two models into one
// search — Update must refuse until the operator explicitly rebuilds.
func TestIndexerUpdateRefusesAfterEmbedderModelChanges(t *testing.T) {
	env := newTestEnv(t)
	gitea := newMutableGiteaServer(t)
	embedder := fakeEmbedderServer(t)
	ctx := context.Background()
	inst := createIndexableGiteaInstance(t, env, gitea.srv.URL, embedder.URL)

	gitea.setFiles(map[string]string{"a.md": "# A\n\nSome content."})
	if err := env.Indexer.Reindex(ctx, systemActor(), inst.ID, ReindexUpdate); err != nil {
		t.Fatalf("first Reindex: %v", err)
	}
	waitForIndexDone(t, env, inst.ID, 5*time.Second)
	countBefore, err := env.Store.SearchIndex().CountChunks(ctx, inst.ID)
	if err != nil {
		t.Fatalf("CountChunks: %v", err)
	}

	newModel := "a-different-embedding-model"
	if _, err := env.Instances.Update(ctx, systemActor(), inst.ID, UpdateInput{EmbedderModel: &newModel}); err != nil {
		t.Fatalf("Update (change model): %v", err)
	}

	if err := env.Indexer.Reindex(ctx, systemActor(), inst.ID, ReindexUpdate); err != nil {
		t.Fatalf("second Reindex (Update after model change): %v", err)
	}
	second := waitForIndexDone(t, env, inst.ID, 5*time.Second)
	if second.LastError == "" {
		t.Fatal("Update after a model change must fail, not silently mix vectors from two models")
	}
	if !strings.Contains(second.LastError, "embedder model changed") {
		t.Errorf("LastError = %q, want it to explain the model change", second.LastError)
	}
	countAfterRefusedUpdate, err := env.Store.SearchIndex().CountChunks(ctx, inst.ID)
	if err != nil {
		t.Fatalf("CountChunks: %v", err)
	}
	if countAfterRefusedUpdate != countBefore {
		t.Errorf("a refused Update must not touch the existing index: count went from %d to %d", countBefore, countAfterRefusedUpdate)
	}

	// Rebuild, by contrast, is exactly the escape hatch: it must succeed and
	// adopt the new model.
	if err := env.Indexer.Reindex(ctx, systemActor(), inst.ID, ReindexRebuild); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	third := waitForIndexDone(t, env, inst.ID, 5*time.Second)
	if third.LastError != "" {
		t.Fatalf("Rebuild after a model change failed: %s", third.LastError)
	}
	if third.ChunksIndexed == 0 {
		t.Error("Rebuild produced no chunks")
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

	if err := env.Indexer.Reindex(ctx, systemActor(), inst.ID, ReindexUpdate); err != nil {
		t.Fatalf("first Reindex: %v", err)
	}
	err := env.Indexer.Reindex(ctx, systemActor(), inst.ID, ReindexUpdate)
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

	err = env.Indexer.Reindex(ctx, systemActor(), inst.ID, ReindexUpdate)
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

	if err := env.Indexer.Reindex(ctx, systemActor(), inst.ID, ReindexUpdate); err != nil {
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

	if err := env.Indexer.Reindex(ctx, systemActor(), inst.ID, ReindexUpdate); err != nil {
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
