package sqlitestore

import (
	"context"
	"database/sql"
	"strings"

	"github.com/00101010xyz/mcpaw/internal/domain"
)

type userRepo struct{ base }

const userColumns = `id, email, password_hash, role, disabled, created_at, updated_at, last_login_at`

func (r *userRepo) Create(ctx context.Context, u *domain.User) error {
	_, err := r.write.ExecContext(ctx,
		`INSERT INTO users (`+userColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, normaliseEmail(u.Email), u.PasswordHash, string(u.Role), boolToInt(u.Disabled),
		formatTime(u.CreatedAt), formatTime(u.UpdatedAt), formatTimePtr(u.LastLoginAt))
	return translate(err, "create user")
}

func (r *userRepo) Update(ctx context.Context, u *domain.User) error {
	res, err := r.write.ExecContext(ctx,
		`UPDATE users SET email = ?, password_hash = ?, role = ?, disabled = ?, updated_at = ?, last_login_at = ? WHERE id = ?`,
		normaliseEmail(u.Email), u.PasswordHash, string(u.Role), boolToInt(u.Disabled),
		formatTime(u.UpdatedAt), formatTimePtr(u.LastLoginAt), u.ID)
	if err != nil {
		return translate(err, "update user")
	}
	return requireAffected(res, "update user")
}

func (r *userRepo) GetByID(ctx context.Context, id string) (*domain.User, error) {
	row := r.read.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id)
	return scanUser(row)
}

func (r *userRepo) GetByEmail(ctx context.Context, email string) (*domain.User, error) {
	row := r.read.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE email = ? COLLATE NOCASE`, normaliseEmail(email))
	return scanUser(row)
}

func (r *userRepo) List(ctx context.Context) ([]*domain.User, error) {
	rows, err := r.read.QueryContext(ctx, `SELECT `+userColumns+` FROM users ORDER BY created_at ASC`)
	if err != nil {
		return nil, translate(err, "list users")
	}
	defer rows.Close()
	var out []*domain.User
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, translate(rows.Err(), "list users")
}

func (r *userRepo) Count(ctx context.Context) (int, error) {
	var n int
	err := r.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&n)
	return n, translate(err, "count users")
}

// scanner abstracts *sql.Row and *sql.Rows so a single scan helper serves both
// single-row and iterating queries.
type scanner interface{ Scan(dest ...any) error }

func scanUser(s scanner) (*domain.User, error) {
	var (
		u         domain.User
		role      string
		disabled  int
		created   string
		updated   string
		lastLogin sql.NullString
	)
	if err := s.Scan(&u.ID, &u.Email, &u.PasswordHash, &role, &disabled, &created, &updated, &lastLogin); err != nil {
		return nil, translate(err, "scan user")
	}
	u.Role = domain.Role(role)
	u.Disabled = disabled == 1
	var err error
	if u.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if u.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, err
	}
	if u.LastLoginAt, err = parseTimePtr(lastLogin); err != nil {
		return nil, err
	}
	return &u, nil
}

func normaliseEmail(e string) string { return strings.ToLower(strings.TrimSpace(e)) }

func requireAffected(res sql.Result, what string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return translate(err, what)
	}
	if n == 0 {
		return translate(sql.ErrNoRows, what)
	}
	return nil
}
