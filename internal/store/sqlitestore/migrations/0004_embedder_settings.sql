-- Adds the per-instance semantic-search embedder settings.
--
-- These are deliberately instance-level columns, not connector variables:
-- which documents to index and how to embed them is a platform feature
-- layered on top of a connector, not part of the API the connector
-- describes. The corresponding API key (when the embedder needs one) is
-- stored through the existing instance_secrets mechanism under the reserved
-- name "embedderApiKey" rather than a new column, since it already handles
-- encryption at rest and per-instance scoping.

ALTER TABLE instances ADD COLUMN embedder_url TEXT NOT NULL DEFAULT '';
ALTER TABLE instances ADD COLUMN embedder_model TEXT NOT NULL DEFAULT '';
