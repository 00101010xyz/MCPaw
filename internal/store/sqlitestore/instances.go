package sqlitestore

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/00101010xyz/mcpaw/internal/domain"
)

type instanceRepo struct{ base }

const instanceColumns = `id, slug, name, description, connector_id, base_url, variables, enabled,
	allow_private_network, host_header_override, embedder_url, embedder_model, timeout_ms,
	rate_limit_per_min, max_concurrent, max_response_bytes, created_at, updated_at, version`

func (r *instanceRepo) Create(ctx context.Context, i *domain.Instance) error {
	vars, err := marshalVariables(i.Variables)
	if err != nil {
		return err
	}
	_, err = r.write.ExecContext(ctx,
		`INSERT INTO instances (`+instanceColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		i.ID, i.Slug, i.Name, i.Description, i.ConnectorID, i.BaseURL, vars, boolToInt(i.Enabled),
		boolToInt(i.AllowPrivateNetwork), i.HostHeaderOverride, i.EmbedderURL, i.EmbedderModel,
		i.TimeoutMS, i.RateLimitPerMin, i.MaxConcurrent, i.MaxResponseBytes,
		formatTime(i.CreatedAt), formatTime(i.UpdatedAt), i.Version)
	return translate(err, "create instance")
}

// Update writes a new configuration and bumps the version counter in the same
// statement, which is what invalidates the compiled-instance cache on the hot
// path without a separate notification channel.
func (r *instanceRepo) Update(ctx context.Context, i *domain.Instance) error {
	vars, err := marshalVariables(i.Variables)
	if err != nil {
		return err
	}
	res, err := r.write.ExecContext(ctx,
		`UPDATE instances SET slug = ?, name = ?, description = ?, base_url = ?, variables = ?,
		   enabled = ?, allow_private_network = ?, host_header_override = ?, embedder_url = ?,
		   embedder_model = ?, timeout_ms = ?, rate_limit_per_min = ?, max_concurrent = ?,
		   max_response_bytes = ?, updated_at = ?, version = version + 1
		 WHERE id = ?`,
		i.Slug, i.Name, i.Description, i.BaseURL, vars, boolToInt(i.Enabled),
		boolToInt(i.AllowPrivateNetwork), i.HostHeaderOverride, i.EmbedderURL, i.EmbedderModel,
		i.TimeoutMS, i.RateLimitPerMin, i.MaxConcurrent, i.MaxResponseBytes, formatTime(i.UpdatedAt), i.ID)
	if err != nil {
		return translate(err, "update instance")
	}
	if err := requireAffected(res, "update instance"); err != nil {
		return err
	}
	i.Version++
	return nil
}

func (r *instanceRepo) Delete(ctx context.Context, id string) error {
	res, err := r.write.ExecContext(ctx, `DELETE FROM instances WHERE id = ?`, id)
	if err != nil {
		return translate(err, "delete instance")
	}
	return requireAffected(res, "delete instance")
}

func (r *instanceRepo) Get(ctx context.Context, id string) (*domain.Instance, error) {
	row := r.read.QueryRowContext(ctx, `SELECT `+instanceColumns+` FROM instances WHERE id = ?`, id)
	return scanInstance(row)
}

func (r *instanceRepo) GetBySlug(ctx context.Context, slug string) (*domain.Instance, error) {
	row := r.read.QueryRowContext(ctx, `SELECT `+instanceColumns+` FROM instances WHERE slug = ?`, slug)
	return scanInstance(row)
}

