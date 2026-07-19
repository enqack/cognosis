package vault

import (
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/google/uuid"
)

// The Cognosis frontmatter contract, ported from silo-kb's validate
// package with the documented changes. One shared implementation for the walk,
// the write path, and reconciliation, so the contract cannot drift.

var categories = map[string]bool{
	"concept": true, "cursed-knowledge": true, "lesson-learned": true,
	"reflection": true, "entry": true,
}

// noteCategories are the categories legal under notes/ (processed, decaying).
var noteCategories = map[string]bool{
	"concept": true, "cursed-knowledge": true, "lesson-learned": true,
}

var maturities = map[string]bool{"seed": true, "developing": true, "stable": true}

// liveStatuses are truth-states of a still-asserted belief; falsified/faded/
// archived are retained-but-not-asserted.
var liveStatuses = map[string]bool{"active": true, "disputed": true, "paused": true}

const (
	StatusFalsified = "falsified"
	StatusFaded     = "faded"
	StatusArchived  = "archived"
)

// TimeLayout is the timestamp convention; bare dates accepted for date-only
// fields. Explicit timestamps replace silo-kb's git-derived staleness.
const TimeLayout = "2006-01-02 15:04:05"

// NewNoteID mints a note id: UUIDv7, time-ordered, so ids sort lexically by
// creation time and index inserts stay sequential rather than scattering a
// b-tree. Every code path that creates a note must use this — Validate rejects
// any other UUID version, and an id is permanent once written.
//
// Callers that cannot handle an error (fixtures, table-driven tests) should use
// a fixed v7 literal instead, which is more deterministic anyway.
func NewNoteID() (string, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("minting note id: %w", err)
	}
	return id.String(), nil
}

func ParseTime(s string) (time.Time, error) {
	if t, err := time.ParseInLocation(TimeLayout, s, time.Local); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("not YYYY-MM-DD HH:MM:SS or YYYY-MM-DD: %q", s)
}

// TimeOf normalizes a frontmatter timestamp: yaml.v3 decodes unquoted
// timestamps into time.Time, quoted ones stay strings — accept both.
func TimeOf(v any) (time.Time, error) {
	switch x := v.(type) {
	case time.Time:
		return x, nil
	case string:
		return ParseTime(x)
	}
	return time.Time{}, fmt.Errorf("not a timestamp: %v", v)
}

// Problem is one contract violation, naming the offending field so the write
// path can reject with a Validation error an agent can self-correct from.
type Problem struct {
	Path   string
	Field  string
	Reason string
}

func (p Problem) String() string {
	if p.Field == "" {
		return fmt.Sprintf("%s: %s", p.Path, p.Reason)
	}
	return fmt.Sprintf("%s: `%s`: %s", p.Path, p.Field, p.Reason)
}

