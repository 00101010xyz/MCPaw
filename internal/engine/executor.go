package engine

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/00101010xyz/mcpaw/internal/connector"
	"github.com/00101010xyz/mcpaw/internal/upstream"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

// Target is the runtime view of a configured instance: everything the engine
// needs to make one call, with no reference back to the database.
//
// Secrets are present in plaintext here and nowhere else. A Target is built per
// call and discarded with it, so decrypted credentials have the lifetime of a
// single request rather than that of a cache.
type Target struct {
	InstanceID string
	Slug       string
	BaseURL    *url.URL
	Vars       map[string]string
	Secrets    map[string]string

	// HostHeader, when set, replaces the outgoing HTTP Host header, independent
	// of the address the request actually connects to. Some local services
	// (Zotero's local API among them) validate the Host header as a
	// DNS-rebinding defense and reject anything but a loopback name — which a
	// container can't present honestly, since it has to dial out via a
	// different address (host.docker.internal) to reach that same loopback
	// service. Empty means "use the address being connected to," Go's default.
	HostHeader string

	Policy           upstream.EgressPolicy
	Timeout          time.Duration
	MaxResponseBytes int64
	RateLimitPerMin  int
	MaxConcurrent    int
}

// Executor runs tool calls against upstream APIs.
type Executor struct {
	client  *upstream.Client
	breaker *upstream.Breaker
	limiter upstream.Limiter
	gate    *upstream.Gate
	now     func() time.Time
}

// Config wires an Executor's collaborators. Every field is an interface or a
// concrete collaborator supplied by the composition root; the Executor
// constructs none of them itself.
type Config struct {
	Client  *upstream.Client
	Breaker *upstream.Breaker
	Limiter upstream.Limiter
	Gate    *upstream.Gate
}

// New builds an Executor, filling in safe defaults for any collaborator the
// caller did not supply.
func New(cfg Config) *Executor {
	e := &Executor{
		client:  cfg.Client,
		breaker: cfg.Breaker,
		limiter: cfg.Limiter,
		gate:    cfg.Gate,
		now:     time.Now,
	}
	if e.client == nil {
		e.client = upstream.New(upstream.Options{})
	}
	if e.breaker == nil {
		e.breaker = upstream.NewBreaker(upstream.BreakerConfig{})
	}
	if e.limiter == nil {
		e.limiter = upstream.NewMemoryLimiter()
	}
	if e.gate == nil {
		e.gate = upstream.NewGate()
	}
	return e
}

// Breaker exposes the circuit breaker so that reconfiguring an instance can
// clear its state.
func (e *Executor) Breaker() *upstream.Breaker { return e.breaker }

// Client exposes the shared guarded HTTP client so other subsystems (the
// semantic-search indexer calling an embedder sidecar) reach the network
// through the same SSRF-guarded transport as every declarative tool call,
// rather than opening a second, unguarded one.
func (e *Executor) Client() *upstream.Client { return e.client }

