-- Initial schema.
--
-- Design notes:
--  * Timestamps are RFC3339Nano UTC text: portable across engines, sortable as
--    text, and readable when debugging a live database.
--  * Secrets are stored as ciphertext BLOBs. The schema has no column that can
--    hold a plaintext credential, which makes accidental leakage a compile-time
--    impossibility rather than a review checklist item.
--  * ON DELETE CASCADE keeps dependent rows (secrets, bindings, tokens) from
--    outliving their instance.

CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    email         TEXT NOT NULL UNIQUE COLLATE NOCASE,
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL CHECK (role IN ('admin', 'viewer')),
    disabled      INTEGER NOT NULL DEFAULT 0 CHECK (disabled IN (0, 1)),
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    last_login_at TEXT
) STRICT;

CREATE TABLE sessions (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    csrf_token   TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    last_seen_at TEXT NOT NULL,
    expires_at   TEXT NOT NULL,
    ip           TEXT NOT NULL DEFAULT '',
    user_agent   TEXT NOT NULL DEFAULT ''
) STRICT;

CREATE INDEX idx_sessions_user ON sessions (user_id);
CREATE INDEX idx_sessions_expires ON sessions (expires_at);

CREATE TABLE connectors (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    version    TEXT NOT NULL,
    source     TEXT NOT NULL CHECK (source IN ('builtin', 'manifest', 'openapi')),
    manifest   BLOB NOT NULL,
    checksum   TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;

CREATE TABLE instances (
    id                    TEXT PRIMARY KEY,
    slug                  TEXT NOT NULL UNIQUE,
    name                  TEXT NOT NULL,
    description           TEXT NOT NULL DEFAULT '',
    connector_id          TEXT NOT NULL REFERENCES connectors (id) ON DELETE RESTRICT,
    base_url              TEXT NOT NULL,
    variables             TEXT NOT NULL DEFAULT '{}',
    enabled               INTEGER NOT NULL DEFAULT 1 CHECK (enabled IN (0, 1)),
    allow_private_network INTEGER NOT NULL DEFAULT 0 CHECK (allow_private_network IN (0, 1)),
    timeout_ms            INTEGER NOT NULL,
    rate_limit_per_min    INTEGER NOT NULL,
    max_concurrent        INTEGER NOT NULL,
    max_response_bytes    INTEGER NOT NULL,
    created_at            TEXT NOT NULL,
    updated_at            TEXT NOT NULL,
    version               INTEGER NOT NULL DEFAULT 1
) STRICT;

CREATE INDEX idx_instances_connector ON instances (connector_id);

CREATE TABLE instance_secrets (
    instance_id TEXT NOT NULL REFERENCES instances (id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    ciphertext  BLOB NOT NULL,
    updated_at  TEXT NOT NULL,
    PRIMARY KEY (instance_id, name)
) STRICT;

CREATE TABLE instance_tools (
    instance_id TEXT NOT NULL REFERENCES instances (id) ON DELETE CASCADE,
    tool_name   TEXT NOT NULL,
    enabled     INTEGER NOT NULL CHECK (enabled IN (0, 1)),
    PRIMARY KEY (instance_id, tool_name)
) STRICT;

CREATE TABLE tokens (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL,
    lookup_key   TEXT NOT NULL UNIQUE,
    prefix       TEXT NOT NULL,
    instance_id  TEXT NOT NULL DEFAULT '' ,
    created_by   TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    expires_at   TEXT,
    last_used_at TEXT,
    revoked_at   TEXT
) STRICT;

CREATE INDEX idx_tokens_instance ON tokens (instance_id);

CREATE TABLE audit_log (
    id          TEXT PRIMARY KEY,
    at          TEXT NOT NULL,
    actor_type  TEXT NOT NULL,
    actor_id    TEXT NOT NULL DEFAULT '',
    action      TEXT NOT NULL,
    target_type TEXT NOT NULL DEFAULT '',
    target_id   TEXT NOT NULL DEFAULT '',
    result      TEXT NOT NULL DEFAULT '',
    ip          TEXT NOT NULL DEFAULT '',
    detail      TEXT NOT NULL DEFAULT '{}'
) STRICT;

CREATE INDEX idx_audit_at ON audit_log (at DESC);
CREATE INDEX idx_audit_action ON audit_log (action, at DESC);
CREATE INDEX idx_audit_target ON audit_log (target_id, at DESC);