// Validate checks the frontmatter contract for a vault-relative path.
func Validate(relPath string, fm map[string]any, hasFM bool) []Problem {
	relPath = path.Clean(strings.TrimPrefix(relPath, "./"))
	base := path.Base(relPath)
	p := func(field, reason string) Problem { return Problem{Path: relPath, Field: field, Reason: reason} }

	// Reserved generated files: root index.md carries exactly okf_version;
	// log.md, history.md (and any nested index.md) no frontmatter at all.
	if base == "index.md" || base == "log.md" || base == "history.md" {
		if relPath == "index.md" {
			if !hasFM || len(fm) != 1 || fm["okf_version"] == nil {
				return []Problem{p("okf_version", "root index.md must have frontmatter containing exactly this one field")}
			}
			return nil
		}
		if hasFM {
			return []Problem{p("", base+" is a reserved generated filename and must have no frontmatter")}
		}
		return nil
	}

	if !hasFM {
		return []Problem{p("", "missing YAML frontmatter; every note needs at least `category`, `created`, `updated` "+
			"(`id` is assigned when omitted). Frontmatter must open with `---` on the very first line")}
	}

	var probs []Problem

	// Note ids are UUIDv7 — see NewNoteID. v4 is rejected rather than merely
	// discouraged: an id is written once and never rewritten, so anything
	// accepted here is permanent.
	id, _ := fm["id"].(string)
	switch parsed, err := uuid.Parse(id); {
	case id == "":
		probs = append(probs, p("id", "required; generate a UUIDv7 and retry — never reuse another note's id"))
	case err != nil:
		probs = append(probs, p("id", fmt.Sprintf("not a valid UUID: %q", id)))
	case parsed.Version() != 7:
		probs = append(probs, p("id", fmt.Sprintf(
			"must be a UUIDv7 (time-ordered), got v%d — ids sort lexically by creation time",
			parsed.Version())))
	}

	cat, _ := fm["category"].(string)
	if cat == "" {
		probs = append(probs, p("category", "required; one of concept, cursed-knowledge, lesson-learned, reflection, entry"))
	} else if !categories[cat] {
		probs = append(probs, p("category", fmt.Sprintf("unknown category %q", cat)))
	}

	// Explicit timestamps are the staleness source — required everywhere.
	for _, f := range []string{"created", "updated"} {
		if v, present := fm[f]; !present {
			probs = append(probs, p(f, "required (YYYY-MM-DD HH:MM:SS)"))
		} else if _, err := TimeOf(v); err != nil {
			probs = append(probs, p(f, err.Error()))
		}
	}

	// summary is an optional one-line cache returned with retrieval hits;
	// when present it must be a plain non-empty string.
	if v, present := fm["summary"]; present {
		if s, ok := v.(string); !ok || strings.TrimSpace(s) == "" {
			probs = append(probs, p("summary", "when set, must be a non-empty one-line string"))
		}
	}

	stage, _ := StageOf(relPath)
	switch stage {
	case StageEntry:
		if cat != "" && cat != "entry" {
			probs = append(probs, p("category", fmt.Sprintf("entries/ holds raw capture and requires `category: entry`, got %q", cat)))
		}
	case StageNote:
		if cat != "" && !noteCategories[cat] {
			probs = append(probs, p("category", fmt.Sprintf("notes/ requires concept, cursed-knowledge, or lesson-learned, got %q", cat)))
		}
		probs = append(probs, validateDecaying(relPath, fm)...)
	case StageReflection:
		if cat != "" && cat != "reflection" {
			probs = append(probs, p("category", fmt.Sprintf("reflections/ requires `category: reflection`, got %q", cat)))
		}
		if persona, _ := fm["persona"].(string); strings.TrimSpace(persona) == "" {
			probs = append(probs, p("persona", "required on reflections: the persona that authored this note"))
		}
		// The comedic/stylized body is never indexed; the description is what
		// gets embedded, so it must exist and be literal.
		if d, _ := fm["description"].(string); strings.TrimSpace(d) == "" {
			probs = append(probs, p("description", "required: a dry, literal one-sentence summary of the event (this is what gets embedded; the styled body is not indexed)"))
		}
	case StageArchive:
		// Archived notes keep whatever fields they faded with — no stage rules.
	}

	// persona is reflection-only.
	if _, present := fm["persona"]; present && stage != StageReflection && stage != StageArchive {
		probs = append(probs, p("persona", "only reflections carry a persona tag"))
	}

	return probs
}

