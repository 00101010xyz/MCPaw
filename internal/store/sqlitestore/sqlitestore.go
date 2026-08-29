// Package sqlitestore is the SQLite adapter for the persistence ports declared
// in package store.
//
// It uses a cgo-free driver so the whole platform ships as a static binary on a
// distroless base image. Two connection pools are maintained: SQLite in WAL
// mode supports one writer with concurrent readers, and modelling that
// explicitly avoids the SQLITE_BUSY errors that a naive shared pool produces
// under load.
package sqlitestore

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/00101010xyz/mcpaw/internal/domain"
	"github.com/00101010xyz/mcpaw/internal/store"

	_ "modernc.org/sqlite" // cgo-free SQLite driver
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// Compile-time proof that the adapter satisfies the port. If a method is ever
// removed or its signature drifts, the build fails here rather than at runtime.
var _ store.Repositories = (*Store)(nil)

// Store is the SQLite-backed implementation of store.Repositories.
type Store struct {
	write *sql.DB
	read  *sql.DB

	users      *userRepo
	sessions   *sessionRepo
	connectors *connectorRepo
	instances  *instanceRepo
	tokens     *tokenRepo
	audit      *auditRepo
	searchIdx  *searchIndexRepo
	platform   *platformRepo
}

// Open connects to (and if necessary creates) the SQLite database at path,
// applies pending migrations and returns a ready Store.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("sqlitestore: database path must not be empty")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("sqlitestore: creating %s: %w", dir, err)
		}
	}

	write, err := openPool(path, 1)
	if err != nil {
		return nil, err
	}
	readers := runtime.GOMAXPROCS(0)
	if readers < 2 {
		readers = 2
	}
	read, err := openPool(path, readers)
	if err != nil {
		_ = write.Close()
		return nil, err
	}

	s := &Store{write: write, read: read}
	if err := s.migrate(ctx); err != nil {
		_ = s.Close()
		return nil, err
	}

	s.users = &userRepo{base{read: read, write: write}}
	s.sessions = &sessionRepo{base{read: read, write: write}}
	s.connectors = &connectorRepo{base{read: read, write: write}}
	s.instances = &instanceRepo{base{read: read, write: write}}
	s.tokens = &tokenRepo{base{read: read, write: write}}
	s.audit = &auditRepo{base{read: read, write: write}}
	s.searchIdx = &searchIndexRepo{base{read: read, write: write}}
	s.platform = &platformRepo{base{read: read, write: write}}
	return s, nil
}

// pragmas are applied to every connection. busy_timeout converts lock
// contention into a bounded wait instead of an immediate error; foreign_keys
// makes the ON DELETE CASCADE declarations in the schema actually take effect
// (SQLite disables them by default, a classic silent-data-corruption footgun).
const pragmas = "?_pragma=journal_mode(WAL)" +
	"&_pragma=busy_timeout(5000)" +
	"&_pragma=foreign_keys(1)" +
	"&_pragma=synchronous(NORMAL)" +
	"&_time_format=sqlite"

func openPool(path string, maxOpen int) (*sql.DB, error) {
	db, err := sql.Open("sqlite", "file:"+path+pragmas)
	if err != nil {
		return nil, fmt.Errorf("sqlitestore: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxOpen)
	db.SetConnMaxIdleTime(5 * time.Minute)
	return db, nil
}

// Users returns the user repository.
func (s *Store) Users() store.UserRepository { return s.users }

// Sessions returns the session repository.
func (s *Store) Sessions() store.SessionRepository { return s.sessions }

// Connectors returns the connector repository.
func (s *Store) Connectors() store.ConnectorRepository { return s.connectors }

// Instances returns the instance repository.
func (s *Store) Instances() store.InstanceRepository { return s.instances }

// Tokens returns the token repository.
func (s *Store) Tokens() store.TokenRepository { return s.tokens }

// Audit returns the audit repository.
func (s *Store) Audit() store.AuditRepository { return s.audit }

// SearchIndex exposes the semantic-search index repository.
func (s *Store) SearchIndex() store.SearchIndexRepository { return s.searchIdx }

// Platform exposes platform-wide settings (currently just the shared
// semantic-search embedder configuration).
func (s *Store) Platform() store.PlatformRepository { return s.platform }

// Ping verifies the database is reachable.
func (s *Store) Ping(ctx context.Context) error {
	if err := s.read.PingContext(ctx); err != nil {
		return fmt.Errorf("sqlitestore: ping: %w", err)
	}
	return nil
}

// Close shuts both pools down, reporting the first failure.
func (s *Store) Close() error {
	var errs []error
	if s.read != nil {
		errs = append(errs, s.read.Close())
	}
	if s.write != nil {
		errs = append(errs, s.write.Close())
	}
	return errors.Join(errs...)
}

// migrate applies every embedded migration that has not yet been recorded, each
// inside its own transaction so a failure cannot leave a half-applied schema.
func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.write.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS schema_migrations (
			name TEXT PRIMARY KEY,
			applied_at TEXT NOT NULL
		) STRICT`); err != nil {
		return fmt.Errorf("sqlitestore: creating migration table: %w", err)
	}

	applied := map[string]bool{}
	rows, err := s.write.QueryContext(ctx, `SELECT name FROM schema_migrations`)
	if err != nil {
		return fmt.Errorf("sqlitestore: reading applied migrations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return fmt.Errorf("sqlitestore: scanning migration row: %w", err)
		}
		applied[n] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("sqlitestore: iterating migrations: %w", err)
	}

	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return fmt.Errorf("sqlitestore: listing migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		if applied[name] {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("sqlitestore: reading migration %s: %w", name, err)
		}
		if err := s.applyMigration(ctx, name, string(body)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) applyMigration(ctx context.Context, name, body string) error {
	tx, err := s.write.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("sqlitestore: begin migration %s: %w", name, err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, body); err != nil {
		return fmt.Errorf("sqlitestore: applying migration %s: %w", name, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO schema_migrations (name, applied_at) VALUES (?, ?)`,
		name, formatTime(time.Now().UTC())); err != nil {
		return fmt.Errorf("sqlitestore: recording migration %s: %w", name, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("sqlitestore: committing migration %s: %w", name, err)
	}
	return nil
}

// base carries the two pools shared by every repository.
type base struct {
	read  *sql.DB
	write *sql.DB
}

const timeLayout = time.RFC3339Nano

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func formatTimePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return formatTime(*t)
}

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("sqlitestore: parsing timestamp %q: %w", s, err)
	}
	return t.UTC(), nil
}

func parseTimePtr(s sql.NullString) (*time.Time, error) {
	if !s.Valid || s.String == "" {
		return nil, nil
	}
	t, err := parseTime(s.String)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// translate maps driver-level failures onto domain sentinel errors so callers
// never need to know SQLite's error vocabulary.
func translate(err error, what string) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s: %w", what, domain.ErrNotFound)
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "UNIQUE constraint failed"):
		return fmt.Errorf("%s: %w", what, domain.ErrConflict)
	case strings.Contains(msg, "FOREIGN KEY constraint failed"):
		return fmt.Errorf("%s: %w", what, domain.ErrConflict)
	case strings.Contains(msg, "CHECK constraint failed"):
		return fmt.Errorf("%s: %w", what, domain.ErrInvalidInput)
	}
	return fmt.Errorf("%s: %w", what, err)
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
