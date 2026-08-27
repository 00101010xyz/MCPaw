package sqlitestore

import (
	"context"
	"database/sql"
	"time"

	"github.com/00101010xyz/mcpaw/internal/domain"
)

type tokenRepo struct{ base }

const tokenColumns = `id, name, lookup_key, prefix, instance_id, created_by, created_at, expires_at, last_used_at, revoked_at`

func (r *tokenRepo) Create(ctx context.Context, t *domain.Token) error {
	_, err := r.write.ExecContext(ctx,
		`INSERT INTO tokens (`+tokenColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.ID, t.Name, t.Hash, t.Prefix, t.InstanceID, t.CreatedBy, formatTime(t.CreatedAt),
		formatTimePtr(t.ExpiresAt), formatTimePtr(t.LastUsedAt), formatTimePtr(t.RevokedAt))
	return translate(err, "create token")
}

// GetByLookupKey resolves a presented bearer token by its keyed digest. The
// unique index makes this an O(log n) index probe, so authentication cost does
// not depend on how many tokens exist and leaks no timing information about
// which token was presented.
func (r *tokenRepo) GetByLookupKey(ctx context.Context, key string) (*domain.Token, error) {
	row := r.read.QueryRowContext(ctx, `SELECT `+tokenColumns+` FROM tokens WHERE lookup_key = ?`, key)
	return scanToken(row)
}

func (r *tokenRepo) List(ctx context.Context) ([]*domain.Token, error) {
	rows, err := r.read.QueryContext(ctx, `SELECT `+tokenColumns+` FROM tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, translate(err, "list tokens")
	}
	defer rows.Close()
	var out []*domain.Token
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, translate(rows.Err(), "list tokens")
}

// Revoke is idempotent on already-revoked tokens: revocation must never fail in
// a way that tempts an operator to skip it.
func (r *tokenRepo) Revoke(ctx context.Context, id string, at time.Time) error {
	res, err := r.write.ExecContext(ctx,
		`UPDATE tokens SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ?`, formatTime(at), id)
	if err != nil {
		return translate(err, "revoke token")
	}
	return requireAffected(res, "revoke token")
}

func (r *tokenRepo) TouchLastUsed(ctx context.Context, id string, at time.Time) error {
	_, err := r.write.ExecContext(ctx, `UPDATE tokens SET last_used_at = ? WHERE id = ?`, formatTime(at), id)
	return translate(err, "touch token")
}

func (r *tokenRepo) DeleteByInstance(ctx context.Context, instanceID string) error {
	_, err := r.write.ExecContext(ctx, `DELETE FROM tokens WHERE instance_id = ?`, instanceID)
	return translate(err, "delete instance tokens")
}

func scanToken(s scanner) (*domain.Token, error) {
	var (
		t                              domain.Token
		created                        string
		expires, lastUsed, revokedAtNS sql.NullString
	)
	if err := s.Scan(&t.ID, &t.Name, &t.Hash, &t.Prefix, &t.InstanceID, &t.CreatedBy,
		&created, &expires, &lastUsed, &revokedAtNS); err != nil {
		return nil, translate(err, "scan token")
	}
	var err error
	if t.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if t.ExpiresAt, err = parseTimePtr(expires); err != nil {
		return nil, err
	}
	if t.LastUsedAt, err = parseTimePtr(lastUsed); err != nil {
		return nil, err
	}
	if t.RevokedAt, err = parseTimePtr(revokedAtNS); err != nil {
		return nil, err
	}
	return &t, nil
}
