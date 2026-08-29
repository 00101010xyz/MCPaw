package webui

import (
	"net/http"
	"strings"

	"github.com/00101010xyz/mcpaw/internal/index"
)

// defaultEmbedderURL is pre-filled on the settings page only when nothing has
// been saved yet, so the common case (a local Ollama sidecar) is "confirm
// and save" rather than "look up and type" — it stays fully editable.
const defaultEmbedderURL = "http://host.docker.internal:11434"

// GetSettingsSearch renders the platform-wide semantic search configuration:
// one embedder shared by every instance capable of indexing, rather than a
// setting repeated per instance (see domain.EmbedderSettings).
func (s *Server) GetSettingsSearch(w http.ResponseWriter, r *http.Request) {
	if s.indexer == nil {
		s.flashError(r, "Semantic search is not available on this deployment.")
		redirect(w, r, "/")
		return
	}
	settings, apiKeySet, err := s.indexer.EmbedderSettings(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	configured := settings.URL != ""
	if !configured {
		settings.URL = defaultEmbedderURL
		settings.Model = index.DefaultEmbedder
	}
	s.render(w, r, http.StatusOK, "settings_search", s.page(r, "Semantic search", "settings-search", map[string]any{
		"Settings": settings, "APIKeySet": apiKeySet, "Configured": configured,
	}))
}

// PostSettingsSearch saves the shared embedder URL, model and rate limit.
func (s *Server) PostSettingsSearch(w http.ResponseWriter, r *http.Request) {
	if s.indexer == nil {
		s.flashError(r, "Semantic search is not available on this deployment.")
		redirect(w, r, "/")
		return
	}
	embedderURL := strings.TrimSpace(r.PostFormValue("embedder_url"))
	embedderModel := strings.TrimSpace(r.PostFormValue("embedder_model"))
	rateLimit := parseIntField(r, "rate_limit_per_min", 60)
	if err := s.indexer.UpdateEmbedderSettings(r.Context(), s.actor(r), embedderURL, embedderModel, rateLimit); err != nil {
		s.flashError(r, "%s", errorMessage(err))
	} else {
		s.flashSuccess(r, "Semantic search settings saved.")
	}
	redirect(w, r, "/settings/semantic-search")
}

// PostSettingsSearchSecret stores the embedder sidecar's API key.
func (s *Server) PostSettingsSearchSecret(w http.ResponseWriter, r *http.Request) {
	value := r.PostFormValue("value")
	if strings.TrimSpace(value) == "" {
		s.flashError(r, "Enter a value, or use Remove to clear it.")
		redirect(w, r, "/settings/semantic-search")
		return
	}
	if err := s.indexer.SetEmbedderAPIKey(r.Context(), s.actor(r), value); err != nil {
		s.flashError(r, "%s", errorMessage(err))
	} else {
		s.flashSuccess(r, "Saved. The value is encrypted and will not be shown again.")
	}
	redirect(w, r, "/settings/semantic-search")
}

// PostSettingsSearchSecretDelete removes the stored embedder API key.
func (s *Server) PostSettingsSearchSecretDelete(w http.ResponseWriter, r *http.Request) {
	if err := s.indexer.DeleteEmbedderAPIKey(r.Context(), s.actor(r)); err != nil {
		s.flashError(r, "%s", errorMessage(err))
	} else {
		s.flashSuccess(r, "Removed.")
	}
	redirect(w, r, "/settings/semantic-search")
}
