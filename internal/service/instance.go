package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/00101010xyz/mcpaw/internal/connector"
	"github.com/00101010xyz/mcpaw/internal/domain"
	"github.com/00101010xyz/mcpaw/internal/engine"
	"github.com/00101010xyz/mcpaw/internal/platform/id"
	"github.com/00101010xyz/mcpaw/internal/secrets"
	"github.com/00101010xyz/mcpaw/internal/store"
	"github.com/00101010xyz/mcpaw/internal/upstream"
)

var slugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)

// Instances is the central application service: it owns instance
// configuration, secret handling and the resolution of a slug into an
// executable target.
type Instances struct {
	repo       store.InstanceRepository
	connectors *Connectors
	sealer     secrets.Sealer
	executor   *engine.Executor
	audit      *Audit
	now        func() time.Time

	cache *targetCache
}

// InstancesConfig wires the instance service.
type InstancesConfig struct {
	Repo       store.InstanceRepository
	Connectors *Connectors
	Sealer     secrets.Sealer
	Executor   *engine.Executor
	Audit      *Audit
	// CacheTTL bounds how stale a cached instance may be. Writes invalidate
	// explicitly; the TTL exists so a second replica sharing the database
	// converges without a coordination channel.
	CacheTTL time.Duration
}

// NewInstances constructs the instance service.
func NewInstances(cfg InstancesConfig) *Instances {
	ttl := cfg.CacheTTL
	if ttl <= 0 {
		ttl = 5 * time.Second
	}
	return &Instances{
		repo: cfg.Repo, connectors: cfg.Connectors, sealer: cfg.Sealer,
		executor: cfg.Executor, audit: cfg.Audit, now: time.Now,
		cache: newTargetCache(ttl),
	}
}

// ---------------------------------------------------------------------------
// Read models
// ---------------------------------------------------------------------------

// Summary is the list-view projection of an instance.
type Summary struct {
	Instance       *domain.Instance
	ConnectorID    string
	ConnectorName  string
	ToolCount      int
	EnabledTools   int
	MissingSecrets []string
	// Problem is non-empty when the instance cannot currently serve requests.
	Problem string
}

// Ready reports whether the instance can serve MCP requests right now.
func (s *Summary) Ready() bool { return s.Instance.Enabled && s.Problem == "" }

// ToolView pairs a compiled tool with its per-instance enablement.
type ToolView struct {
	Tool    *connector.CompiledTool
	Enabled bool
}

// VariableView pairs a declared variable with its configured and effective
// values.
type VariableView struct {
	Def       connector.Variable
	Value     string
	Effective string
}

// SecretView reports whether a secret is configured, never its value.
type SecretView struct {
	Def connector.Secret
	Set bool
}

// Detail is the full read model for one instance.
type Detail struct {
	Summary
	Connector *connector.Entry
	Tools     []ToolView
	Variables []VariableView
	Secrets   []SecretView
}

// ---------------------------------------------------------------------------
// Write models
// ---------------------------------------------------------------------------

// CreateInput describes a new instance.
type CreateInput struct {
	Name                string
	Slug                string
	Description         string
	ConnectorID         string
	BaseURL             string
	Variables           map[string]string
	Enabled             bool
	AllowPrivateNetwork bool
	HostHeaderOverride  string
	TimeoutMS           int
	RateLimitPerMin     int
	MaxConcurrent       int
	MaxResponseBytes    int64
}

// UpdateInput describes a configuration change. Nil pointers leave a field
// untouched, which lets a partial form post and a partial JSON PATCH share one
// code path without either being able to accidentally blank a field.
type UpdateInput struct {
	Name                *string
	Description         *string
	BaseURL             *string
	Variables           map[string]string
	Enabled             *bool
	AllowPrivateNetwork *bool
	HostHeaderOverride  *string
	TimeoutMS           *int
	RateLimitPerMin     *int
	MaxConcurrent       *int
	MaxResponseBytes    *int64
}

// ---------------------------------------------------------------------------
// Commands
// ---------------------------------------------------------------------------

