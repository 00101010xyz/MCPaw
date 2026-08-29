package service

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/00101010xyz/mcpaw/internal/domain"
	"github.com/00101010xyz/mcpaw/internal/engine"
	"github.com/00101010xyz/mcpaw/internal/platform/id"
	"github.com/00101010xyz/mcpaw/internal/usage"
)

// Usage records MCP tool calls into the platform's usage log — a separate,
// size-capped record of what has been called, by what, and with what
// outcome, distinct from Audit's security trail: Audit keeps every
// administrative action for as long as auditRetention says, deliberately
// never the tool arguments or response; Usage exists specifically so an
// operator can see request activity, and is configurable down to bare call
// metadata or off entirely, because a "full" call log is exactly the kind of
// thing that can carry the content of a caller's queries.
type Usage struct {
	store  *usage.Store
	audit  *Audit
	logger *slog.Logger
}

// NewUsage constructs the usage-log service. store may be nil, in which case
// every method is a safe no-op — usage logging is purely additive and its
// absence changes nothing else about the platform.
func NewUsage(store *usage.Store, audit *Audit, logger *slog.Logger) *Usage {
	if logger == nil {
		logger = slog.Default()
	}
	return &Usage{store: store, audit: audit, logger: logger}
}

// maxLoggedFieldBytes bounds how much of a single call's arguments or result
// is stored at LevelFull: large enough to be useful for debugging, small
// enough that one oversized response can't dominate the log's size budget by
// itself.
const maxLoggedFieldBytes = 32 * 1024

// Record logs one completed tool call, according to the configured level.
// Nil-safe and best-effort: a logging failure must never affect the call it
// describes, so every error here is logged and swallowed, exactly like
// Audit.Record.
func (u *Usage) Record(ctx context.Context, actor Actor, resolved *Resolved, tool string, args map[string]any, result *engine.Result, execErr error, elapsed time.Duration) {
	if u == nil || u.store == nil {
		return
	}
	settings, err := u.store.Settings(ctx)
	if err != nil {
		u.logger.Warn("could not read usage log settings", slog.String("error", err.Error()))
		return
	}
	if settings.Level == usage.LevelOff {
		return
	}

	entry := &usage.Entry{
		ID: id.New("use"), At: time.Now().UTC(),
		InstanceID: resolved.Instance.ID, InstanceName: resolved.Instance.Name,
		Tool: tool, ActorType: actor.Type, ActorID: actor.ID, IP: actor.IP,
		DurationMs: elapsed.Milliseconds(),
	}
	if execErr != nil {
		entry.Success = false
		entry.Kind = engine.KindInternal
		if e, ok := engine.AsError(execErr); ok {
			entry.Kind = e.Kind
		}
		entry.Error = execErr.Error()
	} else {
		entry.Success = true
		if result != nil {
			entry.StatusCode = result.StatusCode
		}
	}

	if settings.Level == usage.LevelFull {
		if len(args) > 0 {
			if encoded, err := json.Marshal(args); err == nil {
				entry.Args = truncateLoggedField(string(encoded))
			}
		}
		if result != nil {
			entry.Result = truncateLoggedField(result.Text)
		}
	}

	if err := u.store.Append(ctx, entry); err != nil {
		u.logger.Warn("could not append usage log entry", slog.String("error", err.Error()))
	}
}

func truncateLoggedField(s string) string {
	if len(s) <= maxLoggedFieldBytes {
		return s
	}
	return s[:maxLoggedFieldBytes] + "\n… [truncated]"
}

// List returns recent usage log entries.
func (u *Usage) List(ctx context.Context, filter usage.Filter) ([]*usage.Entry, error) {
	if u == nil || u.store == nil {
		return nil, nil
	}
	return u.store.List(ctx, filter)
}

// Settings returns the platform-wide usage logging configuration.
func (u *Usage) Settings(ctx context.Context) (usage.Settings, error) {
	if u == nil || u.store == nil {
		return usage.Settings{Level: usage.LevelOff}, nil
	}
	return u.store.Settings(ctx)
}

// UpdateSettings changes the log level and/or size cap.
func (u *Usage) UpdateSettings(ctx context.Context, actor Actor, level usage.Level, maxBytes int64) error {
	if u == nil || u.store == nil {
		return nil
	}
	if !level.Valid() {
		return domain.ErrInvalidInput
	}
	if err := u.store.UpdateSettings(ctx, usage.Settings{Level: level, MaxBytes: maxBytes}); err != nil {
		return err
	}
	u.audit.Success(ctx, actor, domain.ActionPlatformSettingsUpdate, "platform", "usage_log",
		map[string]any{"level": string(level), "max_bytes": maxBytes})
	return nil
}

// Clear deletes every stored entry immediately.
func (u *Usage) Clear(ctx context.Context, actor Actor) error {
	if u == nil || u.store == nil {
		return nil
	}
	if err := u.store.Clear(ctx); err != nil {
		return err
	}
	u.audit.Success(ctx, actor, domain.ActionPlatformSettingsUpdate, "platform", "usage_log",
		map[string]any{"cleared": true})
	return nil
}

// Prune enforces the configured size cap, deleting the oldest entries first.
// Called periodically by the composition root's background loop, never on
// the request path.
func (u *Usage) Prune(ctx context.Context) (int64, error) {
	if u == nil || u.store == nil {
		return 0, nil
	}
	return u.store.Prune(ctx)
}

// Enabled reports whether a usage store is configured at all, so the web UI
// can hide the feature entirely rather than show a broken page — mirroring
// how Indexer being nil hides semantic search.
func (u *Usage) Enabled() bool { return u != nil && u.store != nil }
