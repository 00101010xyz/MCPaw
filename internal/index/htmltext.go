package index

import (
	"html"
	"strings"
)

// blockTags is the set of HTML tag names whose boundary should read as a
// paragraph break in stripped output. Real markup routinely has no
// whitespace between adjacent block elements — "<p>First</p><p>Second</p>"
// — which would otherwise strip down to the unreadable, unchunkable
// "FirstSecond".
var blockTags = map[string]bool{
	"p": true, "div": true, "br": true, "hr": true,
	"h1": true, "h2": true, "h3": true, "h4": true, "h5": true, "h6": true,
	"li": true, "tr": true, "td": true, "th": true,
	"blockquote": true, "pre": true, "section": true, "article": true,
	"header": true, "footer": true, "nav": true, "table": true, "ul": true, "ol": true,
}

// skippedTags never contribute their content to the output: it is markup,
// script or metadata, never prose an embedding should see.
var skippedTags = map[string]bool{"script": true, "style": true, "noscript": true, "template": true}

// StripHTML converts an HTML document into plain text suitable for
// embedding: tags are removed (a block-level tag's boundary becomes a
// paragraph break), script/style/comment content is dropped entirely,
// entities are decoded, and whitespace is collapsed to one space within a
// line and at most one blank line between paragraphs.
//
// This is a linear scanner, not an HTML5 parser — enough to turn a saved
// page snapshot into readable, chunkable text, not to render one. It
// assumes a '>' inside a quoted attribute value does not end the tag (which
// it handles) but makes no attempt at anything an HTML5 parser would need a
// full DOM for, such as recovering from an unclosed tag mid-attribute.
func StripHTML(raw string) string {
	var sb strings.Builder
	i, n := 0, len(raw)
	for i < n {
		if raw[i] != '<' {
			sb.WriteByte(raw[i])
			i++
			continue
		}
		if strings.HasPrefix(raw[i:], "<!--") {
			if end := strings.Index(raw[i+4:], "-->"); end >= 0 {
				i += 4 + end + 3
			} else {
				i = n
			}
			continue
		}
		name, bodyStart := scanTag(raw, i)
		if bodyStart < 0 {
			// No closing '>' anywhere ahead: the rest of the document is
			// one unterminated tag, not content.
			break
		}
		if skippedTags[name] {
			i = skipToClosingTag(raw, bodyStart, name)
			continue
		}
		if blockTags[name] {
			sb.WriteByte('\n')
		}
		i = bodyStart
	}
	return collapseHTMLWhitespace(html.UnescapeString(sb.String()))
}

// scanTag reads the element name starting at raw[i] (which must be '<') and
// returns the index just past the tag's closing '>', skipping over any
// quoted attribute value along the way so a literal '>' inside e.g.
// title="5 > 3" does not end the tag early. bodyStart is -1 if no
// (unquoted) '>' is ever found.
func scanTag(raw string, i int) (name string, bodyStart int) {
	j := i + 1
	if j < len(raw) && raw[j] == '/' {
		j++
	}
	start := j
	for j < len(raw) && isTagNameByte(raw[j]) {
		j++
	}
	name = strings.ToLower(raw[start:j])

	for j < len(raw) {
		switch raw[j] {
		case '>':
			return name, j + 1
		case '"', '\'':
			quote := raw[j]
			j++
			for j < len(raw) && raw[j] != quote {
				j++
			}
		}
		j++
	}
	return name, -1
}

func isTagNameByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9' || b == '-'
}

// skipToClosingTag returns the index just past </name>'s '>', searching
// from "from" (just past the opening tag). An unterminated <script> or
// <style> means nothing after it can be trusted as prose either, so the
// whole rest of the document is skipped in that case.
func skipToClosingTag(raw string, from int, name string) int {
	idx := indexFold(raw[from:], "</"+name)
	if idx < 0 {
		return len(raw)
	}
	gt := strings.IndexByte(raw[from+idx:], '>')
	if gt < 0 {
		return len(raw)
	}
	return from + idx + gt + 1
}

func indexFold(s, substr string) int {
	return strings.Index(strings.ToLower(s), strings.ToLower(substr))
}

// collapseHTMLWhitespace squeezes horizontal whitespace to one space per
// line and three-or-more consecutive blank lines down to exactly one blank
// line, so the block-tag newlines StripHTML inserted survive as "\n\n"
// paragraph breaks — the boundary Chunk's own break-point search looks for
// first — rather than being flattened away entirely.
func collapseHTMLWhitespace(s string) string {
	lines := strings.Split(s, "\n")
	var kept []string
	blank := 0
	for _, line := range lines {
		line = strings.Join(strings.Fields(line), " ")
		if line == "" {
			blank++
			if blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}