// Create validates and stores a new instance.
func (s *Instances) Create(ctx context.Context, actor Actor, in CreateInput) (*domain.Instance, error) {
	entry, err := s.connectors.Get(ctx, in.ConnectorID)
	if err != nil {
		return nil, err
	}

	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, fmt.Errorf("%w: a name is required", domain.ErrInvalidInput)
	}
	slug := strings.ToLower(strings.TrimSpace(in.Slug))
	if slug == "" {
		slug = slugify(name)
	}
	if !slugRe.MatchString(slug) {
		return nil, fmt.Errorf("%w: the endpoint slug must be 2-63 characters of a-z, 0-9 and hyphens", domain.ErrInvalidInput)
	}

	baseURL := strings.TrimSpace(in.BaseURL)
	if baseURL == "" {
		baseURL = entry.Compiled.Manifest.Spec.BaseURL.Default
	}
	if entry.Compiled.Manifest.Spec.BaseURL.Locked {
		baseURL = entry.Compiled.Manifest.Spec.BaseURL.Default
	}
	if err := connector.ValidateBaseURL(baseURL); err != nil {
		return nil, fmt.Errorf("%w: base URL %s", domain.ErrInvalidInput, err)
	}
	if err := entry.Compiled.ValidateVariables(in.Variables); err != nil {
		return nil, fmt.Errorf("%w: %s", domain.ErrInvalidInput, err)
	}
	hostHeaderOverride := strings.TrimSpace(in.HostHeaderOverride)
	if err := validateHostHeaderOverride(hostHeaderOverride); err != nil {
		return nil, err
	}

	defaults := entry.Compiled.Manifest.Spec.Defaults
	now := s.now().UTC()
	inst := &domain.Instance{
		ID: id.New("inst"), Slug: slug, Name: name,
		Description: strings.TrimSpace(in.Description),
		ConnectorID: in.ConnectorID, BaseURL: baseURL,
		Variables: in.Variables, Enabled: in.Enabled,
		AllowPrivateNetwork: in.AllowPrivateNetwork,
		HostHeaderOverride:  hostHeaderOverride,
		TimeoutMS:           clampTimeout(pickInt(in.TimeoutMS, defaults.TimeoutMS, connector.DefaultTimeoutMS)),
		RateLimitPerMin:     clampNonNegative(pickInt(in.RateLimitPerMin, defaults.RateLimitPerMin, connector.DefaultRateLimitPerMin)),
		MaxConcurrent:       clampConcurrency(pickInt(in.MaxConcurrent, defaults.MaxConcurrent, connector.DefaultMaxConcurrent)),
		MaxResponseBytes:    clampResponseBytes(pickInt64(in.MaxResponseBytes, defaults.MaxResponseBytes, connector.DefaultMaxResponseBytes)),
		CreatedAt:           now, UpdatedAt: now, Version: 1,
	}
	if inst.Variables == nil {
		inst.Variables = map[string]string{}
	}

	if err := s.repo.Create(ctx, inst); err != nil {
		if errors.Is(err, domain.ErrConflict) {
			return nil, fmt.Errorf("%w: the endpoint slug %q is already in use", domain.ErrConflict, slug)
		}
		return nil, err
	}
	s.cache.invalidateAll()

	detail := map[string]any{
		"slug": slug, "connector": in.ConnectorID, "base_url": baseURL,
		"allow_private_network": inst.AllowPrivateNetwork,
	}
	s.audit.Success(ctx, actor, domain.ActionInstanceCreate, "instance", inst.ID, detail)
	if inst.AllowPrivateNetwork {
		// Opening private-network egress gets its own audit entry so it is
		// greppable independently of ordinary configuration noise.
		s.audit.Success(ctx, actor, domain.ActionInstanceEgressOpen, "instance", inst.ID, detail)
	}
	return inst, nil
}

