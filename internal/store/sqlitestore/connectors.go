package sqlitestore

import (
	"context"

	"github.com/00101010xyz/mcpaw/internal/domain"
)

type connectorRepo struct{ base }

const connectorColumns = `id, name, version, source, manifest, checksum, created_at, updated_at`

// Upsert inserts a connector or replaces its manifest. Built-in connectors are
// re-upserted on every boot so that upgrading the binary upgrades the shipped
// manifests without an operator step.
func (r *connectorRepo) Upsert(ctx context.Context, c *domain.ConnectorRecord) error {
	_, err := r.write.ExecContext(ctx,
		`INSERT INTO connectors (`+connectorColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT (id) DO UPDATE SET
		   name = excluded.name,
		   version = excluded.version,
		   source = excluded.source,
		   manifest = excluded.manifest,
		   checksum = excluded.checksum,
		   updated_at = excluded.updated_at`,
		c.ID, c.Name, c.Version, string(c.Source), c.Manifest, c.Checksum,
		formatTime(c.CreatedAt), formatTime(c.UpdatedAt))
	return translate(err, "upsert connector")
}

func (r *connectorRepo) Get(ctx context.Context, id string) (*domain.ConnectorRecord, error) {
	row := r.read.QueryRowContext(ctx, `SELECT `+connectorColumns+` FROM connectors WHERE id = ?`, id)
	return scanConnector(row)
}

func (r *connectorRepo) List(ctx context.Context) ([]*domain.ConnectorRecord, error) {
	rows, err := r.read.QueryContext(ctx, `SELECT `+connectorColumns+` FROM connectors ORDER BY name ASC`)
	if err != nil {
		return nil, translate(err, "list connectors")
	}
	defer rows.Close()
	var out []*domain.ConnectorRecord
	for rows.Next() {
		c, err := scanConnector(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, translate(rows.Err(), "list connectors")
}

// Delete removes a connector. The ON DELETE RESTRICT foreign key on instances
// makes the database refuse to orphan a running instance, which surfaces as a
// conflict rather than a corrupted configuration.
func (r *connectorRepo) Delete(ctx context.Context, id string) error {
	res, err := r.write.ExecContext(ctx, `DELETE FROM connectors WHERE id = ?`, id)
	if err != nil {
		return translate(err, "delete connector")
	}
	return requireAffected(res, "delete connector")
}

func scanConnector(s scanner) (*domain.ConnectorRecord, error) {
	var (
		c                domain.ConnectorRecord
		source           string
		created, updated string
	)
	if err := s.Scan(&c.ID, &c.Name, &c.Version, &source, &c.Manifest, &c.Checksum, &created, &updated); err != nil {
		return nil, translate(err, "scan connector")
	}
	c.Source = domain.ConnectorSource(source)
	var err error
	if c.CreatedAt, err = parseTime(created); err != nil {
		return nil, err
	}
	if c.UpdatedAt, err = parseTime(updated); err != nil {
		return nil, err
	}
	return &c, nil
}
