// Package gitea is the only place that knows how to walk a Gitea
// repository for semantic-search indexing: which tools to call, which files
// are worth reading, and how to turn Gitea's git-blob response into plain
// text. Nothing outside this package (and its own tests) should need to
// change when that knowledge changes.
package gitea

import (
	"context"
	"encoding/base64"
	"strings"

	"github.com/00101010xyz/mcpaw/internal/index"
	"github.com/00101010xyz/mcpaw/internal/index/source"
)

// connectorID is the only connector this crawler ever names — it appears
// nowhere outside this file.
const connectorID = "gitea"

func init() {
	source.Register(connectorID, Crawler{})
}

// maxFiles bounds how many candidate files (matching a known extension)
// one crawl will fetch and index, independent of whatever truncation the
// Gitea server itself applies to the tree listing. A var, not a const, so a
// test can lower it rather than construct thousands of fixture entries to
// exercise the cap.
var maxFiles = 2000

var requiredTools = []string{"gitea_list_tree", "gitea_get_file"}

// dialectByExtension maps a file's extension to the heading-chunking
// dialect that understands it. A file whose extension is not here is not a
// candidate document at all — the crawler never fetches it, so a binary
// asset or an unsupported text format costs nothing beyond appearing once
// in the tree listing.
var dialectByExtension = map[string]string{
	".md":       index.DialectMarkdown,
	".markdown": index.DialectMarkdown,
	".typ":      index.DialectTypst,
}

// Crawler walks one Gitea repository's file tree at the instance's
// configured ref and reads every markdown or typst file's content.
type Crawler struct{}

// RequiredTools implements source.Crawler.
func (Crawler) RequiredTools() []string { return requiredTools }

// Crawl implements source.Crawler.
func (c Crawler) Crawl(ctx context.Context, rt source.Runtime, emit source.EmitFunc) (bool, error) {
	result, err := rt.Call(ctx, "gitea_list_tree", map[string]any{})
	if err != nil {
		return false, err
	}
	body, _ := result.Data.(map[string]any)
	entries, _ := body["tree"].([]any)
	// The Gitea server itself may have truncated the tree listing (a very
	// large repository), independent of our own maxFiles cap below — either
	// way this run did not see the whole repository.
	truncated, _ := body["truncated"].(bool)

	fetched := 0
	for _, raw := range entries {
		if fetched >= maxFiles {
			truncated = true
			break
		}
		entry, _ := raw.(map[string]any)
		if entryType, _ := entry["type"].(string); entryType != "blob" {
			continue
		}
		path, _ := entry["path"].(string)
		sha, _ := entry["sha"].(string)
		if path == "" || sha == "" {
			continue
		}
		dialect, ok := dialectByExtension[extensionOf(path)]
		if !ok {
			continue
		}
		fetched++
		if err := c.fetchFile(ctx, rt, path, sha, dialect, emit); err != nil {
			return false, err
		}
	}
	return truncated, nil
}

// fetchFile reads one blob's content. A fetch or decode failure degrades to
// an empty-text emit rather than aborting the whole crawl — the same
// posture internal/index/source/zotero takes for one attachment's fulltext
// fetch failing.
func (c Crawler) fetchFile(ctx context.Context, rt source.Runtime, path, sha, dialect string, emit source.EmitFunc) error {
	doc := source.Document{ItemKey: path, AttachmentKey: path, HeadingDialect: dialect}

	result, err := rt.Call(ctx, "gitea_get_file", map[string]any{"sha": sha})
	if err != nil {
		return emit(ctx, doc, "")
	}
	body, _ := result.Data.(map[string]any)
	encoded, _ := body["content"].(string)

	// Git blob content comes base64-encoded, and Gitea (like GitHub) wraps
	// long base64 payloads across multiple lines, which the standard
	// encoding will not decode as-is.
	decoded, err := base64.StdEncoding.DecodeString(stripWhitespace(encoded))
	if err != nil {
		return emit(ctx, doc, "")
	}
	return emit(ctx, doc, string(decoded))
}

func extensionOf(path string) string {
	if i := strings.LastIndexByte(path, '.'); i >= 0 {
		return strings.ToLower(path[i:])
	}
	return ""
}

func stripWhitespace(s string) string {
	return strings.Join(strings.Fields(s), "")
}
