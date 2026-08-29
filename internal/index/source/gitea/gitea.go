// Package gitea is the only place that knows how to walk Gitea repositories
// for semantic-search indexing: which repositories to look at, which tools
// to call, which files are worth reading, and how to turn Gitea's git-blob
// response into plain text. Nothing outside this package (and its own
// tests) should need to change when that knowledge changes.
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

// maxFiles bounds how many candidate files (matching a known extension) one
// crawl will fetch and index across every repository combined, independent
// of whatever truncation the Gitea server itself applies to one tree
// listing. maxRepos bounds how many repositories one crawl will discover in
// the first place. Both are vars, not consts, so a test can lower them
// rather than construct thousands of fixture entries to exercise the caps.
var (
	maxFiles     = 2000
	maxRepos     = 500
	repoPageSize = 50
)

var requiredTools = []string{"gitea_list_repos", "gitea_list_tree", "gitea_get_file"}

// dialectByExtension maps a file's extension to the heading-chunking
// dialect that understands it. A file whose extension is not here is not a
// candidate document at all — the crawler never fetches it, so a binary
// asset or an unsupported text format costs nothing beyond appearing once
// in a tree listing.
var dialectByExtension = map[string]string{
	".md":       index.DialectMarkdown,
	".markdown": index.DialectMarkdown,
	".typ":      index.DialectTypst,
}

// Crawler walks Gitea repositories and reads every markdown or typst file's
// content. By default it discovers every repository the configured token
// can see (see gitea_list_repos); an instance may instead pin a single
// owner/repo (and optionally ref) to index just that one, via the
// connector's owner/repo/ref variables.
type Crawler struct{}

// RequiredTools implements source.Crawler.
func (Crawler) RequiredTools() []string { return requiredTools }

// repoRef identifies one repository to walk, at one branch.
type repoRef struct {
	owner, name, ref string
}

// Crawl implements source.Crawler.
func (c Crawler) Crawl(ctx context.Context, rt source.Runtime, emit source.EmitFunc) (bool, error) {
	repos, truncated, err := c.discoverRepos(ctx, rt)
	if err != nil {
		return false, err
	}

	fetched := 0
	for _, repo := range repos {
		repoTruncated, err := c.crawlRepo(ctx, rt, repo, &fetched, emit)
		if err != nil {
			return false, err
		}
		if repoTruncated {
			truncated = true
		}
		if fetched >= maxFiles {
			return true, nil
		}
	}
	return truncated, nil
}

// discoverRepos resolves which repositories to walk: the single owner/repo
// override on the instance, if set, or every repository the configured
// token can see, paginated through gitea_list_repos until the server
// returns a page shorter than requested.
func (c Crawler) discoverRepos(ctx context.Context, rt source.Runtime) ([]repoRef, bool, error) {
	owner := rt.Target.Vars["owner"]
	name := rt.Target.Vars["repo"]
	if owner != "" && name != "" {
		ref := rt.Target.Vars["ref"]
		if ref == "" {
			result, err := rt.Call(ctx, "gitea_get_repo", map[string]any{"owner": owner, "repo": name})
			if err != nil {
				return nil, false, err
			}
			body, _ := result.Data.(map[string]any)
			if branch, _ := body["default_branch"].(string); branch != "" {
				ref = branch
			} else {
				ref = "main"
			}
		}
		return []repoRef{{owner: owner, name: name, ref: ref}}, false, nil
	}

	var repos []repoRef
	for page := 1; ; page++ {
		result, err := rt.Call(ctx, "gitea_list_repos", map[string]any{"page": page, "limit": repoPageSize})
		if err != nil {
			return nil, false, err
		}
		entries, _ := result.Data.([]any)
		for _, raw := range entries {
			entry, _ := raw.(map[string]any)
			if empty, _ := entry["empty"].(bool); empty {
				// Nothing committed yet; the tree endpoint has nothing to list.
				continue
			}
			ownerObj, _ := entry["owner"].(map[string]any)
			login, _ := ownerObj["login"].(string)
			repoName, _ := entry["name"].(string)
			branch, _ := entry["default_branch"].(string)
			if login == "" || repoName == "" || branch == "" {
				continue
			}
			repos = append(repos, repoRef{owner: login, name: repoName, ref: branch})
			if len(repos) >= maxRepos {
				return repos, true, nil
			}
		}
		if len(entries) < repoPageSize {
			return repos, false, nil
		}
	}
}

// crawlRepo walks one repository's file tree at its resolved ref, fetching
// and emitting every file with a recognised extension. *fetched is a
// running total shared across every repository this crawl visits, since
// maxFiles bounds the whole crawl, not any one repository.
func (c Crawler) crawlRepo(ctx context.Context, rt source.Runtime, repo repoRef, fetched *int, emit source.EmitFunc) (bool, error) {
	result, err := rt.Call(ctx, "gitea_list_tree", map[string]any{"owner": repo.owner, "repo": repo.name, "ref": repo.ref})
	if err != nil {
		return false, err
	}
	body, _ := result.Data.(map[string]any)
	entries, _ := body["tree"].([]any)
	// The Gitea server itself may have truncated this tree listing (a very
	// large repository), independent of our own maxFiles cap below — either
	// way this run did not see the whole repository.
	truncated, _ := body["truncated"].(bool)

	for _, raw := range entries {
		if *fetched >= maxFiles {
			return true, nil
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
		*fetched++
		if err := c.fetchFile(ctx, rt, repo, path, sha, dialect, emit); err != nil {
			return false, err
		}
	}
	return truncated, nil
}

// fetchFile reads one blob's content. A fetch or decode failure degrades to
// an empty-text emit rather than aborting the whole crawl — the same
// posture internal/index/source/zotero takes for one attachment's fulltext
// fetch failing. The document key is prefixed with the repository's
// "owner/name" so files at the same path in two different repositories
// never collide.
func (c Crawler) fetchFile(ctx context.Context, rt source.Runtime, repo repoRef, path, sha, dialect string, emit source.EmitFunc) error {
	key := repo.owner + "/" + repo.name + ":" + path
	doc := source.Document{ItemKey: key, AttachmentKey: key, HeadingDialect: dialect}

	result, err := rt.Call(ctx, "gitea_get_file", map[string]any{"owner": repo.owner, "repo": repo.name, "sha": sha})
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
