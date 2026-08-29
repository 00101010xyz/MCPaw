package index

import (
	"strings"
	"testing"
)

func TestChunkHeadingMarkdownBasic(t *testing.T) {
	text := "# Introduction\n\nThis paper studies X.\n\n## Background\n\nPrior work did Y.\n"
	spans := ChunkHeading(text, DialectMarkdown)
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2: %+v", len(spans), spans)
	}
	if !strings.HasPrefix(spans[0].Text, "Introduction\n\n") {
		t.Errorf("span 0 = %q, want it to start with the Introduction breadcrumb", spans[0].Text)
	}
	if !strings.Contains(spans[0].Text, "This paper studies X.") {
		t.Errorf("span 0 missing its own body: %q", spans[0].Text)
	}
	if !strings.HasPrefix(spans[1].Text, "Introduction > Background\n\n") {
		t.Errorf("span 1 = %q, want the nested breadcrumb", spans[1].Text)
	}
}

func TestChunkHeadingTypstBasic(t *testing.T) {
	text := "= Method\n\nWe collected data.\n\n== Data collection\n\nParticipants were recruited.\n"
	spans := ChunkHeading(text, DialectTypst)
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2: %+v", len(spans), spans)
	}
	if !strings.HasPrefix(spans[1].Text, "Method > Data collection\n\n") {
		t.Errorf("span 1 = %q, want the nested typst breadcrumb", spans[1].Text)
	}
}

// '#' opens a markdown heading but calls a function in typst, and '='
// opens a typst heading but is prose in markdown; chunking under the wrong
// dialect must not recognise the other syntax's marker at all.
func TestChunkHeadingDialectsAreNotCrossRecognised(t *testing.T) {
	markdownText := "# Introduction\n\nBody text here.\n\n## Background\n\nMore text.\n"
	if got := ChunkHeading(markdownText, DialectTypst); len(got) != len(Chunk(markdownText)) {
		t.Errorf("markdown headings recognised under the typst dialect: got %d spans, want the plain-chunk fallback's %d",
			len(got), len(Chunk(markdownText)))
	}

	typstText := "= Method\n\nBody text here.\n\n== Data collection\n\nMore text.\n"
	if got := ChunkHeading(typstText, DialectMarkdown); len(got) != len(Chunk(typstText)) {
		t.Errorf("typst headings recognised under the markdown dialect: got %d spans, want the plain-chunk fallback's %d",
			len(got), len(Chunk(typstText)))
	}
}

// A markdown document with a fenced code block containing a shell comment
// must not treat the comment as a heading.
func TestChunkHeadingSkipsCodeFences(t *testing.T) {
	text := "# Setup\n\nRun this:\n\n```sh\n# this is a shell comment, not a heading\necho hi\n```\n\nDone.\n"
	spans := ChunkHeading(text, DialectMarkdown)
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1 (one section, the comment must not open a new one): %+v", len(spans), spans)
	}
	if !strings.Contains(spans[0].Text, "this is a shell comment") {
		t.Error("the fenced code block's content is missing from the section body")
	}
	if strings.Contains(spans[0].Text, "Setup > this is a shell comment") {
		t.Error("the shell comment was misread as a nested heading")
	}
}

func TestChunkHeadingFallsBackWhenNoHeadingsPresent(t *testing.T) {
	text := "Just a plain paragraph of prose with no heading syntax anywhere in it at all."
	spans := ChunkHeading(text, DialectMarkdown)
	direct := Chunk(text)
	if len(spans) != len(direct) || (len(spans) > 0 && spans[0].Text != direct[0].Text) {
		t.Errorf("ChunkHeading with no headings present should fall back to Chunk's own behaviour; got %+v, want %+v", spans, direct)
	}
}

func TestChunkHeadingUnrecognisedDialectFallsBack(t *testing.T) {
	text := "# Looks like markdown but the dialect is wrong\n\nBody."
	spans := ChunkHeading(text, "some-future-dialect")
	direct := Chunk(text)
	if len(spans) != len(direct) {
		t.Errorf("an unrecognised dialect should fall back to plain size chunking, got %d spans want %d", len(spans), len(direct))
	}
}

func TestChunkHeadingEmpty(t *testing.T) {
	if got := ChunkHeading("", DialectMarkdown); got != nil {
		t.Fatalf("ChunkHeading(\"\", ...) = %v, want nil", got)
	}
}

// Text before the first heading (an abstract with no heading of its own,
// say) must still be indexed, just without a breadcrumb.
func TestChunkHeadingPreambleBeforeFirstHeading(t *testing.T) {
	text := "This is the abstract, with no heading of its own.\n\n# Introduction\n\nBody.\n"
	spans := ChunkHeading(text, DialectMarkdown)
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2 (preamble + Introduction): %+v", len(spans), spans)
	}
	if strings.Contains(spans[0].Text, ">") {
		t.Errorf("the preamble span should have no breadcrumb, got %q", spans[0].Text)
	}
	if !strings.Contains(spans[0].Text, "abstract") {
		t.Errorf("preamble span missing its content: %q", spans[0].Text)
	}
}

// A sibling heading at the same level must close the previous one's scope
// rather than nest under it.
func TestChunkHeadingSiblingsDoNotNest(t *testing.T) {
	text := "# One\n\nFirst.\n\n# Two\n\nSecond.\n"
	spans := ChunkHeading(text, DialectMarkdown)
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2: %+v", len(spans), spans)
	}
	if !strings.HasPrefix(spans[1].Text, "Two\n\n") {
		t.Errorf("span 1 = %q, want breadcrumb just \"Two\", not nested under \"One\"", spans[1].Text)
	}
}

// A section whose body exceeds the chunk-size cap must still be split, and
// every resulting piece must carry the section's breadcrumb.
func TestChunkHeadingOversizedSectionIsSplitWithBreadcrumbOnEveryPiece(t *testing.T) {
	body := strings.Repeat("word ", 40) + "\n\n"
	text := "# Chapter\n\n" + strings.Repeat(body, 20)

	spans := ChunkHeading(text, DialectMarkdown)
	if len(spans) < 2 {
		t.Fatalf("expected an oversized section to be split into multiple spans, got %d", len(spans))
	}
	for i, sp := range spans {
		if !strings.HasPrefix(sp.Text, "Chapter\n\n") {
			t.Errorf("span %d = %q, missing the Chapter breadcrumb", i, sp.Text)
		}
	}
}

// Every span's Index must be sequential from zero, matching Chunk's own
// contract, since IndexChunk.ChunkIndex is derived from it.
func TestChunkHeadingIndicesAreSequential(t *testing.T) {
	text := "# A\n\nOne.\n\n# B\n\nTwo.\n\n# C\n\nThree.\n"
	spans := ChunkHeading(text, DialectMarkdown)
	for i, sp := range spans {
		if sp.Index != i {
			t.Errorf("span %d has Index %d", i, sp.Index)
		}
	}
}

func TestChunkHeadingSpansStayWithinBounds(t *testing.T) {
	text := "# A\n\nOne two three.\n\n## B\n\nFour five six.\n"
	runes := []rune(text)
	spans := ChunkHeading(text, DialectMarkdown)
	for _, sp := range spans {
		if sp.Start < 0 || sp.End > len(runes) || sp.Start > sp.End {
			t.Fatalf("span %+v out of bounds for %d runes", sp, len(runes))
		}
	}
}
