package index

import (
	"strings"
	"testing"
)

func TestStripHTMLBasicTags(t *testing.T) {
	got := StripHTML("<p>Hello <b>world</b>.</p>")
	if got != "Hello world." {
		t.Errorf("got %q", got)
	}
}

func TestStripHTMLBlockTagsBecomeParagraphBreaks(t *testing.T) {
	got := StripHTML("<p>First</p><p>Second</p>")
	want := "First\n\nSecond"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripHTMLSkipsScriptAndStyleContent(t *testing.T) {
	got := StripHTML(`<style>body { color: red; }</style><p>Visible text</p><script>alert("hi > there");</script>`)
	if got != "Visible text" {
		t.Errorf("got %q, want just the paragraph text", got)
	}
}

func TestStripHTMLSkipsComments(t *testing.T) {
	got := StripHTML("<p>Before</p><!-- a comment with <p>fake tags</p> inside --><p>After</p>")
	want := "Before\n\nAfter"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripHTMLDecodesEntities(t *testing.T) {
	got := StripHTML("<p>Tom &amp; Jerry &mdash; 5 &lt; 10 &amp;amp;</p>")
	want := "Tom & Jerry — 5 < 10 &amp;"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A literal '>' inside a quoted attribute value must not be mistaken for
// the tag's own closing bracket.
func TestStripHTMLHandlesGreaterThanInsideQuotedAttribute(t *testing.T) {
	got := StripHTML(`<a title="5 > 3" href="x">link text</a> after`)
	want := "link text after"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestStripHTMLHandlesSingleQuotedAttribute(t *testing.T) {
	got := StripHTML(`<a title='a > b'>text</a>`)
	if got != "text" {
		t.Errorf("got %q", got)
	}
}

func TestStripHTMLCollapsesWhitespace(t *testing.T) {
	got := StripHTML("<p>Lots    of\t\tspace   here</p>")
	if got != "Lots of space here" {
		t.Errorf("got %q", got)
	}
}

func TestStripHTMLCollapsesManyBlankLinesToOne(t *testing.T) {
	got := StripHTML("<p>A</p><div></div><div></div><div></div><p>B</p>")
	want := "A\n\nB"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

// A realistic small page: head metadata (title tag text should NOT appear
// unless inside <title>, which is not a skipped tag here since <title> text
// is often useful) mixed with a nav, article body and a script block.
func TestStripHTMLRealisticPage(t *testing.T) {
	page := `<!DOCTYPE html>
<html>
<head><style>.x{color:blue}</style></head>
<body>
<nav><a href="/">Home</a></nav>
<article>
<h1>Article Title</h1>
<p>First paragraph of the article.</p>
<p>Second paragraph with a <a href="https://example.com">link</a> in it.</p>
</article>
<script>console.log("tracking");</script>
</body>
</html>`
	got := StripHTML(page)
	if strings.Contains(got, "color:blue") {
		t.Error("style content leaked into stripped text")
	}
	if strings.Contains(got, "tracking") {
		t.Error("script content leaked into stripped text")
	}
	if !strings.Contains(got, "Article Title") {
		t.Error("heading text missing")
	}
	if !strings.Contains(got, "First paragraph") || !strings.Contains(got, "Second paragraph") {
		t.Error("paragraph text missing")
	}
	if !strings.Contains(got, "link") {
		t.Error("inline link text missing")
	}
}

func TestStripHTMLEmpty(t *testing.T) {
	if got := StripHTML(""); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestStripHTMLNoTagsAtAll(t *testing.T) {
	if got := StripHTML("just plain text, no markup"); got != "just plain text, no markup" {
		t.Errorf("got %q", got)
	}
}

// An unterminated tag or script block must not panic or hang — the rest of
// the document is simply treated as non-content, per the documented
// "linear scanner, not a parser" contract.
func TestStripHTMLToleratesUnterminatedTag(t *testing.T) {
	got := StripHTML("<p>Before</p><div class=\"unterminated")
	if !strings.Contains(got, "Before") {
		t.Errorf("got %q, want it to at least contain the text before the broken tag", got)
	}
}

func TestStripHTMLToleratesUnterminatedScript(t *testing.T) {
	got := StripHTML("<p>Before</p><script>var x = 1;")
	if got != "Before" {
		t.Errorf("got %q, want just \"Before\" — nothing after an unterminated <script> is trustworthy content", got)
	}
}

// The output produced by StripHTML must actually chunk usefully: this is
// the point of inserting paragraph breaks at block boundaries at all.
func TestStripHTMLOutputIsChunkable(t *testing.T) {
	html := strings.Repeat("<p>"+strings.Repeat("word ", 30)+"</p>", 10)
	text := StripHTML(html)
	spans := Chunk(text)
	if len(spans) == 0 {
		t.Fatal("Chunk produced no spans from stripped HTML")
	}
}
