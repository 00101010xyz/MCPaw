// Package linkding is the only place that knows how to walk a Linkding
// bookmark manager for semantic-search indexing: which tools to call, how
// to recognise a completed HTML snapshot among a bookmark's assets, and how
// to turn one into plain text. Nothing outside this package (and its own
// tests) should need to change when that knowledge changes.
package linkding

import (
	"context"
	"strconv"

	"github.com/00101010xyz/mcpaw/internal/index"
	"github.com/00101010xyz/mcpaw/internal/index/source"
)

// connectorID is the only connector this crawler ever names — it appears
// nowhere outside this file.
const connectorID = "linkding"

func init() {
	source.Register(connectorID, Crawler{})
}

// pageSize and maxBookmarks are vars, not consts, so a test can lower
// maxBookmarks rather than construct thousands of fixture bookmarks to
// exercise the cap.
var (
	pageSize     = 100
	maxBookmarks = 2000
)

var requiredTools = []string{"linkding_list_bookmarks", "linkding_list_assets", "linkding_get_asset_content"}

// Crawler walks every bookmark in a Linkding instance and reads the text of
// each completed HTML snapshot attached to it. A bookmark may have more
// than one snapshot (re-archived over time); each becomes its own document
// rather than guessing which one is "the" current one, since the list
// endpoint's ordering is not part of Linkding's documented contract.
type Crawler struct{}

// RequiredTools implements source.Crawler.
func (Crawler) RequiredTools() []string { return requiredTools }

// Crawl implements source.Crawler.
func (c Crawler) Crawl(ctx context.Context, rt source.Runtime, emit source.EmitFunc) (bool, error) {
	offset := 0
	processed := 0
	for processed < maxBookmarks {
		result, err := rt.Call(ctx, "linkding_list_bookmarks", map[string]any{"limit": pageSize, "offset": offset})
		if err != nil {
			return false, err
		}
		body, _ := result.Data.(map[string]any)
		results, _ := body["results"].([]any)
		if len(results) == 0 {
			return false, nil
		}
		for _, raw := range results {
			bm, _ := raw.(map[string]any)
			id, ok := numericID(bm["id"])
			if !ok {
				continue
			}
			if err := c.crawlBookmark(ctx, rt, id, emit); err != nil {
				return false, err
			}
			processed++
		}
		if len(results) < pageSize {
			return false, nil
		}
		offset += pageSize
	}
	// The loop exited because processed reached maxBookmarks while a full
	// page was still coming back — there are more bookmarks left unwalked.
	return true, nil
}

// crawlBookmark visits one bookmark's assets and emits one document per
// completed HTML snapshot found. A bookmark with no such snapshot, or whose
// assets can't be listed (routine — deleted mid-crawl, say — not fatal to
// the rest of the run), still gets exactly one emit with empty text: the
// caller counts every emit as one document examined, so progress reporting
// stays accurate whether or not the bookmark had anything indexable.
func (c Crawler) crawlBookmark(ctx context.Context, rt source.Runtime, bookmarkID int, emit source.EmitFunc) error {
	itemKey := strconv.Itoa(bookmarkID)
	empty := source.Document{ItemKey: itemKey, AttachmentKey: itemKey}

	result, err := rt.Call(ctx, "linkding_list_assets", map[string]any{"bookmarkId": bookmarkID})
	if err != nil {
		return emit(ctx, empty, "")
	}
	body, _ := result.Data.(map[string]any)
	results, _ := body["results"].([]any)

	visited := false
	for _, raw := range results {
		asset, _ := raw.(map[string]any)
		assetType, _ := asset["asset_type"].(string)
		status, _ := asset["status"].(string)
		if assetType != "snapshot" || status != "complete" {
			continue
		}
		assetID, ok := numericID(asset["id"])
		if !ok {
			continue
		}
		visited = true
		if err := c.crawlAsset(ctx, rt, bookmarkID, assetID, itemKey, emit); err != nil {
			return err
		}
	}
	if !visited {
		return emit(ctx, empty, "")
	}
	return nil
}

// crawlAsset fetches one snapshot's raw HTML and strips it to plain text. A
// fetch failure degrades to an empty-text emit rather than aborting the
// crawl, the same posture every other source in this project takes.
func (c Crawler) crawlAsset(ctx context.Context, rt source.Runtime, bookmarkID, assetID int, itemKey string, emit source.EmitFunc) error {
	doc := source.Document{ItemKey: itemKey, AttachmentKey: strconv.Itoa(assetID)}
	result, err := rt.Call(ctx, "linkding_get_asset_content", map[string]any{"bookmarkId": bookmarkID, "assetId": assetID})
	if err != nil {
		return emit(ctx, doc, "")
	}
	return emit(ctx, doc, index.StripHTML(result.Text))
}

// numericID extracts an integer id from a JSON field, which decodes as
// float64 through map[string]any.
func numericID(v any) (int, bool) {
	f, ok := v.(float64)
	if !ok {
		return 0, false
	}
	return int(f), true
}
