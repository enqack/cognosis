// Package persona is the two-tier persona subsystem: personas live as
// self-contained markdown files (voice guide + structure + checklist, with
// their metadata in frontmatter), enablement lives in config, and discovery
// is split so an agent pays for lightweight metadata when deciding *whether*
// a persona fits and fetches full content only for the one it picked.
package persona

import (
	"context"
	_ "embed"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/enqack/cognosis/internal/cogerr"
	"github.com/enqack/cognosis/internal/vault"
	"github.com/enqack/cognosis/internal/write"
)

// Meta is the tier-1 discovery payload: a few dozen tokens per persona,
// sourced from the persona file's own frontmatter so it can't drift from the
// file.
type Meta struct {
	ID          string
	Name        string
	Description string
	RespondsTo  []string // chain hints this persona is designed to follow up on
}

// Persona is the tier-2 payload: metadata plus the full voice guide body.
type Persona struct {
	Meta
	Body string
	Bias map[string]float64 // category -> retrieval weighting (persona_filter)
}

// Registry resolves enabled personas against their files.
type Registry struct {
	Dir     string   // personas directory ($XDG_DATA_HOME/cognosis/personas)
	Enabled []string // enabled persona ids, from config
	Log     *slog.Logger
}

//go:embed deep-thoughts.md
var deepThoughts []byte

// Seed writes the bundled default persona on first start if the personas
// directory doesn't exist yet. Adding personas later is just adding files.
func Seed(dir string) error {
	const op = "persona.Seed"
	if _, err := os.Stat(dir); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "deep-thoughts.md"), deepThoughts, 0o644); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	return nil
}

func (r *Registry) enabled(id string) bool {
	for _, e := range r.Enabled {
		if e == id {
			return true
		}
	}
	return false
}

// List returns tier-1 metadata for every enabled persona. A missing or
// unparseable file is logged and skipped — a broken persona shouldn't take
// discovery down with it.
func (r *Registry) List() []Meta {
	var out []Meta
	for _, id := range r.Enabled {
		p, err := r.load(id)
		if err != nil {
			if r.Log != nil {
				r.Log.Warn("persona skipped", "id", id, "reason", err)
			}
			continue
		}
		out = append(out, p.Meta)
	}
	return out
}

// Get returns the full persona — tier 2, called once the agent has decided
// this persona fits the moment. Disabled personas are NotFound even when
// their file exists: disabled means not available to invoke.
func (r *Registry) Get(id string) (Persona, error) {
	const op = "persona.Get"
	if !r.enabled(id) {
		return Persona{}, cogerr.Ef(op, cogerr.NotFound, "persona %q is not enabled", id)
	}
	return r.load(id)
}

func (r *Registry) load(id string) (Persona, error) {
	const op = "persona.load"
	content, err := os.ReadFile(filepath.Join(r.Dir, id+".md"))
	if err != nil {
		if os.IsNotExist(err) {
			return Persona{}, cogerr.Ef(op, cogerr.NotFound, "no persona file for %q", id)
		}
		return Persona{}, cogerr.E(op, cogerr.Internal, err)
	}
	n, err := vault.ParseNote("personas/"+id+".md", content)
	if err != nil {
		return Persona{}, err
	}
	if n.Frontmatter == nil {
		return Persona{}, cogerr.Ef(op, cogerr.Validation, "persona %q has no frontmatter", id)
	}
	p := Persona{Body: n.Body}
	p.ID, _ = n.Frontmatter["id"].(string)
	p.Name, _ = n.Frontmatter["name"].(string)
	p.Description, _ = n.Frontmatter["description"].(string)
	if p.ID != id {
		return Persona{}, cogerr.Ef(op, cogerr.Validation,
			"persona file %s.md declares id %q; file name and id must match", id, p.ID)
	}
	if strings.TrimSpace(p.Description) == "" {
		return Persona{}, cogerr.Ef(op, cogerr.Validation, "persona %q needs a one-sentence description", id)
	}
	if rt, ok := n.Frontmatter["responds_to"].([]any); ok {
		for _, v := range rt {
			if s, ok := v.(string); ok {
				p.RespondsTo = append(p.RespondsTo, s)
			}
		}
	}
	if bm, ok := n.Frontmatter["bias"].(map[string]any); ok {
		p.Bias = map[string]float64{}
		for k, v := range bm {
			switch x := v.(type) {
			case float64:
				p.Bias[k] = x
			case int:
				p.Bias[k] = float64(x)
			}
		}
	}
	return p, nil
}

// WriteReflection lands a persona-authored note in reflections/ through the
// sanctioned write pipeline. The persona must be enabled; the description is
// what gets embedded (the styled body never is), so it's required here.
func (r *Registry) WriteReflection(ctx context.Context, pipeline *write.Pipeline, persona, description, body, project, summary string) (string, error) {
	const op = "persona.WriteReflection"
	if !r.enabled(persona) {
		return "", cogerr.Ef(op, cogerr.Validation, "persona %q is not enabled", persona)
	}
	if strings.TrimSpace(description) == "" {
		return "", cogerr.Ef(op, cogerr.Validation, "description is required: a dry, literal one-sentence summary (this is what gets embedded)")
	}
	now := time.Now()
	rel := fmt.Sprintf("reflections/%s-%s.md", now.Format("2006-01-02-15-04"), slugify(description))

	fm := fmt.Sprintf("---\nid: %s\ncategory: reflection\npersona: %s\ndescription: %s\ncreated: %q\nupdated: %q\n",
		uuid.NewString(), persona, yamlEscape(description),
		now.Format(vault.TimeLayout), now.Format(vault.TimeLayout))
	if project != "" {
		fm += "project: " + project + "\n"
	}
	if s := strings.TrimSpace(summary); s != "" {
		fm += "summary: " + yamlEscape(s) + "\n"
	}
	content := fm + "---\n" + strings.TrimRight(body, "\n") + "\n"

	if err := pipeline.Write(ctx, rel, content, project); err != nil {
		return "", err
	}
	return rel, nil
}

// slugify lowercases, hyphenates, and caps a description-derived slug.
func slugify(s string) string {
	var b strings.Builder
	lastHyphen := true
	for _, r := range strings.ToLower(s) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastHyphen = false
		case !lastHyphen:
			b.WriteByte('-')
			lastHyphen = true
		}
		if b.Len() >= 48 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

func yamlEscape(s string) string {
	if strings.ContainsAny(s, ":#{}[]&*!|>'\"%@`") {
		return fmt.Sprintf("%q", s)
	}
	return s
}
