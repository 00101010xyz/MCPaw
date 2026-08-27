package service

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/00101010xyz/mcpaw/internal/connector"
	"github.com/00101010xyz/mcpaw/internal/domain"
	"github.com/00101010xyz/mcpaw/internal/store"
)

// Connectors manages connector manifests and keeps the compiled registry in
// step with the database.
type Connectors struct {
	repo      store.ConnectorRepository
	instances store.InstanceRepository
	registry  *connector.Registry
	audit     *Audit
	logger    *slog.Logger
	now       func() time.Time
}

// NewConnectors constructs the connector service.
func NewConnectors(repo store.ConnectorRepository, instances store.InstanceRepository,
	registry *connector.Registry, audit *Audit, logger *slog.Logger) *Connectors {
	if logger == nil {
		logger = slog.Default()
	}
	return &Connectors{repo: repo, instances: instances, registry: registry,
		audit: audit, logger: logger, now: time.Now}
}

// SyncBuiltins upserts the manifests embedded in the binary.
//
// Running on every boot means upgrading the container upgrades the shipped
// connectors, with no migration step for the operator. Existing instances keep
// working because their configuration lives in the instance row, not the
// manifest.
func (c *Connectors) SyncBuiltins(ctx context.Context) error {
	builtins, err := connector.Builtins()
	if err != nil {
		// A built-in that does not compile is a defect in this build, not an
		// operator problem, and shipping it would surface as a mysterious
		// runtime failure later.
		return fmt.Errorf("service: built-in connectors are invalid: %w", err)
	}
	for _, rec := range builtins {
		existing, err := c.repo.Get(ctx, rec.ID)
		switch {
		case err == nil:
			if existing.Checksum == rec.Checksum {
				continue
			}
			rec.CreatedAt = existing.CreatedAt
			c.logger.Info("updating built-in connector",
				slog.String("connector_id", rec.ID), slog.String("version", rec.Version))
		case errors.Is(err, domain.ErrNotFound):
			c.logger.Info("installing built-in connector",
				slog.String("connector_id", rec.ID), slog.String("version", rec.Version))
		default:
			return err
		}
		rec.UpdatedAt = c.now().UTC()
		if err := c.repo.Upsert(ctx, rec); err != nil {
			return err
		}
	}
	return nil
}

// LoadAll hydrates the in-memory registry from the database.
//
// A stored manifest that no longer compiles is logged and skipped rather than
// aborting startup: one bad imported connector must not take the whole platform
// — including every other instance — offline.
func (c *Connectors) LoadAll(ctx context.Context) error {
	records, err := c.repo.List(ctx)
	if err != nil {
		return err
	}
	for _, rec := range records {
		if _, err := c.registry.Put(rec); err != nil {
			c.logger.Error("stored connector could not be compiled and will be unavailable",
				slog.String("connector_id", rec.ID),
				slog.String("source", string(rec.Source)),
				slog.String("error", err.Error()))
		}
	}
	return nil
}

// List returns every compiled connector.
func (c *Connectors) List(context.Context) []*connector.Entry { return c.registry.List() }

// Get returns one compiled connector.
func (c *Connectors) Get(_ context.Context, connectorID string) (*connector.Entry, error) {
	entry, ok := c.registry.Get(connectorID)
	if !ok {
		return nil, fmt.Errorf("connector %q: %w", connectorID, domain.ErrNotFound)
	}
	return entry, nil
}

// ImportManifest validates and stores an operator-supplied manifest.
//
// Validation happens before persistence, so the database can only ever contain
// manifests that compile. The registry is updated only after the write
// succeeds, so a failed write cannot leave memory ahead of storage.
func (c *Connectors) ImportManifest(ctx context.Context, actor Actor, data []byte, source domain.ConnectorSource) (*connector.Entry, error) {
	manifest, err := connector.ParseManifest(data)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", domain.ErrInvalidInput, err)
	}
	if _, err := connector.Compile(manifest); err != nil {
		return nil, fmt.Errorf("%w: %s", domain.ErrInvalidInput, err)
	}
	if source == "" {
		source = domain.SourceManifest
	}

	existing, err := c.repo.Get(ctx, manifest.Metadata.ID)
	if err != nil && !errors.Is(err, domain.ErrNotFound) {
		return nil, err
	}
	if existing != nil && !existing.Editable() {
		return nil, fmt.Errorf("connector %q is built in and cannot be replaced: %w",
			manifest.Metadata.ID, domain.ErrForbidden)
	}

	now := c.now().UTC()
	rec := &domain.ConnectorRecord{
		ID: manifest.Metadata.ID, Name: manifest.Metadata.Name, Version: manifest.Metadata.Version,
		Source: source, Manifest: data, Checksum: connector.Checksum(data),
		CreatedAt: now, UpdatedAt: now,
	}
	if existing != nil {
		rec.CreatedAt = existing.CreatedAt
	}
	if err := c.repo.Upsert(ctx, rec); err != nil {
		return nil, err
	}
	entry, err := c.registry.Put(rec)
	if err != nil {
		return nil, err
	}

	c.audit.Success(ctx, actor, domain.ActionConnectorImport, "connector", rec.ID, map[string]any{
		"name": rec.Name, "version": rec.Version, "source": string(source),
		"tools": len(entry.Compiled.Tools), "replaced": existing != nil,
	})
	return entry, nil
}

// Delete removes an imported connector.
func (c *Connectors) Delete(ctx context.Context, actor Actor, connectorID string) error {
	rec, err := c.repo.Get(ctx, connectorID)
	if err != nil {
		return err
	}
	if !rec.Editable() {
		return fmt.Errorf("connector %q is built in and cannot be deleted: %w", connectorID, domain.ErrForbidden)
	}
	// The database enforces this too, but checking here produces an error an
	// operator can act on instead of a foreign-key violation.
	count, err := c.instances.CountByConnector(ctx, connectorID)
	if err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("%w: %d instance(s) still use this connector", domain.ErrConflict, count)
	}
	if err := c.repo.Delete(ctx, connectorID); err != nil {
		return err
	}
	c.registry.Remove(connectorID)
	c.audit.Success(ctx, actor, domain.ActionConnectorDelete, "connector", connectorID, nil)
	return nil
}
