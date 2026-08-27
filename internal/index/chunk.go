// Package index implements the offline half of semantic search: splitting
// extracted document text into overlapping chunks, calling a configured
// embedder for their vectors, and fusing keyword and vector rankings at
// query time. It has no dependency on storage, connectors or the MCP
// protocol, so it is testable in isolation.
package index

import "strings"

// Span is one chunk of a larger text, with its offsets into the original.
type Span struct {
	Index int
	Start int
	End   int
	Text  string
}

// defaultSize and defaultOverlap mirror common RAG chunking practice: large
// enough to hold a coherent paragraph or two, small enough that a query
// embedding still resolves distinctly against it.
const (
	defaultSize    = 1000
	defaultOverlap = 150
)

// Chunk splits text into overlapping spans, breaking on paragraph boundaries
// where possible and falling back to sentence, then hard, boundaries so a
// chunk does not split a word or a citation mid-token.
func Chunk(text string) []Span {
	return ChunkSize(text, defaultSize, defaultOverlap)
}

// ChunkSize is Chunk with an explicit target size and overlap, in runes.
func ChunkSize(text string, size, overlap int) []Span {
	runes := []rune(text)
	if len(runes) == 0 {
		return nil
	}
	if size <= 0 {
		size = defaultSize
	}
	if overlap < 0 || overlap >= size {
		overlap = 0
	}

	var spans []Span
	start := 0
	for start < len(runes) {
		end := start + size
		if end >= len(runes) {
			end = len(runes)
		} else {
			end = breakPoint(runes, start, end)
		}
		chunk := strings.TrimSpace(string(runes[start:end]))
		if chunk != "" {
			spans = append(spans, Span{Index: len(spans), Start: start, End: end, Text: chunk})
		}
		if end >= len(runes) {
			break
		}
		next := end - overlap
		if next <= start {
			next = end
		}
		start = next
	}
	return spans
}

// breakPoint looks backward from a raw cut point for a paragraph break, then
// a sentence break, then a space, so chunks read as prose rather than
// arbitrary character windows. It gives up and returns the raw cut only when
// none exists within a reasonable lookback.
func breakPoint(runes []rune, start, rawEnd int) int {
	lookback := rawEnd - start/2
	if lookback < start {
		lookback = start
	}
	minCut := rawEnd - (rawEnd-start)/3 // don't shrink a chunk by more than a third
	if minCut < start {
		minCut = start
	}

	if i := lastIndexAfter(runes, minCut, rawEnd, "\n\n"); i > 0 {
		return i
	}
	for _, sep := range []string{". ", "? ", "! ", "\n"} {
		if i := lastIndexAfter(runes, minCut, rawEnd, sep); i > 0 {
			return i
		}
	}
	if i := lastIndexAfter(runes, minCut, rawEnd, " "); i > 0 {
		return i
	}
	return rawEnd
}

// lastIndexAfter finds the last occurrence of sep in runes[from:to], and
// returns the rune offset just past it, or -1.
//
// strings.LastIndex works in bytes, so its result is converted back to a
// rune count via the prefix it matched; runes[from:to] is small (at most one
// chunk), so re-counting here is cheap.
func lastIndexAfter(runes []rune, from, to int, sep string) int {
	s := string(runes[from:to])
	i := strings.LastIndex(s, sep)
	if i < 0 {
		return -1
	}
	return from + len([]rune(s[:i])) + len([]rune(sep))
}
