package service

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/00101010xyz/mcpaw/internal/connector"
	"github.com/00101010xyz/mcpaw/internal/domain"
	"github.com/00101010xyz/mcpaw/internal/engine"
	"github.com/00101010xyz/mcpaw/internal/index"
	"github.com/00101010xyz/mcpaw/internal/store"
)

// zoteroConnectorID is the only connector Phase 1 indexing understands how to
// crawl: it calls specific Zotero tool names to enumerate items, find PDF and
// snapshot attachments, and pull their extracted text. Generalising this to
// any connector (via a declarative "how to enumerate content" block in the
// manifest) is future work, not something to guess at here.
const zoteroConnectorID = "zotero-local"

// Chunking, batching and safety caps for one reindex run. These bound how
// long and how much memory an operator-triggered reindex can consume; they
// are deliberately generous for a personal reference library and deliberately
// not exposed as configuration, to keep the feature's surface small.
const (
	reindexPageSize         = 100
	reindexMaxItems         = 2000
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
	Running        bool
	ItemsProcessed int
	ChunksIndexed  int
	LastError      string
	StartedAt      time.Time
	FinishedAt     time.Time
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
	Instances *Instances
	Audit     *Audit
	Embedder  *index.Embedder
	Logger    *slog.Logger
}

// Indexer builds and serves the semantic-search index over Zotero PDF and
// snapshot text. It is entirely additive: an instance with no embedder
// configured and no chunks indexed behaves exactly as it did before this
// feature existed — Ready reports false and no tool is advertised.
type Indexer struct {
	repo      store.SearchIndexRepository
	instances *Instances
	audit     *Audit
	embedder  *index.Embedder
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
		repo: cfg.Repo, instances: cfg.Instances, audit: cfg.Audit, embedder: cfg.Embedder,
		logger: logger, status: map[string]*IndexStatus{},
	}
}

// Supported reports whether a connector is one this indexer knows how to
// crawl. The web UI uses this to decide whether to show the feature at all.
func (s *Indexer) Supported(connectorID string) bool { return connectorID == zoteroConnectorID }

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

// Reindex clears and rebuilds an instance's index from scratch, in the
// background: a real library's worth of PDFs takes many embedding calls, far
// longer than an operator should have to wait on an HTTP response for.
func (s *Indexer) Reindex(ctx context.Context, actor Actor, instanceID string) error {
	resolved, err := s.instances.ResolveByID(ctx, instanceID)
	if err != nil {
		return err
	}
	if !s.Supported(resolved.ConnectorRec.ID) {
		return fmt.Errorf("%w: semantic search indexing currently supports only the Zotero (Local API) connector", domain.ErrInvalidInput)
	}

	s.mu.Lock()
	if st, ok := s.status[instanceID]; ok && st.Running {
		s.mu.Unlock()
		return fmt.Errorf("a reindex is already running for this instance")
	}
	s.status[instanceID] = &IndexStatus{Running: true, StartedAt: time.Now()}
	s.mu.Unlock()

	s.audit.Success(ctx, actor, domain.ActionIndexReindex, "instance", instanceID, map[string]any{"action": "started"})
	go s.runReindex(instanceID)
	return nil
}

func (s *Indexer) runReindex(instanceID string) {
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
	if resolved.Instance.EmbedderURL == "" {
		s.fail(instanceID, fmt.Errorf("set the embedder URL on this instance before indexing"))
		return
	}

	topItems, ok := s.enabledTool(resolved, "zotero_list_top_items")
	if !ok {
		s.fail(instanceID, fmt.Errorf("this instance needs zotero_list_top_items enabled to index"))
		return
	}
	children, ok := s.enabledTool(resolved, "zotero_get_item_children")
	if !ok {
		s.fail(instanceID, fmt.Errorf("this instance needs zotero_get_item_children enabled to index"))
		return
	}
	fulltext, ok := s.enabledTool(resolved, "zotero_get_item_fulltext")
	if !ok {
		s.fail(instanceID, fmt.Errorf("this instance needs zotero_get_item_fulltext enabled to index"))
		return
	}

	if err := s.repo.ClearInstance(ctx, instanceID); err != nil {
		s.fail(instanceID, err)
		return
	}

	executor := s.instances.Executor()
	start := 0
	for processed := 0; processed < reindexMaxItems; {
		result, err := executor.Execute(ctx, target, resolved.Connector, topItems, map[string]any{"limit": reindexPageSize, "start": start})
		if err != nil {
			s.fail(instanceID, err)
			return
		}
		items, _ := result.Data.([]any)
		if len(items) == 0 {
			break
		}
		for _, raw := range items {
			item, _ := raw.(map[string]any)
			key, _ := item["key"].(string)
			if key == "" {
				continue
			}
			s.indexItem(ctx, executor, target, resolved, children, fulltext, instanceID, key)
			processed++
			s.addItem(instanceID)
		}
		if len(items) < reindexPageSize {
			break
		}
		start += reindexPageSize
	}
}

