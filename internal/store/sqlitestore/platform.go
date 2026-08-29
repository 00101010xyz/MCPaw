package sqlitestore

import (
	"context"

	"github.com/00101010xyz/mcpaw/internal/domain"
)

// platformRepo persists the single platform_settings row (id = 1), created by
// migration 0006 so it always exists — every read here can assume the row is
// there rather than handling a "not configured yet" case separately.
type platformRepo struct{ base }

func (r *platformRepo) GetEmbedderSettings(ctx context.Context) (domain.EmbedderSettings, error) {
	var (
		s         domain.EmbedderSettings
		updatedAt string
	)
	err := r.read.QueryRowContext(ctx,
		`SELECT embedder_url, embedder_model, embedder_rate_limit_per_min, updated_at
		 FROM platform_settings WHERE id = 1`,
	).Scan(&s.URL, &s.Model, &s.RateLimitPerMin, &updatedAt)
	if err != nil {
		return domain.EmbedderSettings{}, translate(err, "get embedder settings")
	}
	if s.UpdatedAt, err = parseTime(updatedAt); err != nil {
		return domain.EmbedderSettings{}, err
	}
	return s, nil
}

func (r *platformRepo) SetEmbedderSettings(ctx context.Context, s domain.EmbedderSettings) error {
	_, err := r.write.ExecContext(ctx,
		`UPDATE platform_settings
		 SET embedder_url = ?, embedder_model = ?, embedder_rate_limit_per_min = ?, updated_at = ?
		 WHERE id = 1`,
		s.URL, s.Model, s.RateLimitPerMin, formatTime(s.UpdatedAt))
	return translate(err, "set embedder settings")
}

func (r *platformRepo) GetEmbedderAPIKey(ctx context.Context) ([]byte, bool, error) {
	var ct []byte
	err := r.read.QueryRowContext(ctx,
		`SELECT embedder_api_key_ciphertext FROM platform_settings WHERE id = 1`).Scan(&ct)
	if err != nil {
		return nil, false, translate(err, "get embedder api key")
	}
	if ct == nil {
		return nil, false, nil
	}
	return ct, true, nil
}

func (r *platformRepo) SetEmbedderAPIKey(ctx context.Context, ciphertext []byte) error {
	_, err := r.write.ExecContext(ctx,
		`UPDATE platform_settings SET embedder_api_key_ciphertext = ? WHERE id = 1`, ciphertext)
	return translate(err, "set embedder api key")
}

func (r *platformRepo) DeleteEmbedderAPIKey(ctx context.Context) error {
	_, err := r.write.ExecContext(ctx,
		`UPDATE platform_settings SET embedder_api_key_ciphertext = NULL WHERE id = 1`)
	return translate(err, "delete embedder api key")
}
