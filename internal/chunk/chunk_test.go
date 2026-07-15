package chunk

import (
	"strings"
	"testing"

	"github.com/enqack/cognosis/internal/vault"
)

func note(t *testing.T, path, content string) *vault.Note {
	t.Helper()
	n, err := vault.ParseNote(path, []byte(content))
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestReflectionEmbedsDescriptionOnly(t *testing.T) {
	n := note(t, "reflections/2026-07-12-x.md", `---
description: Implemented the write pipeline for the memory daemon.
persona: deep-thoughts
---
> A stylized comedic body about molten lava that must never be embedded.
`)
	chunks := Split(n)
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d, want 1", len(chunks))
	}
	if chunks[0].Content != "Implemented the write pipeline for the memory daemon." {
		t.Fatalf("content = %q", chunks[0].Content)
	}
	if strings.Contains(chunks[0].Content, "lava") {
		t.Fatal("styled body leaked into the embeddable chunk")
	}
	if chunks[0].HeadingPath != "" {
		t.Fatalf("heading path = %q, want empty", chunks[0].HeadingPath)
	}
}

func TestSplitAtH2WithPreamble(t *testing.T) {
	long := strings.Repeat("alpha bravo charlie delta echo foxtrot golf hotel. ", 8) // > mergeBelow
	body := "# Title\n\nintro " + long + "\n\n## First\n\ncontent one " + long +
		"\n\n## Second\n\ncontent two " + long + "\n"
	n := note(t, "entries/2026-07-12.md", "---\nid: x\n---\n"+body)
	chunks := Split(n)
	if len(chunks) != 3 {
		t.Fatalf("chunks = %d, want 3 (preamble + 2 sections): %+v", len(chunks), headings(chunks))
	}
	if chunks[0].HeadingPath != "Title" {
		t.Fatalf("preamble heading = %q", chunks[0].HeadingPath)
	}
	if chunks[1].HeadingPath != "Title > First" || chunks[2].HeadingPath != "Title > Second" {
		t.Fatalf("heading paths = %v", headings(chunks))
	}
	for i, c := range chunks {
		if c.Ordinal != i {
			t.Fatalf("ordinal %d = %d", i, c.Ordinal)
		}
	}
}

func headings(cs []Chunk) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.HeadingPath)
	}
	return out
}

// TestCodeFenceHeadingIgnored — '#' inside a fenced code block must not split.
func TestCodeFenceHeadingIgnored(t *testing.T) {
	pad := strings.Repeat("word ", 60)
	body := "## Real\n\n" + pad + "\n\n```\n## not a heading\n```\n\n" + pad + "\n"
	n := note(t, "entries/e.md", "---\nid: x\n---\n"+body)
	chunks := Split(n)
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d, want 1 — the fenced pseudo-heading split the note: %v", len(chunks), headings(chunks))
	}
}

func TestMergeSmallFoldsIntoPredecessor(t *testing.T) {
	big := strings.Repeat("x", 300)
	body := "## A\n\n" + big + "\n\n## Tiny\n\nshort\n"
	n := note(t, "entries/e.md", "---\nid: x\n---\n"+body)
	chunks := Split(n)
	if len(chunks) != 1 {
		t.Fatalf("chunks = %d, want 1 (tiny merged into A)", len(chunks))
	}
	if !strings.Contains(chunks[0].Content, "short") {
		t.Fatal("tiny section content lost in merge")
	}
}

func TestHardSplitAtParagraphs(t *testing.T) {
	para := strings.Repeat("p", 2500)
	body := "## Big\n\n" + para + "\n\n" + para + "\n\n" + para + "\n"
	n := note(t, "entries/e.md", "---\nid: x\n---\n"+body)
	chunks := Split(n)
	if len(chunks) < 2 {
		t.Fatalf("chunks = %d, want >= 2 (7.5k chars must split)", len(chunks))
	}
	for _, c := range chunks {
		if len(c.Content) > hardSplitOver {
			t.Fatalf("piece of %d chars exceeds the hard cap", len(c.Content))
		}
	}
	// Ordinals stay monotonic across the split pieces.
	for i, c := range chunks {
		if c.Ordinal != i {
			t.Fatalf("ordinal %d = %d", i, c.Ordinal)
		}
	}
}

func TestHashStableAndDistinct(t *testing.T) {
	n := note(t, "entries/e.md", "---\nid: x\n---\nsame body\n")
	a, b := Split(n), Split(n)
	if a[0].Hash != b[0].Hash {
		t.Fatal("hash not deterministic")
	}
	n2 := note(t, "entries/e.md", "---\nid: x\n---\ndifferent body\n")
	if Split(n2)[0].Hash == a[0].Hash {
		t.Fatal("distinct content hashed identically")
	}
}

func TestEmptyBody(t *testing.T) {
	n := note(t, "entries/e.md", "---\nid: x\n---\n\n")
	if got := Split(n); got != nil {
		t.Fatalf("empty body chunks = %v, want nil", got)
	}
}