// Execute validates arguments, renders the upstream request, applies every
// runtime control and maps the response.
//
// The order of the controls is deliberate and is the whole point of routing
// every call through here:
//
//  1. validate arguments — never spend a rate-limit token on a malformed call;
//  2. rate limit — cheapest rejection of a caller sending too much;
//  3. circuit breaker — do not queue behind an upstream known to be down;
//  4. concurrency gate — bound in-flight memory;
//  5. deadline — every call is bounded in wall-clock time;
//  6. request — guarded egress with a response size cap;
//  7. map — normalise the response, hiding upstream internals.
func (e *Executor) Execute(
	ctx context.Context,
	target *Target,
	conn *connector.Compiled,
	tool *connector.CompiledTool,
	args map[string]any,
) (*Result, error) {
	if target == nil || conn == nil || tool == nil {
		return nil, newError(KindInternal, "execute called with an incomplete configuration", nil)
	}
	if target.BaseURL == nil {
		return nil, newError(KindNotConfigured, "the instance has no base URL configured", nil)
	}
	if args == nil {
		args = map[string]any{}
	}

	if err := validateArguments(tool, args); err != nil {
		return nil, err
	}

	if !e.limiter.Allow(target.InstanceID, target.RateLimitPerMin) {
		return nil, &Error{
			Kind:    KindRateLimited,
			Message: fmt.Sprintf("this instance is limited to %d requests per minute", target.RateLimitPerMin),
		}
	}

	if err := e.breaker.Allow(target.InstanceID); err != nil {
		return nil, newError(KindCircuitOpen,
			"the upstream API is currently unreachable and requests are being shed; it will be retried automatically", err)
	}

	release, err := e.gate.Acquire(ctx, target.InstanceID, target.MaxConcurrent)
	if err != nil {
		return nil, newError(KindTimeout, "timed out waiting for a free request slot", err)
	}
	defer release()

	req, err := buildRequest(target, conn, tool, args)
	if err != nil {
		return nil, err
	}

	timeout := target.Timeout
	if timeout <= 0 {
		timeout = time.Duration(connector.DefaultTimeoutMS) * time.Millisecond
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req = req.WithContext(callCtx)

	maxBytes := target.MaxResponseBytes
	if tool.Def.Response.MaxBytes > 0 && tool.Def.Response.MaxBytes < maxBytes {
		// A tool may tighten the instance limit but never loosen it.
		maxBytes = tool.Def.Response.MaxBytes
	}
	if maxBytes <= 0 {
		maxBytes = connector.DefaultMaxResponseBytes
	}

	resp, err := e.client.Do(req, target.Policy, maxBytes)
	if err != nil {
		e.breaker.Record(target.InstanceID, false)
		return nil, classifyTransportError(err)
	}

	result, mapErr := mapResponse(tool, resp, args)
	// A 5xx or a transport-level problem means the upstream is unhealthy. A 4xx
	// means *this call* was wrong, which says nothing about upstream health and
	// must not be allowed to trip the breaker for everyone else.
	e.breaker.Record(target.InstanceID, resp.StatusCode < 500)
	if mapErr != nil {
		return nil, mapErr
	}
	return result, nil
}

func classifyTransportError(err error) error {
	var blocked *upstream.BlockedIPError
	if errors.As(err, &blocked) {
		return &Error{
			Kind: KindEgressBlocked,
			Message: "the configured upstream address is not permitted by this instance's egress policy; " +
				"enable private-network egress on the instance if the API runs on the local machine or a private network",
			Details: []string{blocked.Error()},
			err:     err,
		}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return newError(KindTimeout, "the upstream API did not respond in time", err)
	}
	if errors.Is(err, context.Canceled) {
		return newError(KindTimeout, "the request was cancelled", err)
	}
	// The underlying message can contain internal hostnames and addresses, so
	// it goes to the log through the wrapped error and not into Message.
	return newError(KindUpstreamFailure, "could not reach the upstream API", err)
}

// validateArguments checks the caller's arguments against the tool's compiled
// JSON Schema.
//
// This runs before anything is rendered. Tool arguments come from a language
// model and are therefore attacker-influenceable whenever the model reads
// untrusted content, so the schema is the contract that decides what may reach
// the templating layer at all.
func validateArguments(tool *connector.CompiledTool, args map[string]any) error {
	if tool.InputSchema == nil {
		return newError(KindInternal, "the tool has no compiled input schema", nil)
	}
	// The schema library needs a plain JSON value; a map[string]any decoded
	// from JSON already is one.
	if err := tool.InputSchema.Validate(any(args)); err != nil {
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			return &Error{
				Kind:    KindInvalidArguments,
				Message: "the supplied arguments do not match the tool's input schema",
				Details: schemaProblems(ve),
			}
		}
		return newError(KindInvalidArguments, "the supplied arguments could not be validated", err)
	}
	return nil
}

// maxSchemaProblems bounds how much detail is echoed back, so a deeply nested
// invalid payload cannot produce a megabyte of error text.
const maxSchemaProblems = 10

func schemaProblems(ve *jsonschema.ValidationError) []string {
	out := collectProblems(ve.BasicOutput(), nil)
	if len(out) > maxSchemaProblems {
		out = append(out[:maxSchemaProblems],
			fmt.Sprintf("(and %d more)", len(out)-maxSchemaProblems))
	}
	return out
}

func collectProblems(unit *jsonschema.OutputUnit, acc []string) []string {
	if unit == nil {
		return acc
	}
	if unit.Error != nil {
		location := strings.TrimPrefix(unit.InstanceLocation, "/")
		if location == "" {
			location = "(root)"
		} else {
			location = strings.ReplaceAll(location, "/", ".")
		}
		acc = append(acc, location+": "+unit.Error.String())
	}
	for i := range unit.Errors {
		acc = collectProblems(&unit.Errors[i], acc)
	}
	return acc
}
