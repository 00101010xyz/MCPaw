package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/00101010xyz/mcpaw/internal/domain"
	"github.com/00101010xyz/mcpaw/internal/index"
	"github.com/00101010xyz/mcpaw/internal/index/source"
	"github.com/00101010xyz/mcpaw/internal/secrets"
	"github.com/00101010xyz/mcpaw/internal/store"
	"github.com/00101010xyz/mcpaw/internal/upstream"
)

// platformSecretScope is the AAD scope used to seal the platform-wide
// embedder API key — a sentinel in place of an instance ID, since this
// secret belongs to no one instance (see domain.EmbedderSettings).
const platformSecretScope = "platform"

// embedderEgressPolicy always allows private-network egress for embedder
// calls: the embedder is platform infrastructure the operator points at
// their own sidecar (a local Ollama instance is the common case), not an
// upstream API whose egress should be gated by any one instance's own
// policy. Cloud instance-metadata addresses stay blocked regardless — see
// upstream.CheckIP.
var embedderEgressPolicy = upstream.EgressPolicy{AllowPrivateNetworks: true}

// Chunking, batching and timeout caps for one reindex run. These bound how
// long and how much memory an operator-triggered reindex can consume; a
// per-source crawl (internal/index/source/*) has its own pagination caps for
// how much of a library it is willing to walk. These stay here because they
// govern the chunk/embed/store step every source shares, deliberately
// generous for a personal library and deliberately not exposed as
// configuration, to keep the feature's surface small.
const (
	reindexMaxChunksPerFile = 200
	reindexEmbedBatch       = 16
	reindexTimeout          = 30 * time.Minute
)

// Search tuning.
const (
	defaultSearchLimit   = 6
	maxSearchLimit       = 20
	searchCandidatePool  = 40
	maxChunkPreviewRunes = 700
)

// IndexStatus reports the state of an instance's semantic-search index for
// the web UI.
type IndexStatus struct {
	Running bool
	// ItemsProcessed counts every document the crawl examined, whether or
	// not it produced any indexable text — a rough progress signal, not an
	// exact count of anything more specific than "documents visited".
	ItemsProcessed int
	// ChunksIndexed counts chunks actually (re)written this run — a document
	// an "Update index" run recognised as unchanged contributes nothing
	// here, which is the point: it says how much work this run actually did.
	ChunksIndexed int
	// DocumentsSkipped counts documents an "Update index" run recognised as
	// unchanged (same content hash) and left alone.
	DocumentsSkipped int
	// DocumentsPruned counts documents that were indexed before but were not
	// seen this crawl — deleted or renamed upstream — and so were removed.
	DocumentsPruned int
	// DocumentsEmptyText counts documents the crawl visited but which had no
	// extracted text at all — nothing to chunk or embed, so nothing was
	// written for them. A run with a high count here alongside a low
	// ChunksIndexed usually means there was nothing to index yet (no Zotero
	// fulltext extracted, no completed Linkding snapshot, no matching Gitea
	// file), not that embedding is broken.
	DocumentsEmptyText int
	// DocumentsFailed counts documents that errored while embedding or
	// storing chunks; LastError holds the most recent such error. Unlike
	// DocumentsEmptyText, a nonzero count here usually means the embedder is
	// unreachable or misconfigured.
	DocumentsFailed int
	LastError       string
	StartedAt       time.Time
	FinishedAt      time.Time
}

// SearchHit is one ranked result from a semantic search.
type SearchHit struct {
	ItemKey       string
	AttachmentKey string
	ChunkIndex    int
	Score         float64
	Text          string
}

// IndexerConfig wires the Indexer service.
type IndexerConfig struct {
	Repo      store.SearchIndexRepository
	Platform  store.PlatformRepository
	Instances *Instances
	Audit     *Audit
	Embedder  *index.Embedder
	Sealer    secrets.Sealer
	Logger    *slog.Logger
}

