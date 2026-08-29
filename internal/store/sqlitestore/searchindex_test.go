package sqlitestore

import (
	"context"
	"testing"
	"time"

	"github.com/00101010xyz/mcpaw/internal/domain"
)

func TestSearchIndexInsertLoadAndClear(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedConnector(t, s)
	inst := seedInstance(t, s, "zotero")

	chunks := []domain.IndexChunk{
		{ItemKey: "ITEM0001", AttachmentKey: "ATT00001", ChunkIndex: 0, CharStart: 0, CharEnd: 20,
			Text: "the quick brown fox", Embedding: []float32{1, 0, 0}},
		{ItemKey: "ITEM0001", AttachmentKey: "ATT00001", ChunkIndex: 1, CharStart: 20, CharEnd: 40,
			Text: "jumps over a lazy dog", Embedding: []float32{0, 1, 0}},
	}
	if err := s.SearchIndex().InsertChunks(ctx, inst.ID, chunks); err != nil {
		t.Fatalf("InsertChunks: %v", err)
	}

	count, err := s.SearchIndex().CountChunks(ctx, inst.ID)
	if err != nil {
		t.Fatalf("CountChunks: %v", err)
	}
	if count != 2 {
		t.Fatalf("CountChunks = %d, want 2", count)
	}

	loaded, err := s.SearchIndex().LoadAll(ctx, inst.ID)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("LoadAll returned %d chunks, want 2", len(loaded))
	}
	for _, c := range loaded {
		if c.ID == 0 {
			t.Error("expected a nonzero assigned ID")
		}
		if c.InstanceID != inst.ID {
			t.Errorf("InstanceID = %q, want %q", c.InstanceID, inst.ID)
		}
		if len(c.Embedding) != 3 {
			t.Errorf("embedding round-trip: got %v", c.Embedding)
		}
	}

	// A chunk belonging to a different instance must never surface in this
	// instance's search results — that would leak one client's library into
	// a token scoped to another.
	other := seedInstance(t, s, "other-zotero")
	if err := s.SearchIndex().InsertChunks(ctx, other.ID, []domain.IndexChunk{
		{ItemKey: "OTHR0001", AttachmentKey: "OATT0001", Text: "unrelated content", Embedding: []float32{0, 0, 1}},
	}); err != nil {
		t.Fatalf("InsertChunks (other instance): %v", err)
	}
	loaded, err = s.SearchIndex().LoadAll(ctx, inst.ID)
	if err != nil {
		t.Fatalf("LoadAll after seeding a second instance: %v", err)
	}
	if len(loaded) != 2 {
		t.Fatalf("cross-instance leak: LoadAll returned %d chunks, want 2", len(loaded))
	}

	hits, err := s.SearchIndex().BM25Search(ctx, inst.ID, `"fox"`, 10)
	if err != nil {
		t.Fatalf("BM25Search: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("BM25Search(fox) returned %d hits, want 1", len(hits))
	}

	if err := s.SearchIndex().ClearInstance(ctx, inst.ID); err != nil {
		t.Fatalf("ClearInstance: %v", err)
	}
	count, err = s.SearchIndex().CountChunks(ctx, inst.ID)
	if err != nil {
		t.Fatalf("CountChunks after clear: %v", err)
	}
	if count != 0 {
		t.Fatalf("CountChunks after clear = %d, want 0", count)
	}
	// Clearing one instance must not touch another's chunks.
	otherCount, err := s.SearchIndex().CountChunks(ctx, other.ID)
	if err != nil {
		t.Fatalf("CountChunks (other instance): %v", err)
	}
	if otherCount != 1 {
		t.Fatalf("ClearInstance affected another instance: count = %d, want 1", otherCount)
	}
}

