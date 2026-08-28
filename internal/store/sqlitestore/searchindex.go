package sqlitestore

import (
	"context"
	"database/sql"
	"encoding/binary"
	"errors"
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

	for _, stmt := range []string{
		`DELETE FROM index_chunks_fts WHERE instance_id = ?`,
		`DELETE FROM index_chunks WHERE instance_id = ?`,
		`DELETE FROM index_documents WHERE instance_id = ?`,
		`DELETE FROM index_meta WHERE instance_id = ?`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, instanceID); err != nil {
			return translate(err, "clear search index")
		}
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

// DeleteDocumentChunks removes every chunk belonging to one document. The
// FTS table carries no item_key/attachment_key of its own (see the migration
// comment), so its rows have to be found by chunk ID rather than filtered
// directly — the same reason ClearInstance can filter FTS by instance_id
// alone but this narrower delete cannot.
func (r *searchIndexRepo) DeleteDocumentChunks(ctx context.Context, instanceID, itemKey, attachmentKey string) error {
	tx, err := r.write.BeginTx(ctx, nil)
	if err != nil {
		return translate(err, "delete document chunks")
	}
	defer tx.Rollback()

	// A closure so rows is closed (deferred) before the transaction issues
	// any further statement on the same connection — the caller below must
	// not still be holding this result set open.
	ids, err := func() ([]int64, error) {
		rows, err := tx.QueryContext(ctx,
			`SELECT id FROM index_chunks WHERE instance_id = ? AND item_key = ? AND attachment_key = ?`,
			instanceID, itemKey, attachmentKey)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var ids []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		return ids, rows.Err()
	}()
	if err != nil {
		return translate(err, "delete document chunks")
	}

	deleteFTS, err := tx.PrepareContext(ctx, `DELETE FROM index_chunks_fts WHERE chunk_id = ?`)
	if err != nil {
		return translate(err, "delete document chunks")
	}
	defer deleteFTS.Close()
	for _, id := range ids {
		if _, err := deleteFTS.ExecContext(ctx, id); err != nil {
			return translate(err, "delete document chunks")
		}
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM index_chunks WHERE instance_id = ? AND item_key = ? AND attachment_key = ?`,
		instanceID, itemKey, attachmentKey); err != nil {
		return translate(err, "delete document chunks")
	}
	return translate(tx.Commit(), "delete document chunks")
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

func (r *searchIndexRepo) ListDocuments(ctx context.Context, instanceID string) ([]domain.IndexDocument, error) {
	rows, err := r.read.QueryContext(ctx,
		`SELECT item_key, attachment_key, content_hash, chunk_count, updated_at
		 FROM index_documents WHERE instance_id = ?`, instanceID)
	if err != nil {
		return nil, translate(err, "list index documents")
	}
	defer rows.Close()

	var out []domain.IndexDocument
	for rows.Next() {
		var d domain.IndexDocument
		var updatedAt string
		if err := rows.Scan(&d.ItemKey, &d.AttachmentKey, &d.ContentHash, &d.ChunkCount, &updatedAt); err != nil {
			return nil, translate(err, "scan index document")
		}
		d.InstanceID = instanceID
		if d.UpdatedAt, err = parseTime(updatedAt); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, translate(rows.Err(), "list index documents")
}

func (r *searchIndexRepo) UpsertDocument(ctx context.Context, doc domain.IndexDocument) error {
	_, err := r.write.ExecContext(ctx,
		`INSERT INTO index_documents (instance_id, item_key, attachment_key, content_hash, chunk_count, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT (instance_id, item_key, attachment_key)
		 DO UPDATE SET content_hash = excluded.content_hash, chunk_count = excluded.chunk_count,
		               updated_at = excluded.updated_at`,
		doc.InstanceID, doc.ItemKey, doc.AttachmentKey, doc.ContentHash, doc.ChunkCount, formatTime(doc.UpdatedAt))
	return translate(err, "upsert index document")
}

func (r *searchIndexRepo) DeleteDocument(ctx context.Context, instanceID, itemKey, attachmentKey string) error {
	_, err := r.write.ExecContext(ctx,
		`DELETE FROM index_documents WHERE instance_id = ? AND item_key = ? AND attachment_key = ?`,
		instanceID, itemKey, attachmentKey)
	return translate(err, "delete index document")
}

func (r *searchIndexRepo) GetMeta(ctx context.Context, instanceID string) (domain.IndexMeta, bool, error) {
	var (
		m         domain.IndexMeta
		updatedAt string
	)
	err := r.read.QueryRowContext(ctx,
		`SELECT embedder_model, embedder_dimension, updated_at FROM index_meta WHERE instance_id = ?`,
		instanceID).Scan(&m.EmbedderModel, &m.EmbedderDimension, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.IndexMeta{}, false, nil
	}
	if err != nil {
		return domain.IndexMeta{}, false, translate(err, "get index meta")
	}
	m.InstanceID = instanceID
	if m.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return domain.IndexMeta{}, false, err
	}
	return m, true, nil
}

func (r *searchIndexRepo) SetMeta(ctx context.Context, meta domain.IndexMeta) error {
	_, err := r.write.ExecContext(ctx,
		`INSERT INTO index_meta (instance_id, embedder_model, embedder_dimension, updated_at)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT (instance_id)
		 DO UPDATE SET embedder_model = excluded.embedder_model,
		               embedder_dimension = excluded.embedder_dimension,
		               updated_at = excluded.updated_at`,
		meta.InstanceID, meta.EmbedderModel, meta.EmbedderDimension, formatTime(meta.UpdatedAt))
	return translate(err, "set index meta")
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