// Indexer builds and serves the semantic-search index over Zotero PDF and
// snapshot text. It is entirely additive: with no embedder configured and no
// chunks indexed, an instance behaves exactly as it did before this feature
// existed — Ready reports false and no tool is advertised.
type Indexer struct {
	repo      store.SearchIndexRepository
	platform  store.PlatformRepository
	instances *Instances
	audit     *Audit
	embedder  *index.Embedder
	sealer    secrets.Sealer
	logger    *slog.Logger

	mu     sync.Mutex
	status map[string]*IndexStatus
}

// NewIndexer constructs the indexing service.
func NewIndexer(cfg IndexerConfig) *Indexer {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Indexer{
		repo: cfg.Repo, platform: cfg.Platform, instances: cfg.Instances, audit: cfg.Audit,
		embedder: cfg.Embedder, sealer: cfg.Sealer,
		logger: logger, status: map[string]*IndexStatus{},
	}
}

// EmbedderSettings reports the platform-wide semantic-search embedder
// configuration and whether an API key is currently set (never its value).
func (s *Indexer) EmbedderSettings(ctx context.Context) (domain.EmbedderSettings, bool, error) {
	settings, err := s.platform.GetEmbedderSettings(ctx)
	if err != nil {
		return domain.EmbedderSettings{}, false, err
	}
	_, apiKeySet, err := s.platform.GetEmbedderAPIKey(ctx)
	if err != nil {
		return domain.EmbedderSettings{}, false, err
	}
	return settings, apiKeySet, nil
}

// UpdateEmbedderSettings saves the embedder URL, model and rate limit shared
// by every instance. Leaving url empty leaves semantic search off entirely
// for every instance, with no other effect.
func (s *Indexer) UpdateEmbedderSettings(ctx context.Context, actor Actor, url, model string, rateLimitPerMin int) error {
	if rateLimitPerMin < 0 {
		rateLimitPerMin = 0
	}
	settings := domain.EmbedderSettings{
		URL: strings.TrimSpace(url), Model: strings.TrimSpace(model),
		RateLimitPerMin: rateLimitPerMin, UpdatedAt: time.Now(),
	}
	if err := s.platform.SetEmbedderSettings(ctx, settings); err != nil {
		return err
	}
	s.audit.Success(ctx, actor, domain.ActionPlatformSettingsUpdate, "platform", "embedder",
		map[string]any{"url_set": settings.URL != "", "rate_limit_per_min": settings.RateLimitPerMin})
	return nil
}

// SetEmbedderAPIKey encrypts and stores the embedder sidecar's API key.
func (s *Indexer) SetEmbedderAPIKey(ctx context.Context, actor Actor, plaintext string) error {
	if plaintext == "" {
		return fmt.Errorf("%w: the secret value must not be empty; delete it instead", domain.ErrInvalidInput)
	}
	if len(plaintext) > 8192 {
		return fmt.Errorf("%w: the secret value is too long", domain.ErrInvalidInput)
	}
	ciphertext, err := s.sealer.Seal(secrets.SecretAAD(platformSecretScope, index.EmbedderAPIKey), []byte(plaintext))
	if err != nil {
		return err
	}
	if err := s.platform.SetEmbedderAPIKey(ctx, ciphertext); err != nil {
		return err
	}
	s.audit.Success(ctx, actor, domain.ActionPlatformSettingsUpdate, "platform", "embedder",
		map[string]any{"secret": "set"})
	return nil
}

// DeleteEmbedderAPIKey removes the stored embedder API key.
func (s *Indexer) DeleteEmbedderAPIKey(ctx context.Context, actor Actor) error {
	if err := s.platform.DeleteEmbedderAPIKey(ctx); err != nil {
		return err
	}
	s.audit.Success(ctx, actor, domain.ActionPlatformSettingsUpdate, "platform", "embedder",
		map[string]any{"secret": "deleted"})
	return nil
}

// embedderAPIKey decrypts the stored embedder API key, if any is set.
func (s *Indexer) embedderAPIKey(ctx context.Context) (string, error) {
	ciphertext, ok, err := s.platform.GetEmbedderAPIKey(ctx)
	if err != nil || !ok {
		return "", err
	}
	plaintext, err := s.sealer.Open(secrets.SecretAAD(platformSecretScope, index.EmbedderAPIKey), ciphertext)
	if err != nil {
		return "", fmt.Errorf("could not decrypt the embedder API key; the master key may have changed: %w", err)
	}
	return string(plaintext), nil
}