func TestSearchIndexDeleteDocumentChunks(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedConnector(t, s)
	inst := seedInstance(t, s, "zotero")

	if err := s.SearchIndex().InsertChunks(ctx, inst.ID, []domain.IndexChunk{
		{ItemKey: "ITEM0001", AttachmentKey: "ATT00001", ChunkIndex: 0, Text: "keep this out", Embedding: []float32{1}},
		{ItemKey: "ITEM0001", AttachmentKey: "ATT00001", ChunkIndex: 1, Text: "also keep this out", Embedding: []float32{1}},
		{ItemKey: "ITEM0002", AttachmentKey: "ATT00002", ChunkIndex: 0, Text: "keep this in", Embedding: []float32{1}},
	}); err != nil {
		t.Fatalf("InsertChunks: %v", err)
	}

	if err := s.SearchIndex().DeleteDocumentChunks(ctx, inst.ID, "ITEM0001", "ATT00001"); err != nil {
		t.Fatalf("DeleteDocumentChunks: %v", err)
	}

	loaded, err := s.SearchIndex().LoadAll(ctx, inst.ID)
	if err != nil {
		t.Fatalf("LoadAll: %v", err)
	}
	if len(loaded) != 1 || loaded[0].AttachmentKey != "ATT00002" {
		t.Fatalf("LoadAll after delete = %+v, want only ATT00002's chunk", loaded)
	}

	// The FTS side table must not retain a row for the deleted chunks either
	// — a stale FTS row would keep matching keyword searches for text that
	// no longer has a corresponding chunk.
	hits, err := s.SearchIndex().BM25Search(ctx, inst.ID, `"keep this out"`, 10)
	if err != nil {
		t.Fatalf("BM25Search: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("BM25Search still finds the deleted document's text: %v", hits)
	}
}

func TestSearchIndexDocumentBookkeeping(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedConnector(t, s)
	inst := seedInstance(t, s, "zotero")
	now := time.Now().UTC().Truncate(time.Second)

	doc := domain.IndexDocument{
		InstanceID: inst.ID, ItemKey: "ITEM0001", AttachmentKey: "ATT00001",
		ContentHash: "hash-v1", ChunkCount: 3, UpdatedAt: now,
	}
	if err := s.SearchIndex().UpsertDocument(ctx, doc); err != nil {
		t.Fatalf("UpsertDocument (insert): %v", err)
	}

	docs, err := s.SearchIndex().ListDocuments(ctx, inst.ID)
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	if len(docs) != 1 || docs[0].ContentHash != "hash-v1" || docs[0].ChunkCount != 3 {
		t.Fatalf("ListDocuments = %+v, want one row with hash-v1/3", docs)
	}

	// Upserting the same (instance, item, attachment) again must replace the
	// row, not add a second one — that is what lets an incremental run
	// re-record a changed document's new hash in place.
	doc.ContentHash = "hash-v2"
	doc.ChunkCount = 5
	if err := s.SearchIndex().UpsertDocument(ctx, doc); err != nil {
		t.Fatalf("UpsertDocument (update): %v", err)
	}
	docs, err = s.SearchIndex().ListDocuments(ctx, inst.ID)
	if err != nil {
		t.Fatalf("ListDocuments after update: %v", err)
	}
	if len(docs) != 1 || docs[0].ContentHash != "hash-v2" || docs[0].ChunkCount != 5 {
		t.Fatalf("ListDocuments after update = %+v, want one row with hash-v2/5", docs)
	}

	if err := s.SearchIndex().DeleteDocument(ctx, inst.ID, "ITEM0001", "ATT00001"); err != nil {
		t.Fatalf("DeleteDocument: %v", err)
	}
	docs, err = s.SearchIndex().ListDocuments(ctx, inst.ID)
	if err != nil {
		t.Fatalf("ListDocuments after delete: %v", err)
	}
	if len(docs) != 0 {
		t.Fatalf("ListDocuments after delete = %+v, want none", docs)
	}
}

// Document bookkeeping is scoped per instance, the same as chunks — one
// instance's document rows must never appear when listing another's.
func TestSearchIndexDocumentsScopedPerInstance(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedConnector(t, s)
	instA := seedInstance(t, s, "inst-a")
	instB := seedInstance(t, s, "inst-b")

	if err := s.SearchIndex().UpsertDocument(ctx, domain.IndexDocument{
		InstanceID: instA.ID, ItemKey: "X", AttachmentKey: "X", ContentHash: "h", ChunkCount: 1,
		UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertDocument A: %v", err)
	}

	docsB, err := s.SearchIndex().ListDocuments(ctx, instB.ID)
	if err != nil {
		t.Fatalf("ListDocuments B: %v", err)
	}
	if len(docsB) != 0 {
		t.Fatalf("instance B sees instance A's document rows: %+v", docsB)
	}
}

func TestSearchIndexMeta(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedConnector(t, s)
	inst := seedInstance(t, s, "zotero")

	if _, ok, err := s.SearchIndex().GetMeta(ctx, inst.ID); err != nil {
		t.Fatalf("GetMeta (unset): %v", err)
	} else if ok {
		t.Fatal("GetMeta reported ok=true for an instance that was never indexed")
	}

	meta := domain.IndexMeta{
		InstanceID: inst.ID, EmbedderModel: "nomic-embed-text", EmbedderDimension: 768,
		UpdatedAt: time.Now().UTC().Truncate(time.Second),
	}
	if err := s.SearchIndex().SetMeta(ctx, meta); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}

	got, ok, err := s.SearchIndex().GetMeta(ctx, inst.ID)
	if err != nil {
		t.Fatalf("GetMeta: %v", err)
	}
	if !ok {
		t.Fatal("GetMeta reported ok=false after SetMeta")
	}
	if got.EmbedderModel != "nomic-embed-text" || got.EmbedderDimension != 768 {
		t.Errorf("GetMeta = %+v, want nomic-embed-text/768", got)
	}

	// SetMeta again with a different model must replace, not duplicate —
	// this is the write path a "Rebuild from scratch" after a model change
	// depends on.
	meta.EmbedderModel = "mxbai-embed-large"
	meta.EmbedderDimension = 1024
	if err := s.SearchIndex().SetMeta(ctx, meta); err != nil {
		t.Fatalf("SetMeta (replace): %v", err)
	}
	got, _, err = s.SearchIndex().GetMeta(ctx, inst.ID)
	if err != nil {
		t.Fatalf("GetMeta after replace: %v", err)
	}
	if got.EmbedderModel != "mxbai-embed-large" || got.EmbedderDimension != 1024 {
		t.Errorf("GetMeta after replace = %+v, want mxbai-embed-large/1024", got)
	}
}

// ClearInstance must also drop document and meta rows, not just chunks —
// otherwise a "Rebuild from scratch" would leave stale bookkeeping that a
// subsequent incremental run could wrongly treat as "already indexed,
// unchanged".
func TestSearchIndexClearInstanceAlsoClearsDocumentsAndMeta(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedConnector(t, s)
	inst := seedInstance(t, s, "zotero")

	if err := s.SearchIndex().UpsertDocument(ctx, domain.IndexDocument{
		InstanceID: inst.ID, ItemKey: "X", AttachmentKey: "X", ContentHash: "h", ChunkCount: 1,
		UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}
	if err := s.SearchIndex().SetMeta(ctx, domain.IndexMeta{
		InstanceID: inst.ID, EmbedderModel: "m", EmbedderDimension: 4, UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("SetMeta: %v", err)
	}

	if err := s.SearchIndex().ClearInstance(ctx, inst.ID); err != nil {
		t.Fatalf("ClearInstance: %v", err)
	}

	docs, err := s.SearchIndex().ListDocuments(ctx, inst.ID)
	if err != nil {
		t.Fatalf("ListDocuments: %v", err)
	}
	if len(docs) != 0 {
		t.Errorf("ClearInstance left %d document rows behind", len(docs))
	}
	if _, ok, err := s.SearchIndex().GetMeta(ctx, inst.ID); err != nil {
		t.Fatalf("GetMeta: %v", err)
	} else if ok {
		t.Error("ClearInstance left a meta row behind")
	}
}

func TestSearchIndexBM25MalformedQueryDegradesGracefully(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	seedConnector(t, s)
	inst := seedInstance(t, s, "zotero")

	if err := s.SearchIndex().InsertChunks(ctx, inst.ID, []domain.IndexChunk{
		{ItemKey: "ITEM0001", AttachmentKey: "ATT00001", Text: "some indexed text", Embedding: []float32{1}},
	}); err != nil {
		t.Fatalf("InsertChunks: %v", err)
	}

	// Unbalanced quoting is invalid FTS5 syntax; the hybrid search path
	// depends on this returning an empty result rather than an error, so the
	// vector half can still serve the query.
	if _, err := s.SearchIndex().BM25Search(ctx, inst.ID, `"unterminated`, 10); err != nil {
		t.Fatalf("BM25Search with malformed query returned an error instead of degrading: %v", err)
	}
}
