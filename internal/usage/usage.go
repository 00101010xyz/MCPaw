// Package usage stores a bounded log of MCP tool calls in its own SQLite
// database (usage.db), kept entirely separate from the platform's own data
// (mcpaw.db). Two things follow from that separation:
//
//   - it can be capped by size and rotated (see Store.Prune) without any risk
//     to the platform's own tables, and
//   - an operator who wants to inspect or wipe call history can do so without
//     touching anything mcpaw.db is responsible for.
//
// Unlike sqlitestore, this package has no migration ledger: the schema is two
// small, independent tables with nothing yet to evolve, so a single
// idempotent CREATE TABLE IF NOT EXISTS is simpler and just as safe. A real
// migration mechanism can be added the day this schema actually needs one.
package usage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // cgo-free SQLite driver
)

// Level controls how much detail Record keeps for a call.
type Level string

const (
	// LevelOff records nothing at all.
	LevelOff Level = "off"
	// LevelMetadata records that a call happened — instance, tool, actor,
	// outcome, timing — but never the arguments or response, which can carry
	// a caller's queries or another connector's personal data.
	LevelMetadata Level = "metadata"
	// LevelFull additionally records the call's arguments and rendered
	// result, bounded per entry (see service.maxLoggedFieldBytes).
	LevelFull Level = "full"
)

// Valid reports whether l is one of the known levels.
func (l Level) Valid() bool {
	switch l {
	case LevelOff, LevelMetadata, LevelFull:
		return true
	}
	return false
}

// DefaultMaxBytes is the log's target size cap: large enough to hold a
// meaningful window of activity, small enough that an operator never has to
// think about disk usage. ~1 GiB, per the feature's own request.
const DefaultMaxBytes int64 = 1 << 30

// Settings is the platform-wide usage-logging configuration.
type Settings struct {
	Level    Level
	MaxBytes int64
}

// Entry is one recorded MCP tool call.
type Entry struct {
	ID           string
	At           time.Time
	InstanceID   string
	InstanceName string
	Tool         string
	ActorType    string
	ActorID      string
	IP           string
	Success      bool
	StatusCode   int
	Kind         string
	DurationMs   int64
	// Args and Result are populated only at LevelFull; at LevelMetadata (or
	// on a failed call with no result) they are empty strings, never null, so
	// a template can render them unconditionally.
	Args   string
	Result string
	Error  string
}

// Filter narrows a List call.
type Filter struct {
	InstanceID string
	Tool       string
	Limit      int
}

// maxListLimit bounds how many rows one List call can return, independent of
// what a caller asks for, so a webui page can never turn into an unbounded
// table scan.
const maxListLimit = 500

// Store is the SQLite-backed usage log.
type Store struct {
	db   *sql.DB
	path string
}

// pragmas apply to every connection at open time. busy_timeout converts lock
// contention into a bounded wait rather than an immediate error.
//
// journal_mode is deliberately not set here: switching to WAL persists a
// flag into the database's first page, and once that page exists,
// auto_vacuum can no longer be enabled on the file. Open sets auto_vacuum
// and journal_mode explicitly, in that order, on one pinned connection
// before anything else touches the database, so a brand new file ends up
// with both — see the comment there.
const pragmas = "?_pragma=busy_timeout(5000)" +
	"&_pragma=synchronous(NORMAL)"

const schemaSQL = `
CREATE TABLE IF NOT EXISTS settings (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	level TEXT NOT NULL DEFAULT 'metadata',
	max_bytes INTEGER NOT NULL DEFAULT 1073741824
) STRICT;
INSERT OR IGNORE INTO settings (id, level, max_bytes) VALUES (1, 'metadata', 1073741824);

CREATE TABLE IF NOT EXISTS entries (
	id TEXT PRIMARY KEY,
	at TEXT NOT NULL,
	instance_id TEXT NOT NULL,
	instance_name TEXT NOT NULL,
	tool TEXT NOT NULL,
	actor_type TEXT NOT NULL,
	actor_id TEXT NOT NULL,
	ip TEXT NOT NULL,
	success INTEGER NOT NULL,
	status_code INTEGER NOT NULL,
	kind TEXT NOT NULL DEFAULT '',
	duration_ms INTEGER NOT NULL,
	args TEXT NOT NULL DEFAULT '',
	result TEXT NOT NULL DEFAULT '',
	error TEXT NOT NULL DEFAULT ''
) STRICT;
CREATE INDEX IF NOT EXISTS idx_usage_entries_at ON entries(at);
CREATE INDEX IF NOT EXISTS idx_usage_entries_instance ON entries(instance_id, at);
`