// Supported reports whether a connector has a registered crawler (see
// internal/index/source). The web UI uses this to decide whether to show the
// feature at all.
func (s *Indexer) Supported(connectorID string) bool {
	_, ok := source.Get(connectorID)
	return ok
}

// Ready reports whether an instance has a usable index, which is what gates
// advertising the semantic search tool to MCP clients: an empty or
// unconfigured index must be indistinguishable from the feature not existing.
func (s *Indexer) Ready(ctx context.Context, instanceID string) bool {
	n, err := s.repo.CountChunks(ctx, instanceID)
	return err == nil && n > 0
}

// Status reports the current or most recent reindex run, plus the number of
// chunks currently stored.
func (s *Indexer) Status(ctx context.Context, instanceID string) (IndexStatus, int, error) {
	count, err := s.repo.CountChunks(ctx, instanceID)
	if err != nil {
		return IndexStatus{}, 0, err
	}
	s.mu.Lock()
	st := s.status[instanceID]
	s.mu.Unlock()
	if st == nil {
		return IndexStatus{}, count, nil
	}
	return *st, count, nil
}

// ReindexMode selects how Reindex treats an instance's existing index.
type ReindexMode int

const (
	// ReindexUpdate is the default, incremental mode: a document whose
	// content hash has not changed since the last run is left untouched —
	// not re-chunked, not re-embedded — and a document that existed before
	// but was not seen this crawl (deleted or renamed upstream) is pruned,
	// as long as the crawl was not truncated. It refuses to run at all if
	// the instance's embedder model has changed since the index was built,
	// since mixing vectors from two models degrades silently to noise
	// rather than erroring.
	ReindexUpdate ReindexMode = iota
	// ReindexRebuild clears the instance's index first and rebuilds it from
	// scratch, embedding every document regardless of whether its content
	// has changed. It is the only mode that may change the embedder model
	// an index is built with.
	ReindexRebuild
)

// docKey identifies one document within an instance's index, for diffing a
// fresh crawl against what is already stored.
type docKey struct{ itemKey, attachmentKey string }

// Reindex builds or updates an instance's index in the background: a real
// library's worth of documents takes many embedding calls, far longer than
// an operator should have to wait on an HTTP response for.
func (s *Indexer) Reindex(ctx context.Context, actor Actor, instanceID string, mode ReindexMode) error {
	resolved, err := s.instances.ResolveByID(ctx, instanceID)
	if err != nil {
		return err
	}
	if !s.Supported(resolved.ConnectorRec.ID) {
		return fmt.Errorf("%w: semantic search indexing is not available for this connector", domain.ErrInvalidInput)
	}

	s.mu.Lock()
	if st, ok := s.status[instanceID]; ok && st.Running {
		s.mu.Unlock()
		return fmt.Errorf("a reindex is already running for this instance")
	}
	s.status[instanceID] = &IndexStatus{Running: true, StartedAt: time.Now()}
	s.mu.Unlock()

	action := "update"
	if mode == ReindexRebuild {
		action = "rebuild"
	}
	s.audit.Success(ctx, actor, domain.ActionIndexReindex, "instance", instanceID, map[string]any{"action": "started", "mode": action})
	go s.runReindex(instanceID, mode)
	return nil
}

