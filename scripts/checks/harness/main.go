// Command harness drives Cognosis feature slices over real Streamable HTTP MCP
// and exits nonzero on any deviation. One shared MCP client backs every slice;
// invoke one slice at a time:
//
//	go run ./scripts/checks/harness <slice>
//
// where slice is memory-loop | retrieval | knowledge | platform | migration.
// Env: COGNOSIS_MCP_URL, COGNOSIS_TOKEN_FILE (or COGNOSIS_TOKEN); the platform
// slice needs COGNOSIS_DSN for its audit-table assertions and memory-loop needs
// it to read back the indexed note id across an edit. The check scripts under
// scripts/checks/ boot a daemon and call the matching slice.
//
// One mode is not an MCP slice:
//
//	go run ./scripts/checks/harness gen-cert <dir>
//
// writes the throwaway TLS trust chain the tls check needs, before any daemon
// exists to talk to (see gencert.go).
package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// version is the harness's advertised MCP client version. It stays "dev" for the
// usual `go run ./scripts/checks/harness` invocation and can be stamped at link
// time via -ldflags "-X main.version=<v>".
var version = "dev"

func main() {
	os.Exit(run())
}

// run returns 0 on success and 1 on any failure, including a usage error.
//
// It never returns 2, even though 2-means-usage is the usual CLI convention:
// in this subsystem 2 already means "skip" — scripts/checks/_lib.sh's
// require_env exits 2 for a missing prerequisite and check-all.sh reports that
// check as skipped and carries on. A harness usage error is a programmer
// mistake, the opposite of a skippable one, so it must never be able to wear
// that number. Today `go run` collapses any non-zero exit to 1 and every caller
// consumes the code with `|| fail`, which would hide the collision; both are
// accidents, not guarantees.
func run() int {
	// gen-cert takes a directory, so it is dispatched ahead of the slices —
	// they all share the one-arg, MCP-driving shape and it shares neither.
	if len(os.Args) > 1 && os.Args[1] == "gen-cert" {
		if len(os.Args) != 3 {
			fmt.Fprintln(os.Stderr, "usage: harness gen-cert <dir>")
			return 1
		}
		if err := genCert(os.Args[2]); err != nil {
			fmt.Fprintf(os.Stderr, "harness gen-cert: %v\n", err)
			return 1
		}
		fmt.Println("harness gen-cert: OK")
		return 0
	}
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: harness <memory-loop|retrieval|knowledge|platform|migration> | gen-cert <dir>")
		return 1
	}
	slice := os.Args[1]
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	slices := map[string]func(context.Context) error{
		"memory-loop": memoryLoop,
		"retrieval":   retrieval,
		"knowledge":   knowledge,
		"platform":    platform,
		"migration":   migration,
	}
	fn, ok := slices[slice]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown slice %q\n", slice)
		return 1
	}
	if err := fn(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "harness %s: %v\n", slice, err)
		return 1
	}
	fmt.Printf("harness %s: OK\n", slice)
	return 0
}

// --- memory-loop: write -> hybrid query -> get -> list, + contract + auth ----

func memoryLoop(ctx context.Context) error {
	endpoint := envOr("COGNOSIS_MCP_URL", "http://127.0.0.1:7433")
	// Auth is enforced even locally: a tokenless request must 401.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader("{}"))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if resp, err := http.DefaultClient.Do(req); err == nil {
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			return fmt.Errorf("tokenless request got %s, want 401", resp.Status)
		}
	}

	s, err := connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	marker := "zephyr-manifold-" + uuid.Must(uuid.NewV7()).String()[:8]
	path := "entries/memloop-" + marker + ".md"
	now := time.Now().Format("2006-01-02 15:04:05")
	content := fmt.Sprintf("---\nid: %s\ncategory: entry\ncreated: %q\nupdated: %q\n---\nThe reconciliation sweep interval is configurable; marker %s.\n",
		uuid.Must(uuid.NewV7()).String(), now, now, marker)

	if _, err := call(ctx, s, "write_note", map[string]any{"path": path, "content": content}); err != nil {
		return fmt.Errorf("write_note: %w", err)
	}
	res, err := call(ctx, s, "query_knowledge", map[string]any{"text": "configurable reconciliation sweep interval " + marker})
	if err != nil {
		return fmt.Errorf("query_knowledge: %w", err)
	}
	if !strings.Contains(res, path) {
		return fmt.Errorf("query did not surface the written note %s:\n%s", path, res)
	}
	if !strings.Contains(res, "score") {
		return fmt.Errorf("results are not score-ranked:\n%s", res)
	}
	got, err := call(ctx, s, "get_note", map[string]any{"path": path})
	if err != nil {
		return fmt.Errorf("get_note: %w", err)
	}
	if got != content {
		return fmt.Errorf("get_note round trip mismatch:\n--- wrote ---\n%s--- got ---\n%s", content, got)
	}
	listing, err := call(ctx, s, "list_notes", map[string]any{})
	if err != nil {
		return fmt.Errorf("list_notes: %w", err)
	}
	if !strings.Contains(listing, path) {
		return fmt.Errorf("list_notes missing %s", path)
	}
	// Contract enforcement: an invalid note is rejected with the field named.
	if _, err := call(ctx, s, "write_note", map[string]any{
		"path": "entries/invalid.md", "content": "---\nid: nope\n---\nbroken\n",
	}); err == nil {
		return fmt.Errorf("invalid frontmatter was accepted")
	} else if !strings.Contains(err.Error(), "id") {
		return fmt.Errorf("rejection does not name the field: %w", err)
	}
	return editNote(ctx, s)
}

