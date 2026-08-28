// Package zotero is the only place that knows how to walk a Zotero library
// for semantic-search indexing: which tools to call, in what order, and how
// to recognise a PDF or snapshot attachment in the response shape. Nothing
// outside this package (and its own tests) should need to change when that
// knowledge changes.
package zotero

import (
	"context"

	"github.com/00101010xyz/mcpaw/internal/index/source"
)

// connectorID is the only connector this crawler ever names — it appears
// nowhere outside this file.
const connectorID = "zotero-local"

func init() {
	source.Register(connectorID, Crawler{})
}

// Safety caps for one crawl: bound how long and how much a single
// operator-triggered reindex can consume against a personal library.
const (
	pageSize      = 100
	maxItems      = 2000
	childrenLimit = 50
)

var requiredTools = []string{
	"zotero_list_top_items",
	"zotero_get_item_children",
	"zotero_get_item_fulltext",
}

// Crawler walks a Zotero library: every top-level item, every PDF or
// HTML-snapshot attachment on it, and that attachment's extracted text.
type Crawler struct{}

// RequiredTools implements source.Crawler.
func (Crawler) RequiredTools() []string { return requiredTools }

// Crawl implements source.Crawler.
func (c Crawler) Crawl(ctx context.Context, rt source.Runtime, emit source.EmitFunc) error {
	start := 0
	for processed := 0; processed < maxItems; {
		result, err := rt.Call(ctx, "zotero_list_top_items", map[string]any{"limit": pageSize, "start": start})
		if err != nil {
			return err
		}
		items, _ := result.Data.([]any)
		if len(items) == 0 {
			break
		}
		for _, raw := range items {
			item, _ := raw.(map[string]any)
			key, _ := item["key"].(string)
			if key == "" {
				continue
			}
			if err := c.crawlItem(ctx, rt, key, emit); err != nil {
				return err
			}
			processed++
		}
		if len(items) < pageSize {
			break
		}
		start += pageSize
	}
	return nil
}

// crawlItem visits one top-level item's children and emits one document per
// PDF or HTML-snapshot attachment found. An item with no such attachment, or
// whose children can't be listed (Zotero returning an error for one item —
// deleted mid-crawl, say — is routine, not fatal to the rest of the run),
// still gets exactly one emit with empty text: the caller counts every emit
// as one document examined, so progress reporting stays accurate whether or
// not the item had anything indexable.
func (c Crawler) crawlItem(ctx context.Context, rt source.Runtime, itemKey string, emit source.EmitFunc) error {
	empty := source.Document{ItemKey: itemKey, AttachmentKey: itemKey}

	result, err := rt.Call(ctx, "zotero_get_item_children", map[string]any{"itemKey": itemKey, "limit": childrenLimit})
	if err != nil {
		return emit(ctx, empty, "")
	}
	children, _ := result.Data.([]any)

	visited := false
	for _, raw := range children {
		child, _ := raw.(map[string]any)
		data, _ := child["data"].(map[string]any)
		if data == nil {
			continue
		}
		if itemType, _ := data["itemType"].(string); itemType != "attachment" {
			continue
		}
		contentType, _ := data["contentType"].(string)
		if contentType != "application/pdf" && contentType != "text/html" {
			continue
		}
		attKey, _ := child["key"].(string)
		if attKey == "" {
			continue
		}
		visited = true
		if err := c.crawlAttachment(ctx, rt, itemKey, attKey, emit); err != nil {
			return err
		}
	}
	if !visited {
		return emit(ctx, empty, "")
	}
	return nil
}

// crawlAttachment fetches one attachment's extracted text. A fetch error —
// most commonly a 404, because Zotero has not extracted text for this
// attachment yet — is routine and reported as an empty-text emit rather than
// aborting the crawl.
func (c Crawler) crawlAttachment(ctx context.Context, rt source.Runtime, itemKey, attachmentKey string, emit source.EmitFunc) error {
	doc := source.Document{ItemKey: itemKey, AttachmentKey: attachmentKey}
	result, err := rt.Call(ctx, "zotero_get_item_fulltext", map[string]any{"itemKey": attachmentKey})
	if err != nil {
		return emit(ctx, doc, "")
	}
	data, _ := result.Data.(map[string]any)
	content, _ := data["content"].(string)
	return emit(ctx, doc, content)
}
