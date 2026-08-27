package sqlitestore

import (
	"context"
	"testing"

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