// Open connects to (and if necessary creates) the usage database at path.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" {
		return nil, errors.New("usage: database path must not be empty")
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("usage: creating %s: %w", dir, err)
		}
	}

	db, err := sql.Open("sqlite", "file:"+path+pragmas)
	if err != nil {
		return nil, fmt.Errorf("usage: open %s: %w", path, err)
	}

	// auto_vacuum only takes effect on a database with no tables yet — and,
	// critically, before journal_mode is switched to WAL, since that write
	// alone is enough to end the "no tables yet" state auto_vacuum needs.
	// Pinning a single connection guarantees all three statements — the
	// pragma, the mode switch, and the schema creation — run in that order
	// against the exact same freshly opened file, with nothing else able to
	// interleave a write in between. On an existing file (a restart) both
	// pragmas are documented no-ops, and the schema statements are themselves
	// idempotent, so this is safe to repeat on every start.
	conn, err := db.Conn(ctx)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("usage: acquiring connection: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA auto_vacuum = INCREMENTAL`); err != nil {
		_ = conn.Close()
		_ = db.Close()
		return nil, fmt.Errorf("usage: setting auto_vacuum: %w", err)
	}
	if _, err := conn.ExecContext(ctx, `PRAGMA journal_mode = WAL`); err != nil {
		_ = conn.Close()
		_ = db.Close()
		return nil, fmt.Errorf("usage: setting journal_mode: %w", err)
	}
	if _, err := conn.ExecContext(ctx, schemaSQL); err != nil {
		_ = conn.Close()
		_ = db.Close()
		return nil, fmt.Errorf("usage: applying schema: %w", err)
	}
	if err := conn.Close(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("usage: releasing setup connection: %w", err)
	}

	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxIdleTime(5 * time.Minute)
	return &Store{db: db, path: path}, nil
}

// Close releases the database connection.
func (s *Store) Close() error { return s.db.Close() }

// Ping verifies the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

// Settings reads the current log level and size cap.
func (s *Store) Settings(ctx context.Context) (Settings, error) {
	var level string
	var maxBytes int64
	err := s.db.QueryRowContext(ctx, `SELECT level, max_bytes FROM settings WHERE id = 1`).
		Scan(&level, &maxBytes)
	if err != nil {
		return Settings{}, fmt.Errorf("usage: get settings: %w", err)
	}
	return Settings{Level: Level(level), MaxBytes: maxBytes}, nil
}

// UpdateSettings changes the log level and/or size cap.
func (s *Store) UpdateSettings(ctx context.Context, settings Settings) error {
	if !settings.Level.Valid() {
		return fmt.Errorf("usage: invalid level %q", settings.Level)
	}
	if settings.MaxBytes <= 0 {
		settings.MaxBytes = DefaultMaxBytes
	}
	_, err := s.db.ExecContext(ctx, `UPDATE settings SET level = ?, max_bytes = ? WHERE id = 1`,
		string(settings.Level), settings.MaxBytes)
	if err != nil {
		return fmt.Errorf("usage: update settings: %w", err)
	}
	return nil
}

// Append records one entry.
func (s *Store) Append(ctx context.Context, e *Entry) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO entries
			(id, at, instance_id, instance_name, tool, actor_type, actor_id, ip,
			 success, status_code, kind, duration_ms, args, result, error)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, formatTime(e.At), e.InstanceID, e.InstanceName, e.Tool, e.ActorType, e.ActorID, e.IP,
		boolToInt(e.Success), e.StatusCode, e.Kind, e.DurationMs, e.Args, e.Result, e.Error)
	if err != nil {
		return fmt.Errorf("usage: append entry: %w", err)
	}
	return nil
}

// List returns recent entries, most recent first, optionally narrowed by
// instance and/or tool.
func (s *Store) List(ctx context.Context, f Filter) ([]*Entry, error) {
	limit := f.Limit
	if limit <= 0 || limit > maxListLimit {
		limit = maxListLimit
	}

	query := `SELECT id, at, instance_id, instance_name, tool, actor_type, actor_id, ip,
			success, status_code, kind, duration_ms, args, result, error
		FROM entries WHERE 1=1`
	var args []any
	if f.InstanceID != "" {
		query += " AND instance_id = ?"
		args = append(args, f.InstanceID)
	}
	if f.Tool != "" {
		query += " AND tool = ?"
		args = append(args, f.Tool)
	}
	query += " ORDER BY at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("usage: list entries: %w", err)
	}
	defer rows.Close()

	var out []*Entry
	for rows.Next() {
		e := &Entry{}
		var at string
		var success int
		if err := rows.Scan(&e.ID, &at, &e.InstanceID, &e.InstanceName, &e.Tool, &e.ActorType, &e.ActorID, &e.IP,
			&success, &e.StatusCode, &e.Kind, &e.DurationMs, &e.Args, &e.Result, &e.Error); err != nil {
			return nil, fmt.Errorf("usage: scanning entry: %w", err)
		}
		e.Success = success != 0
		if e.At, err = parseTime(at); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("usage: iterating entries: %w", err)
	}
	return out, nil
}

