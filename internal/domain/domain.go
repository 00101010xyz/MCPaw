// Package domain contains the core entities and value objects of MCPaw.
//
// It is the innermost layer of the architecture: it models the problem space
// (connectors, instances, tokens, users) and deliberately imports nothing from
// the rest of the project. Every other package may depend on domain; domain
// depends on none of them.
package domain

import (
	"errors"
	"time"
)

// Sentinel errors. Adapters translate infrastructure failures into these so
// that application services can make decisions without knowing whether the
// store is SQLite, Postgres or a test fake.
var (
	ErrNotFound      = errors.New("not found")
	ErrConflict      = errors.New("conflict")
	ErrInvalidInput  = errors.New("invalid input")
	ErrUnauthorized  = errors.New("unauthorized")
	ErrForbidden     = errors.New("forbidden")
	ErrDisabled      = errors.New("disabled")
	ErrRateLimited   = errors.New("rate limited")
	ErrUpstream      = errors.New("upstream failure")
	ErrEgressBlocked = errors.New("egress blocked by policy")
)

// Role enumerates administrative privilege levels for the web UI and admin API.
type Role string

const (
	// RoleAdmin may change any configuration, including egress policy.
	RoleAdmin Role = "admin"
	// RoleViewer may read configuration but never mutate it or reveal secrets.
	RoleViewer Role = "viewer"
)

// Valid reports whether r is a role this build understands.
func (r Role) Valid() bool { return r == RoleAdmin || r == RoleViewer }

// User is an administrator of the platform. Only the password *hash* is ever
// held in memory; the plaintext password never becomes a field of this struct.
type User struct {
	ID           string
	Email        string
	PasswordHash string
	Role         Role
	Disabled     bool
	CreatedAt    time.Time
	UpdatedAt    time.Time
	LastLoginAt  *time.Time
}

// Session is a server-side web session. ID is the SHA-256 hash of the cookie
// value, so a database leak does not yield usable session cookies.
type Session struct {
	ID         string
	UserID     string
	CSRFToken  string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
	IP         string
	UserAgent  string
}

// Expired reports whether the session has passed its absolute expiry.
func (s *Session) Expired(now time.Time) bool { return !now.Before(s.ExpiresAt) }

// ConnectorSource records where a connector manifest came from, which drives
// whether it may be edited or deleted.
type ConnectorSource string

const (
	// SourceBuiltin manifests are embedded in the binary and are read-only.
	SourceBuiltin ConnectorSource = "builtin"
	// SourceManifest manifests were uploaded by an administrator.
	SourceManifest ConnectorSource = "manifest"
	// SourceOpenAPI manifests were translated from an OpenAPI document.
	SourceOpenAPI ConnectorSource = "openapi"
)

// ConnectorRecord is the persisted form of a connector: the raw manifest plus
// provenance. The parsed representation lives in the connector package, which
// keeps YAML concerns out of the domain.
type ConnectorRecord struct {
	ID        string
	Name      string
	Version   string
	Source    ConnectorSource
	Manifest  []byte
	Checksum  string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Editable reports whether an administrator may modify or delete this connector.
func (c *ConnectorRecord) Editable() bool { return c.Source != SourceBuiltin }

// Instance is a connector bound to a concrete deployment of the API it
// describes: a resolved base URL, variable values, secrets, egress policy and
// resource limits. It is what gets served at /mcp/{slug}.
type Instance struct {
	ID          string
	Slug        string
	Name        string
	Description string
	ConnectorID string

	BaseURL   string
	Variables map[string]string

	Enabled bool

	// AllowPrivateNetwork is the explicit, audited opt-in that permits egress
	// to loopback and RFC1918-style addresses. It defaults to false and must be
	// enabled deliberately — the Zotero local API needs it.
	AllowPrivateNetwork bool

	TimeoutMS        int
	RateLimitPerMin  int
	MaxConcurrent    int
	MaxResponseBytes int64

	CreatedAt time.Time
	UpdatedAt time.Time
	// Version increments on every write and is used to invalidate the compiled
	// instance cache on the hot path.
	Version int64
}

// Timeout returns the per-request upstream timeout as a duration.
func (i *Instance) Timeout() time.Duration { return time.Duration(i.TimeoutMS) * time.Millisecond }

// InstanceSecret is an encrypted credential belonging to an instance. Plaintext
// is never persisted and never leaves the process.
type InstanceSecret struct {
	InstanceID string
	Name       string
	Ciphertext []byte
	UpdatedAt  time.Time
}

// ToolBinding records an administrator's decision about one tool of one
// instance. Absence of a binding means "follow the connector default".
type ToolBinding struct {
	InstanceID string
	ToolName   string
	Enabled    bool
}

// Token is a bearer credential presented by an MCP client. Only the SHA-256
// hash is stored; the plaintext is displayed exactly once at creation.
type Token struct {
	ID     string
	Name   string
	Hash   string
	Prefix string
	// InstanceID scopes the token to a single instance. Empty means the token
	// is valid for every enabled instance.
	InstanceID string
	CreatedBy  string
	CreatedAt  time.Time
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
}

// Usable reports whether the token may currently authenticate a request.
func (t *Token) Usable(now time.Time) bool {
	if t.RevokedAt != nil {
		return false
	}
	if t.ExpiresAt != nil && !now.Before(*t.ExpiresAt) {
		return false
	}
	return true
}

// Scopes reports whether the token grants access to the given instance.
func (t *Token) Scopes(instanceID string) bool {
	return t.InstanceID == "" || t.InstanceID == instanceID
}

// AuditEvent is one entry of the append-only administrative audit trail.
type AuditEvent struct {
	ID         string
	At         time.Time
	ActorType  string
	ActorID    string
	Action     string
	TargetType string
	TargetID   string
	Result     string
	IP         string
	Detail     map[string]any
}

// Audit action names. Kept as constants so the set of auditable operations is
// discoverable and greppable rather than scattered as string literals.
const (
	ActionLogin              = "auth.login"
	ActionLoginFailed        = "auth.login_failed"
	ActionLogout             = "auth.logout"
	ActionUserCreate         = "user.create"
	ActionUserUpdate         = "user.update"
	ActionInstanceCreate     = "instance.create"
	ActionInstanceUpdate     = "instance.update"
	ActionInstanceDelete     = "instance.delete"
	ActionInstanceSecretSet  = "instance.secret_set"
	ActionInstanceEgressOpen = "instance.egress_private_enabled"
	ActionInstanceTest       = "instance.test"
	ActionConnectorImport    = "connector.import"
	ActionConnectorDelete    = "connector.delete"
	ActionTokenCreate        = "token.create"
	ActionTokenRevoke        = "token.revoke"
	ActionToolCall           = "tool.call"
)
