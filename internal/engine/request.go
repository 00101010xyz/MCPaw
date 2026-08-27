package engine

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/00101010xyz/mcpaw/internal/connector"
	"github.com/00101010xyz/mcpaw/internal/tmpl"
)

// buildRequest renders a compiled tool against the caller's arguments and the
// instance configuration, producing a ready-to-send HTTP request.
//
// Rendering happens in one place so that the escaping rules are applied per
// sink exactly once: path segments are path-escaped, query values are handed to
// url.Values (which percent-encodes on Encode), header values are checked for
// control characters, and body values are marshalled as JSON.
func buildRequest(target *Target, conn *connector.Compiled, tool *connector.CompiledTool, args map[string]any) (*http.Request, error) {
	scope := tmpl.MapScope{Input: args, Vars: target.Vars, Secrets: target.Secrets}

	u, err := renderURL(target, tool, scope)
	if err != nil {
		return nil, err
	}

	body, contentType, err := renderBody(tool, scope)
	if err != nil {
		return nil, err
	}

	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(tool.Def.Request.Method, u.String(), reader)
	if err != nil {
		return nil, newError(KindInternal, "could not construct the upstream request", err)
	}
	if body != nil {
		req.ContentLength = int64(len(body))
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
	}

	// Connector defaults first, tool headers second: a tool may deliberately
	// override an inherited default (for example asking for a different Accept).
	if err := applyHeaders(req.Header, conn.Headers, scope); err != nil {
		return nil, err
	}
	if err := applyHeaders(req.Header, tool.Headers, scope); err != nil {
		return nil, err
	}
	if err := applyAuth(req, conn, tool, scope); err != nil {
		return nil, err
	}
	return req, nil
}

func renderURL(target *Target, tool *connector.CompiledTool, scope tmpl.Scope) (*url.URL, error) {
	if tool.Path == nil {
		return nil, newError(KindInternal, "tool has no compiled request path", nil)
	}
	// Values interpolated into the path are path-escaped, so a tool argument
	// can never introduce a new path segment, a query string or a traversal.
	rendered, ok, err := tool.Path.Render(scope, url.PathEscape)
	if err != nil {
		return nil, newError(KindInvalidArguments, "could not render the request path", err)
	}
	if !ok {
		return nil, newError(KindInvalidArguments, "the request path references a value that was not supplied", nil)
	}

	joined := strings.TrimSuffix(target.BaseURL.EscapedPath(), "/") + rendered
	// Defence in depth: escaping already makes traversal impossible from
	// arguments, so a ".." here could only come from a manifest literal.
	if containsTraversal(joined) {
		return nil, newError(KindInternal, "the rendered request path contains a traversal segment", nil)
	}

	ref, err := url.Parse(joined)
	if err != nil {
		return nil, newError(KindInternal, "the rendered request path is not a valid URL path", err)
	}

	u := *target.BaseURL
	u.Path = ref.Path
	u.RawPath = ref.RawPath
	u.User = nil
	u.Fragment = ""

	query := url.Values{}
	for _, pair := range tool.Query {
		value, ok, err := pair.Value.Render(scope, tmpl.Identity)
		if err != nil {
			return nil, newError(KindInvalidArguments,
				fmt.Sprintf("could not render query parameter %q", pair.Key), err)
		}
		// An optional parameter whose value was not supplied is omitted
		// entirely. Sending it empty would turn "no filter" into "filter on the
		// empty string", which most APIs answer with everything or nothing.
		if !ok || value == "" {
			continue
		}
		query.Set(pair.Key, value)
	}
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}
	return &u, nil
}

