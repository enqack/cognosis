package vault

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/enqack/cognosis/internal/cogerr"
)

// Read-side history: log, dashboard, show, restore, and the hard-delete purge.
func (h *History) Log(ctx context.Context, relPath string) ([]string, error) {
	const op = "vault.History.Log"
	out, err := h.git(ctx, "log", "--format=%h %aI %s", "--", relPath)
	if err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}
	return strings.Split(out, "\n"), nil
}

// historyDashboardDepth is how many revertable states the generated history.md
// surfaces -- enough to cover a working session, few enough to stay scannable.
const historyDashboardDepth = 10

// Commit is one whole-repo history entry: the revertable state a dashboard row
// (or the vault_history MCP tool) describes.
type Commit struct {
	Hash    string   // full commit hash
	When    string   // ISO-8601 author date
	Subject string   // commit message subject
	Paths   []string // vault-relative paths the commit touched
}

// LogAll returns the most recent n commits across the whole vault (newest
// first) with the paths each touched -- backs the history.md dashboard and the
// vault_history MCP tool. A tab-delimited header line per commit plus
// --name-only file lists; header lines are told apart by a full 40-hex hash.
func (h *History) LogAll(ctx context.Context, n int) ([]Commit, error) {
	const op = "vault.History.LogAll"
	out, err := h.git(ctx, "log", fmt.Sprintf("-n%d", n), "--name-only",
		"--format=%H%x09%aI%x09%s")
	if err != nil {
		// A freshly-initialized repo (daemon just booted, nothing committed yet)
		// has no HEAD -- that is empty history, not a failure.
		if strings.Contains(err.Error(), "does not have any commits") {
			return nil, nil
		}
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	var commits []Commit
	for _, line := range strings.Split(out, "\n") {
		if line == "" {
			continue
		}
		if fields := strings.SplitN(line, "\t", 3); len(fields) == 3 && isFullHash(fields[0]) {
			commits = append(commits, Commit{Hash: fields[0], When: fields[1], Subject: fields[2]})
			continue
		}
		if len(commits) > 0 {
			last := &commits[len(commits)-1]
			last.Paths = append(last.Paths, line)
		}
	}
	return commits, nil
}

func isHexDigit(r rune) bool {
	return (r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')
}

func isFullHash(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, r := range s {
		if !isHexDigit(r) {
			return false
		}
	}
	return true
}

// WriteDashboard regenerates the vault-root history.md: a read-only, human-
// facing projection of the otherwise-invisible git history. It lists the most
// recent revertable states with the exact restore command for each, so an
// operator in Obsidian never has to drop to a terminal to discover their
// recovery options. Overwritten wholesale each call; never indexed (see
// IsReserved). A short hash is unambiguous for the restore command.
func (h *History) WriteDashboard(ctx context.Context) error {
	commits, err := h.LogAll(ctx, historyDashboardDepth)
	if err != nil {
		return err
	}
	var b strings.Builder
	b.WriteString("# Vault history\n\n")
	b.WriteString("_Generated, read-only -- Cognosis rewrites this file on every compile pass and daemon start; " +
		"your edits here will be lost. It projects the auto-managed git history so you can recover a prior " +
		"state without leaving your editor: copy a restore command below into a terminal._\n\n")
	if len(commits) == 0 {
		b.WriteString("No history yet.\n")
	} else {
		fmt.Fprintf(&b, "The last %d revertable states (newest first):\n\n", len(commits))
		for _, c := range commits {
			short := c.Hash
			if len(short) > 12 {
				short = short[:12]
			}
			fmt.Fprintf(&b, "## %s -- %s\n\n", c.When, c.Subject)
			fmt.Fprintf(&b, "- commit `%s`\n", short)
			if len(c.Paths) > 0 {
				b.WriteString("- restore a file to this state:\n\n  ```sh\n")
				for _, p := range c.Paths {
					fmt.Fprintf(&b, "  cognosis vault restore %s --at %s\n", p, short)
				}
				b.WriteString("  ```\n")
			}
			b.WriteString("\n")
		}
	}
	return WriteFileAtomic(filepath.Join(h.dir, "history.md"), []byte(b.String()))
}

// Show returns a path's content at a given ref -- the read half of
// `cognosis vault restore`.
func (h *History) Show(ctx context.Context, ref, relPath string) ([]byte, error) {
	const op = "vault.History.Show"
	out, err := h.git(ctx, "show", ref+":"+relPath)
	if err != nil {
		return nil, cogerr.Ef(op, cogerr.NotFound, "no %s at ref %s: %v", relPath, ref, err)
	}
	return []byte(out), nil
}

// Restore writes a path's content-at-ref back as the current version. History
// moves forward: the restore is itself a new commit, never a rewrite.
//
// The restored content is validated against the *current* contract before
// anything is written. Old commits are not bound by today's rules -- a note
// predating the UUIDv7 id requirement is the concrete case -- and without this
// check the restore succeeds, the file lands on disk, and reconciliation then
// silently refuses to index it. That leaves a note that exists but cannot be
// retrieved, reported to the caller as success. Refusing outright is worse for
// nobody: the content is still readable via Show.
func (h *History) Restore(ctx context.Context, ref, relPath string) error {
	const op = "vault.History.Restore"
	content, err := h.Show(ctx, ref, relPath)
	if err != nil {
		return err
	}
	n, err := ParseNote(relPath, content)
	if err != nil {
		return cogerr.Ef(op, cogerr.Validation,
			"%s at %s does not parse under the current contract: %v", relPath, ref, err)
	}
	if probs := Validate(relPath, n.Frontmatter, n.Frontmatter != nil); len(probs) > 0 {
		msgs := make([]string, len(probs))
		for i, p := range probs {
			msgs[i] = p.Field + ": " + p.Reason
		}
		return cogerr.Ef(op, cogerr.Validation,
			"%s at %s violates the current contract and would be written but never indexed (%s)",
			relPath, ref, strings.Join(msgs, "; "))
	}
	if err := WriteFileAtomic(filepath.Join(h.dir, filepath.FromSlash(relPath)), content); err != nil {
		return err
	}
	return h.CommitAll(ctx, fmt.Sprintf("restore: %s @ %s", relPath, ref))
}

// PurgePath erases a path from every commit in the history repo -- the one
// sanctioned history rewrite, reserved for hard-delete (erasure that leaves
// the content recoverable from history isn't erasure). There is no remote to
// coordinate with, which is what makes this tractable.
func (h *History) PurgePath(ctx context.Context, relPath string) error {
	const op = "vault.History.PurgePath"

	// filter-branch refuses outright on a dirty working tree ("Cannot rewrite
	// branches: You have unstaged changes"), and this repo is dirty most of the
	// time: history.md is generated and rewritten on every daemon boot and
	// compile pass. Committing first is what the daemon does routinely, and it
	// preserves the pending drift rather than discarding it -- the alternative,
	// stashing across a history rewrite, leaves the stash pointing at commits
	// that no longer exist.
	//
	// Retried, because committing once is not enough. The vault has live
	// external writers -- the daemon regenerates history.md, and an open
	// Obsidian rewrites .obsidian/workspace.json continuously -- so the tree can
	// be dirtied again in the milliseconds between the commit and the rewrite.
	// Observed exactly that: the pre-purge commit landed and filter-branch
	// still refused.
	//
	// A few attempts covers the ordinary case. If an editor is writing
	// continuously this still gives up rather than looping, and it gives up
	// having destroyed nothing -- the caller runs this before it touches the
	// file, the row or the log.
	//
	// There is one case retrying cannot fix, and the failure message has to name
	// it. CommitAll now stages only the paths Cognosis owns, so a vault created
	// before that change -- where history.md or .obsidian/workspace.json are
	// already *tracked* -- stays dirty no matter how many times we commit: the
	// files change constantly and nothing here will ever stage them again.
	// Retrying is futile there, and the remedy is the one-time untracking step
	// in docs/setup-guide.md rather than closing an editor.
	const attempts = 3
	var lastErr error
	for i := range attempts {
		// Outside the lock: CommitAll takes gitIndexMu itself.
		if err := h.CommitAll(ctx, "pre-purge: commit pending drift so history can be rewritten"); err != nil {
			return err
		}
		lastErr = h.purgeOnce(ctx, relPath)
		if lastErr == nil {
			break
		}
		if i == attempts-1 {
			return cogerr.Ef(op, cogerr.Conflict,
				"could not rewrite history after %d attempts: the vault working tree will not come clean. "+
					"Nothing has been deleted. Two causes: something is writing to the vault right now "+
					"(an open editor, or the daemon) -- close it and retry; or generated files are still "+
					"tracked from before Cognosis stopped committing them, which retrying will never fix. "+
					"For the second, run once in the vault: "+
					"git rm -r --cached --quiet history.md .obsidian/workspace.json .obsidian/graph.json "+
					"&& git commit -m 'stop tracking editor and generated state'. "+
					"See docs/setup-guide.md. Underlying error: %v",
				attempts, lastErr)
		}
	}
	gitIndexMu.Lock()
	defer gitIndexMu.Unlock()
	// Drop the safety refs and unreachable objects so the content is gone,
	// not just unreferenced.
	_ = os.RemoveAll(filepath.Join(h.dir, ".git", "refs", "original"))
	if _, err := h.git(ctx, "reflog", "expire", "--expire=now", "--all"); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	if _, err := h.git(ctx, "gc", "--prune=now"); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	return nil
}

// purgeOnce is one filter-branch attempt. filter-branch is deprecated in
// favour of the out-of-tree filter-repo, but it ships with git itself -- no new
// dependency for one rare path.
func (h *History) purgeOnce(ctx context.Context, relPath string) error {
	gitIndexMu.Lock()
	defer gitIndexMu.Unlock()
	_, err := h.git(ctx, "filter-branch", "--force", "--index-filter",
		"git rm --cached --ignore-unmatch -- "+shellQuote(relPath),
		"--prune-empty", "--", "--all")
	return err
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
