package webui

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/00101010xyz/mcpaw/internal/connector"
	"github.com/00101010xyz/mcpaw/internal/service"
)

// GetInstances renders the dashboard.
func (s *Server) GetInstances(w http.ResponseWriter, r *http.Request) {
	instances, err := s.instances.List(r.Context())
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	s.render(w, r, http.StatusOK, "instances",
		s.page(r, "Instances", "instances", map[string]any{"Instances": instances}))
}

// instanceForm is the sticky form state for the creation page, so a validation
// failure does not throw away what the operator typed.
type instanceForm struct {
	Name                string
	Slug                string
	Description         string
	BaseURL             string
	Variables           map[string]string
	Enabled             bool
	AllowPrivateNetwork bool
	HostHeaderOverride  string
}

// GetNewInstance renders the creation form.
func (s *Server) GetNewInstance(w http.ResponseWriter, r *http.Request) {
	s.renderNewInstance(w, r, http.StatusOK, r.URL.Query().Get("connector"), nil)
}

func (s *Server) renderNewInstance(w http.ResponseWriter, r *http.Request, status int, connectorID string, form *instanceForm) {
	entries := s.connectors.List(r.Context())
	if connectorID == "" && len(entries) > 0 {
		connectorID = entries[0].Record.ID
	}

	var selected *connector.Entry
	for _, e := range entries {
		if e.Record.ID == connectorID {
			selected = e
			break
		}
	}
	if form == nil {
		form = defaultForm(selected)
	}

	s.render(w, r, status, "instance_new", s.page(r, "New instance", "instances", map[string]any{
		"Connectors":        entries,
		"SelectedConnector": connectorID,
		"Selected":          selected,
		"Form":              form,
	}))
}

// defaultForm pre-fills the creation form from the connector's own defaults, so
// the common case is "review and confirm" rather than "look everything up".
func defaultForm(selected *connector.Entry) *instanceForm {
	form := &instanceForm{Variables: map[string]string{}, Enabled: true}
	if selected == nil {
		return form
	}
	form.Name = selected.Record.Name
	form.BaseURL = selected.Compiled.Manifest.Spec.BaseURL.Default
	for _, v := range selected.Compiled.Variables() {
		form.Variables[v.Name] = v.Default
	}
	// Pre-ticking the egress box for a connector that documents it as required
	// would defeat the purpose of asking, so it stays off and the form explains
	// why it exists.
	form.HostHeaderOverride = suggestHostHeaderOverride(selected.Compiled.Manifest.Spec.BaseURL.Default)
	return form
}

// suggestHostHeaderOverride pre-fills the Host header override for the one
// case it is reliably correct to guess: a connector whose default base URL
// points at host.docker.internal, the address a container uses to reach a
// service bound to the host machine's own loopback interface. Such a service
// (the Zotero desktop app's local API is the shipped example) very often
// validates the Host header as a DNS-rebinding defense and rejects anything
// but a loopback name — exactly what host.docker.internal is not. Presenting
// 127.0.0.1 there, while still connecting via host.docker.internal, is
// correct far more often than not; the field stays editable either way.
func suggestHostHeaderOverride(defaultBaseURL string) string {
	u, err := url.Parse(defaultBaseURL)
	if err != nil || u.Hostname() != "host.docker.internal" {
		return ""
	}
	if port := u.Port(); port != "" {
		return "127.0.0.1:" + port
	}
	return "127.0.0.1"
}

