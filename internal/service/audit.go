// Package service holds MCPaw's application services: the use cases that
// coordinate the domain, the persistence ports and the execution engine.
//
// Services are the only place business rules live. HTTP handlers translate
// requests into service calls and translate results back; they contain no
// policy of their own, which keeps the rules testable without a server and
// identical whether they are reached from the web UI or the JSON API.
package service

import (
	"context"
	"log/slog"
	"time"

	"github.com/00101010xyz/mcpaw/internal/domain"
	"github.com/00101010xyz/mcpaw/internal/platform/id"
	"github.com/00101010xyz/mcpaw/internal/platform/logging"
	"github.com/00101010xyz/mcpaw/internal/store"
)

// Actor identifies who is performing an action, for the audit trail.
type Actor struct {
	Type string
	ID   string
	IP   string
}

// Actor types.
const (
	ActorUser   = "user"
	ActorToken  = "token"
	ActorSystem = "system"
)

// SystemActor is used for actions the platform takes on its own behalf, such as
// syncing built-in connectors at startup.
func SystemActor() Actor { return Actor{Type: ActorSystem, ID: "mcpaw"} }

// Audit records security-relevant actions.
type Audit struct {
	repo   store.AuditRepository
	logger *slog.Logger
	now    func() time.Time
}

// NewAudit constructs the audit service.
func NewAudit(repo store.AuditRepository, logger *slog.Logger) *Audit {
	if logger == nil {
		logger = slog.Default()
	}
	return &Audit{repo: repo, logger: logger, now: time.Now}
}

// Record appends an audit event.
//
// Failures are logged but never propagated: an audit write must not be able to
// fail the operation it describes, or an attacker could use a full disk to turn
// auditing off by making every audited action error out. The log line is the
// fallback record.
func (a *Audit) Record(ctx context.Context, actor Actor, action, targetType, targetID, result string, detail map[string]any) {
	event := &domain.AuditEvent{
		ID:         id.New("aud"),
		At:         a.now().UTC(),
		ActorType:  actor.Type,
		ActorID:    actor.ID,
		Action:     action,
		TargetType: targetType,
		TargetID:   targetID,
		Result:     result,
		IP:         actor.IP,
		Detail:     detail,
	}
	if err := a.repo.Append(ctx, event); err != nil {
		logging.FromContext(ctx).Error("could not write audit event",
			slog.String("action", action),
			slog.String("target_id", targetID),
			slog.String("error", err.Error()))
	}
}

// Success records a successful action.
func (a *Audit) Success(ctx context.Context, actor Actor, action, targetType, targetID string, detail map[string]any) {
	a.Record(ctx, actor, action, targetType, targetID, "success", detail)
}

// Failure records a rejected or failed action.
func (a *Audit) Failure(ctx context.Context, actor Actor, action, targetType, targetID string, reason string) {
	a.Record(ctx, actor, action, targetType, targetID, "failure", map[string]any{"reason": reason})
}

// List returns audit events matching a filter.
func (a *Audit) List(ctx context.Context, filter store.AuditFilter) ([]*domain.AuditEvent, error) {
	return a.repo.List(ctx, filter)
}

// Prune removes events older than the retention window.
func (a *Audit) Prune(ctx context.Context, retention time.Duration) (int64, error) {
	return a.repo.Prune(ctx, a.now().Add(-retention))
}