func (r *instanceRepo) List(ctx context.Context) ([]*domain.Instance, error) {
	rows, err := r.read.QueryContext(ctx, `SELECT `+instanceColumns+` FROM instances ORDER BY name ASC`)
	if err != nil {
		return nil, translate(err, "list instances")
	}
	defer rows.Close()
	var out []*domain.Instance
	for rows.Next() {
		i, err := scanInstance(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, translate(rows.Err(), "list instances")
}

func (r *instanceRepo) CountByConnector(ctx context.Context, connectorID string) (int, error) {
	var n int
	err := r.read.QueryRowContext(ctx, `SELECT COUNT(*) FROM instances WHERE connector_id = ?`, connectorID).Scan(&n)
	return n, translate(err, "count instances by connector")
}

func (r *instanceRepo) SetSecret(ctx context.Context, s *domain.InstanceSecret) error {
	_, err := r.write.ExecContext(ctx,
		`INSERT INTO instance_secrets (instance_id, name, ciphertext, updated_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT (instance_id, name) DO UPDATE SET ciphertext = excluded.ciphertext, updated_at = excluded.updated_at`,
		s.InstanceID, s.Name, s.Ciphertext, formatTime(s.UpdatedAt))
	return translate(err, "set instance secret")
}

func (r *instanceRepo) DeleteSecret(ctx context.Context, instanceID, name string) error {
	_, err := r.write.ExecContext(ctx,
		`DELETE FROM instance_secrets WHERE instance_id = ? AND name = ?`, instanceID, name)
	return translate(err, "delete instance secret")
}

// ListSecretNames returns which secrets are configured without revealing any
// ciphertext, which is what the admin UI renders.
func (r *instanceRepo) ListSecretNames(ctx context.Context, instanceID string) ([]string, error) {
	rows, err := r.read.QueryContext(ctx,
		`SELECT name FROM instance_secrets WHERE instance_id = ? ORDER BY name ASC`, instanceID)
	if err != nil {
		return nil, translate(err, "list secret names")
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, translate(err, "scan secret name")
		}
		out = append(out, n)
	}
	return out, translate(rows.Err(), "list secret names")
}

func (r *instanceRepo) LoadSecrets(ctx context.Context, instanceID string) (map[string][]byte, error) {
	rows, err := r.read.QueryContext(ctx,
		`SELECT name, ciphertext FROM instance_secrets WHERE instance_id = ?`, instanceID)
	if err != nil {
		return nil, translate(err, "load secrets")
	}
	defer rows.Close()
	out := map[string][]byte{}
	for rows.Next() {
		var (
			name string
			ct   []byte
		)
		if err := rows.Scan(&name, &ct); err != nil {
			return nil, translate(err, "scan secret")
		}
		out[name] = ct
	}
	return out, translate(rows.Err(), "load secrets")
}

func (r *instanceRepo) SetToolBinding(ctx context.Context, b *domain.ToolBinding) error {
	_, err := r.write.ExecContext(ctx,
		`INSERT INTO instance_tools (instance_id, tool_name, enabled) VALUES (?, ?, ?)
		 ON CONFLICT (instance_id, tool_name) DO UPDATE SET enabled = excluded.enabled`,
		b.InstanceID, b.ToolName, boolToInt(b.Enabled))
	return translate(err, "set tool binding")
}

func (r *instanceRepo) ListToolBindings(ctx context.Context, instanceID string) ([]*domain.ToolBinding, error) {
	rows, err := r.read.QueryContext(ctx,
		`SELECT instance_id, tool_name, enabled FROM instance_tools WHERE instance_id = ?`, instanceID)
	if err != nil {
		return nil, translate(err, "list tool bindings")
	}
	defer rows.Close()
	var out []*domain.ToolBinding
	for rows.Next() {
		var (
			b       domain.ToolBinding
			enabled int
		)
		if err := rows.Scan(&b.InstanceID, &b.ToolName, &enabled); err != nil {
			return nil, translate(err, "scan tool binding")
		}
		b.Enabled = enabled == 1
		out = append(out, &b)
	}
	return out, translate(rows.Err(), "list tool bindings")
}

func marshalVariables(v map[string]string) (string, error) {
	if v == nil {
		v = map[string]string{}
	}
	b, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("sqlitestore: encoding instance variables: %w", err)
	}
	return string(b), nil
}

func scanInstance(s scanner) (*domain.Instance, error) {
	var (
		i                          domain.Instance
		vars                       string
		enabled, allowPrivate      int
		createdAt, updatedAtString string
	)
	if err := s.Scan(&i.ID, &i.Slug, &i.Name, &i.Description, &i.ConnectorID, &i.BaseURL, &vars,
		&enabled, &allowPrivate, &i.HostHeaderOverride, &i.EmbedderURL, &i.EmbedderModel,
		&i.TimeoutMS, &i.RateLimitPerMin, &i.MaxConcurrent, &i.MaxResponseBytes,
		&createdAt, &updatedAtString, &i.Version); err != nil {
		return nil, translate(err, "scan instance")
	}
	i.Enabled = enabled == 1
	i.AllowPrivateNetwork = allowPrivate == 1
	i.Variables = map[string]string{}
	if err := json.Unmarshal([]byte(vars), &i.Variables); err != nil {
		return nil, fmt.Errorf("sqlitestore: decoding instance variables: %w", err)
	}
	var err error
	if i.CreatedAt, err = parseTime(createdAt); err != nil {
		return nil, err
	}
	if i.UpdatedAt, err = parseTime(updatedAtString); err != nil {
		return nil, err
	}
	return &i, nil
}
