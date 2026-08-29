-- Moves semantic-search embedder configuration from a per-instance setting to
-- a single platform-wide row: every instance shares one embedder (see
-- domain.EmbedderSettings), since picking a model to turn text into vectors
-- has nothing to do with which upstream API the text came from. The API key
-- (when the embedder needs one) is a separate ciphertext column rather than
-- going through instance_secrets, since it is no longer scoped to an
-- instance.
--
-- Best-effort carries forward one existing instance's embedder URL/model (the
-- oldest configured instance, if any had one set) as the initial platform
-- default; any previously-set per-instance embedder API key is not migrated,
-- since its ciphertext is bound to that instance's own encryption context and
-- cannot be safely re-keyed inside a plain SQL migration — an operator who
-- had one set will need to re-enter it once on the new settings page.
CREATE TABLE platform_settings (
    id                          INTEGER PRIMARY KEY CHECK (id = 1),
    embedder_url                TEXT NOT NULL DEFAULT '',
    embedder_model              TEXT NOT NULL DEFAULT '',
    embedder_rate_limit_per_min INTEGER NOT NULL DEFAULT 60,
    embedder_api_key_ciphertext BLOB,
    updated_at                  TEXT NOT NULL
) STRICT;

-- Timestamps elsewhere in this schema are RFC3339Nano text written by Go's
-- time.Format (see sqlitestore.formatTime); strftime's %fZ gives the same
-- shape (fractional seconds, literal Z for UTC) so this row parses the same
-- way every other timestamp in the database does.
INSERT INTO platform_settings (id, embedder_url, embedder_model, updated_at)
SELECT 1, embedder_url, embedder_model, strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
FROM instances
WHERE embedder_url != ''
ORDER BY created_at
LIMIT 1;

INSERT INTO platform_settings (id, updated_at)
SELECT 1, strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
WHERE NOT EXISTS (SELECT 1 FROM platform_settings WHERE id = 1);

ALTER TABLE instances DROP COLUMN embedder_url;
ALTER TABLE instances DROP COLUMN embedder_model;
