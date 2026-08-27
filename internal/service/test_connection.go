package service

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/00101010xyz/mcpaw/internal/connector"
	"github.com/00101010xyz/mcpaw/internal/domain"
	"github.com/00101010xyz/mcpaw/internal/engine"
)

// TestResult reports the outcome of a live connectivity check.
type TestResult struct {
	OK         bool
	Tool       string
	StatusCode int
	DurationMS int64
	Message    string
	// Preview is a short excerpt of the successful response, so an operator can
	// confirm they are looking at the right library rather than merely that
	// something answered.
	Preview string
	// Hint suggests a concrete next step when the check fails.
	Hint string
}

// maxPreviewBytes bounds what the test surfaces in the UI.
const maxPreviewBytes = 600

// TestConnection performs one real tool call so an operator can verify a
// configuration before handing the endpoint to a client.
//
// This is the single most valuable diagnostic in the product: nearly every
// failure (Zotero not running, the container unable to reach the host, private
// egress not enabled, a wrong user id) presents identically to an MCP client as
// "the tool didn't work". Running the call here, with the operator watching,
// turns each of those into a specific message.
func (s *Instances) TestConnection(ctx context.Context, actor Actor, instanceID, toolName string) (*TestResult, error) {
	resolved, err := s.ResolveByID(ctx, instanceID)
	if err != nil {
		return nil, err
	}

	tool, err := s.pickTestTool(resolved, toolName)
	if err != nil {
		return &TestResult{
			OK:      false,
			Message: err.Error(),
			Hint:    "Enable a tool that takes no required arguments, or choose one explicitly.",
		}, nil
	}

	target, err := s.Target(resolved)
	if err != nil {
		return &TestResult{
			OK:      false,
			Tool:    tool.Name(),
			Message: err.Error(),
			Hint:    "Re-enter the instance credentials; they could not be decrypted with the current master key.",
		}, nil
	}

	// The test gets its own short deadline: an operator staring at a form
	// should not wait out a two-minute upstream timeout.
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	started := time.Now()
	result, execErr := s.executor.Execute(callCtx, target, resolved.Connector, tool, map[string]any{})
	elapsed := time.Since(started).Milliseconds()

	out := &TestResult{Tool: tool.Name(), DurationMS: elapsed}
	if execErr != nil {
		out.OK = false
		if e, ok := engine.AsError(execErr); ok {
			out.Message = e.FullMessage()
			out.StatusCode = e.StatusCode
			out.Hint = hintFor(e, resolved)
		} else {
			out.Message = execErr.Error()
		}
		s.audit.Record(ctx, actor, domain.ActionInstanceTest, "instance", instanceID, "failure",
			map[string]any{"tool": tool.Name(), "message": out.Message})
		return out, nil
	}

	out.OK = true
	out.StatusCode = result.StatusCode
	out.Message = fmt.Sprintf("%s responded successfully.", resolved.Connector.Manifest.Metadata.Name)
	out.Preview = truncateString(result.Text, maxPreviewBytes)
	s.audit.Success(ctx, actor, domain.ActionInstanceTest, "instance", instanceID,
		map[string]any{"tool": tool.Name(), "status": result.StatusCode, "duration_ms": elapsed})
	return out, nil
}

// pickTestTool chooses the tool to exercise: the operator's choice when given,
// otherwise the first enabled read-only tool that needs no arguments.
func (s *Instances) pickTestTool(r *Resolved, toolName string) (*connector.CompiledTool, error) {
	if toolName != "" {
		tool, ok := r.Connector.Tool(toolName)
		if !ok {
			return nil, fmt.Errorf("this connector has no tool named %q", toolName)
		}
		if !r.EnabledTools[toolName] {
			return nil, fmt.Errorf("the tool %q is not enabled on this instance", toolName)
		}
		return tool, nil
	}
	for _, tool := range r.Connector.Tools {
		if !r.EnabledTools[tool.Name()] {
			continue
		}
		if !isReadOnly(tool) || hasRequiredArguments(tool) {
			continue
		}
		return tool, nil
	}
	return nil, fmt.Errorf("no enabled read-only tool takes zero required arguments, so no automatic test is possible")
}

func isReadOnly(t *connector.CompiledTool) bool {
	a := t.Def.Annotations
	if a != nil && a.ReadOnlyHint != nil {
		return *a.ReadOnlyHint
	}
	// Absent an explicit hint, only a safe HTTP method may be used for an
	// automatic test: a connection check must never create anything.
	return t.Def.Request.Method == "GET" || t.Def.Request.Method == "HEAD"
}

func hasRequiredArguments(t *connector.CompiledTool) bool {
	required, ok := t.Def.InputSchema["required"].([]any)
	return ok && len(required) > 0
}

// hintFor turns a failure kind into an actionable next step. Generic advice
// would be worse than none; each of these maps to something the operator can
// actually change.
func hintFor(e *engine.Error, r *Resolved) string {
	switch e.Kind {
	case engine.KindEgressBlocked:
		return "Enable \"Allow private network egress\" on this instance. " +
			"It is required whenever the API runs on your own machine or LAN."
	case engine.KindUpstreamFailure:
		if r.Connector.Manifest.Spec.BaseURL.RequiresPrivateNetwork {
			return "Check that the desktop application is running and that its local API is enabled. " +
				"From a container, the host is reachable as host.docker.internal, not localhost."
		}
		return "Check that the base URL is correct and reachable from this container."
	case engine.KindTimeout:
		return "The upstream accepted the connection but did not answer in time. " +
			"Raise the instance timeout, or check whether the API is under load."
	case engine.KindUpstreamStatus:
		switch {
		case e.StatusCode == 401 || e.StatusCode == 403:
			return "The upstream rejected the credentials. Check the instance's secret values."
		case e.StatusCode == 404:
			return "The upstream returned 404. Check the base URL and the instance variables " +
				"(for the Zotero local API the user id must be 0)."
		case e.StatusCode == 400 && r.Instance.AllowPrivateNetwork && r.Instance.HostHeaderOverride == "":
			return "The upstream returned 400. If a direct curl to the same address succeeds, the " +
				"upstream is likely rejecting the request's Host header as a DNS-rebinding defense " +
				"(Zotero's local API only accepts 127.0.0.1, localhost or [::1]) — try setting " +
				"\"Host header override\" below to " + suggestedHostOverride(r.Instance.BaseURL) + "."
		default:
			return "The upstream answered with an error status; the excerpt above is its own explanation."
		}
	case engine.KindRateLimited:
		return "This instance's own rate limit rejected the test. Raise it or wait a moment."
	case engine.KindCircuitOpen:
		return "Recent calls failed, so requests are being shed. Fix the upstream and retry in a few seconds."
	default:
		return ""
	}
}

// suggestedHostOverride derives a concrete "127.0.0.1[:port]" suggestion from
// the instance's own configured base URL, so the hint names an exact value to
// type rather than making the operator work out the port themselves.
func suggestedHostOverride(baseURL string) string {
	if u, err := url.Parse(baseURL); err == nil {
		if port := u.Port(); port != "" {
			return "127.0.0.1:" + port
		}
	}
	return "127.0.0.1"
}