func containsTraversal(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

func applyHeaders(h http.Header, pairs []connector.Pair, scope tmpl.Scope) error {
	for _, pair := range pairs {
		value, ok, err := pair.Value.Render(scope, tmpl.Identity)
		if err != nil {
			return newError(KindInvalidArguments, fmt.Sprintf("could not render header %q", pair.Key), err)
		}
		if !ok || value == "" {
			continue
		}
		if err := validateHeaderValue(pair.Key, value); err != nil {
			return err
		}
		h.Set(pair.Key, value)
	}
	return nil
}

// validateHeaderValue rejects control characters. A CR or LF in a header value
// is the classic request-splitting primitive; Go's own transport also rejects
// it, but failing here gives a precise error instead of an opaque one and keeps
// the guarantee local to this package.
func validateHeaderValue(name, value string) error {
	for i := 0; i < len(value); i++ {
		if c := value[i]; c < 0x20 || c == 0x7f {
			return newError(KindInvalidArguments,
				fmt.Sprintf("header %q would contain a control character", name), nil)
		}
	}
	return nil
}

func applyAuth(req *http.Request, conn *connector.Compiled, tool *connector.CompiledTool, scope tmpl.Scope) error {
	if tool.Def.Request.Auth == connector.AuthNone {
		return nil
	}
	auth := conn.Auth
	if auth == nil || auth.Type == connector.AuthNone {
		return nil
	}

	render := func(t *tmpl.Template) (string, bool, error) {
		if t == nil {
			return "", false, nil
		}
		v, ok, err := t.Render(scope, tmpl.Identity)
		if err != nil {
			// The error is deliberately not wrapped: it could quote the
			// credential template's resolved value.
			return "", false, newError(KindInternal, "could not render the authentication credential", nil)
		}
		return v, ok && v != "", nil
	}

	switch auth.Type {
	case connector.AuthAPIKey:
		value, ok, err := render(auth.Value)
		if err != nil {
			return err
		}
		// An unset optional credential means "send no credential", which is
		// exactly what the local Zotero API expects.
		if !ok {
			return nil
		}
		if err := validateHeaderValue(auth.Name, value); err != nil {
			return err
		}
		if auth.In == "query" {
			q := req.URL.Query()
			q.Set(auth.Name, value)
			req.URL.RawQuery = q.Encode()
			return nil
		}
		req.Header.Set(auth.Name, value)

	case connector.AuthBearer:
		value, ok, err := render(auth.Value)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		if err := validateHeaderValue("Authorization", value); err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+value)

	case connector.AuthBasic:
		user, userOK, err := render(auth.Username)
		if err != nil {
			return err
		}
		pass, passOK, err := render(auth.Password)
		if err != nil {
			return err
		}
		if !userOK || !passOK {
			return nil
		}
		token := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
		req.Header.Set("Authorization", "Basic "+token)
	}
	return nil
}

func renderBody(tool *connector.CompiledTool, scope tmpl.Scope) ([]byte, string, error) {
	body := tool.Body
	if body == nil {
		return nil, "", nil
	}
	switch {
	case body.JSON != nil:
		value, ok := renderNode(body.JSON, scope)
		if !ok {
			return nil, "", nil
		}
		data, err := json.Marshal(value)
		if err != nil {
			return nil, "", newError(KindInternal, "could not encode the request body", err)
		}
		return data, body.ContentType, nil

	case body.From != nil:
		value, ok, err := body.From.RenderValue(scope)
		if err != nil {
			return nil, "", newError(KindInvalidArguments, "could not render the request body", err)
		}
		if !ok {
			return nil, "", nil
		}
		data, err := json.Marshal(value)
		if err != nil {
			return nil, "", newError(KindInternal, "could not encode the request body", err)
		}
		return data, body.ContentType, nil

	case body.Text != nil:
		text, ok, err := body.Text.Render(scope, tmpl.Identity)
		if err != nil {
			return nil, "", newError(KindInvalidArguments, "could not render the request body", err)
		}
		if !ok {
			return nil, "", nil
		}
		return []byte(text), body.ContentType, nil
	}
	return nil, "", nil
}

// renderNode walks a JSON body template. Object fields whose templates do not
// resolve are dropped, which is how an optional field stays absent rather than
// being sent as null — a distinction many APIs treat as "clear this value".
func renderNode(n *connector.BodyNode, scope tmpl.Scope) (any, bool) {
	switch {
	case n.HasLiteral():
		return n.Literal(), true
	case n.Template() != nil:
		v, ok, err := n.Template().RenderValue(scope)
		if err != nil || !ok {
			return nil, false
		}
		return v, true
	case n.Object() != nil:
		out := map[string]any{}
		for k, child := range n.Object() {
			if v, ok := renderNode(child, scope); ok {
				out[k] = v
			}
		}
		return out, true
	case n.Array() != nil:
		out := make([]any, 0, len(n.Array()))
		for _, child := range n.Array() {
			// An array element that cannot be rendered would shift every later
			// index, so an unresolvable element makes the whole array
			// unresolvable.
			v, ok := renderNode(child, scope)
			if !ok {
				return nil, false
			}
			out = append(out, v)
		}
		return out, true
	}
	return nil, false
}