// Update applies a configuration change.
func (s *Instances) Update(ctx context.Context, actor Actor, instanceID string, in UpdateInput) (*domain.Instance, error) {
	inst, err := s.repo.Get(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	entry, err := s.connectors.Get(ctx, inst.ConnectorID)
	if err != nil {
		return nil, err
	}

	changed := map[string]any{}
	if in.Name != nil {
		name := strings.TrimSpace(*in.Name)
		if name == "" {
			return nil, fmt.Errorf("%w: a name is required", domain.ErrInvalidInput)
		}
		inst.Name, changed["name"] = name, name
	}
	if in.Description != nil {
		inst.Description = strings.TrimSpace(*in.Description)
	}
	if in.BaseURL != nil && !entry.Compiled.Manifest.Spec.BaseURL.Locked {
		baseURL := strings.TrimSpace(*in.BaseURL)
		if err := connector.ValidateBaseURL(baseURL); err != nil {
			return nil, fmt.Errorf("%w: base URL %s", domain.ErrInvalidInput, err)
		}
		inst.BaseURL, changed["base_url"] = baseURL, baseURL
	}
	if in.Variables != nil {
		if err := entry.Compiled.ValidateVariables(in.Variables); err != nil {
			return nil, fmt.Errorf("%w: %s", domain.ErrInvalidInput, err)
		}
		inst.Variables = in.Variables
		changed["variables"] = true
	}
	if in.Enabled != nil {
		inst.Enabled, changed["enabled"] = *in.Enabled, *in.Enabled
	}
	egressOpened := false
	if in.AllowPrivateNetwork != nil {
		egressOpened = *in.AllowPrivateNetwork && !inst.AllowPrivateNetwork
		inst.AllowPrivateNetwork = *in.AllowPrivateNetwork
		changed["allow_private_network"] = *in.AllowPrivateNetwork
	}
	if in.HostHeaderOverride != nil {
		override := strings.TrimSpace(*in.HostHeaderOverride)
		if err := validateHostHeaderOverride(override); err != nil {
			return nil, err
		}
		inst.HostHeaderOverride, changed["host_header_override"] = override, override
	}
	if in.TimeoutMS != nil {
		inst.TimeoutMS, changed["timeout_ms"] = clampTimeout(*in.TimeoutMS), clampTimeout(*in.TimeoutMS)
	}
	if in.RateLimitPerMin != nil {
		inst.RateLimitPerMin = clampNonNegative(*in.RateLimitPerMin)
		changed["rate_limit_per_min"] = inst.RateLimitPerMin
	}
	if in.MaxConcurrent != nil {
		inst.MaxConcurrent = clampConcurrency(*in.MaxConcurrent)
		changed["max_concurrent"] = inst.MaxConcurrent
	}
	if in.MaxResponseBytes != nil {
		inst.MaxResponseBytes = clampResponseBytes(*in.MaxResponseBytes)
		changed["max_response_bytes"] = inst.MaxResponseBytes
	}

	inst.UpdatedAt = s.now().UTC()
	if err := s.repo.Update(ctx, inst); err != nil {
		return nil, err
	}
	s.cache.invalidateAll()
	// The previous target may be gone entirely; stale failure counts would
	// otherwise keep shedding requests to a freshly fixed configuration.
	s.executor.Breaker().Reset(inst.ID)

	s.audit.Success(ctx, actor, domain.ActionInstanceUpdate, "instance", inst.ID, changed)
	if egressOpened {
		s.audit.Success(ctx, actor, domain.ActionInstanceEgressOpen, "instance", inst.ID,
			map[string]any{"base_url": inst.BaseURL})
	}
	return inst, nil
}

// Delete removes an instance and everything scoped to it.
func (s *Instances) Delete(ctx context.Context, actor Actor, instanceID string) error {
	if err := s.repo.Delete(ctx, instanceID); err != nil {
		return err
	}
	s.cache.invalidateAll()
	s.executor.Breaker().Reset(instanceID)
	s.audit.Success(ctx, actor, domain.ActionInstanceDelete, "instance", instanceID, nil)
	return nil
}

// SetSecret encrypts and stores a credential.
//
// The plaintext parameter is the only place a credential exists outside the
// caller: it is sealed immediately and never logged, echoed or returned.
func (s *Instances) SetSecret(ctx context.Context, actor Actor, instanceID, name, plaintext string) error {
	inst, err := s.repo.Get(ctx, instanceID)
	if err != nil {
		return err
	}
	entry, err := s.connectors.Get(ctx, inst.ConnectorID)
	if err != nil {
		return err
	}
	if err := entry.Compiled.ValidateSecretNames([]string{name}); err != nil {
		return fmt.Errorf("%w: %s", domain.ErrInvalidInput, err)
	}
	if plaintext == "" {
		return fmt.Errorf("%w: the secret value must not be empty; delete it instead", domain.ErrInvalidInput)
	}
	if len(plaintext) > 8192 {
		return fmt.Errorf("%w: the secret value is too long", domain.ErrInvalidInput)
	}

	ciphertext, err := s.sealer.Seal(secrets.SecretAAD(instanceID, name), []byte(plaintext))
	if err != nil {
		return err
	}
	if err := s.repo.SetSecret(ctx, &domain.InstanceSecret{
		InstanceID: instanceID, Name: name, Ciphertext: ciphertext, UpdatedAt: s.now().UTC(),
	}); err != nil {
		return err
	}
	s.cache.invalidateAll()
	// The audit record names the secret but never its value or its length.
	s.audit.Success(ctx, actor, domain.ActionInstanceSecretSet, "instance", instanceID,
		map[string]any{"secret": name, "action": "set"})
	return nil
}

// DeleteSecret removes a stored credential.
func (s *Instances) DeleteSecret(ctx context.Context, actor Actor, instanceID, name string) error {
	if err := s.repo.DeleteSecret(ctx, instanceID, name); err != nil {
		return err
	}
	s.cache.invalidateAll()
	s.audit.Success(ctx, actor, domain.ActionInstanceSecretSet, "instance", instanceID,
		map[string]any{"secret": name, "action": "deleted"})
	return nil
}

// SetToolEnabled turns one tool on or off for an instance.
func (s *Instances) SetToolEnabled(ctx context.Context, actor Actor, instanceID, toolName string, enabled bool) error {
	inst, err := s.repo.Get(ctx, instanceID)
	if err != nil {
		return err
	}
	entry, err := s.connectors.Get(ctx, inst.ConnectorID)
	if err != nil {
		return err
	}
	if _, ok := entry.Compiled.Tool(toolName); !ok {
		return fmt.Errorf("%w: this connector has no tool named %q", domain.ErrNotFound, toolName)
	}
	if err := s.repo.SetToolBinding(ctx, &domain.ToolBinding{
		InstanceID: instanceID, ToolName: toolName, Enabled: enabled,
	}); err != nil {
		return err
	}
	s.cache.invalidateAll()
	s.audit.Success(ctx, actor, domain.ActionInstanceUpdate, "instance", instanceID,
		map[string]any{"tool": toolName, "enabled": enabled})
	return nil
}

// ---------------------------------------------------------------------------
// Queries
// ---------------------------------------------------------------------------

// Get returns one instance by ID.
func (s *Instances) Get(ctx context.Context, instanceID string) (*domain.Instance, error) {
	return s.repo.Get(ctx, instanceID)
}

// List returns the summary view of every instance.
func (s *Instances) List(ctx context.Context) ([]*Summary, error) {
	instances, err := s.repo.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*Summary, 0, len(instances))
	for _, inst := range instances {
		summary, err := s.summarise(ctx, inst)
		if err != nil {
			return nil, err
		}
		out = append(out, summary)
	}
	return out, nil
}

