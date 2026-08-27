CREATE TABLE index_chunks (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	instance_id    TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
	item_key       TEXT NOT NULL,
	attachment_key TEXT NOT NULL,
	chunk_index    INTEGER NOT NULL,
	char_start     INTEGER NOT NULL,
	char_end       INTEGER NOT NULL,
	text           TEXT NOT NULL,
	embedding      BLOB NOT NULL,
	created_at     TEXT NOT NULL
);

CREATE INDEX idx_index_chunks_instance ON index_chunks(instance_id);

-- A plain (non "external content") FTS5 table: it duplicates the chunk text
-- but needs no sync triggers, which keeps the write path a single insert per
-- chunk instead of two tables that can drift apart.
CREATE VIRTUAL TABLE index_chunks_fts USING fts5(
	text,
	instance_id UNINDEXED,
	chunk_id UNINDEXED,
	tokenize = 'porter unicode61'
);
