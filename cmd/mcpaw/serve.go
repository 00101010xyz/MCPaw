package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/00101010xyz/mcpaw/internal/connector"
	"github.com/00101010xyz/mcpaw/internal/engine"
	"github.com/00101010xyz/mcpaw/internal/httpapi"
	"github.com/00101010xyz/mcpaw/internal/httpx"
	"github.com/00101010xyz/mcpaw/internal/index"
	_ "github.com/00101010xyz/mcpaw/internal/index/source/gitea"  // registers the Gitea crawler
	_ "github.com/00101010xyz/mcpaw/internal/index/source/zotero" // registers the Zotero crawler
	"github.com/00101010xyz/mcpaw/internal/mcp"
	"github.com/00101010xyz/mcpaw/internal/platform/config"
	"github.com/00101010xyz/mcpaw/internal/platform/logging"
	"github.com/00101010xyz/mcpaw/internal/platform/metrics"
	"github.com/00101010xyz/mcpaw/internal/secrets"
	"github.com/00101010xyz/mcpaw/internal/service"
	"github.com/00101010xyz/mcpaw/internal/store/sqlitestore"
	"github.com/00101010xyz/mcpaw/internal/upstream"
	"github.com/00101010xyz/mcpaw/internal/webui"
)

// pruneInterval controls how often expired sessions and stale audit events are
// swept. Fifteen minutes is frequent enough that neither table grows
// unboundedly between sweeps, and infrequent enough to be free.
const pruneInterval = 15 * time.Minute

// auditRetention bounds how long tool-call and administrative audit events are
// kept. Ninety days covers a quarterly review without keeping the log growing
// forever on a machine nobody is pruning by hand.
const auditRetention = 90 * 24 * time.Hour

func runServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("loading configuration: %w", err)
	}

	logger := logging.New(os.Stdout, cfg.LogLevel, cfg.LogFormat)
	slog.SetDefault(logger)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		return fmt.Errorf("creating data directory %s: %w", cfg.DataDir, err)
	}

	// --- Cryptography ------------------------------------------------------
	masterKey, generated, err := secrets.LoadOrCreateMasterKey(cfg.MasterKeyB64, cfg.DataDir)
	if err != nil {
		return fmt.Errorf("loading master key: %w", err)
	}
	if generated {
		logger.Warn("generated a new master key; back up the key file or every stored secret becomes unrecoverable",
			slog.String("key_file", filepath.Join(cfg.DataDir, "master.key")))
	}
	keyring, err := secrets.NewKeyring(masterKey)
	if err != nil {
		return fmt.Errorf("deriving keyring: %w", err)
	}
	sealer, err := keyring.NewSealer()
	if err != nil {
		return fmt.Errorf("constructing sealer: %w", err)
	}

	// --- Storage -------------------------------------------------------------
	repos, err := sqlitestore.Open(ctx, filepath.Join(cfg.DataDir, "mcpaw.db"))
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer func() {
		if err := repos.Close(); err != nil {
			logger.Error("closing database", slog.String("error", err.Error()))
		}
	}()

	// --- Application services -------------------------------------------------
	audit := service.NewAudit(repos.Audit(), logger)

	registry := connector.NewRegistry()
	connectors := service.NewConnectors(repos.Connectors(), repos.Instances(), registry, audit, logger)
	if err := connectors.SyncBuiltins(ctx); err != nil {
		return fmt.Errorf("syncing built-in connectors: %w", err)
	}
	if err := connectors.LoadAll(ctx); err != nil {
		return fmt.Errorf("loading connectors: %w", err)
	}

	metricsRegistry := metrics.NewRegistry()

	upstreamClient := upstream.New(upstream.Options{})
	breaker := upstream.NewBreaker(upstream.BreakerConfig{})
	limiter := upstream.NewMemoryLimiter()
	gate := upstream.NewGate()
	executor := engine.New(engine.Config{Client: upstreamClient, Breaker: breaker, Limiter: limiter, Gate: gate})

	users := service.NewUsers(repos.Users(), repos.Sessions(), audit)
	sessions := service.NewSessions(service.SessionsConfig{
		Repo: repos.Sessions(), Users: repos.Users(), Keyring: keyring, Audit: audit,
		IdleTimeout: cfg.SessionIdleTimeout, AbsoluteTimeout: cfg.SessionAbsoluteTimeout,
	})
	tokens := service.NewTokens(repos.Tokens(), repos.Instances(), keyring, audit)
	instances := service.NewInstances(service.InstancesConfig{
		Repo: repos.Instances(), Connectors: connectors, Sealer: sealer, Executor: executor, Audit: audit,
	})
	indexer := service.NewIndexer(service.IndexerConfig{
		Repo: repos.SearchIndex(), Instances: instances, Audit: audit,
		Embedder: &index.Embedder{Client: executor.Client()}, Logger: logger,
	})
	mcpBackend := service.NewMCPBackend(instances, indexer, audit, version, logger)

	// --- MCP protocol server ---------------------------------------------------
	mcpSessions := mcp.NewSessionStore(mcp.SessionConfig{})
	mcpServer, err := mcp.NewServer(mcp.ServerConfig{
		Backend: mcpBackend, Sessions: mcpSessions,
		Info: mcp.Implementation{Name: "mcpaw", Version: version},
	})
	if err != nil {
		return fmt.Errorf("constructing mcp server: %w", err)
	}
	mcpHandler := mcp.NewHandler(mcpServer, mcp.HandlerOptions{MaxBodyBytes: cfg.MaxRequestBytes})

	// --- Web UI and HTTP surface ------------------------------------------------
	webUI, err := webui.New(webui.Config{
		Users: users, Sessions: sessions, Instances: instances, Connectors: connectors,
		Tokens: tokens, Audit: audit, Indexer: indexer, Logger: logger,
		PublicURL: cfg.PublicURL, Version: version, SecureCookies: cfg.SecureCookies,
		SessionMaxAge: cfg.SessionAbsoluteTimeout,
		LoginLimiter:  httpx.NewAttemptLimiter(cfg.LoginRateLimitPerMin, time.Minute),
	})
	if err != nil {
		return fmt.Errorf("constructing web ui: %w", err)
	}

	handler, err := httpapi.New(httpapi.Deps{
		Config: cfg, Logger: logger, Metrics: metricsRegistry, Repos: repos,
		Sessions: sessions, Tokens: tokens, Instances: instances,
		WebUI: webUI, MCPHandler: mcpHandler, Version: version,
	})
	if err != nil {
		return fmt.Errorf("constructing http router: %w", err)
	}

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           handler,
		ReadHeaderTimeout: cfg.ReadHeaderTimeout,
		ReadTimeout:       cfg.ReadTimeout,
		WriteTimeout:      cfg.WriteTimeout,
		IdleTimeout:       cfg.IdleTimeout,
	}

	go pruneLoop(ctx, logger, sessions, audit)

	serveErr := make(chan error, 1)
	go func() {
		logger.Info("mcpaw listening", slog.String("addr", cfg.Addr), slog.String("version", version))
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serveErr <- err
			return
		}
		serveErr <- nil
	}()

	select {
	case err := <-serveErr:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
	case <-ctx.Done():
		logger.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.ShutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("graceful shutdown failed", slog.String("error", err.Error()))
		}
		// Drain the goroutine so its result (already discarded — we're
		// exiting either way) doesn't leak.
		<-serveErr
	}
	return nil
}

// pruneLoop periodically clears expired web sessions and ages out old audit
// events. Nothing else in the process calls these; without this loop the
// sessions and audit_log tables would grow without bound.
func pruneLoop(ctx context.Context, logger *slog.Logger, sessions *service.Sessions, audit *service.Audit) {
	ticker := time.NewTicker(pruneInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n, err := sessions.PruneExpired(ctx); err != nil {
				logger.Warn("pruning expired sessions", slog.String("error", err.Error()))
			} else if n > 0 {
				logger.Debug("pruned expired sessions", slog.Int64("count", n))
			}
			if n, err := audit.Prune(ctx, auditRetention); err != nil {
				logger.Warn("pruning old audit events", slog.String("error", err.Error()))
			} else if n > 0 {
				logger.Debug("pruned old audit events", slog.Int64("count", n))
			}
		}
	}
}