// enabledTool looks up a tool the connector declares and requires it to be
// enabled on this instance: indexing runs unattended, so it must respect the
// same on/off switch an operator uses to turn a tool off for MCP clients,
// rather than reaching upstream behind their back.
func (s *Indexer) enabledTool(r *Resolved, name string) (*connector.CompiledTool, bool) {
	tool, ok := r.Connector.Tool(name)
	if !ok || !r.EnabledTools[name] {
		return nil, false
	}
	return tool, true
}

func (s *Indexer) indexItem(ctx context.Context, executor *engine.Executor, target *engine.Target, resolved *Resolved, childrenTool, fulltextTool *connector.CompiledTool, instanceID, itemKey string) {
	result, err := executor.Execute(ctx, target, resolved.Connector, childrenTool, map[string]any{"itemKey": itemKey, "limit": 50})
	if err != nil {
		s.logger.Debug("index: listing children failed", slog.String("item", itemKey), slog.String("error", err.Error()))
		return
	}
	children, _ := result.Data.([]any)
	for _, raw := range children {
		child, _ := raw.(map[string]any)
		data, _ := child["data"].(map[string]any)
		if data == nil {
			continue
		}
		if itemType, _ := data["itemType"].(string); itemType != "attachment" {
			continue
		}
		contentType, _ := data["contentType"].(string)
		if contentType != "application/pdf" && contentType != "text/html" {
			continue
		}
		attKey, _ := child["key"].(string)
		if attKey == "" {
			continue
		}
		s.indexAttachment(ctx, executor, target, resolved, fulltextTool, instanceID, itemKey, attKey)
	}
}

func (s *Indexer) indexAttachment(ctx context.Context, executor *engine.Executor, target *engine.Target, resolved *Resolved, fulltextTool *connector.CompiledTool, instanceID, itemKey, attachmentKey string) {
	result, err := executor.Execute(ctx, target, resolved.Connector, fulltextTool, map[string]any{"itemKey": attachmentKey})
	if err != nil {
		// Most commonly a 404: Zotero has not extracted text for this
		// attachment yet. Not worth aborting or even logging per-attachment.
		return
	}
	data, _ := result.Data.(map[string]any)
	content, _ := data["content"].(string)
	if strings.TrimSpace(content) == "" {
		return
	}

	spans := index.Chunk(content)
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
		vectors, err := s.embedder.Embed(ctx, resolved.Instance.EmbedderURL, resolved.Instance.EmbedderModel,
			target.Secrets[index.EmbedderAPIKey], target.Policy, texts[i:end])
		if err != nil {
			s.logger.Warn("index: embedding failed", slog.String("attachment", attachmentKey), slog.String("error", err.Error()))
			return
		}
		for j, v := range vectors {
			sp := spans[i+j]
			chunks = append(chunks, domain.IndexChunk{
				InstanceID: instanceID, ItemKey: itemKey, AttachmentKey: attachmentKey,
				ChunkIndex: sp.Index, CharStart: sp.Start, CharEnd: sp.End, Text: sp.Text, Embedding: v,
			})
		}
	}
	if len(chunks) == 0 {
		return
	}
	if err := s.repo.InsertChunks(ctx, instanceID, chunks); err != nil {
		s.logger.Warn("index: storing chunks failed", slog.String("attachment", attachmentKey), slog.String("error", err.Error()))
		return
	}
	s.addChunks(instanceID, len(chunks))
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

	resolved, err := s.instances.ResolveByID(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	target, err := s.instances.Target(resolved)
	if err != nil {
		return nil, err
	}
	if resolved.Instance.EmbedderURL == "" {
		return nil, fmt.Errorf("semantic search is not configured for this instance")
	}

	vectors, err := s.embedder.Embed(ctx, resolved.Instance.EmbedderURL, resolved.Instance.EmbedderModel,
		target.Secrets[index.EmbedderAPIKey], target.Policy, []string{query})
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
