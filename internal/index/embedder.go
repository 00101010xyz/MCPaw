package index

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/00101010xyz/mcpaw/internal/upstream"
)

// EmbedderAPIKey names the platform secret used to store the embedder
// sidecar's API key, when it needs one — see domain.EmbedderSettings for why
// it is a single platform-wide credential rather than a per-instance one.
const (
	EmbedderAPIKey  = "embedderApiKey"
	DefaultEmbedder = "nomic-embed-text"
)

// maxEmbedResponseBytes bounds one embedding response: a batch of chunk
// vectors is small (a few KB per vector), so anything larger indicates a
// misconfigured endpoint rather than a legitimate reply.
const maxEmbedResponseBytes = 16 << 20

// embedderLimiterKey is the single rate-limit bucket every embed call shares:
// the embedder is one platform-wide sidecar (see domain.EmbedderSettings),
// not a per-instance upstream, so there is exactly one budget to enforce
// rather than one per instance.
const embedderLimiterKey = "embedder"

// Embedder calls a local embedding sidecar over the same SSRF-guarded
// client every connector call uses, so embedding text is subject to the same
// egress policy as reaching Zotero itself.
type Embedder struct {
	Client *upstream.Client
	// Limiter enforces the configurable rate limit on embed calls (see
	// domain.EmbedderSettings.RateLimitPerMin). Nil means unlimited, which
	// keeps the zero value usable in tests that don't care about throttling.
	Limiter upstream.Limiter
}

// embedRequest/embedResponse follow the Ollama /api/embed contract
// (https://github.com/ollama/ollama/blob/main/docs/api.md#generate-embeddings),
// which TEI (text-embeddings-inference) and most local embedding sidecars
// also accept. Phase 1 targets this one shape deliberately rather than
// abstracting over several; see docs/ARCHITECTURE.md for the tradeoff.
type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Embeddings [][]float32 `json:"embeddings"`
	Error      string      `json:"error"`
}

// Embed returns one vector per input text, in order. baseURL, model and
// policy come from the caller's already-resolved settings so this package
// never has to know how they are stored. rateLimitPerMin is enforced through
// Limiter before the call is made; a non-positive value means unlimited.
func (e *Embedder) Embed(ctx context.Context, baseURL, model, apiKey string, rateLimitPerMin int, policy upstream.EgressPolicy, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if baseURL == "" {
		return nil, fmt.Errorf("index: no embedder URL is configured")
	}
	if model == "" {
		model = DefaultEmbedder
	}
	if e.Limiter != nil && !e.Limiter.Allow(embedderLimiterKey, rateLimitPerMin) {
		return nil, fmt.Errorf("index: %w", upstream.ErrRateLimited)
	}

	body, err := json.Marshal(embedRequest{Model: model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("index: encoding embed request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/api/embed", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("index: building embed request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}

	resp, err := e.Client.Do(req, policy, maxEmbedResponseBytes)
	if err != nil {
		return nil, fmt.Errorf("index: calling embedder: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("index: embedder returned %d: %s", resp.StatusCode, truncate(resp.Body, 300))
	}

	var out embedResponse
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return nil, fmt.Errorf("index: decoding embed response: %w", err)
	}
	if out.Error != "" {
		return nil, fmt.Errorf("index: embedder error: %s", out.Error)
	}
	if len(out.Embeddings) != len(texts) {
		return nil, fmt.Errorf("index: embedder returned %d vectors for %d inputs", len(out.Embeddings), len(texts))
	}
	return out.Embeddings, nil
}

func truncate(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}