func (s *Indexer) runReindex(instanceID string, mode ReindexMode) {
	ctx, cancel := context.WithTimeout(context.Background(), reindexTimeout)
	defer cancel()
	defer s.finish(instanceID)

	resolved, err := s.instances.ResolveByID(ctx, instanceID)
	if err != nil {
		s.fail(instanceID, err)
		return
	}
	target, err := s.instances.Target(resolved)
	if err != nil {
		s.fail(instanceID, err)
		return
	}
	settings, err := s.platform.GetEmbedderSettings(ctx)
	if err != nil {
		s.fail(instanceID, err)
		return
	}
	if settings.URL == "" {
		s.fail(instanceID, fmt.Errorf("set the embedder URL under Settings → Semantic search before indexing"))
		return
	}
	apiKey, err := s.embedderAPIKey(ctx)
	if err != nil {
		s.fail(instanceID, err)
		return
	}

	crawler, ok := source.Get(resolved.ConnectorRec.ID)
	if !ok {
		s.fail(instanceID, fmt.Errorf("semantic search indexing is not available for this connector"))
		return
	}
	for _, toolName := range crawler.RequiredTools() {
		if _, declared := resolved.Connector.Tool(toolName); !declared || !resolved.EnabledTools[toolName] {
			s.fail(instanceID, fmt.Errorf("this instance needs %q enabled to index", toolName))
			return
		}
	}

	effectiveModel := settings.Model
	if effectiveModel == "" {
		effectiveModel = index.DefaultEmbedder
	}

	existing := map[docKey]domain.IndexDocument{}
	if mode == ReindexRebuild {
		if err := s.repo.ClearInstance(ctx, instanceID); err != nil {
			s.fail(instanceID, err)
			return
		}
	} else {
		meta, ok, err := s.repo.GetMeta(ctx, instanceID)
		if err != nil {
			s.fail(instanceID, err)
			return
		}
		if ok && meta.EmbedderModel != effectiveModel {
			s.fail(instanceID, fmt.Errorf("%w: the embedder model changed from %q to %q since this index was built; use Rebuild from scratch instead of Update",
				domain.ErrInvalidInput, meta.EmbedderModel, effectiveModel))
			return
		}
		docs, err := s.repo.ListDocuments(ctx, instanceID)
		if err != nil {
			s.fail(instanceID, err)
			return
		}
		for _, d := range docs {
			existing[docKey{d.ItemKey, d.AttachmentKey}] = d
		}
	}

	rt := source.Runtime{
		Executor: s.instances.Executor(), Target: target,
		Connector: resolved.Connector, EnabledTools: resolved.EnabledTools,
	}
	seen := map[docKey]bool{}
	dimension := 0
	truncated, err := crawler.Crawl(ctx, rt, func(ctx context.Context, doc source.Document, text string) error {
		key := docKey{doc.ItemKey, doc.AttachmentKey}
		seen[key] = true
		s.addItem(instanceID)
		if strings.TrimSpace(text) == "" {
			s.addEmptyText(instanceID)
			return nil
		}

		hash := hashText(text)
		if prior, ok := existing[key]; ok {
			if prior.ContentHash == hash {
				s.addSkipped(instanceID)
				return nil
			}
			if err := s.repo.DeleteDocumentChunks(ctx, instanceID, doc.ItemKey, doc.AttachmentKey); err != nil {
				s.addFailed(instanceID, fmt.Errorf("clearing %q's old chunks: %w", doc.AttachmentKey, err))
				return nil
			}
		}

		chunkCount, chunkDim, ierr := s.indexDocument(ctx, instanceID, settings, apiKey, doc, text)
		if ierr != nil {
			s.addFailed(instanceID, ierr)
			return nil
		}
		if chunkCount == 0 {
			return nil
		}
		dimension = chunkDim
		if err := s.repo.UpsertDocument(ctx, domain.IndexDocument{
			InstanceID: instanceID, ItemKey: doc.ItemKey, AttachmentKey: doc.AttachmentKey,
			ContentHash: hash, ChunkCount: chunkCount, UpdatedAt: time.Now(),
		}); err != nil {
			s.logger.Warn("index: recording document bookkeeping failed",
				slog.String("attachment", doc.AttachmentKey), slog.String("error", err.Error()))
		}
		return nil
	})
	if err != nil {
		s.fail(instanceID, err)
		return
	}

	// Pruning trusts that the crawl saw everything: a truncated run's
	// "unseen" documents may simply be ones it never reached, not ones
	// deleted upstream, so treating them as deletions would be data loss.
	if mode == ReindexUpdate && !truncated {
		for key, doc := range existing {
			if seen[key] {
				continue
			}
			if err := s.repo.DeleteDocumentChunks(ctx, instanceID, doc.ItemKey, doc.AttachmentKey); err != nil {
				s.logger.Warn("index: pruning a document's chunks failed",
					slog.String("attachment", doc.AttachmentKey), slog.String("error", err.Error()))
				continue
			}
			if err := s.repo.DeleteDocument(ctx, instanceID, doc.ItemKey, doc.AttachmentKey); err != nil {
				s.logger.Warn("index: pruning a document's bookkeeping failed",
					slog.String("attachment", doc.AttachmentKey), slog.String("error", err.Error()))
				continue
			}
			s.addPruned(instanceID)
		}
	}

	if dimension > 0 {
		if err := s.repo.SetMeta(ctx, domain.IndexMeta{
			InstanceID: instanceID, EmbedderModel: effectiveModel, EmbedderDimension: dimension, UpdatedAt: time.Now(),
		}); err != nil {
			s.logger.Warn("index: recording embedder meta failed", slog.String("error", err.Error()))
		}
	}
}

