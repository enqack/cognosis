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

// memory-loop slice: write -> query -> get -> edit round trip.
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
	// The duplicated sentence is deliberate -- it is the multi-match fixture, and
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
// table -- the id the tools return is the one in the markdown, and the eviction
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
