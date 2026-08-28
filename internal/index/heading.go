package index

import "strings"

// Heading-chunking dialects. Exactly one applies to any call to
// ChunkHeading — the caller picks it, typically from a file extension,
// never sniffed from content: '#' opens a markdown heading but calls a
// function in typst, so guessing from the text itself would misfire on
// exactly the documents this exists to handle well.
const (
	DialectTypst    = "typst"
	DialectMarkdown = "markdown"
)

// headingMarker returns the marker byte a dialect's headings start with, or
// ok=false for an unrecognised dialect.
func headingMarker(dialect string) (byte, bool) {
	switch dialect {
	case DialectTypst:
		return '=', true
	case DialectMarkdown:
		return '#', true
	default:
		return 0, false
	}
}

type headingLine struct {
	level int
	title string
}

// parseHeading reports the level and title of line if it is a heading line
// for marker: one or more marker bytes at the very start of the line,
// followed by a space and a non-empty title. Requiring column zero avoids
// misreading an indented example or a quoted heading inside other content.
func parseHeading(line string, marker byte) (headingLine, bool) {
	i := 0
	for i < len(line) && line[i] == marker {
		i++
	}
	if i == 0 || i >= len(line) || line[i] != ' ' {
		return headingLine{}, false
	}
	title := strings.TrimSpace(line[i+1:])
	if title == "" {
		return headingLine{}, false
	}
	return headingLine{level: i, title: title}, true
}

// isFence reports whether line is a code-fence delimiter (``` or ~~~,
// optionally followed by a language tag). Both markdown and typst use
// triple-backtick fences for raw/code blocks, and a shell or Python snippet
// inside one very plausibly has a line starting with '#' that is a comment,
// not a heading.
func isFence(line string) bool {
	t := strings.TrimSpace(line)
	return strings.HasPrefix(t, "```") || strings.HasPrefix(t, "~~~")
}

type headingStackEntry struct {
	level int
	title string
}

type headingSection struct {
	breadcrumb string
	bodyStart  int
	bodyEnd    int
}

// splitSections walks text line by line, recognising dialect's heading
// syntax outside of code fences, and returns the resulting sections, each
// carrying a breadcrumb built from every enclosing heading still open at
// that point. Returns nil if the text has no recognisable heading at all —
// an unrecognised dialect, a document that just doesn't use them, or one
// where every '#'/'=' happens to be inside a code fence — which the caller
// takes as "fall back to plain size chunking".
func splitSections(text, dialect string) []headingSection {
	marker, ok := headingMarker(dialect)
	if !ok {
		return nil
	}

	lines := strings.SplitAfter(text, "\n")
	var stack []headingStackEntry
	var sections []headingSection
	pos := 0
	bodyStart := 0
	inFence := false
	haveHeading := false

	flush := func(end int) {
		if end <= bodyStart {
			return
		}
		titles := make([]string, len(stack))
		for i, e := range stack {
			titles[i] = e.title
		}
		sections = append(sections, headingSection{
			breadcrumb: strings.Join(titles, " > "), bodyStart: bodyStart, bodyEnd: end,
		})
	}

	for _, line := range lines {
		trimmed := strings.TrimRight(line, "\n")
		lineLen := len([]rune(line))

		if isFence(trimmed) {
			inFence = !inFence
		} else if !inFence {
			if hl, ok := parseHeading(trimmed, marker); ok {
				haveHeading = true
				flush(pos)
				for len(stack) > 0 && stack[len(stack)-1].level >= hl.level {
					stack = stack[:len(stack)-1]
				}
				stack = append(stack, headingStackEntry{level: hl.level, title: hl.title})
				bodyStart = pos + lineLen
			}
		}
		pos += lineLen
	}
	flush(pos)

	if !haveHeading {
		return nil
	}
	return sections
}

// ChunkHeading splits text into spans along dialect's heading syntax,
// falling back to ordinary size chunking (see Chunk) when the text has no
// recognisable heading at all.
//
// Every span's embedded Text is prefixed with a breadcrumb of the headings
// it falls under ("Method > Data collection") — this matters more than the
// split itself: a bare paragraph embeds distinctly worse than the same text
// carrying its section context, since the section title is often exactly
// the query term a search is looking for. A section whose own body exceeds
// the shared chunk-size cap is split further with the ordinary size
// chunker, with the same breadcrumb on every piece — a two-paragraph
// subsection stays one chunk, but a forty-page chapter under one heading
// does not become one giant embedding.
func ChunkHeading(text, dialect string) []Span {
	sections := splitSections(text, dialect)
	if sections == nil {
		return Chunk(text)
	}

	runes := []rune(text)
	var spans []Span
	for _, sec := range sections {
		body := strings.TrimSpace(string(runes[sec.bodyStart:sec.bodyEnd]))
		if body == "" {
			continue
		}
		if len([]rune(body)) <= defaultSize {
			spans = append(spans, Span{
				Index: len(spans), Start: sec.bodyStart, End: sec.bodyEnd,
				Text: withBreadcrumb(sec.breadcrumb, body),
			})
			continue
		}
		for _, sub := range ChunkSize(body, defaultSize, defaultOverlap) {
			// Start/End span the whole oversized section for every piece of
			// it, rather than the sub-chunk's own narrower slice: nothing
			// currently reads these offsets more precisely than "roughly
			// where in the document this came from", and getting that exact
			// would mean re-deriving them through TrimSpace's shift, which
			// isn't worth the complexity for advisory metadata.
			spans = append(spans, Span{
				Index: len(spans), Start: sec.bodyStart, End: sec.bodyEnd,
				Text: withBreadcrumb(sec.breadcrumb, sub.Text),
			})
		}
	}
	return spans
}

func withBreadcrumb(breadcrumb, body string) string {
	if breadcrumb == "" {
		return body
	}
	return breadcrumb + "\n\n" + body
}
