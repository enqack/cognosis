// Package vault owns the markdown source of truth: frontmatter
// parse/validate/serialize, the stage-folder layout, and atomic file writes.
// Mechanics ported from silo-kb's vault/validate packages with the documented
// changes: folders encode processing stage (entries/notes/reflections/archive)
// while semantic category moves to frontmatter, and explicit created/updated
// timestamps replace git-derived staleness.
package vault

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/enqack/cognosis/internal/cogerr"
)

// Stage is the processing-stage folder a note lives in.
type Stage string

const (
	StageEntry      Stage = "entries"     // raw, timestamped capture
	StageNote       Stage = "notes"       // atomic processed notes (decaying)
	StageReflection Stage = "reflections" // persona-authored freeform writing
	StageArchive    Stage = "archive"     // retired notes
)

// Note is one parsed markdown file. Path is vault-relative.
type Note struct {
	Path        string
	Stage       Stage
	Frontmatter map[string]any
	FMNode      *yaml.Node // lossless round-trip re-serialization
	Body        string
}

func (n *Note) str(key string) string {
	s, _ := n.Frontmatter[key].(string)
	return s
}

func (n *Note) ID() string       { return n.str("id") }
func (n *Note) Category() string { return n.str("category") }
func (n *Note) Project() string  { return n.str("project") }
func (n *Note) Status() string {
	if s := n.str("status"); s != "" {
		return s
	}
	return "active"
}

// StageOf classifies a vault-relative path by its first segment. ok is false
// outside the four stage folders (e.g. the root index.md).
func StageOf(relPath string) (Stage, bool) {
	first := strings.SplitN(filepath.ToSlash(relPath), "/", 2)[0]
	switch Stage(first) {
	case StageEntry, StageNote, StageReflection, StageArchive:
		return Stage(first), true
	}
	return "", false
}

// SplitFrontmatter separates a leading YAML frontmatter block from the body.
func SplitFrontmatter(content []byte) (fm string, body string, hasFM bool) {
	s := string(content)
	if !strings.HasPrefix(s, "---\n") {
		return "", s, false
	}
	rest := s[4:]
	end := strings.Index(rest, "\n---\n")
	if end < 0 {
		if strings.HasSuffix(rest, "\n---") {
			return rest[:len(rest)-4], "", true
		}
		return "", s, false
	}
	return rest[:end], rest[end+5:], true
}

// ParseNote parses one file's content (no contract validation — see Validate).
func ParseNote(relPath string, content []byte) (*Note, error) {
	const op = "vault.ParseNote"
	fm, body, hasFM := SplitFrontmatter(content)
	stage, _ := StageOf(relPath)
	n := &Note{Path: filepath.ToSlash(relPath), Stage: stage, Body: body}
	if hasFM {
		var node yaml.Node
		if err := yaml.Unmarshal([]byte(fm), &node); err != nil {
			return nil, cogerr.Ef(op, cogerr.Validation, "%s: invalid YAML frontmatter: %v", relPath, err)
		}
		var m map[string]any
		if err := yaml.Unmarshal([]byte(fm), &m); err != nil {
			return nil, cogerr.Ef(op, cogerr.Validation, "%s: invalid YAML frontmatter: %v", relPath, err)
		}
		n.FMNode = &node
		n.Frontmatter = m
	}
	return n, nil
}

// Serialize renders the note back to file bytes. Frontmatter goes through the
// retained yaml.Node so comments/ordering survive a round trip.
func (n *Note) Serialize() ([]byte, error) {
	const op = "vault.Serialize"
	if n.Frontmatter == nil {
		return []byte(n.Body), nil
	}
	var fmBytes []byte
	var err error
	if n.FMNode != nil {
		fmBytes, err = yaml.Marshal(n.FMNode)
	} else {
		fmBytes, err = yaml.Marshal(n.Frontmatter)
	}
	if err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	var b strings.Builder
	b.WriteString("---\n")
	b.Write(fmBytes)
	b.WriteString("---\n")
	b.WriteString(n.Body)
	return []byte(b.String()), nil
}

// WriteFileAtomic writes via temp-file-then-rename in the target's directory,
// so a reader (or the watcher) never sees a half-written note.
func WriteFileAtomic(path string, content []byte) error {
	const op = "vault.WriteFileAtomic"
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	tmp, err := os.CreateTemp(dir, ".cognosis-write-*")
	if err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return cogerr.E(op, cogerr.Internal, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return cogerr.E(op, cogerr.Internal, err)
	}
	if err := tmp.Close(); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	return nil
}

// IsReserved reports whether the basename is one of the generated files.
func IsReserved(relPath string) bool {
	base := filepath.Base(relPath)
	return base == "index.md" || base == "log.md" || base == "history.md"
}

// Walk parses and validates every note under root. Reserved files are checked
// against their own rules but excluded from the result; validation problems
// across all files aggregate into one Validation error; duplicate ids are
// rejected vault-wide (two files must not fight over one index row).
func Walk(root string) ([]*Note, error) {
	const op = "vault.Walk"
	var notes []*Note
	var problems []string

	// os.Root confines every read inside this walk to root, closing the
	// symlink-swap TOCTOU window a plain filepath.WalkDir + os.ReadFile
	// pair leaves open between the directory scan and the file open.
	r, err := os.OpenRoot(root)
	if err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	defer func() { _ = r.Close() }()

	err = fs.WalkDir(r.FS(), ".", func(rel string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(rel, ".md") {
			return nil
		}
		rel = filepath.ToSlash(rel)

		f, err := r.Open(rel)
		if err != nil {
			return err
		}
		content, err := io.ReadAll(f)
		_ = f.Close()
		if err != nil {
			return err
		}
		n, perr := ParseNote(rel, content)
		if perr != nil {
			problems = append(problems, perr.Error())
			return nil
		}
		if IsReserved(rel) {
			for _, p := range Validate(rel, n.Frontmatter, n.Frontmatter != nil) {
				problems = append(problems, p.String())
			}
			return nil
		}
		if _, ok := StageOf(rel); !ok {
			return nil // outside the four stage folders
		}
		if probs := Validate(rel, n.Frontmatter, n.Frontmatter != nil); len(probs) > 0 {
			for _, p := range probs {
				problems = append(problems, p.String())
			}
			return nil
		}
		notes = append(notes, n)
		return nil
	})
	if err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}

	byID := map[string]string{}
	for _, n := range notes {
		if prev, dup := byID[n.ID()]; dup {
			problems = append(problems, fmt.Sprintf(
				"%s: duplicate id %s (also in %s) — ids are assigned once and never reused; give one a fresh UUID",
				n.Path, n.ID(), prev))
		}
		byID[n.ID()] = n.Path
	}
	if len(problems) > 0 {
		return nil, cogerr.Ef(op, cogerr.Validation, "vault validation failed:\n  %s",
			strings.Join(problems, "\n  "))
	}
	return notes, nil
}