func (s *Instances) summarise(ctx context.Context, inst *domain.Instance) (*Summary, error) {
	summary := &Summary{Instance: inst, ConnectorID: inst.ConnectorID}

	entry, err := s.connectors.Get(ctx, inst.ConnectorID)
	if err != nil {
		// A connector that failed to compile leaves its instances visible but
		// clearly broken, which is far more debuggable than hiding them.
		summary.ConnectorName = inst.ConnectorID
		summary.Problem = "the connector for this instance is unavailable or failed to compile"
		return summary, nil
	}
	summary.ConnectorName = entry.Record.Name
	summary.ToolCount = len(entry.Compiled.Tools)

	bindings, err := s.repo.ListToolBindings(ctx, inst.ID)
	if err != nil {
		return nil, err
	}
	enabled := enabledToolSet(entry.Compiled, bindings)
	summary.EnabledTools = len(enabled)

	stored, err := s.repo.LoadSecrets(ctx, inst.ID)
	if err != nil {
		return nil, err
	}
	summary.MissingSecrets = entry.Compiled.MissingRequiredSecrets(stored)
	if len(summary.MissingSecrets) > 0 {
		summary.Problem = "required credentials are not configured: " + strings.Join(summary.MissingSecrets, ", ")
	} else if summary.EnabledTools == 0 {
		summary.Problem = "no tools are enabled"
	}
	return summary, nil
}

