// Package source is the seam between MCPaw's semantic-search indexer (which
// knows how to chunk, embed and store text) and the connector-specific
// knowledge of how to walk one particular API's data model to find that
// text.
//
// Each indexable connector (Zotero, and later Gitea, Linkding, ...) gets its
// own subpackage implementing Crawler and registering itself by connector
// ID. internal/service and this package's own top level must never contain
// a connector ID or a shape assumption about one connector's responses —
// that knowledge belongs entirely inside the per-source subpackage, which is
// what lets one source change without any risk to another. See
// internal/index/source/zotero for a worked example.
package source

import (
	"context"
	"fmt"

	"github.com/00101010xyz/mcpaw/internal/connector"
	"github.com/00101010xyz/mcpaw/internal/engine"
)

// Document identifies one piece of indexable content within an instance's
// library — one Zotero attachment, one Gitea file, one Linkding bookmark
// snapshot. AttachmentKey is the document itself; ItemKey is whatever it
// logically belongs to, for citing back to the source (for a flat source
// with no such grouping, the two may be equal).
type Document struct {
	ItemKey       string
	AttachmentKey string
}

// EmitFunc is called by a Crawler once per candidate document it finds,
// including ones with no usable text (empty text is still a real document
// visited, so progress reporting stays accurate; the caller simply has
// nothing to chunk for it). Returning an error stops the crawl.
type EmitFunc func(ctx context.Context, doc Document, text string) error

// Runtime is what a Crawler gets to actually call the connector's tools. It
// centralises the "is this tool enabled on this instance" check so no
// individual Crawler has to reimplement it.
type Runtime struct {
	Executor     *engine.Executor
	Target       *engine.Target
	Connector    *connector.Compiled
	EnabledTools map[string]bool
}

// Call invokes one connector tool by name, refusing tools that are not
// enabled on this instance — a background crawl must respect the exact same
// on/off switch an operator uses to control what MCP clients can reach,
// rather than bypassing it because nothing is watching.
func (rt Runtime) Call(ctx context.Context, toolName string, args map[string]any) (*engine.Result, error) {
	tool, ok := rt.Connector.Tool(toolName)
	if !ok || !rt.EnabledTools[toolName] {
		return nil, fmt.Errorf("tool %q is not enabled on this instance", toolName)
	}
	return rt.Executor.Execute(ctx, rt.Target, rt.Connector, tool, args)
}

// Crawler walks one connector's data model and reports every document worth
// considering for the index. Implementations live one per connector, in
// their own subpackage, and register themselves via Register.
type Crawler interface {
	// RequiredTools names the connector tools this crawler needs enabled on
	// an instance before Crawl can run at all. The indexer checks these
	// up front, one at a time, so a misconfigured instance gets a specific
	// "enable this tool" error instead of a crawl that fails partway through.
	RequiredTools() []string
	// Crawl walks the library and calls emit for every candidate document.
	Crawl(ctx context.Context, rt Runtime, emit EmitFunc) error
}

var registry = map[string]Crawler{}

// Register associates a Crawler with the connector ID it knows how to walk.
// Called from each source subpackage's init(), so simply importing that
// package (for its side effect) is what makes a connector indexable —
// internal/service never names a connector ID to decide.
func Register(connectorID string, c Crawler) {
	if connectorID == "" || c == nil {
		panic("source: Register requires a connector ID and a non-nil Crawler")
	}
	if _, exists := registry[connectorID]; exists {
		panic("source: a Crawler is already registered for connector " + connectorID)
	}
	registry[connectorID] = c
}

// Get returns the Crawler registered for a connector ID, if any.
func Get(connectorID string) (Crawler, bool) {
	c, ok := registry[connectorID]
	return c, ok
}
