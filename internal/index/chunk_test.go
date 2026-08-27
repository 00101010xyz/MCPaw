package index

import (
	"strings"
	"testing"
)

func TestChunkEmpty(t *testing.T) {
	if got := Chunk(""); got != nil {
		t.Fatalf("Chunk(\"\") = %v, want nil", got)
	}
}

func TestChunkShortTextIsOneSpan(t *testing.T) {
	spans := Chunk("a short paragraph that fits in one chunk")
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1", len(spans))
	}
	if spans[0].Text != "a short paragraph that fits in one chunk" {
		t.Errorf("unexpected text: %q", spans[0].Text)
	}
}

func TestChunkCoversWholeText(t *testing.T) {
	// Every rune of the input must fall inside at least one span, so nothing
	// silently disappears from the index.
	para := strings.Repeat("word ", 40) + "\n\n"
	text := strings.Repeat(para, 20)
	spans := ChunkSize(text, 200, 40)
	if len(spans) < 2 {
		t.Fatalf("expected multiple spans for %d runes, got %d", len([]rune(text)), len(spans))
	}
	runes := []rune(text)
	for _, sp := range spans {
		if sp.Start < 0 || sp.End > len(runes) || sp.Start >= sp.End {
			t.Fatalf("span %+v out of bounds for %d runes", sp, len(runes))
		}
	}
	if last := spans[len(spans)-1]; last.End != len(runes) {
		t.Errorf("last span ends at %d, want %d (end of text)", last.End, len(runes))
	}
}

func TestChunkOverlaps(t *testing.T) {
	text := strings.Repeat("word ", 200)
	spans := ChunkSize(text, 300, 50)
	for i := 1; i < len(spans); i++ {
		if spans[i].Start >= spans[i-1].End {
			t.Errorf("span %d starts at %d, which is not before the previous span's end %d",
				i, spans[i].Start, spans[i-1].End)
		}
	}
}

func TestChunkHandlesMultibyteRunes(t *testing.T) {
	// A regression check for the byte/rune offset bug: café, naïve, Müller
	// and similar accented text must not panic or corrupt offsets.
	text := strings.Repeat("Café Müller étude naïve résumé. ", 100)
	spans := ChunkSize(text, 120, 20)
	if len(spans) < 2 {
		t.Fatalf("expected multiple spans, got %d", len(spans))
	}
	runes := []rune(text)
	for _, sp := range spans {
		if string(runes[sp.Start:sp.End]) != sp.Text && strings.TrimSpace(string(runes[sp.Start:sp.End])) != sp.Text {
			t.Errorf("span text does not match runes[%d:%d]: got %q, span.Text %q",
				sp.Start, sp.End, string(runes[sp.Start:sp.End]), sp.Text)
		}
	}
}

func TestChunkNoOverlapWhenLargerThanSize(t *testing.T) {
	spans := ChunkSize(strings.Repeat("x", 100), 10, 20) // invalid overlap >= size
	if len(spans) == 0 {
		t.Fatal("expected at least one span")
	}
}
