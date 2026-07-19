package vault

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

func TestSplitFrontmatter(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		fm, body string
		hasFM    bool
	}{
		{"normal", "---\nid: x\n---\nbody\n", "id: x", "body\n", true},
		{"no frontmatter", "just body\n", "", "just body\n", false},
		{"unterminated", "---\nid: x\nbody\n", "", "---\nid: x\nbody\n", false},
		{"empty body", "---\nid: x\n---", "id: x", "", true},
		{"leading blank line", "\n---\nid: x\n---\nbody", "", "\n---\nid: x\n---\nbody", false},
	}
	for _, c := range cases {
		fm, body, has := SplitFrontmatter([]byte(c.in))
		if fm != c.fm || body != c.body || has != c.hasFM {
			t.Errorf("%s: got (%q,%q,%v), want (%q,%q,%v)", c.name, fm, body, has, c.fm, c.body, c.hasFM)
		}
	}
}

func TestStageOf(t *testing.T) {
	cases := []struct {
		path  string
		stage Stage
		ok    bool
	}{
		{"entries/2026-07-12.md", StageEntry, true},
		{"notes/rrf-fusion.md", StageNote, true},
		{"reflections/2026-07-12-first.md", StageReflection, true},
		{"archive/old-theory.md", StageArchive, true},
		{"index.md", "", false},
		{"log.md", "", false},
		{"random/other.md", "", false},
	}
	for _, c := range cases {
		stage, ok := StageOf(c.path)
		if stage != c.stage || ok != c.ok {
			t.Errorf("StageOf(%q) = (%q,%v), want (%q,%v)", c.path, stage, ok, c.stage, c.ok)
		}
	}
}

// TestRoundTripIdentity is the round-trip property: parse -> serialize -> parse
// yields the same frontmatter and body, including preserved comments.
func TestRoundTripIdentity(t *testing.T) {
	src := `---
# a comment that must survive
id: 01920000-0000-7000-8000-00000000000a
category: concept
created: "2026-07-12 10:00:00"
updated: "2026-07-12 10:00:00"
---
# Heading

Body with a [[wikilink]] and trailing newline.
`
	n1, err := ParseNote("notes/x.md", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	out, err := n1.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	n2, err := ParseNote("notes/x.md", out)
	if err != nil {
		t.Fatalf("re-parse of serialized output failed: %v\n---\n%s", err, out)
	}
	if !reflect.DeepEqual(n1.Frontmatter, n2.Frontmatter) {
		t.Fatalf("frontmatter drifted:\n%v\nvs\n%v", n1.Frontmatter, n2.Frontmatter)
	}
	if n1.Body != n2.Body {
		t.Fatalf("body drifted: %q vs %q", n1.Body, n2.Body)
	}
	if string(out) != src {
		t.Logf("note: byte-level output differs (allowed; semantic identity is the contract)")
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "notes", "a.md")
	if err := WriteFileAtomic(p, []byte("one")); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(p, []byte("two")); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "two" {
		t.Fatalf("content = %q, err %v", b, err)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "notes"))
	if len(entries) != 1 {
		t.Fatalf("temp files leaked: %v", entries)
	}
}

func writeVault(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func validNote(id string) string {
	return `---
id: ` + id + `
category: concept
created: "2026-07-12 10:00:00"
updated: "2026-07-12 10:00:00"
confidence: 0.5
maturity: seed
last_reinforced: "2026-07-12 10:00:00"
reinforce_count: 0
sources:
  - "[[2026-07-12]]"
---
Body.
`
}

func TestWalkAcceptsValidVault(t *testing.T) {
	root := writeVault(t, map[string]string{
		"notes/a.md": validNote(uuid.Must(uuid.NewV7()).String()),
		"entries/2026-07-12.md": `---
id: ` + uuid.Must(uuid.NewV7()).String() + `
category: entry
created: "2026-07-12 09:00:00"
updated: "2026-07-12 09:00:00"
---
Raw capture.
`,
		"log.md":     "append-only log, no frontmatter\n",
		"index.md":   "---\nokf_version: 1\n---\ngenerated\n",
		"stray.txt":  "not markdown",
		"other/x.md": "outside the stage folders -- skipped\n",
	})
	notes, err := Walk(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 2 {
		t.Fatalf("notes = %d, want 2", len(notes))
	}
}

func TestWalkRejectsDuplicateIDs(t *testing.T) {
	id := uuid.Must(uuid.NewV7()).String()
	root := writeVault(t, map[string]string{
		"notes/a.md": validNote(id),
		"notes/b.md": validNote(id),
	})
	if _, err := Walk(root); err == nil {
		t.Fatal("duplicate ids must fail the walk")
	}
}

func TestWalkRejectsReservedFrontmatter(t *testing.T) {
	root := writeVault(t, map[string]string{
		"log.md": "---\nid: nope\n---\ncontent\n",
	})
	if _, err := Walk(root); err == nil {
		t.Fatal("log.md with frontmatter must fail")
	}
}
