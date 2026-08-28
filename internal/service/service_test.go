package service

import (
	"context"
	"testing"

	"github.com/00101010xyz/mcpaw/internal/connector"
	"github.com/00101010xyz/mcpaw/internal/engine"
	"github.com/00101010xyz/mcpaw/internal/index"
	_ "github.com/00101010xyz/mcpaw/internal/index/source/gitea"  // registers the Gitea crawler
	_ "github.com/00101010xyz/mcpaw/internal/index/source/zotero" // registers the Zotero crawler
	"github.com/00101010xyz/mcpaw/internal/secrets"
	"github.com/00101010xyz/mcpaw/internal/store/sqlitestore"
)

// testEnv wires the real SQLite store, the real built-in Zotero manifest and
// the real crypto/engine stack together, rather than stubbing every
// repository interface: this package's job is coordinating those
// collaborators, so a fake would just re-implement the thing under test.
type testEnv struct {
	Store     *sqlitestore.Store
	Registry  *connector.Registry
	Sealer    secrets.Sealer
	Executor  *engine.Executor
	Audit     *Audit
	Connector *Connectors
	Instances *Instances
	Tokens    *Tokens
	Indexer   *Indexer
}

func newTestEnv(t *testing.T) *testEnv {
	t.Helper()
	ctx := context.Background()

	st, err := sqlitestore.Open(ctx, t.TempDir()+"/test.db")
	if err != nil {
		t.Fatalf("sqlitestore.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	keyring, err := secrets.NewKeyring(key)
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	sealer, err := keyring.NewSealer()
	if err != nil {
		t.Fatalf("NewSealer: %v", err)
	}

	audit := NewAudit(st.Audit(), nil)
	registry := connector.NewRegistry()
	connectors := NewConnectors(st.Connectors(), st.Instances(), registry, audit, nil)
	if err := connectors.SyncBuiltins(ctx); err != nil {
		t.Fatalf("SyncBuiltins: %v", err)
	}
	if err := connectors.LoadAll(ctx); err != nil {
		t.Fatalf("LoadAll: %v", err)
	}

	executor := engine.New(engine.Config{})
	instances := NewInstances(InstancesConfig{
		Repo: st.Instances(), Connectors: connectors, Sealer: sealer,
		Executor: executor, Audit: audit,
	})
	tokens := NewTokens(st.Tokens(), st.Instances(), keyring, audit)
	indexer := NewIndexer(IndexerConfig{
		Repo: st.SearchIndex(), Instances: instances, Audit: audit,
		Embedder: &index.Embedder{Client: executor.Client()},
	})

	return &testEnv{
		Store: st, Registry: registry, Sealer: sealer, Executor: executor,
		Audit: audit, Connector: connectors, Instances: instances, Tokens: tokens,
		Indexer: indexer,
	}
}

func systemActor() Actor { return SystemActor() }

// zoteroCreateInput returns a valid CreateInput against the real built-in
// Zotero connector, so each test only has to override what it cares about.
func zoteroCreateInput(slug string) CreateInput {
	return CreateInput{
		Name: "My Zotero", Slug: slug, ConnectorID: "zotero-local",
		BaseURL:             "http://host.docker.internal:23119",
		Variables:           map[string]string{"userId": "0"},
		Enabled:             true,
		AllowPrivateNetwork: true,
	}
}