// editNote is the edit_note leg of the memory loop: a surgical replacement is a
// full write, so it must land in the *index*, not merely in the file, and it
// must leave the note's identity alone. It lives here rather than in the
// knowledge slice because it is a write path into the vault, and this is the
// slice that owns "a write becomes retrievable".
//
// The two refusals are the design, not edge cases: a caller editing a file it
// cannot see has no way to know that "the sweep interval" appears twice, so a
// silent first-match replacement would corrupt a note while reporting success.
// The multi-match message must carry the count, because the count is what tells
// the caller how much more context to include.
func editNote(ctx context.Context, s *mcp.ClientSession) error {
	dsn := os.Getenv("COGNOSIS_DSN")
	if dsn == "" {
		return fmt.Errorf("COGNOSIS_DSN is required for the edit_note leg")
	}

	marker := "carbonyl-lattice-" + uuid.Must(uuid.NewV7()).String()[:8]
	path := "entries/editnote-" + marker + ".md"
	now := time.Now().Format("2006-01-02 15:04:05")
	// No id in the fixture: write_note mints one, and this leg's whole point is
	// that the minted id survives the edit. Supplying one here would be testing
	// the harness's uuid call instead.
	//
	// The duplicated sentence is deliberate — it is the multi-match fixture, and
	// it has to be in the same file as the unique one so that one write sets up
	// both the success and the ambiguity.
	content := fmt.Sprintf("---\ncategory: entry\ncreated: %q\nupdated: %q\n---\n"+
		"Field notes on the %s cadence experiment.\n"+
		"The reconciliation sweep interval is hourly.\n"+
		"A duplicated observation. A duplicated observation.\n",
		now, now, marker)
	if _, err := call(ctx, s, "write_note", map[string]any{"path": path, "content": content}); err != nil {
		return fmt.Errorf("edit_note fixture write: %w", err)
	}

	idBefore, err := indexedNoteID(ctx, dsn, path)
	if err != nil {
		return err
	}

	const (
		oldStr = "The reconciliation sweep interval is hourly."
		newStr = "The reconciliation sweep interval is every eleven minutes."
	)
	if _, err := call(ctx, s, "edit_note", map[string]any{
		"path": path, "old_string": oldStr, "new_string": newStr,
	}); err != nil {
		return fmt.Errorf("edit_note unique match: %w", err)
	}

	// The index, not the file. query_knowledge prints the stored chunk text, so
	// an edit that rewrote the markdown but skipped re-chunking and re-embedding
	// shows up here as the old sentence still being served. Asserting only that
	// the call returned ok, or only that get_note shows the new text, would miss
	// exactly that failure.
	res, err := call(ctx, s, "query_knowledge", map[string]any{"text": marker + " cadence experiment"})
	if err != nil {
		return fmt.Errorf("query after edit: %w", err)
	}
	if !strings.Contains(res, path) {
		return fmt.Errorf("edited note %s not retrievable:\n%s", path, res)
	}
	if !strings.Contains(res, "every eleven minutes") {
		return fmt.Errorf("index does not serve the edited text:\n%s", res)
	}
	if strings.Contains(res, "interval is hourly") {
		return fmt.Errorf("index still serves the pre-edit text:\n%s", res)
	}
	if got, err := call(ctx, s, "get_note", map[string]any{"path": path}); err != nil {
		return fmt.Errorf("get_note after edit: %w", err)
	} else if !strings.Contains(got, newStr) || strings.Contains(got, oldStr) {
		return fmt.Errorf("file does not reflect the edit:\n%s", got)
	}

	// Identity is preserved. A fresh id for the same path is an eviction: the
	// note row is replaced and its inbound links are re-pointed, which looks
	// like success from the tool's return value.
	idAfter, err := indexedNoteID(ctx, dsn, path)
	if err != nil {
		return err
	}
	if idAfter != idBefore {
		return fmt.Errorf("edit changed the indexed note id: %s -> %s", idBefore, idAfter)
	}

	// Refusal 1: no match at all.
	_, err = call(ctx, s, "edit_note", map[string]any{
		"path": path, "old_string": "text that is nowhere in this note", "new_string": "x",
	})
	if err == nil {
		return fmt.Errorf("edit_note accepted an old_string that matches nothing")
	}
	if !strings.Contains(err.Error(), "does not appear") {
		return fmt.Errorf("zero-match refusal does not explain itself: %w", err)
	}

	// Refusal 2: ambiguous match, and the message reports how many.
	_, err = call(ctx, s, "edit_note", map[string]any{
		"path": path, "old_string": "A duplicated observation.", "new_string": "A single observation.",
	})
	if err == nil {
		return fmt.Errorf("edit_note accepted an old_string that matches twice")
	}
	if !strings.Contains(err.Error(), "appears 2 times") {
		return fmt.Errorf("multi-match refusal does not report the count: %w", err)
	}

	// A refused edit must not have touched anything on its way to refusing.
	if got, err := call(ctx, s, "get_note", map[string]any{"path": path}); err != nil {
		return fmt.Errorf("get_note after refusals: %w", err)
	} else if !strings.Contains(got, "A duplicated observation. A duplicated observation.") {
		return fmt.Errorf("a refused edit mutated the note:\n%s", got)
	}
	return nil
}