// PostInstances handles both the "choose connector" refresh and the create
// submission from the same form.
func (s *Server) PostInstances(w http.ResponseWriter, r *http.Request) {
	connectorID := r.PostFormValue("connector_id")
	if r.PostFormValue("action") == "choose" {
		s.renderNewInstance(w, r, http.StatusOK, connectorID, nil)
		return
	}

	entry, err := s.connectors.Get(r.Context(), connectorID)
	if err != nil {
		s.flashError(r, "%s", errorMessage(err))
		redirect(w, r, "/instances/new")
		return
	}

	form := &instanceForm{
		Name:                r.PostFormValue("name"),
		Slug:                r.PostFormValue("slug"),
		Description:         r.PostFormValue("description"),
		BaseURL:             r.PostFormValue("base_url"),
		Variables:           collectVariables(r, entry),
		Enabled:             checkbox(r, "enabled"),
		AllowPrivateNetwork: checkbox(r, "allow_private_network"),
		HostHeaderOverride:  strings.TrimSpace(r.PostFormValue("host_header_override")),
	}

	instance, err := s.instances.Create(r.Context(), s.actor(r), service.CreateInput{
		Name: form.Name, Slug: form.Slug, Description: form.Description,
		ConnectorID: connectorID, BaseURL: form.BaseURL, Variables: form.Variables,
		Enabled: form.Enabled, AllowPrivateNetwork: form.AllowPrivateNetwork,
		HostHeaderOverride: form.HostHeaderOverride,
	})
	if err != nil {
		data := s.page(r, "New instance", "instances", nil)
		data.Flash = &Flash{Level: FlashError, Message: errorMessage(err)}
		entries := s.connectors.List(r.Context())
		data.Data = map[string]any{
			"Connectors": entries, "SelectedConnector": connectorID,
			"Selected": entry, "Form": form,
		}
		s.render(w, r, http.StatusBadRequest, "instance_new", data)
		return
	}

	s.flashSuccess(r, "Instance created. Issue a token, then point your MCP client at /mcp/%s.", instance.Slug)
	redirect(w, r, "/instances/"+instance.ID)
}

func collectVariables(r *http.Request, entry *connector.Entry) map[string]string {
	out := map[string]string{}
	for _, v := range entry.Compiled.Variables() {
		// Only declared variables are read from the form: an extra field in a
		// crafted POST cannot smuggle a value the manifest never declared.
		if value := strings.TrimSpace(r.PostFormValue("var_" + v.Name)); value != "" {
			out[v.Name] = value
		}
	}
	return out
}

// GetInstance renders the configuration page for one instance.
func (s *Server) GetInstance(w http.ResponseWriter, r *http.Request) {
	detail, err := s.instances.Detail(r.Context(), r.PathValue("id"))
	if err != nil {
		s.notFound(w, r, err)
		return
	}
	data := map[string]any{"Detail": detail}
	if s.indexer != nil && detail.Connector != nil && s.indexer.Supported(detail.Connector.Record.ID) {
		status, count, err := s.indexer.Status(r.Context(), detail.Instance.ID)
		if err != nil {
			s.logger.Warn("reading search index status", "instance_id", detail.Instance.ID, "error", err)
		} else {
			data["SearchIndex"] = map[string]any{"Status": status, "ChunkCount": count}
		}
	}
	s.render(w, r, http.StatusOK, "instance_detail",
		s.page(r, detail.Instance.Name, "instances", data))
}

// PostInstanceReindex starts a background rebuild of an instance's semantic
// search index.
func (s *Server) PostInstanceReindex(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if s.indexer == nil {
		s.flashError(r, "Semantic search is not available on this deployment.")
		redirect(w, r, "/instances/"+instanceID)
		return
	}
	if err := s.indexer.Reindex(r.Context(), s.actor(r), instanceID); err != nil {
		s.flashError(r, "%s", errorMessage(err))
	} else {
		s.flashSuccess(r, "Reindexing started. This can take a while for a large library; refresh this page to check progress.")
	}
	redirect(w, r, "/instances/"+instanceID)
}

// PostInstance saves a configuration change.
func (s *Server) PostInstance(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	detail, err := s.instances.Detail(r.Context(), instanceID)
	if err != nil {
		s.notFound(w, r, err)
		return
	}

	name := r.PostFormValue("name")
	description := r.PostFormValue("description")
	baseURL := r.PostFormValue("base_url")
	enabled := checkbox(r, "enabled")
	allowPrivate := checkbox(r, "allow_private_network")
	hostHeaderOverride := strings.TrimSpace(r.PostFormValue("host_header_override"))

	in := service.UpdateInput{
		Name: &name, Description: &description, BaseURL: &baseURL,
		Enabled: &enabled, AllowPrivateNetwork: &allowPrivate,
		HostHeaderOverride: &hostHeaderOverride,
	}
	if detail.Connector != nil {
		vars := collectVariables(r, detail.Connector)
		in.Variables = vars
	}
	timeout := parseIntField(r, "timeout_ms", detail.Instance.TimeoutMS)
	rate := parseIntField(r, "rate_limit_per_min", detail.Instance.RateLimitPerMin)
	concurrent := parseIntField(r, "max_concurrent", detail.Instance.MaxConcurrent)
	maxBytes := parseInt64Field(r, "max_response_bytes", detail.Instance.MaxResponseBytes)
	in.TimeoutMS, in.RateLimitPerMin = &timeout, &rate
	in.MaxConcurrent, in.MaxResponseBytes = &concurrent, &maxBytes

	if _, err := s.instances.Update(r.Context(), s.actor(r), instanceID, in); err != nil {
		s.flashError(r, "%s", errorMessage(err))
	} else {
		s.flashSuccess(r, "Configuration saved.")
	}
	redirect(w, r, "/instances/"+instanceID)
}

