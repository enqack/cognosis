// Package chunk splits notes into embeddable pieces. Mechanics ported from
// silo-kb with one stage adaptation: reflections embed only their frontmatter
// description -- a stylized comedic body would embed literally and scatter the
// note across vector space, while the dry description clusters it near the
// events it records.
//
// Chunk identity is positional (Ordinal) so duplicate headings can't corrupt
// delta diffs; per-chunk content hashes make delta re-embeds cheap for
// append-only entry logs.
package chunk

import (
	"encoding/hex"
	"strings"

	"github.com/zeebo/blake3"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/text"

	"github.com/enqack/cognosis/internal/vault"
)

const (
	// mergeBelow folds undersized sections into their predecessor.
	mergeBelow = 200
	// hardSplitOver splits oversized sections at paragraph boundaries.
	hardSplitOver = 6000
)

// Chunk is one embeddable piece of a note.
type Chunk struct {
	Ordinal     int
	HeadingPath string // "" for single-chunk notes
	Content     string
	Hash        string
}

// Split chunks a parsed note by its stage.
func Split(n *vault.Note) []Chunk {
	var raw []section
	if n.Stage == vault.StageReflection {
		// Embed the description, never the styled body.
		desc, _ := n.Frontmatter["description"].(string)
		desc = strings.TrimSpace(desc)
		if desc == "" {
			desc = strings.TrimSpace(n.Body) // validation normally forbids this
		}
		if desc == "" {
			return nil
		}
		raw = []section{{content: desc}}
	} else {
		raw = splitAtH2(n.Body)
	}

	merged := mergeSmall(raw)
	var out []Chunk
	for _, s := range merged {
		for _, piece := range hardSplit(s.content) {
			out = append(out, Chunk{
				Ordinal:     len(out),
				HeadingPath: s.headingPath,
				Content:     piece,
				Hash:        hash(piece),
			})
		}
	}
	return out
}

type section struct {
	headingPath string
	content     string
}

var md = goldmark.New()

// headingText reconstructs a heading's text from its source line segments --
// the replacement for the deprecated ast.Node.Text, which the library now
// steers callers away from in favor of the line segments.
func headingText(h *ast.Heading, src []byte) string {
	var b strings.Builder
	lines := h.Lines()
	for i := 0; i < lines.Len(); i++ {
		seg := lines.At(i)
		b.Write(seg.Value(src))
	}
	return strings.TrimSpace(b.String())
}

// splitAtH2 sections a markdown body at its level-2 headings, using the
// goldmark AST so '#' inside fenced code blocks or setext headings can't
// fool it. The preamble before the first h2 (h1 title + intro) becomes its
// own section.
func splitAtH2(body string) []section {
	src := []byte(body)
	doc := md.Parser().Parse(text.NewReader(src))

	var h1Title string
	type h2 struct {
		offset int
		title  string
	}
	var h2s []h2

	for c := doc.FirstChild(); c != nil; c = c.NextSibling() {
		h, ok := c.(*ast.Heading)
		if !ok {
			continue
		}
		switch h.Level {
		case 1:
			if h1Title == "" {
				h1Title = headingText(h, src)
			}
		case 2:
			lines := h.Lines()
			if lines.Len() == 0 {
				continue
			}
			start := lines.At(0).Start
			// Back up to the start of the heading line (the AST segment
			// begins after the '## ' marker).
			for start > 0 && src[start-1] != '\n' {
				start--
			}
			h2s = append(h2s, h2{offset: start, title: headingText(h, src)})
		}
	}

	if len(h2s) == 0 {
		content := strings.TrimSpace(body)
		if content == "" {
			return nil
		}
		return []section{{headingPath: h1Title, content: content}}
	}

	var out []section
	if pre := strings.TrimSpace(body[:h2s[0].offset]); pre != "" {
		out = append(out, section{headingPath: h1Title, content: pre})
	}
	for i, h := range h2s {
		end := len(body)
		if i+1 < len(h2s) {
			end = h2s[i+1].offset
		}
		content := strings.TrimSpace(body[h.offset:end])
		if content == "" {
			continue
		}
		out = append(out, section{headingPath: joinPath(h1Title, h.title), content: content})
	}
	return out
}

func joinPath(h1, h2 string) string {
	if h1 == "" {
		return h2
	}
	return h1 + " > " + h2
}

// mergeSmall folds sections shorter than mergeBelow into their predecessor.
// A lone undersized first section has nothing to merge into and is kept.
func mergeSmall(in []section) []section {
	var out []section
	for _, s := range in {
		if len(out) > 0 && len(s.content) < mergeBelow {
			out[len(out)-1].content += "\n\n" + s.content
			continue
		}
		out = append(out, s)
	}
	return out
}

// hardSplit breaks oversized content at paragraph boundaries, greedily
// packing paragraphs so each piece stays at or under hardSplitOver.
func hardSplit(content string) []string {
	if len(content) <= hardSplitOver {
		return []string{content}
	}
	paras := strings.Split(content, "\n\n")
	var out []string
	var cur strings.Builder
	for _, p := range paras {
		if cur.Len() > 0 && cur.Len()+len(p)+2 > hardSplitOver {
			out = append(out, cur.String())
			cur.Reset()
		}
		if cur.Len() > 0 {
			cur.WriteString("\n\n")
		}
		cur.WriteString(p)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}

// hash is the chunk content digest. BLAKE3, matching every other content hash
// in the project (write.FileMeta.Blake3, the watcher's drift detection,
// vault.Note.SrcBlake3) so there is one answer to "did this content change"
// rather than two.
//
// Changing it invalidated every stored chunk hash, and that does **not** heal
// on restart. Reconciliation decides what to re-index by comparing the
// file-level digest, which this change did not touch, so no note is re-read and
// existing rows keep their sha256-era values until each note is next edited.
//
// Harmless, and the reason is specific: nothing reads this column. It is
// written at three sites and never compared, held against a delta re-embedding
// feature that does not exist yet. Taken as a breaking change before 1.0 rather
// than carrying a second algorithm forward behind a migration; a vault wanting
// uniform values now must be rebuilt rather than restarted.
func hash(s string) string {
	sum := blake3.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
