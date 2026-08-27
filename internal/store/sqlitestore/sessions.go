package sqlitestore

import (
	"context"
	"time"

	"github.com/00101010xyz/mcpaw/internal/domain"
)

type sessionRepo struct{ base }

const sessionColumns = `id, user_id, csrf_token, created_at, last_seen_at, expires_at, ip, user_agent`

func (r *sessionRepo) Create(ctx context.Context, s *domain.Session) error {
	_, err := r.write.ExecContext(ctx,
		`INSERT INTO sessions (`+sessionColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		s.ID, s.UserID, s.CSRFToken, formatTime(s.CreatedAt), formatTime(s.LastSeenAt),
		formatTime(s.ExpiresAt), s.IP, s.UserAgent)
	return translate(err, "create session")
}

func (r *sessionRepo) Get(ctx context.Context, id string) (*domain.Session, error) {
	row := r.read.QueryRowContext(ctx, `SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id)
	var (
		s                          domain.Session
		created, lastSeen, expires string
	)
	if err := row.Scan(&s.ID, &s.UserID, &s.CSRFToken, &created, &lastSeen, &expires, &s.IP, &s.UserAgent); err != nil {
		return nil, translate(err, "get session")
	}
	var err error
	if s.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if s.LastSeenAt, err = parseTime(lastSeen); err != nil {
		return nil, err
	}
	if s.ExpiresAt, err = parseTime(expires); err != nil {
		return nil, err
	}
	return &s, nil
}

func (r *sessionRepo) Touch(ctx context.Context, id string, lastSeen, expiresAt time.Time) error {
	res, err := r.write.ExecContext(ctx,
		`UPDATE sessions SET last_seen_at = ?, expires_at = ? WHERE id = ?`,
		formatTime(lastSeen), formatTime(expiresAt), id)
	if err != nil {
		return translate(err, "touch session")
	}
	return requireAffected(res, "touch session")
}

func (r *sessionRepo) Delete(ctx context.Context, id string) error {
	_, err := r.write.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id)
	return translate(err, "delete session")
}

func (r *sessionRepo) DeleteByUser(ctx context.Context, userID string) error {
	_, err := r.write.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	return translate(err, "delete user sessions")
}

func (r *sessionRepo) DeleteExpired(ctx context.Context, now time.Time) (int64, error) {
	res, err := r.write.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at <= ?`, formatTime(now))
	if err != nil {
		return 0, translate(err, "prune sessions")
	}
	n, err := res.RowsAffected()
	return n, translate(err, "prune sessions")
}