// PostInstanceSecret stores a credential.
func (s *Server) PostInstanceSecret(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	name := r.PostFormValue("name")
	value := r.PostFormValue("value")

	if strings.TrimSpace(value) == "" {
		s.flashError(r, "Enter a value for %s, or use Remove to clear it.", name)
		redirect(w, r, "/instances/"+instanceID)
		return
	}
	if err := s.instances.SetSecret(r.Context(), s.actor(r), instanceID, name, value); err != nil {
		s.flashError(r, "%s", errorMessage(err))
	} else {
		s.flashSuccess(r, "Saved %s. The value is encrypted and will not be shown again.", name)
	}
	redirect(w, r, "/instances/"+instanceID)
}

// PostInstanceSecretDelete removes a credential.
func (s *Server) PostInstanceSecretDelete(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	name := r.PostFormValue("name")
	if err := s.instances.DeleteSecret(r.Context(), s.actor(r), instanceID, name); err != nil {
		s.flashError(r, "%s", errorMessage(err))
	} else {
		s.flashSuccess(r, "Removed %s.", name)
	}
	redirect(w, r, "/instances/"+instanceID)
}

// PostInstanceTool toggles a tool.
func (s *Server) PostInstanceTool(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	tool := r.PostFormValue("tool")
	enabled := r.PostFormValue("enabled") == "true"

	if err := s.instances.SetToolEnabled(r.Context(), s.actor(r), instanceID, tool, enabled); err != nil {
		s.flashError(r, "%s", errorMessage(err))
	} else if enabled {
		s.flashSuccess(r, "Enabled %s.", tool)
	} else {
		s.flashSuccess(r, "Disabled %s. It is no longer advertised to clients.", tool)
	}
	redirect(w, r, "/instances/"+instanceID)
}

// PostInstanceTest runs a live connectivity check.
func (s *Server) PostInstanceTest(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	result, err := s.instances.TestConnection(r.Context(), s.actor(r), instanceID, r.PostFormValue("tool"))
	if err != nil {
		s.flashError(r, "%s", errorMessage(err))
		redirect(w, r, "/instances/"+instanceID)
		return
	}

	level := FlashSuccess
	message := "Connection test succeeded."
	if !result.OK {
		level = FlashError
		message = "Connection test failed."
	}
	s.flash(r, &Flash{Level: level, Message: message, Test: &TestOutcome{
		OK: result.OK, Tool: result.Tool, StatusCode: result.StatusCode,
		DurationMS: result.DurationMS, Message: result.Message,
		Hint: result.Hint, Preview: result.Preview,
	}})
	redirect(w, r, "/instances/"+instanceID)
}

// PostInstanceDelete removes an instance after slug confirmation.
func (s *Server) PostInstanceDelete(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	instance, err := s.instances.Get(r.Context(), instanceID)
	if err != nil {
		s.notFound(w, r, err)
		return
	}
	// Typing the slug is a deliberate speed bump: deletion destroys stored
	// credentials and every token scoped to the instance.
	if r.PostFormValue("confirm_slug") != instance.Slug {
		s.flashError(r, "Type the slug %q exactly to confirm deletion.", instance.Slug)
		redirect(w, r, "/instances/"+instanceID)
		return
	}
	if err := s.instances.Delete(r.Context(), s.actor(r), instanceID); err != nil {
		s.flashError(r, "%s", errorMessage(err))
		redirect(w, r, "/instances/"+instanceID)
		return
	}
	s.flashSuccess(r, "Deleted %s.", instance.Name)
	redirect(w, r, "/")
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request, err error) {
	s.flashError(r, "%s", errorMessage(err))
	redirect(w, r, "/")
}
