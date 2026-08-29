package webui

import (
	"net/http"
	"strings"

	"github.com/00101010xyz/mcpaw/internal/usage"
)

// usageLevels is the fixed order the level select offers, off-first since
// that is the most conservative choice.
var usageLevels = []usage.Level{usage.LevelOff, usage.LevelMetadata, usage.LevelFull}

// GetUsage renders the usage log: every MCP tool call recorded at the
// currently configured level, most recent first.
func (s *Server) GetUsage(w http.ResponseWriter, r *http.Request) {
	if !s.usage.Enabled() {
		s.flashError(r, "The usage log is not available on this deployment.")
		redirect(w, r, "/")
		return
	}
	ctx := r.Context()

	instanceID := r.URL.Query().Get("instance")
	tool := strings.TrimSpace(r.URL.Query().Get("tool"))

	entries, err := s.usage.List(ctx, usage.Filter{InstanceID: instanceID, Tool: tool})
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	instances, err := s.instances.List(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	settings, err := s.usage.Settings(ctx)
	if err != nil {
		s.internalError(w, r, err)
		return
	}

	s.render(w, r, http.StatusOK, "usage", s.page(r, "Usage log", "usage", map[string]any{
		"Entries": entries, "Instances": instances, "Level": settings.Level,
		"SelectedInstance": instanceID, "SelectedTool": tool,
	}))
}

// defaultUsageMaxMB pre-fills the settings form the same way settings_search
// pre-fills the embedder URL: the common case is "confirm and save", not
// "look up and type".
const defaultUsageMaxMB = usage.DefaultMaxBytes / (1 << 20)

// GetSettingsUsage renders the usage log's level and size-cap configuration.
func (s *Server) GetSettingsUsage(w http.ResponseWriter, r *http.Request) {
	if !s.usage.Enabled() {
		s.flashError(r, "The usage log is not available on this deployment.")
		redirect(w, r, "/")
		return
	}
	settings, err := s.usage.Settings(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	maxMB := settings.MaxBytes / (1 << 20)
	if maxMB <= 0 {
		maxMB = defaultUsageMaxMB
	}
	s.render(w, r, http.StatusOK, "settings_usage", s.page(r, "Usage log settings", "settings-usage", map[string]any{
		"Levels": usageLevels, "Level": settings.Level, "MaxMB": maxMB,
	}))
}

// PostSettingsUsage saves the log level and size cap.
func (s *Server) PostSettingsUsage(w http.ResponseWriter, r *http.Request) {
	level := usage.Level(r.PostFormValue("level"))
	maxMB := parseInt64Field(r, "max_mb", defaultUsageMaxMB)
	if maxMB <= 0 {
		maxMB = defaultUsageMaxMB
	}
	if err := s.usage.UpdateSettings(r.Context(), s.actor(r), level, maxMB*(1<<20)); err != nil {
		s.flashError(r, "%s", errorMessage(err))
	} else {
		s.flashSuccess(r, "Usage log settings saved.")
	}
	redirect(w, r, "/settings/usage")
}

// PostUsageClear deletes every stored usage log entry immediately.
func (s *Server) PostUsageClear(w http.ResponseWriter, r *http.Request) {
	if err := s.usage.Clear(r.Context(), s.actor(r)); err != nil {
		s.flashError(r, "%s", errorMessage(err))
	} else {
		s.flashSuccess(r, "Usage log cleared.")
	}
	redirect(w, r, "/usage")
}
