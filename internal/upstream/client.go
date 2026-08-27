package upstream

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Options configures the shared outbound transport.
type Options struct {
	DialTimeout           time.Duration
	TLSHandshakeTimeout   time.Duration
	ResponseHeaderTimeout time.Duration
	IdleConnTimeout       time.Duration
	MaxIdleConnsPerHost   int
	MaxRedirects          int
	UserAgent             string
}

func (o Options) withDefaults() Options {
	if o.DialTimeout == 0 {
		o.DialTimeout = 5 * time.Second
	}
	if o.TLSHandshakeTimeout == 0 {
		o.TLSHandshakeTimeout = 10 * time.Second
	}
	if o.ResponseHeaderTimeout == 0 {
		o.ResponseHeaderTimeout = 30 * time.Second
	}
	if o.IdleConnTimeout == 0 {
		o.IdleConnTimeout = 90 * time.Second
	}
	if o.MaxIdleConnsPerHost == 0 {
		o.MaxIdleConnsPerHost = 8
	}
	if o.MaxRedirects == 0 {
		o.MaxRedirects = 3
	}
	if o.UserAgent == "" {
		o.UserAgent = "MCPaw/1.0 (+https://github.com/00101010xyz/mcpaw)"
	}
	return o
}

// Client performs guarded outbound requests.
//
// Two http.Clients are held, one per egress policy, so that connection pooling
// still works while a connection opened under the permissive policy can never
// be reused by a request that is not permitted to reach private networks.
type Client struct {
	strict     *http.Client
	permissive *http.Client
	opts       Options
}

// New builds a Client with the given options.
func New(opts Options) *Client {
	opts = opts.withDefaults()
	return &Client{
		strict:     newHTTPClient(EgressPolicy{}, opts),
		permissive: newHTTPClient(EgressPolicy{AllowPrivateNetworks: true}, opts),
		opts:       opts,
	}
}

func newHTTPClient(policy EgressPolicy, opts Options) *http.Client {
	dialer := &net.Dialer{
		Timeout:   opts.DialTimeout,
		KeepAlive: 30 * time.Second,
		Control:   controlFunc(policy),
	}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   opts.TLSHandshakeTimeout,
		ResponseHeaderTimeout: opts.ResponseHeaderTimeout,
		IdleConnTimeout:       opts.IdleConnTimeout,
		MaxIdleConnsPerHost:   opts.MaxIdleConnsPerHost,
		MaxIdleConns:          64,
		ForceAttemptHTTP2:     true,
		// Proxies are deliberately not honoured: a proxy would terminate the
		// connection somewhere the Control hook cannot inspect, silently
		// defeating the SSRF policy.
		Proxy: nil,
	}
	return &http.Client{
		Transport:     transport,
		CheckRedirect: checkRedirect(opts.MaxRedirects),
		// No client-level Timeout: each request carries a context deadline, so
		// the bound is per call and visible to the caller.
	}
}

// checkRedirect refuses cross-origin redirects outright.
//
// Go strips Authorization on a cross-domain redirect, but a connector's
// credential may live in an arbitrary header (Zotero uses Zotero-API-Key) that
// the standard library cannot know about. Rather than enumerate which headers
// are sensitive, we never follow a redirect that changes the origin, so a
// compromised or hostile upstream cannot bounce a credential elsewhere.
func checkRedirect(max int) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= max {
			return fmt.Errorf("stopped after %d redirects", max)
		}
		if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
			return fmt.Errorf("refusing redirect to scheme %q", req.URL.Scheme)
		}
		origin := via[0].URL
		if !strings.EqualFold(req.URL.Host, origin.Host) {
			return fmt.Errorf("refusing cross-origin redirect from %s to %s", origin.Host, req.URL.Host)
		}
		return nil
	}
}

// Response is a fully buffered upstream response.
type Response struct {
	StatusCode int
	Header     http.Header
	Body       []byte
	// Truncated reports that the body hit the size cap and is incomplete.
	Truncated bool
	Duration  time.Duration
}

// ErrResponseTooLarge indicates the upstream body exceeded the configured cap.
var ErrResponseTooLarge = errors.New("upstream response exceeded the configured size limit")

// Do executes req under the given egress policy, buffering at most maxBytes of
// the response body.
//
// The body is buffered rather than streamed because an MCP tool result is a
// single message: there is no partial delivery, and buffering with a hard cap
// is what makes memory use per in-flight call predictable.
func (c *Client) Do(req *http.Request, policy EgressPolicy, maxBytes int64) (*Response, error) {
	if req == nil {
		return nil, errors.New("upstream: request must not be nil")
	}
	if maxBytes <= 0 {
		return nil, errors.New("upstream: maxBytes must be positive")
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", c.opts.UserAgent)
	}

	client := c.strict
	if policy.AllowPrivateNetworks {
		client = c.permissive
	}

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return nil, classify(err)
	}
	defer func() {
		// Draining a little before closing lets the connection be reused; the
		// cap keeps a hostile upstream from making the drain itself expensive.
		_, _ = io.CopyN(io.Discard, resp.Body, 4096)
		_ = resp.Body.Close()
	}()

	// Reading one byte past the limit is how we distinguish "exactly at the
	// cap" from "truncated".
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, fmt.Errorf("upstream: reading response body: %w", err)
	}
	out := &Response{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Duration:   time.Since(start),
	}
	if int64(len(body)) > maxBytes {
		out.Truncated = true
		out.Body = body[:maxBytes]
	} else {
		out.Body = body
	}
	return out, nil
}

// classify turns transport errors into ones callers can act on, keeping the
// egress refusal distinguishable from an ordinary connection failure.
func classify(err error) error {
	var blocked *BlockedIPError
	if errors.As(err, &blocked) {
		return blocked
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("upstream: request timed out: %w", err)
	}
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("upstream: request cancelled: %w", err)
	}
	return fmt.Errorf("upstream: %w", err)
}