func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// indexDocument chunks, embeds and stores one document's text, returning how
// many chunks it produced and their vector dimension. This is the part
// every source shares — chunking and embedding know nothing about where the
// text came from — unlike the crawl itself, which is entirely
// source-specific (see internal/index/source). A non-nil error means the
// caller should count this document as failed rather than silently empty.
func (s *Indexer) indexDocument(ctx context.Context, instanceID string, settings domain.EmbedderSettings, apiKey string, doc source.Document, text string) (chunkCount, dimension int, err error) {
	spans := chunkFor(doc, text)
	if len(spans) > reindexMaxChunksPerFile {
		spans = spans[:reindexMaxChunksPerFile]
	}
	texts := make([]string, len(spans))
	for i, sp := range spans {
		texts[i] = sp.Text
	}

	var chunks []domain.IndexChunk
	for i := 0; i < len(texts); i += reindexEmbedBatch {
		end := min(i+reindexEmbedBatch, len(texts))
		vectors, embedErr := s.embedder.Embed(ctx, settings.URL, settings.Model, apiKey, settings.RateLimitPerMin,
			embedderEgressPolicy, texts[i:end])
		if embedErr != nil {
			return 0, 0, fmt.Errorf("embedding %q: %w", doc.AttachmentKey, embedErr)
		}
		for j, v := range vectors {
			sp := spans[i+j]
			chunks = append(chunks, domain.IndexChunk{
				InstanceID: instanceID, ItemKey: doc.ItemKey, AttachmentKey: doc.AttachmentKey,
				ChunkIndex: sp.Index, CharStart: sp.Start, CharEnd: sp.End, Text: sp.Text, Embedding: v,
			})
		}
	}
	if len(chunks) == 0 {
		return 0, 0, nil
	}
	if err := s.repo.InsertChunks(ctx, instanceID, chunks); err != nil {
		return 0, 0, fmt.Errorf("storing chunks for %q: %w", doc.AttachmentKey, err)
	}
	s.addChunks(instanceID, len(chunks))
	return len(chunks), len(chunks[0].Embedding), nil
}

// chunkFor picks the splitter a document's crawler asked for. A source sets
// HeadingDialect on a per-document basis (a Gitea repository can mix
// markdown and typst files, say), never globally per connector, so the
// dispatch has to happen here rather than once per crawl.
func chunkFor(doc source.Document, text string) []index.Span {
	if doc.HeadingDialect != "" {
		return index.ChunkHeading(text, doc.HeadingDialect)
	}
	return index.Chunk(text)
}

func (s *Indexer) addItem(instanceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.status[instanceID]; st != nil {
		st.ItemsProcessed++
	}
}

func (s *Indexer) addChunks(instanceID string, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.status[instanceID]; st != nil {
		st.ChunksIndexed += n
	}
}

func (s *Indexer) addSkipped(instanceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.status[instanceID]; st != nil {
		st.DocumentsSkipped++
	}
}

func (s *Indexer) addPruned(instanceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.status[instanceID]; st != nil {
		st.DocumentsPruned++
	}
}

func (s *Indexer) addEmptyText(instanceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.status[instanceID]; st != nil {
		st.DocumentsEmptyText++
	}
}