// Clear deletes every stored entry immediately, for an operator who wants a
// clean slate without waiting for the size cap to catch up.
func (s *Store) Clear(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM entries`); err != nil {
		return fmt.Errorf("usage: clear: %w", err)
	}
	return s.reclaim(ctx)
}

// pruneBatch bounds how many rows Prune deletes in a single pass, so that a
// very large overshoot (the cap was just lowered a lot, say) is brought
// under control over a few passes rather than one very long delete.
const pruneBatch = 500

// Prune enforces the configured size cap by deleting the oldest entries
// first, reclaiming the freed space after each pass, until the file is back
// under budget. Each pass estimates how many rows to delete from the
// average row size observed so far, rather than always deleting a full
// pruneBatch — with few, large rows that would delete far more than the cap
// actually requires, up to and including the entire log for an overshoot a
// handful of rows could have fixed.
//
// It is meant to be called periodically (see cmd/mcpaw's prune loop), not
// per request: reclaiming space involves a checkpoint, which is too heavy
// for the one path in the platform that must never be blocked on logging.
func (s *Store) Prune(ctx context.Context) (int64, error) {
	settings, err := s.Settings(ctx)
	if err != nil {
		return 0, err
	}
	maxBytes := settings.MaxBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}

	var total int64
	for {
		size, err := s.fileSize()
		if err != nil {
			return total, err
		}
		if size <= maxBytes {
			break
		}
		count, err := s.rowCount(ctx)
		if err != nil {
			return total, err
		}
		if count == 0 {
			// Nothing left to delete but still over budget — nothing more
			// this pass can do (an oversized single entry that survived its
			// own deletion, most likely, though that should not happen).
			break
		}

		overage := size - maxBytes
		avgBytes := size / int64(count)
		if avgBytes <= 0 {
			avgBytes = 1
		}
		// +1 rounds up rather than down, so a small overage still deletes at
		// least one row instead of stalling forever on an estimate of zero.
		toDelete := int(overage/avgBytes) + 1
		if toDelete > pruneBatch {
			toDelete = pruneBatch
		}
		if int64(toDelete) > count {
			toDelete = int(count)
		}

		deleted, err := s.deleteOldest(ctx, toDelete)
		if err != nil {
			return total, err
		}
		if deleted == 0 {
			break
		}
		total += deleted
		// Deleting rows alone does not shrink the file: reclaiming after each
		// pass is what lets the size check above actually see progress on
		// the next iteration, and what keeps the next pass's average-row-size
		// estimate accurate.
		if err := s.reclaim(ctx); err != nil {
			return total, err
		}
	}
	return total, nil
}

func (s *Store) rowCount(ctx context.Context) (int64, error) {
	var n int64
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM entries`).Scan(&n); err != nil {
		return 0, fmt.Errorf("usage: counting entries: %w", err)
	}
	return n, nil
}

// reclaim moves freed pages out of the file (incremental_vacuum, possible
// because the database was opened with auto_vacuum=INCREMENTAL) and then
// checkpoints the WAL back into the main file with TRUNCATE, which is what
// actually shrinks the file on disk — in WAL mode neither step alone does.
//
// Each PRAGMA incremental_vacuum statement only removes one page, regardless
// of an N argument — that only bounds how many statements are worth issuing,
// not a guarantee any one of them drains the whole backlog — so this loops
// until PRAGMA freelist_count reports nothing left to reclaim. maxSteps
// caps the loop itself, not how much it can free per step, so a very large
// backlog still makes full progress: it just takes more calls of Prune's own
// outer loop (each already followed by a fresh size check) to finish.
func (s *Store) reclaim(ctx context.Context) error {
	const maxSteps = 4096
	for i := 0; i < maxSteps; i++ {
		var freelist int
		if err := s.db.QueryRowContext(ctx, `PRAGMA freelist_count`).Scan(&freelist); err != nil {
			return fmt.Errorf("usage: freelist_count: %w", err)
		}
		if freelist == 0 {
			break
		}
		if _, err := s.db.ExecContext(ctx, `PRAGMA incremental_vacuum`); err != nil {
			return fmt.Errorf("usage: incremental vacuum: %w", err)
		}
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("usage: wal checkpoint: %w", err)
	}
	return nil
}

// fileSize reports the usage database's actual size on disk — the main file
// plus its WAL, since both hold real bytes an operator's disk usage cares
// about. It reads os.Stat directly rather than PRAGMA page_count so it
// reflects what is actually taking up space, independent of how much of it
// has been checkpointed at any given moment.
func (s *Store) fileSize() (int64, error) {
	info, err := os.Stat(s.path)
	if err != nil {
		return 0, fmt.Errorf("usage: stat %s: %w", s.path, err)
	}
	size := info.Size()
	if wal, err := os.Stat(s.path + "-wal"); err == nil {
		size += wal.Size()
	}
	return size, nil
}

func (s *Store) deleteOldest(ctx context.Context, n int) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM entries WHERE id IN (SELECT id FROM entries ORDER BY at ASC LIMIT ?)`, n)
	if err != nil {
		return 0, fmt.Errorf("usage: delete oldest: %w", err)
	}
	return res.RowsAffected()
}

const timeLayout = time.RFC3339Nano

func formatTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(timeLayout, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("usage: parsing timestamp %q: %w", s, err)
	}
	return t.UTC(), nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
