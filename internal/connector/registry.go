package connector

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/00101010xyz/mcpaw/internal/domain"
)

//go:embed builtin/*.yaml
var builtinFS embed.FS

// Builtins parses and validates every manifest embedded in the binary.
//
// Validation failures here are programming errors — a shipped manifest that
// does not compile would fail at the operator's first tool call — so callers
// treat the error as fatal at startup rather than degrading silently.
func Builtins() ([]*domain.ConnectorRecord, error) {
	entries, err := fs.ReadDir(builtinFS, "builtin")
	if err != nil {
		return nil, fmt.Errorf("connector: reading built-in manifests: %w", err)
	}
	now := time.Now().UTC()

	var out []*domain.ConnectorRecord
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		data, err := builtinFS.ReadFile("builtin/" + e.Name())
		if err != nil {
			return nil, fmt.Errorf("connector: reading built-in %s: %w", e.Name(), err)
		}
		m, err := ParseManifest(data)
		if err != nil {
			return nil, fmt.Errorf("connector: built-in %s: %w", e.Name(), err)
		}
		if _, err := Compile(m); err != nil {
			return nil, fmt.Errorf("connector: built-in %s: %w", e.Name(), err)
		}
		out = append(out, &domain.ConnectorRecord{
			ID:        m.Metadata.ID,
			Name:      m.Metadata.Name,
			Version:   m.Metadata.Version,
			Source:    domain.SourceBuiltin,
			Manifest:  data,
			Checksum:  Checksum(data),
			CreatedAt: now,
			UpdatedAt: now,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Entry pairs a stored connector record with its compiled form.
type Entry struct {
	Record   *domain.ConnectorRecord
	Compiled *Compiled
}

// Registry is the in-memory cache of compiled connectors.
//
// Compilation is expensive (JSON Schema compilation dominates) and the result
// is immutable, so it happens on write and never on the request path. The
// registry is safe for concurrent use.
type Registry struct {
	mu    sync.RWMutex
	items map[string]*Entry
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry { return &Registry{items: map[string]*Entry{}} }

// Put parses, validates and caches a connector record.
//
// The record's declared ID must match the manifest's own metadata.id, so a
// manifest cannot be stored under a name that misrepresents what it contains.
func (r *Registry) Put(rec *domain.ConnectorRecord) (*Entry, error) {
	m, err := ParseManifest(rec.Manifest)
	if err != nil {
		return nil, err
	}
	if m.Metadata.ID != rec.ID {
		return nil, fmt.Errorf("connector: record id %q does not match manifest metadata.id %q", rec.ID, m.Metadata.ID)
	}
	compiled, err := Compile(m)
	if err != nil {
		return nil, err
	}
	entry := &Entry{Record: rec, Compiled: compiled}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.items[rec.ID] = entry
	return entry, nil
}

// Get returns the compiled connector with the given ID.
func (r *Registry) Get(id string) (*Entry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.items[id]
	return e, ok
}

// List returns every cached connector ordered by display name.
func (r *Registry) List() []*Entry {
	r.mu.RLock()
	out := make([]*Entry, 0, len(r.items))
	for _, e := range r.items {
		out = append(out, e)
	}
	r.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool {
		if out[i].Record.Name == out[j].Record.Name {
			return out[i].Record.ID < out[j].Record.ID
		}
		return out[i].Record.Name < out[j].Record.Name
	})
	return out
}

// Remove drops a connector from the cache.
func (r *Registry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.items, id)
}

// Len reports how many connectors are cached.
func (r *Registry) Len() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.items)
}
