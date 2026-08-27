package sqlitestore

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/00101010xyz/mcpaw/internal/domain"
	"github.com/00101010xyz/mcpaw/internal/store"
)

type auditRepo struct{ base }

const auditColumns = `id, at, actor_type, actor_id, action, target_type, target_id, result, ip, detail`

// maxAuditListLimit bounds a single audit query so a malicious or mistaken
// limit parameter cannot turn the admin UI into a memory-exhaustion vector.
const maxAuditListLimit = 500

func (r *auditRepo) Append(ctx context.Context, e *domain.AuditEvent) error {
	detail := "{}"
	if len(e.Detail) > 0 {
		b, err := json.Marshal(e.Detail)
		if err != nil {
			return fmt.Errorf("sqlitestore: encoding audit detail: %w", err)
		}
		detail = string(b)
	}
	_, err := r.write.ExecContext(ctx,
		`INSERT INTO audit_log (`+auditColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, formatTime(e.At), e.ActorType, e.ActorID, e.Action, e.TargetType, e.TargetID,
		e.Result, e.IP, detail)
	return translate(err, "append audit event")
}

func (r *auditRepo) List(ctx context.Context, f store.AuditFilter) ([]*domain.AuditEvent, error) {
	var (
		where []string
		args  []any
	)
	if f.Action != "" {
		where = append(where, "action = ?")
		args = append(args, f.Action)
	}
	if f.ActorID != "" {
		where = append(where, "actor_id = ?")
		args = append(args, f.ActorID)
	}
	if f.TargetID != "" {
		where = append(where, "target_id = ?")
		args = append(args, f.TargetID)
	}
	if f.Before != nil {
		where = append(where, "at < ?")
		args = append(args, formatTime(*f.Before))
	}

	limit := f.Limit
	if limit <= 0 || limit > maxAuditListLimit {
		limit = 100
	}
	q := `SELECT ` + auditColumns + ` FROM audit_log`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += " ORDER BY at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := r.read.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, translate(err, "list audit events")
	}
	defer rows.Close()
	var out []*domain.AuditEvent
	for rows.Next() {
		var (
			e      domain.AuditEvent
			at     string
			detail string
		)
		if err := rows.Scan(&e.ID, &at, &e.ActorType, &e.ActorID, &e.Action, &e.TargetType,
			&e.TargetID, &e.Result, &e.IP, &detail); err != nil {
			return nil, translate(err, "scan audit event")
		}
		if e.At, err = parseTime(at); err != nil {
			return nil, err
		}
		e.Detail = map[string]any{}
		if detail != "" {
			// A malformed detail blob must not hide the rest of the audit
			// trail, so decoding failures degrade to an empty map.
			_ = json.Unmarshal([]byte(detail), &e.Detail)
		}
		out = append(out, &e)
	}
	return out, translate(rows.Err(), "list audit events")
}

func (r *auditRepo) Prune(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := r.write.ExecContext(ctx, `DELETE FROM audit_log WHERE at < ?`, formatTime(olderThan))
	if err != nil {
		return 0, translate(err, "prune audit log")
	}
	n, err := res.RowsAffected()
	return n, translate(err, "prune audit log")
}
