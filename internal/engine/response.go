package engine

import (
	"encoding/json"
	"fmt"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/00101010xyz/mcpaw/internal/connector"
	"github.com/00101010xyz/mcpaw/internal/upstream"
)

// Result is a normalised upstream response ready to become an MCP tool result.
type Result struct {
	StatusCode int
	// Text is the human-readable rendering handed to the model.
	Text string
	// Data is the parsed (and optionally projected) JSON payload. It is nil for
	// non-JSON responses.
	Data any
	// IsJSON reports whether Data is populated.
	IsJSON bool
	// Headers contains only the response headers the manifest allowlisted.
	Headers  map[string]string
	Duration int64
}

// maxTextBytes bounds the textual rendering handed back to the client. The
// response body is already capped, but pretty-printing JSON can inflate it, and
// an MCP client should never be handed an unbounded string.
const maxTextBytes = 512 * 1024

// mapResponse converts an upstream response into a Result, or into an *Error
// when the status is not a success.
func mapResponse(tool *connector.CompiledTool, resp *upstream.Response) (*Result, error) {
	headers := selectHeaders(tool.IncludeHeaders, resp.Header)

	if !isSuccess(tool, resp.StatusCode) {
		return nil, &Error{
			Kind:       KindUpstreamStatus,
			StatusCode: resp.StatusCode,
			Message: fmt.Sprintf("the upstream API returned %d %s",
				resp.StatusCode, http.StatusText(resp.StatusCode)),
			// A bounded excerpt of the upstream body is genuinely useful for
			// diagnosis ("item not found", "invalid API key") and is bounded so
			// a hostile upstream cannot use it to flood the client.
			Details: []string{excerpt(resp.Body, 512)},
		}
	}

	format := tool.Def.Response.Format
	if format == "" {
		format = connector.FormatAuto
	}
	treatAsJSON := format == connector.FormatJSON ||
		(format == connector.FormatAuto && looksLikeJSON(resp.Header))

	result := &Result{
		StatusCode: resp.StatusCode,
		Headers:    headers,
		Duration:   resp.Duration.Milliseconds(),
	}

	if !treatAsJSON {
		if resp.Truncated {
			// Text can be usefully truncated, so this is a note rather than a
			// failure.
			result.Text = truncate(string(resp.Body), maxTextBytes) +
				"\n\n[truncated: the response exceeded this instance's size limit]"
		} else {
			result.Text = truncate(string(resp.Body), maxTextBytes)
		}
		return result, nil
	}

	if resp.Truncated {
		// A truncated JSON document cannot be parsed, and handing a model a
		// half-object is worse than an honest error.
		return nil, &Error{
			Kind:       KindResponseTooLarge,
			StatusCode: resp.StatusCode,
			Message: "the upstream response exceeded this instance's size limit and could not be parsed; " +
				"narrow the query, lower the requested page size, or raise the instance's response limit",
		}
	}

	var parsed any
	if len(strings.TrimSpace(string(resp.Body))) == 0 {
		parsed = nil
	} else if err := json.Unmarshal(resp.Body, &parsed); err != nil {
		// The upstream promised JSON and did not deliver. Fall back to text
		// rather than failing: the body usually explains what went wrong.
		result.Text = truncate(string(resp.Body), maxTextBytes)
		return result, nil
	}

	projected, err := project(parsed, tool.SelectPath)
	if err != nil {
		return nil, err
	}
	result.Data = projected
	result.IsJSON = true

	pretty, err := json.MarshalIndent(projected, "", "  ")
	if err != nil {
		return nil, newError(KindInternal, "could not render the upstream response", err)
	}
	result.Text = truncate(string(pretty), maxTextBytes)
	return result, nil
}

func isSuccess(tool *connector.CompiledTool, status int) bool {
	if len(tool.SuccessCodes) > 0 {
		return tool.SuccessCodes[status]
	}
	return status >= 200 && status < 300
}

func looksLikeJSON(h http.Header) bool {
	ct := h.Get("Content-Type")
	if ct == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(ct)
	if err != nil {
		return false
	}
	return mediaType == "application/json" ||
		strings.HasSuffix(mediaType, "+json") ||
		mediaType == "text/json"
}

// selectHeaders copies only the headers the manifest allowlisted.
//
// An allowlist rather than a denylist: a response header the manifest author
// never considered (Set-Cookie, an internal routing header, a rate-limit token)
// must not reach the model by default.
func selectHeaders(allow []string, h http.Header) map[string]string {
	if len(allow) == 0 {
		return nil
	}
	out := map[string]string{}
	for _, name := range allow {
		if v := h.Get(name); v != "" {
			out[name] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// project walks a dotted path into the parsed body, so a manifest can hand back
// `data.items` rather than the whole envelope.
func project(v any, path []string) (any, error) {
	cur := v
	for i, seg := range path {
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[seg]
			if !ok {
				return nil, &Error{
					Kind: KindUpstreamFailure,
					Message: fmt.Sprintf("the upstream response does not contain the expected field %q",
						strings.Join(path[:i+1], ".")),
				}
			}
			cur = next
		case []any:
			// A numeric segment indexes an array, which keeps `data.0.id`
			// working without a second path syntax.
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(node) {
				return nil, &Error{
					Kind: KindUpstreamFailure,
					Message: fmt.Sprintf("the upstream response has no element at %q",
						strings.Join(path[:i+1], ".")),
				}
			}
			cur = node[idx]
		default:
			return nil, &Error{
				Kind: KindUpstreamFailure,
				Message: fmt.Sprintf("the upstream response is not shaped as expected at %q",
					strings.Join(path[:i+1], ".")),
			}
		}
	}
	return cur, nil
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n… [truncated]"
}

// excerpt renders a bounded, printable snippet of an upstream error body.
func excerpt(body []byte, max int) string {
	s := strings.TrimSpace(string(body))
	if s == "" {
		return "(empty response body)"
	}
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' || r >= 0x20 {
			return r
		}
		return -1
	}, s)
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