// indexedNoteID reads the note id the index holds for a path, straight from the
// table — the id the tools return is the one in the markdown, and the eviction
// this guards against is a database-side identity change.
func indexedNoteID(ctx context.Context, dsn, path string) (string, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return "", fmt.Errorf("index db connect: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()
	var id string
	if err := conn.QueryRow(ctx, `select id::text from notes where path = $1`, path).Scan(&id); err != nil {
		return "", fmt.Errorf("read indexed id for %s: %w", path, err)
	}
	return id, nil
}

// --- retrieval: summaries, as_of, list_decaying, soft-delete exclusion -------

func retrieval(ctx context.Context) error {
	s, err := connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	marker := "quintet-" + uuid.Must(uuid.NewV7()).String()[:8]
	now := time.Now().Format("2006-01-02 15:04:05")

	// Summaries: cached at write, shown with hits.
	sumNote := fmt.Sprintf("---\nid: %s\ncategory: entry\nsummary: The %s capture proves cached summaries flow end to end.\ncreated: %q\nupdated: %q\n---\nLong-form body about the %s experiment and its many details.\n",
		uuid.Must(uuid.NewV7()).String(), marker, now, now, marker)
	if _, err := call(ctx, s, "write_note", map[string]any{"path": "entries/ret-" + marker + ".md", "content": sumNote}); err != nil {
		return fmt.Errorf("write summary note: %w", err)
	}
	res, err := call(ctx, s, "query_knowledge", map[string]any{"text": marker + " experiment"})
	if err != nil {
		return err
	}
	if !strings.Contains(res, "proves cached summaries flow end to end") {
		return fmt.Errorf("summary missing from retrieval output:\n%s", res)
	}

	// as_of: the note didn't exist last year.
	res, err = call(ctx, s, "query_knowledge", map[string]any{"text": marker + " experiment", "as_of": "2025-01-01 00:00:00"})
	if err != nil {
		return err
	}
	if strings.Contains(res, marker) {
		return fmt.Errorf("as_of 2025 shows a note created today:\n%s", res)
	}

	// list_decaying: a stale theory surfaces.
	//
	// "Stale" now means nobody has explicitly asserted it lately, so the
	// fixture must be old by `created` (or by last_explicit_reinforce), not
	// merely by last_reinforced — that field is moved by passive citation
	// refresh and by decay, so it cannot answer "has anyone asserted this".
	stale := time.Now().AddDate(0, 0, -90).Format("2006-01-02 15:04:05")
	theoryPath := "notes/ret-theory-" + marker + ".md"
	theory := fmt.Sprintf("---\nid: %s\ncategory: concept\ncreated: %q\nupdated: %q\nconfidence: 0.6\nmaturity: seed\nlast_reinforced: %q\nreinforce_count: 1\nsources:\n  - \"[[ret-%s]]\"\n---\nA stale %s theory awaiting reinforcement.\n",
		uuid.Must(uuid.NewV7()).String(), stale, now, stale, marker, marker)
	if _, err := call(ctx, s, "write_note", map[string]any{"path": theoryPath, "content": theory}); err != nil {
		return fmt.Errorf("write theory: %w", err)
	}
	res, err = call(ctx, s, "list_decaying", map[string]any{"threshold_days": 30})
	if err != nil {
		return fmt.Errorf("list_decaying: %w", err)
	}
	if !strings.Contains(res, theoryPath) {
		return fmt.Errorf("stale theory missing from list_decaying:\n%s", res)
	}

	// Soft-delete exclusion: a faded, archived note is out of default retrieval
	// but returns with include_archived.
	archPath := "archive/ret-shelved-" + marker + ".md"
	archNote := fmt.Sprintf("---\nid: %s\ncategory: entry\nstatus: faded\narchived_at: %q\ncreated: %q\nupdated: %q\n---\nA shelved account of the %s experiment, now archived.\n",
		uuid.Must(uuid.NewV7()).String(), now, now, now, marker)
	if _, err := call(ctx, s, "write_note", map[string]any{"path": archPath, "content": archNote}); err != nil {
		return fmt.Errorf("write archived note: %w", err)
	}
	res, err = call(ctx, s, "query_knowledge", map[string]any{"text": marker + " shelved experiment"})
	if err != nil {
		return err
	}
	if strings.Contains(res, archPath) {
		return fmt.Errorf("archived note leaked into default retrieval:\n%s", res)
	}
	res, err = call(ctx, s, "query_knowledge", map[string]any{"text": marker + " shelved experiment", "include_archived": true})
	if err != nil {
		return err
	}
	if !strings.Contains(res, archPath) {
		return fmt.Errorf("include_archived did not surface the archived note:\n%s", res)
	}
	return nil
}

// --- knowledge: lifecycle + verify, personas, vault history ------------------

func knowledge(ctx context.Context) error {
	s, err := connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	stale := time.Now().Add(-35 * 24 * time.Hour).Format("2006-01-02 15:04:05")
	now := time.Now().Format("2006-01-02 15:04:05")

	// A related entry so the verify pass has related-context to surface.
	if _, err := call(ctx, s, "write_note", map[string]any{
		"path":    "entries/know-capture.md",
		"content": fmt.Sprintf("---\nid: %s\ncategory: entry\ncreated: %q\nupdated: %q\n---\nThe original chronicle of the index design and its tradeoffs.\n", uuid.Must(uuid.NewV7()).String(), now, now),
	}); err != nil {
		return fmt.Errorf("write related capture: %w", err)
	}

	notePath := "notes/know-theory.md"
	note := fmt.Sprintf("---\nid: %s\ncategory: concept\ncreated: %q\nupdated: %q\nconfidence: 0.5\nmaturity: seed\nlast_reinforced: %q\nreinforce_count: 0\nsources:\n  - \"[[know-capture]]\"\n---\nA stale theory about the index design and its tradeoffs.\n",
		uuid.Must(uuid.NewV7()).String(), stale, now, stale)
	if _, err := call(ctx, s, "write_note", map[string]any{"path": notePath, "content": note}); err != nil {
		return fmt.Errorf("write_note: %w", err)
	}

	// Dry run reports the decay but changes nothing.
	report, err := call(ctx, s, "compile_lifecycle", map[string]any{"dry_run": true})
	if err != nil {
		return fmt.Errorf("compile dry_run: %w", err)
	}
	if !strings.Contains(report, "decayed") || !strings.Contains(report, "know-theory") {
		return fmt.Errorf("dry run did not report the pending decay:\n%s", report)
	}
	if got, _ := call(ctx, s, "get_note", map[string]any{"path": notePath}); !strings.Contains(got, "confidence: 0.5") {
		return fmt.Errorf("dry run mutated the note:\n%s", got)
	}
	// Real run decays it.
	if _, err := call(ctx, s, "compile_lifecycle", map[string]any{}); err != nil {
		return fmt.Errorf("compile: %w", err)
	}
	if got, _ := call(ctx, s, "get_note", map[string]any{"path": notePath}); !strings.Contains(got, "confidence: \"0.4\"") && !strings.Contains(got, "confidence: 0.4") {
		return fmt.Errorf("decay did not land:\n%s", got)
	}
	// Reinforce raises it back.
	if report, err = call(ctx, s, "compile_lifecycle", map[string]any{"reinforce": []string{notePath}}); err != nil {
		return fmt.Errorf("reinforce: %w", err)
	} else if !strings.Contains(report, "reinforced") {
		return fmt.Errorf("no reinforcement recorded:\n%s", report)
	}
	// Falsify with verify:true → excluded from default retrieval, and the run
	// surfaces related-context advisories.
	report, err = call(ctx, s, "compile_lifecycle", map[string]any{
		"falsify": map[string]string{notePath: "harness determined it false"},
		"verify":  true,
		"dry_run": true,
	})
	if err != nil {
		return fmt.Errorf("compile verify: %w", err)
	}
	if !strings.Contains(report, "related-context") {
		return fmt.Errorf("verify produced no related-context advisory:\n%s", report)
	}
	if _, err := call(ctx, s, "compile_lifecycle", map[string]any{"falsify": map[string]string{notePath: "harness determined it false"}}); err != nil {
		return fmt.Errorf("falsify: %w", err)
	}
	if res, _ := call(ctx, s, "query_knowledge", map[string]any{"text": "stale theory index design"}); strings.Contains(res, notePath) {
		return fmt.Errorf("falsified note leaked into default retrieval:\n%s", res)
	}

	// Personas: two-tier discovery, reflection, disabled rejection.
	if list, err := call(ctx, s, "list_personas", map[string]any{}); err != nil {
		return fmt.Errorf("list_personas: %w", err)
	} else if !strings.Contains(list, "deep-thoughts") {
		return fmt.Errorf("deep-thoughts persona missing:\n%s", list)
	} else if strings.Contains(list, "Gentle Setup") {
		return fmt.Errorf("tier-1 discovery leaked full persona content")
	}
	if guide, err := call(ctx, s, "get_persona", map[string]any{"id": "deep-thoughts"}); err != nil {
		return fmt.Errorf("get_persona: %w", err)
	} else if !strings.Contains(guide, "Gentle Setup") {
		return fmt.Errorf("tier-2 fetch missing the voice guide")
	}
	if written, err := call(ctx, s, "write_reflection", map[string]any{
		"persona": "deep-thoughts", "description": "Verified the knowledge feature set end to end.",
		"content": "> I checked every feature I built, and they all worked, which I have decided to find suspicious.",
	}); err != nil {
		return fmt.Errorf("write_reflection: %w", err)
	} else if !strings.Contains(written, "reflections/") {
		return fmt.Errorf("reflection path: %s", written)
	}
	if _, err := call(ctx, s, "write_reflection", map[string]any{"persona": "not-a-persona", "description": "d", "content": "c"}); err == nil {
		return fmt.Errorf("disabled persona accepted")
	}

	// Vault history: read the log, mediate a rollback.
	histPath := "entries/know-history.md"
	// No id: write_note mints one for a new path and reuses the existing one
	// when overwriting. Minting a fresh id per version — what this did before —
	// gave the same path a different identity on every write, which evicts the
	// note row and re-points its inbound links. The check passed anyway, so the
	// suite was exercising that eviction and calling it success.
	mk := func(body string) string {
		return fmt.Sprintf("---\ncategory: entry\ncreated: %q\nupdated: %q\n---\n%s\n", now, now, body)
	}
	if _, err := call(ctx, s, "write_note", map[string]any{"path": histPath, "content": mk("VERSION-ALPHA original")}); err != nil {
		return fmt.Errorf("history write v1: %w", err)
	}
	if _, err := call(ctx, s, "write_note", map[string]any{"path": histPath, "content": mk("VERSION-BETA overwrite")}); err != nil {
		return fmt.Errorf("history write v2: %w", err)
	}
	hist, err := call(ctx, s, "vault_history", map[string]any{"path": histPath})
	if err != nil {
		return fmt.Errorf("vault_history: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(hist), "\n")
	if len(lines) < 3 { // header + two commits
		return fmt.Errorf("vault_history did not report both writes:\n%s", hist)
	}
	oldestRef := strings.Fields(lines[len(lines)-1])[0]
	if _, err := call(ctx, s, "restore_note", map[string]any{"path": histPath, "ref": oldestRef}); err != nil {
		return fmt.Errorf("restore_note: %w", err)
	}
	if restored, _ := call(ctx, s, "get_note", map[string]any{"path": histPath}); !strings.Contains(restored, "VERSION-ALPHA") {
		return fmt.Errorf("restore_note did not bring back the original version:\n%s", restored)
	}
	return nil
}

// --- platform: audit-trail redaction (straight against the table) -----------

func platform(ctx context.Context) error {
	dsn := os.Getenv("COGNOSIS_DSN")
	if dsn == "" {
		return fmt.Errorf("COGNOSIS_DSN is required for the platform slice")
	}
	s, err := connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()

	canary := "CANARY-molten-lava-keys"
	now := time.Now().Format("2006-01-02 15:04:05")
	note := fmt.Sprintf("---\nid: %s\ncategory: entry\ncreated: %q\nupdated: %q\n---\nA note carrying the secret %s.\n", uuid.Must(uuid.NewV7()).String(), now, now, canary)
	if _, err := call(ctx, s, "write_note", map[string]any{"path": "entries/platform-canary.md", "content": note}); err != nil {
		return fmt.Errorf("write_note: %w", err)
	}

	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return fmt.Errorf("audit db connect: %w", err)
	}
	defer func() { _ = conn.Close(ctx) }()

	var writes int
	if err := conn.QueryRow(ctx, `select count(*) from audit_log where tool_name = 'write_note' and success`).Scan(&writes); err != nil {
		return fmt.Errorf("audit query: %w", err)
	}
	if writes == 0 {
		return fmt.Errorf("no successful write_note audit rows recorded")
	}
	var leaked int
	if err := conn.QueryRow(ctx, `select count(*) from audit_log where args_summary like '%'||$1||'%' or error like '%'||$1||'%'`, canary).Scan(&leaked); err != nil {
		return fmt.Errorf("canary query: %w", err)
	}
	if leaked != 0 {
		return fmt.Errorf("audit rows contain note content (canary found %d times)", leaked)
	}
	var anonymous int
	if err := conn.QueryRow(ctx, `select count(*) from audit_log where token_id is null`).Scan(&anonymous); err != nil {
		return err
	}
	if anonymous != 0 {
		return fmt.Errorf("%d audit rows lack a token identity", anonymous)
	}
	return nil
}

// --- migration: get_migration_status answers idle ---------------------------

func migration(ctx context.Context) error {
	s, err := connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = s.Close() }()
	out, err := call(ctx, s, "get_migration_status", map[string]any{})
	if err != nil {
		return fmt.Errorf("get_migration_status: %w", err)
	}
	if !strings.Contains(out, "No migration in progress") &&
		!strings.Contains(out, "complete") && !strings.Contains(out, "rolled_back") {
		return fmt.Errorf("unexpected migration status: %s", out)
	}
	return nil
}

// --- shared plumbing --------------------------------------------------------

type bearerTransport struct {
	token string
	base  http.RoundTripper
}

func (b *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(req)
}

// connect opens an authenticated MCP session from the standard env.
func connect(ctx context.Context) (*mcp.ClientSession, error) {
	endpoint := envOr("COGNOSIS_MCP_URL", "http://127.0.0.1:7433")
	token := strings.TrimSpace(os.Getenv("COGNOSIS_TOKEN"))
	if token == "" {
		path := os.Getenv("COGNOSIS_TOKEN_FILE")
		if path == "" {
			return nil, fmt.Errorf("set COGNOSIS_TOKEN or COGNOSIS_TOKEN_FILE")
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read token file: %w", err)
		}
		token = strings.TrimSpace(string(b))
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "harness", Version: version}, nil)
	return client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   endpoint,
		HTTPClient: &http.Client{Transport: &bearerTransport{token: token, base: http.DefaultTransport}},
	}, nil)
}

// call invokes one tool and returns its text content; tool errors become Go
// errors carrying the server's message.
func call(ctx context.Context, s *mcp.ClientSession, tool string, args map[string]any) (string, error) {
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		return "", err
	}
	var text strings.Builder
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			text.WriteString(t.Text)
		}
	}
	if res.IsError {
		return "", fmt.Errorf("%s", text.String())
	}
	return text.String(), nil
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
