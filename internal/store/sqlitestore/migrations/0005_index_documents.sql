-- Tracks per-document state for incremental reindexing.
--
-- A reindex used to always clear and rebuild an instance's whole index, even
-- when almost nothing had changed upstream — every document got re-fetched,
-- re-chunked and re-embedded regardless. index_documents lets an "Update
-- index" run recognise a document whose content hash hasn't changed and
-- skip the expensive chunk/embed/store step for it entirely, and recognise
-- a document that used to be indexed but was not seen this crawl (deleted
-- or renamed upstream) so its stale chunks can be pruned.
CREATE TABLE index_documents (
	instance_id    TEXT NOT NULL REFERENCES instances(id) ON DELETE CASCADE,
	item_key       TEXT NOT NULL,
	attachment_key TEXT NOT NULL,
	content_hash   TEXT NOT NULL,
	chunk_count    INTEGER NOT NULL,
	updated_at     TEXT NOT NULL,
	PRIMARY KEY (instance_id, item_key, attachment_key)
);

-- Records which embedder model (and vector dimension) built an instance's
-- current index. Vectors from two different models are not comparable —
-- cosine similarity against a mismatched pair degrades silently to noise
-- rather than erroring — so an incremental "Update index" run must refuse
-- to mix them in; only "Rebuild from scratch" may change the model.
CREATE TABLE index_meta (
	instance_id        TEXT PRIMARY KEY REFERENCES instances(id) ON DELETE CASCADE,
	embedder_model     TEXT NOT NULL,
	embedder_dimension INTEGER NOT NULL,
	updated_at         TEXT NOT NULL
);
