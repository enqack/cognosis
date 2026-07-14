package vault

import (
	"regexp"
	"strings"
)

// Link extraction, ported from silo-kb's links package. Markdown-only parsing;
// resolving a basename to a note id (including the cross-project
// `project:note-id` form) and persistence are the caller's job.

// LinkKind is the relationship a link records; values match links.kind.
type LinkKind string

const (
	Wikilink LinkKind = "wikilink" // a [[...]] reference in the note body
	Source   LinkKind = "source"   // a `sources` frontmatter entry (provenance)
)

// Ref is one outbound link: the target's wikilink basename, an optional
// project qualifier for cross-project disambiguation ([[project:basename]]),
// and how it was declared.
type Ref struct {
	Name    string
	Project string // "" for unqualified links
	Kind    LinkKind
}

var wikiRe = regexp.MustCompile(`\[\[([^\]\[]+)\]\]`)

// Targets returns a note's outbound links, de-duplicated by (name, kind):
// `sources` entries first, then body wikilinks. Empty names are skipped.
func Targets(n *Note) []Ref {
	var out []Ref
	seen := map[string]bool{}
	add := func(raw string, k LinkKind) {
		name := cleanName(raw)
		if name == "" {
			return
		}
		// Cross-project qualifier: [[project:basename]]. A single colon with
		// non-empty halves splits; anything else stays a plain basename.
		project := ""
		if before, after, ok := strings.Cut(name, ":"); ok &&
			before != "" && after != "" && !strings.Contains(after, ":") {
			project, name = before, after
		}
		key := string(k) + "\x00" + project + "\x00" + name
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, Ref{Name: name, Project: project, Kind: k})
	}

	if srcs, ok := n.Frontmatter["sources"].([]any); ok {
		for _, s := range srcs {
			if str, ok := s.(string); ok {
				add(str, Source)
			}
		}
	}
	for _, m := range wikiRe.FindAllStringSubmatch(n.Body, -1) {
		add(m[1], Wikilink)
	}
	return out
}

// Sources returns just the provenance basenames a note declares.
func Sources(n *Note) []string {
	var out []string
	seen := map[string]bool{}
	if srcs, ok := n.Frontmatter["sources"].([]any); ok {
		for _, s := range srcs {
			str, ok := s.(string)
			if !ok {
				continue
			}
			name := cleanName(str)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// cleanName normalizes a wikilink target to a bare basename: strips [[ ]]
// delimiters, an optional |alias or #anchor, and surrounding whitespace.
func cleanName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "[[")
	s = strings.TrimSuffix(s, "]]")
	if i := strings.IndexAny(s, "|#"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
