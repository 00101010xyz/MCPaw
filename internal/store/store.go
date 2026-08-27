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

// SearchIndexRepository persists the semantic-search index. An instance's
// embedder configuration is not stored here: it travels as an ordinary
// connector variable/secret, so it reuses the existing sealing, validation
// and web UI rather than a parallel configuration path.
type SearchIndexRepository interface {
	// ClearInstance removes every chunk belonging to an instance. A reindex
	// always starts here so stale chunks from a prior model or a deleted
	// item can never linger and be served as if current.
	ClearInstance(ctx context.Context, instanceID string) error
	// InsertChunks appends chunks, incrementally during a reindex, so a
	// crash or timeout partway through leaves a partial-but-usable index
	// rather than nothing.
	InsertChunks(ctx context.Context, instanceID string, chunks []domain.IndexChunk) error
	CountChunks(ctx context.Context, instanceID string) (int, error)
	// LoadAll returns every chunk (with its embedding) for an instance, for
	// brute-force vector comparison. Sized for a personal reference library,
	// not for building an ANN index.
	LoadAll(ctx context.Context, instanceID string) ([]domain.IndexChunk, error)
	// BM25Search returns chunk IDs ranked by full-text relevance, best first.
	BM25Search(ctx context.Context, instanceID, query string, limit int) ([]int64, error)
}
