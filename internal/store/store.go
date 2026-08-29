// Package store defines the persistence ports of MCPaw.
//
// These are interfaces expressed purely in domain terms. Application services
// depend on them and never on a concrete database, which is what makes the
// SQLite adapter replaceable by Postgres — or by an in-memory fake in tests —
// without touching a single use case.
package store

import (
	"context"
	"time"

	"github.com/00101010xyz/mcpaw/internal/domain"
)

// Repositories aggregates every persistence port behind one injectable value.
type Repositories interface {
	Users() UserRepository
	Sessions() SessionRepository
	Connectors() ConnectorRepository
	Instances() InstanceRepository
	Tokens() TokenRepository
	Audit() AuditRepository
	SearchIndex() SearchIndexRepository
	Platform() PlatformRepository

	// Ping verifies the datastore is reachable; used by the readiness probe.
	Ping(ctx context.Context) error
	// Close releases datastore resources.
	Close() error
}

// UserRepository persists administrator accounts.
type UserRepository interface {
	Create(ctx context.Context, u *domain.User) error
	Update(ctx context.Context, u *domain.User) error
	GetByID(ctx context.Context, id string) (*domain.User, error)
	GetByEmail(ctx context.Context, email string) (*domain.User, error)
	List(ctx context.Context) ([]*domain.User, error)
	Count(ctx context.Context) (int, error)
}

// SessionRepository persists web sessions. IDs are keyed digests of the cookie
// value, never the cookie value itself.
type SessionRepository interface {
	Create(ctx context.Context, s *domain.Session) error
	Get(ctx context.Context, id string) (*domain.Session, error)
	Touch(ctx context.Context, id string, lastSeen, expiresAt time.Time) error
	Delete(ctx context.Context, id string) error
	DeleteByUser(ctx context.Context, userID string) error
	DeleteExpired(ctx context.Context, now time.Time) (int64, error)
}

// ConnectorRepository persists connector manifests.
type ConnectorRepository interface {
	Upsert(ctx context.Context, c *domain.ConnectorRecord) error
	Get(ctx context.Context, id string) (*domain.ConnectorRecord, error)
	List(ctx context.Context) ([]*domain.ConnectorRecord, error)
	Delete(ctx context.Context, id string) error
}

// InstanceRepository persists configured connector instances together with
// their secrets and per-tool bindings. Secrets are handed over already
// encrypted: this port never sees plaintext.
type InstanceRepository interface {
	Create(ctx context.Context, i *domain.Instance) error
	Update(ctx context.Context, i *domain.Instance) error
	Delete(ctx context.Context, id string) error
	Get(ctx context.Context, id string) (*domain.Instance, error)
	GetBySlug(ctx context.Context, slug string) (*domain.Instance, error)
	List(ctx context.Context) ([]*domain.Instance, error)
	CountByConnector(ctx context.Context, connectorID string) (int, error)

	SetSecret(ctx context.Context, s *domain.InstanceSecret) error
	DeleteSecret(ctx context.Context, instanceID, name string) error
	ListSecretNames(ctx context.Context, instanceID string) ([]string, error)
	LoadSecrets(ctx context.Context, instanceID string) (map[string][]byte, error)

	SetToolBinding(ctx context.Context, b *domain.ToolBinding) error
	ListToolBindings(ctx context.Context, instanceID string) ([]*domain.ToolBinding, error)
}

// TokenRepository persists MCP bearer tokens by keyed digest.
type TokenRepository interface {
	Create(ctx context.Context, t *domain.Token) error
	GetByLookupKey(ctx context.Context, key string) (*domain.Token, error)
	List(ctx context.Context) ([]*domain.Token, error)
	Revoke(ctx context.Context, id string, at time.Time) error
	TouchLastUsed(ctx context.Context, id string, at time.Time) error
	DeleteByInstance(ctx context.Context, instanceID string) error
}

// AuditFilter narrows an audit log query.
type AuditFilter struct {
	Action   string
	ActorID  string
	TargetID string
	Limit    int
	Before   *time.Time
}

// AuditRepository is an append-only record of security-relevant actions.
type AuditRepository interface {
	Append(ctx context.Context, e *domain.AuditEvent) error
	List(ctx context.Context, f AuditFilter) ([]*domain.AuditEvent, error)
	Prune(ctx context.Context, olderThan time.Time) (int64, error)
}

// SearchIndexRepository persists the semantic-search index: chunks and their
// embeddings, plus the per-document and per-instance bookkeeping an
// incremental "Update index" run needs to know what changed.
type SearchIndexRepository interface {
	// ClearInstance removes every chunk, document record and meta row
	// belonging to an instance. A "Rebuild from scratch" run always starts
	// here so stale chunks from a prior model, or a deleted document, can
	// never linger and be served as if current.
	ClearInstance(ctx context.Context, instanceID string) error
	// InsertChunks appends chunks, incrementally during a reindex, so a
	// crash or timeout partway through leaves a partial-but-usable index
	// rather than nothing.
	InsertChunks(ctx context.Context, instanceID string, chunks []domain.IndexChunk) error
	// DeleteDocumentChunks removes every chunk (and its FTS row) belonging to
	// one document, so a changed document's stale chunks can be replaced
	// without touching any other document's.
	DeleteDocumentChunks(ctx context.Context, instanceID, itemKey, attachmentKey string) error
	CountChunks(ctx context.Context, instanceID string) (int, error)
	// LoadAll returns every chunk (with its embedding) for an instance, for
	// brute-force vector comparison. Sized for a personal reference library,
	// not for building an ANN index.
	LoadAll(ctx context.Context, instanceID string) ([]domain.IndexChunk, error)
	// BM25Search returns chunk IDs ranked by full-text relevance, best first.
	BM25Search(ctx context.Context, instanceID, query string, limit int) ([]int64, error)

	// ListDocuments returns the incremental-reindex bookkeeping row for every
	// document currently indexed for an instance, keyed by (item key,
	// attachment key) so an "Update index" run can diff a fresh crawl against
	// what is already stored.
	ListDocuments(ctx context.Context, instanceID string) ([]domain.IndexDocument, error)
	// UpsertDocument records (or updates) one document's content hash and
	// chunk count after it has been (re)indexed.
	UpsertDocument(ctx context.Context, doc domain.IndexDocument) error
	// DeleteDocument removes one document's bookkeeping row — paired with
	// DeleteDocumentChunks when pruning a document no longer seen upstream.
	DeleteDocument(ctx context.Context, instanceID, itemKey, attachmentKey string) error

	// GetMeta returns which embedder model and dimension built an instance's
	// current index, or ok=false if the instance has never been indexed.
	GetMeta(ctx context.Context, instanceID string) (meta domain.IndexMeta, ok bool, err error)
	// SetMeta records which embedder model and dimension the index is now
	// built with.
	SetMeta(ctx context.Context, meta domain.IndexMeta) error
}

// PlatformRepository persists platform-wide settings that apply to every
// instance rather than to one of them — currently just the shared semantic
// search embedder configuration (see domain.EmbedderSettings). Its API key,
// when the embedder needs one, is stored separately as ciphertext, the same
// way an instance secret is.
type PlatformRepository interface {
	GetEmbedderSettings(ctx context.Context) (domain.EmbedderSettings, error)
	SetEmbedderSettings(ctx context.Context, s domain.EmbedderSettings) error

	GetEmbedderAPIKey(ctx context.Context) (ciphertext []byte, ok bool, err error)
	SetEmbedderAPIKey(ctx context.Context, ciphertext []byte) error
	DeleteEmbedderAPIKey(ctx context.Context) error
}
