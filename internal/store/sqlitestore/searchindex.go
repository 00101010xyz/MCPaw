package sqlitestore

import (
	"context"
	"encoding/binary"
	"math"
	"strings"
	"time"

	"github.com/00101010xyz/mcpaw/internal/domain"
)

type searchIndexRepo struct{ base }

// encodeVector packs a float32 embedding as little-endian bytes for BLOB
// storage. A dedicated column type (rather than JSON) keeps a chunk row a
// fixed, predictable size and avoids float round-tripping through text.
func encodeVector(v []float32) []byte {
	out := make([]byte, 4*len(v))
	for i, f := range v {
		binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(f))
	}
	return out
}

func decodeVector(b []byte) []float32 {
	out := make([]float32, len(b)/4)
	for i := range out {
		out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[i*4:]))
	}
	return out
}

func (r *searchIndexRepo) ClearInstance(ctx context.Context, instanceID string) error {
	tx, err := r.write.BeginTx(ctx, nil)
	if err != nil {
		return translate(err, "clear search index")
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM index_chunks_fts WHERE instance_id = ?`, instanceID); err != nil {
		return translate(err, "clear search index")
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM index_chunks WHERE instance_id = ?`, instanceID); err != nil {
		return translate(err, "clear search index")
	}
	return translate(tx.Commit(), "clear search index")
}

func (r *searchIndexRepo) InsertChunks(ctx context.Context, instanceID string, chunks []domain.IndexChunk) error {
	if len(chunks) == 0 {
		return nil
	}
	tx, err := r.write.BeginTx(ctx, nil)
	if err != nil {
		return translate(err, "insert index chunks")
	}
	defer tx.Rollback()

	insertChunk, err := tx.PrepareContext(ctx,
		`INSERT INTO index_chunks (instance_id, item_key, attachment_key, chunk_index, char_start, char_end, text, embedding, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return translate(err, "insert index chunks")
	}
	defer insertChunk.Close()

	insertFTS, err := tx.PrepareContext(ctx,
		`INSERT INTO index_chunks_fts (rowid, text, instance_id, chunk_id) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return translate(err, "insert index chunks")
	}
	defer insertFTS.Close()

	now := formatTime(time.Now())
	for _, c := range chunks {
		res, err := insertChunk.ExecContext(ctx, instanceID, c.ItemKey, c.AttachmentKey, c.ChunkIndex,
			c.CharStart, c.CharEnd, c.Text, encodeVector(c.Embedding), now)
		if err != nil {
			return translate(err, "insert index chunk")
		}
		id, err := res.LastInsertId()
		if err != nil {
			return translate(err, "insert index chunk")
		}
		if _, err := insertFTS.ExecContext(ctx, id, c.Text, instanceID, id); err != nil {
			return translate(err, "insert index chunk fts row")
		}
	}
	return translate(tx.Commit(), "insert index chunks")
}

func (r *searchIndexRepo) CountChunks(ctx context.Context, instanceID string) (int, error) {
	var n int
	err := r.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM index_chunks WHERE instance_id = ?`, instanceID).Scan(&n)
	return n, translate(err, "count index chunks")
}

func (r *searchIndexRepo) LoadAll(ctx context.Context, instanceID string) ([]domain.IndexChunk, error) {
	rows, err := r.read.QueryContext(ctx,
		`SELECT id, item_key, attachment_key, chunk_index, char_start, char_end, text, embedding
		 FROM index_chunks WHERE instance_id = ?`, instanceID)
	if err != nil {
		return nil, translate(err, "load index chunks")
	}
	defer rows.Close()

	var out []domain.IndexChunk
	for rows.Next() {
		var c domain.IndexChunk
		var embedding []byte
		if err := rows.Scan(&c.ID, &c.ItemKey, &c.AttachmentKey, &c.ChunkIndex,
			&c.CharStart, &c.CharEnd, &c.Text, &embedding); err != nil {
			return nil, translate(err, "scan index chunk")
		}
		c.InstanceID = instanceID
		c.Embedding = decodeVector(embedding)
		out = append(out, c)
	}
	return out, translate(rows.Err(), "load index chunks")
}

// BM25Search returns chunk IDs ranked by full-text relevance, best first. A
// query FTS5 rejects as invalid syntax degrades to "no keyword matches"
// rather than failing the whole hybrid search: the vector half still runs.
func (r *searchIndexRepo) BM25Search(ctx context.Context, instanceID, query string, limit int) ([]int64, error) {
	if query == "" {
		return nil, nil
	}
	rows, err := r.read.QueryContext(ctx,
		`SELECT chunk_id FROM index_chunks_fts
		 WHERE index_chunks_fts.text MATCH ? AND instance_id = ?
		 ORDER BY rank LIMIT ?`, query, instanceID, limit)
	if err != nil {
		if isMatchSyntaxError(err) {
			return nil, nil
		}
		return nil, translate(err, "bm25 search")
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, translate(err, "scan bm25 hit")
		}
		out = append(out, id)
	}
	return out, translate(rows.Err(), "bm25 search")
}

// isMatchSyntaxError reports whether err is SQLite/FTS5 rejecting the MATCH
// expression itself, as opposed to a genuine database failure. The query has
// no other failure surface (instanceID and limit are bound parameters, the
// schema is fixed), so this is the only runtime error this specific query
// can produce, and different SQLite builds phrase it slightly differently —
// "unterminated string" for a bad quote, "fts5: syntax error" for other
// malformed queries.
func isMatchSyntaxError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "fts5") ||
		strings.Contains(msg, "unterminated") ||
		strings.Contains(msg, "syntax error")
}