// Detail returns the full read model for the configuration UI.
func (s *Instances) Detail(ctx context.Context, instanceID string) (*Detail, error) {
	inst, err := s.repo.Get(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	summary, err := s.summarise(ctx, inst)
	if err != nil {
		return nil, err
	}
	entry, err := s.connectors.Get(ctx, inst.ConnectorID)
	if err != nil {
		return &Detail{Summary: *summary}, nil
	}

	bindings, err := s.repo.ListToolBindings(ctx, inst.ID)
	if err != nil {
		return nil, err
	}
	enabled := enabledToolSet(entry.Compiled, bindings)

	detail := &Detail{Summary: *summary, Connector: entry}
	for _, tool := range entry.Compiled.Tools {
		detail.Tools = append(detail.Tools, ToolView{Tool: tool, Enabled: enabled[tool.Name()]})
	}

	effective := entry.Compiled.ResolveVariables(inst.Variables)
	for _, def := range entry.Compiled.Variables() {
		detail.Variables = append(detail.Variables, VariableView{
			Def: def, Value: inst.Variables[def.Name], Effective: effective[def.Name],
		})
	}

	names, err := s.repo.ListSecretNames(ctx, inst.ID)
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, n := range names {
		set[n] = true
	}
	for _, def := range entry.Compiled.Secrets() {
		detail.Secrets = append(detail.Secrets, SecretView{Def: def, Set: set[def.Name]})
	}
	return detail, nil
}

// enabledToolSet applies per-instance bindings on top of the connector's
// defaults. A tool with no binding follows the manifest, which is how a
// connector upgrade that adds a read-only tool makes it available without an
// operator having to notice.
func enabledToolSet(compiled *connector.Compiled, bindings []*domain.ToolBinding) map[string]bool {
	overrides := make(map[string]bool, len(bindings))
	for _, b := range bindings {
		overrides[b.ToolName] = b.Enabled
	}
	out := map[string]bool{}
	for _, tool := range compiled.Tools {
		enabled := tool.EnabledByDefault()
		if override, ok := overrides[tool.Name()]; ok {
			enabled = override
		}
		if enabled {
			out[tool.Name()] = true
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Resolution for the request path
// ---------------------------------------------------------------------------

// Resolved is everything the engine needs to serve one instance.
type Resolved struct {
	Instance     *domain.Instance
	Connector    *connector.Compiled
	ConnectorRec *domain.ConnectorRecord
	EnabledTools map[string]bool
	// secretsCiphertext stays encrypted until a call actually needs it.
	secretsCiphertext map[string][]byte
}

// ResolveBySlug loads an instance for the MCP endpoint, using a short-lived
// cache so the hot path does not re-read and re-compile on every call.
func (s *Instances) ResolveBySlug(ctx context.Context, slug string) (*Resolved, error) {
	if cached, ok := s.cache.get(slug); ok {
		return cached, nil
	}
	inst, err := s.repo.GetBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	resolved, err := s.resolve(ctx, inst)
	if err != nil {
		return nil, err
	}
	s.cache.put(slug, resolved)
	return resolved, nil
}

// ResolveByID loads an instance by identifier, bypassing the slug cache. Used
// by the connection test, which must always see the configuration as just
// saved.
func (s *Instances) ResolveByID(ctx context.Context, instanceID string) (*Resolved, error) {
	inst, err := s.repo.Get(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	return s.resolve(ctx, inst)
}

func (s *Instances) resolve(ctx context.Context, inst *domain.Instance) (*Resolved, error) {
	entry, err := s.connectors.Get(ctx, inst.ConnectorID)
	if err != nil {
		return nil, err
	}
	bindings, err := s.repo.ListToolBindings(ctx, inst.ID)
	if err != nil {
		return nil, err
	}
	ciphertext, err := s.repo.LoadSecrets(ctx, inst.ID)
	if err != nil {
		return nil, err
	}
	return &Resolved{
		Instance: inst, Connector: entry.Compiled, ConnectorRec: entry.Record,
		EnabledTools: enabledToolSet(entry.Compiled, bindings), secretsCiphertext: ciphertext,
	}, nil
}

// Target builds the per-call execution target, decrypting secrets at the last
// possible moment so plaintext lives only for the duration of one tool call.
func (s *Instances) Target(r *Resolved) (*engine.Target, error) {
	base, err := url.Parse(r.Instance.BaseURL)
	if err != nil {
		return nil, fmt.Errorf("%w: the configured base URL is not valid", domain.ErrInvalidInput)
	}
	plaintext := make(map[string]string, len(r.secretsCiphertext))
	for name, ct := range r.secretsCiphertext {
		value, err := s.sealer.Open(secrets.SecretAAD(r.Instance.ID, name), ct)
		if err != nil {
			// This means the master key changed or the row was tampered with.
			// Failing the call is the only safe option: silently proceeding
			// without a credential would send unauthenticated requests.
			return nil, fmt.Errorf("could not decrypt the %q credential; the master key may have changed: %w", name, err)
		}
		plaintext[name] = string(value)
	}

	return &engine.Target{
		InstanceID: r.Instance.ID,
		Slug:       r.Instance.Slug,
		BaseURL:    base,
		Vars:       r.Connector.ResolveVariables(r.Instance.Variables),
		Secrets:    plaintext,
		HostHeader: r.Instance.HostHeaderOverride,
		Policy: upstream.EgressPolicy{
			AllowPrivateNetworks: r.Instance.AllowPrivateNetwork,
		},
		Timeout:          r.Instance.Timeout(),
		MaxResponseBytes: r.Instance.MaxResponseBytes,
		RateLimitPerMin:  r.Instance.RateLimitPerMin,
		MaxConcurrent:    r.Instance.MaxConcurrent,
	}, nil
}

// Executor exposes the engine so the MCP backend can invoke tools.
func (s *Instances) Executor() *engine.Executor { return s.executor }

// InvalidateCache drops every cached instance, used after out-of-band changes.
func (s *Instances) InvalidateCache() { s.cache.invalidateAll() }

// ---------------------------------------------------------------------------
// Cache
// ---------------------------------------------------------------------------

// targetCache memoises resolved instances for a short window.
//
// Only ciphertext is cached, never decrypted secrets: the cache is a
// performance device and must not extend the lifetime of a plaintext
// credential.
type targetCache struct {
	ttl   time.Duration
	now   func() time.Time
	mu    sync.RWMutex
	items map[string]cachedTarget
}

type cachedTarget struct {
	resolved *Resolved
	expires  time.Time
}

func newTargetCache(ttl time.Duration) *targetCache {
	return &targetCache{ttl: ttl, now: time.Now, items: map[string]cachedTarget{}}
}

func (c *targetCache) get(slug string) (*Resolved, bool) {
	c.mu.RLock()
	item, ok := c.items[slug]
	c.mu.RUnlock()
	if !ok || c.now().After(item.expires) {
		return nil, false
	}
	return item.resolved, true
}

func (c *targetCache) put(slug string, resolved *Resolved) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[slug] = cachedTarget{resolved: resolved, expires: c.now().Add(c.ttl)}
}

func (c *targetCache) invalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = map[string]cachedTarget{}
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func pickInt(supplied, connectorDefault, platformDefault int) int {
	if supplied > 0 {
		return supplied
	}
	if connectorDefault > 0 {
		return connectorDefault
	}
	return platformDefault
}

func pickInt64(supplied, connectorDefault, platformDefault int64) int64 {
	if supplied > 0 {
		return supplied
	}
	if connectorDefault > 0 {
		return connectorDefault
	}
	return platformDefault
}

// validateHostHeaderOverride enforces the shape of an HTTP Host header value.
// Empty is valid — it means "no override, use the connection address" — but a
// non-empty value must be a plausible host[:port] with no whitespace or
// control characters, which would otherwise be a request-splitting vector
// once it reaches the outgoing request's Host field.
func validateHostHeaderOverride(v string) error {
	if v == "" {
		return nil
	}
	if len(v) > 255 {
		return fmt.Errorf("%w: the host header override is too long", domain.ErrInvalidInput)
	}
	for _, r := range v {
		if r <= ' ' || r == 0x7f {
			return fmt.Errorf("%w: the host header override must not contain whitespace or control characters", domain.ErrInvalidInput)
		}
	}
	return nil
}

func clampTimeout(ms int) int {
	if ms < 500 {
		return 500
	}
	if ms > connector.MaxTimeoutMS {
		return connector.MaxTimeoutMS
	}
	return ms
}

func clampNonNegative(v int) int {
	if v < 0 {
		return 0
	}
	return v
}

func clampConcurrency(v int) int {
	if v < 1 {
		return 1
	}
	if v > 64 {
		return 64
	}
	return v
}

func clampResponseBytes(v int64) int64 {
	if v < 1024 {
		return 1024
	}
	if v > connector.MaxResponseBytesCap {
		return connector.MaxResponseBytesCap
	}
	return v
}

// slugify derives a URL-safe endpoint name from a display name.
func slugify(name string) string {
	var sb strings.Builder
	lastHyphen := true
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			sb.WriteRune(r)
			lastHyphen = false
		default:
			if !lastHyphen {
				sb.WriteByte('-')
				lastHyphen = true
			}
		}
	}
	slug := strings.Trim(sb.String(), "-")
	if len(slug) > 63 {
		slug = strings.Trim(slug[:63], "-")
	}
	return slug
}