// addFailed records a per-document failure: it still logs (an operator
// tailing the logs sees every failure, not just the last), but unlike the
// old warn-only handling it also counts the failure and surfaces its message
// as LastError, so a broken embedder is visible on the instance page instead
// of only in ChunksIndexed silently staying at zero.
func (s *Indexer) addFailed(instanceID string, err error) {
	s.logger.Warn("index: document failed", slog.String("error", err.Error()))
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.status[instanceID]; st != nil {
		st.DocumentsFailed++
		st.LastError = err.Error()
	}
}

func (s *Indexer) fail(instanceID string, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.status[instanceID]
	if st == nil {
		st = &IndexStatus{}
		s.status[instanceID] = st
	}
	st.LastError = err.Error()
}

func (s *Indexer) finish(instanceID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.status[instanceID]; st != nil {
		st.Running = false
		st.FinishedAt = time.Now()
	}
}

// Search runs a hybrid keyword + vector search over an instance's index and
// returns short, targeted excerpts rather than whole documents — the point
// of the feature is to keep what a consumer has to read small.
func (s *Indexer) Search(ctx context.Context, instanceID, query string, limit int) ([]SearchHit, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("%w: query must not be empty", domain.ErrInvalidInput)
	}
	if limit <= 0 || limit > maxSearchLimit {
		limit = defaultSearchLimit
	}

	if _, err := s.instances.ResolveByID(ctx, instanceID); err != nil {
		return nil, err
	}
	settings, err := s.platform.GetEmbedderSettings(ctx)
	if err != nil {
		return nil, err
	}
	if settings.URL == "" {
		return nil, fmt.Errorf("semantic search is not configured on this deployment")
	}
	apiKey, err := s.embedderAPIKey(ctx)
	if err != nil {
		return nil, err
	}

	vectors, err := s.embedder.Embed(ctx, settings.URL, settings.Model, apiKey, settings.RateLimitPerMin,
		embedderEgressPolicy, []string{query})
	if err != nil {
		return nil, err
	}
	queryVec := vectors[0]

	all, err := s.repo.LoadAll(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	if len(all) == 0 {
		return nil, nil
	}

	byID := make(map[int64]domain.IndexChunk, len(all))
	scoreByID := make(map[int64]float32, len(all))
	type scored struct {
		id    int64
		score float32
	}
	ranked := make([]scored, 0, len(all))
	for _, c := range all {
		byID[c.ID] = c
		sc := index.Cosine(queryVec, c.Embedding)
		scoreByID[c.ID] = sc
		ranked = append(ranked, scored{c.ID, sc})
	}
	sort.Slice(ranked, func(i, j int) bool { return ranked[i].score > ranked[j].score })

	pool := searchCandidatePool
	if pool > len(ranked) {
		pool = len(ranked)
	}
	vectorIDs := make([]int64, pool)
	for i := 0; i < pool; i++ {
		vectorIDs[i] = ranked[i].id
	}

	bm25IDs, err := s.repo.BM25Search(ctx, instanceID, sanitizeFTSQuery(query), searchCandidatePool)
	if err != nil {
		// Keyword search failing must not take down vector search.
		bm25IDs = nil
	}

	fused := index.FuseRRF(vectorIDs, bm25IDs)
	if len(fused) > limit {
		fused = fused[:limit]
	}

	hits := make([]SearchHit, 0, len(fused))
	for _, id := range fused {
		c, ok := byID[id]
		if !ok {
			continue
		}
		hits = append(hits, SearchHit{
			ItemKey: c.ItemKey, AttachmentKey: c.AttachmentKey, ChunkIndex: c.ChunkIndex,
			Score: float64(scoreByID[id]), Text: truncateRunes(c.Text, maxChunkPreviewRunes),
		})
	}
	return hits, nil
}

func sanitizeFTSQuery(q string) string {
	fields := strings.Fields(q)
	if len(fields) == 0 {
		return ""
	}
	var b strings.Builder
	for i, f := range fields {
		if i > 0 {
			b.WriteString(" OR ")
		}
		f = strings.ReplaceAll(f, `"`, `""`)
		b.WriteByte('"')
		b.WriteString(f)
		b.WriteByte('"')
	}
	return b.String()
}

func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}