// validateDecaying enforces the decay-field rules on notes/* (working theory),
// ported from silo-kb's validateKnowledge.
func validateDecaying(relPath string, fm map[string]any) []Problem {
	var probs []Problem
	p := func(field, reason string) Problem { return Problem{Path: relPath, Field: field, Reason: reason} }

	conf, ok := toFloat(fm["confidence"])
	if !ok {
		probs = append(probs, p("confidence", "notes/* require numeric `confidence` (0.0–1.0)"))
	} else if conf < 0 || conf > 1 {
		probs = append(probs, p("confidence", fmt.Sprintf("must be within 0.0–1.0, got %v", conf)))
	}

	if m, _ := fm["maturity"].(string); !maturities[m] {
		probs = append(probs, p("maturity", "notes/* require one of: seed, developing, stable"))
	}

	if lr, present := fm["last_reinforced"]; !present {
		probs = append(probs, p("last_reinforced", "notes/* require this (YYYY-MM-DD HH:MM:SS)"))
	} else if _, err := TimeOf(lr); err != nil {
		probs = append(probs, p("last_reinforced", err.Error()))
	}

	if _, ok := toInt(fm["reinforce_count"]); !ok {
		probs = append(probs, p("reinforce_count", "notes/* require an integer (0 for new notes)"))
	}

	if srcs, ok := fm["sources"].([]any); !ok || len(srcs) == 0 {
		probs = append(probs, p("sources", "notes/* require non-empty provenance (wikilinks to entries/reflections)"))
	}

	// graduated_at marks in-place canon: the vault layout has no canon folder,
	// so "asserted, no longer decaying" is a frontmatter fact. Optional; when
	// present it must be a timestamp, and the lifecycle skips decay for it.
	if ga, present := fm["graduated_at"]; present {
		if _, err := TimeOf(ga); err != nil {
			probs = append(probs, p("graduated_at", err.Error()))
		}
	}

	// last_explicit_reinforce anchors the passive-refresh budget: citation can
	// only extend a note's life so far past the last time an agent actually
	// asserted it. Optional, and it must stay optional — every note written
	// before this field existed lacks it, there is no frontmatter backfill,
	// and reconciliation refuses to index a note that fails validation, so
	// requiring it would strand every existing vault.
	if lr, present := fm["last_explicit_reinforce"]; present {
		if _, err := TimeOf(lr); err != nil {
			probs = append(probs, p("last_explicit_reinforce", err.Error()))
		}
	}

	if s, present := fm["status"]; present {
		str, _ := s.(string)
		switch {
		case str == StatusFalsified:
			if r, _ := fm["falsified_reason"].(string); strings.TrimSpace(r) == "" {
				probs = append(probs, p("falsified_reason", "`status: falsified` requires a non-empty reason"))
			}
			if fa, present := fm["falsified_at"]; !present {
				probs = append(probs, p("falsified_at", "`status: falsified` requires this (YYYY-MM-DD HH:MM:SS)"))
			} else if _, err := TimeOf(fa); err != nil {
				probs = append(probs, p("falsified_at", err.Error()))
			}
			if sb, present := fm["superseded_by"]; present {
				if str, _ := sb.(string); strings.TrimSpace(str) == "" {
					probs = append(probs, p("superseded_by", "if set, must be a non-empty wikilink"))
				}
			}
		case str == "disputed":
			if da, present := fm["disputed_at"]; present {
				if _, err := TimeOf(da); err != nil {
					probs = append(probs, p("disputed_at", err.Error()))
				}
			}
		case str == StatusFaded || str == StatusArchived:
			// Legal terminal states; compile moves faded notes to archive/ but
			// the status may exist transiently in place. archived_at (stamped by
			// the compile pass) is optional but, when present, must be a
			// timestamp — it drives the as_of temporal view of archived notes.
			if aa, present := fm["archived_at"]; present {
				if _, err := TimeOf(aa); err != nil {
					probs = append(probs, p("archived_at", err.Error()))
				}
			}
		case !liveStatuses[str]:
			probs = append(probs, p("status", "if set, must be one of: active, disputed, paused, faded, archived, falsified"))
		}
	}
	return probs
}

func toFloat(v any) (float64, bool) {
	switch x := v.(type) {
	case float64:
		return x, true
	case int:
		return float64(x), true
	}
	return 0, false
}

func toInt(v any) (int, bool) {
	switch x := v.(type) {
	case int:
		return x, true
	case float64:
		if x == float64(int(x)) {
			return int(x), true
		}
	}
	return 0, false
}
