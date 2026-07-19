package vault

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/enqack/cognosis/internal/cogerr"
)

// gitIndexMu serializes operations that write the history repo's index
// (add/commit/checkout/filter-branch). The daemon is single-instance, but
// several goroutines commit to the one vault repo — the write pipeline per
// write, the compile pass, restore_note, and the reconciliation watcher — and
// two concurrent index writers otherwise race on .git/index.lock. A single
// process-wide lock (History instances are cheap, per-call values sharing one
// on-disk repo) is the simplest correct guard.
var gitIndexMu sync.Mutex

// Vault history: an auto-managed, local-only git repository inside the
// vault — recovery net, not staleness signal. No remote, no branches; the
// operator never has to know it's there. Implemented by shelling out to the
// system git binary (dependency ledger: less code and risk than go-git for
// five subcommands).

// History wraps the vault's git repo. Zero value is unusable; use NewHistory.
type History struct {
	dir string // vault root (worktree)
}

func NewHistory(vaultDir string) *History { return &History{dir: vaultDir} }

func (h *History) git(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = h.dir
	// The history repo is self-contained: never inherit identity or hooks from
	// the operator's global config, and never mistake an enclosing repo for it.
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=cognosis", "GIT_AUTHOR_EMAIL=daemon@cognosis.local",
		"GIT_COMMITTER_NAME=cognosis", "GIT_COMMITTER_EMAIL=daemon@cognosis.local",
	)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, out.String())
	}
	return out.String(), nil
}

// EnsureRepo initializes the history repo on first daemon start if absent
// (idempotent). The .git dir living inside the vault is invisible to the
// markdown walk (non-.md files are skipped).
func (h *History) EnsureRepo(ctx context.Context) error {
	const op = "vault.History.EnsureRepo"
	if _, err := os.Stat(filepath.Join(h.dir, ".git")); err == nil {
		return nil
	}
	if err := os.MkdirAll(h.dir, 0o750); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	if _, err := h.git(ctx, "init", "-q"); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	// Seeded only here, on the git-init path, so an existing vault is never
	// written to: the early return above means this never runs twice.
	if err := os.WriteFile(filepath.Join(h.dir, ".gitignore"), []byte(seedGitignore), 0o600); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	return nil
}

// seedGitignore is written into a vault Cognosis creates.
//
// It is belt to CommitAll's braces rather than the primary mechanism — the
// pathspec there already prevents these from being committed. This exists so
// `git status` in the vault is quiet for a human, and so the files do not read
// as "untracked, probably should be added".
//
// .obsidian is not ignored wholesale: app.json, appearance.json and
// core-plugins.json are genuine user configuration worth versioning. Only the
// two that Obsidian rewrites continuously are listed, matching Obsidian's own
// guidance for shared vaults.
const seedGitignore = `# Generated from git log on every boot and compile — regenerable, and tracking
# it would make the dashboard list its own commits as restorable.
history.md

# Obsidian rewrites these continuously while it is open. The rest of
# .obsidian/ is real configuration and stays tracked.
.obsidian/workspace.json
.obsidian/graph.json
`

// CommitAll stages the paths Cognosis owns and commits with the given message.
// A clean tree is not an error — reconciliation may find no drift to record.
//
// The pathspec is the point. This was `git add -A`, so anything any tool left
// in the vault directory became part of the knowledge audit trail: on a real
// vault, 22% of commits touched no note at all, and some carried
// "watch: <note>.md edited out-of-band" subjects while containing only editor
// state, because by the time the commit ran the note was already recorded and
// all that remained to stage was churn.
//
// Staging only what Cognosis defines also makes the emptiness check below
// meaningful: a sweep that finds nothing but an editor's scratch files now
// commits nothing, rather than manufacturing a commit that says otherwise.
func (h *History) CommitAll(ctx context.Context, message string) error {
	const op = "vault.History.CommitAll"
	gitIndexMu.Lock()
	defer gitIndexMu.Unlock()

	paths, err := h.presentOwnedPaths(ctx)
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		return nil // nothing Cognosis owns exists yet
	}
	// `--` then the paths: everything after is a pathspec, never a rev. `-A`
	// within a pathspec still records deletions inside those paths.
	add := append([]string{"add", "-A", "--"}, paths...)
	if _, err := h.git(ctx, add...); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	// Scoped to the same paths, or an untracked editor file would make this
	// non-empty and produce an empty commit.
	status := append([]string{"status", "--porcelain", "--"}, paths...)
	out, err := h.git(ctx, status...)
	if err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	if strings.TrimSpace(out) == "" {
		return nil
	}
	if _, err := h.git(ctx, "commit", "-q", "-m", message); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	return nil
}

// ownedPaths lists the vault-relative paths the history repo records: the
// processing stages, plus the append-only lifecycle log.
//
// Derived from vault.Stages() rather than restated, so adding a stage cannot
// silently leave it unversioned. history.md is deliberately absent — it is
// generated from git log, so committing it makes the dashboard cite its own
// churn as restorable.
func ownedPaths() []string {
	out := make([]string, 0, len(Stages())+1)
	for _, st := range Stages() {
		out = append(out, string(st))
	}
	return append(out, "log.md")
}

// presentOwnedPaths narrows ownedPaths to those git will accept as a pathspec.
//
// git errors on a pathspec matching nothing in either the working tree or the
// index, and a vault need not contain every stage directory. Tracked-but-absent
// paths are kept deliberately: a stage directory deleted wholesale still has
// files in the index, and dropping it here would leave that deletion
// unrecorded.
func (h *History) presentOwnedPaths(ctx context.Context) ([]string, error) {
	const op = "vault.History.CommitAll"
	var missing []string
	out := make([]string, 0, len(ownedPaths()))
	for _, p := range ownedPaths() {
		if _, err := os.Stat(filepath.Join(h.dir, filepath.FromSlash(p))); err == nil {
			out = append(out, p)
			continue
		}
		missing = append(missing, p)
	}
	if len(missing) == 0 {
		return out, nil
	}
	// ls-files exits zero on an unmatched pathspec, so this is safe to ask.
	tracked, err := h.git(ctx, append([]string{"ls-files", "--"}, missing...)...)
	if err != nil {
		return nil, cogerr.E(op, cogerr.Internal, err)
	}
	for _, p := range missing {
		if strings.Contains(tracked, p+"/") || strings.Contains(tracked, p+"\n") ||
			strings.TrimSpace(tracked) == p {
			out = append(out, p)
		}
	}
	return out, nil
}

// Log returns the one-line history for a vault-relative path (newest first) —
// backs `cognosis vault history`.
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
// surfaces — enough to cover a working session, few enough to stay scannable.
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
// first) with the paths each touched — backs the history.md dashboard and the
// vault_history MCP tool. A tab-delimited header line per commit plus
// --name-only file lists; header lines are told apart by a full 40-hex hash.
func (h *History) LogAll(ctx context.Context, n int) ([]Commit, error) {
	const op = "vault.History.LogAll"
	out, err := h.git(ctx, "log", fmt.Sprintf("-n%d", n), "--name-only",
		"--format=%H%x09%aI%x09%s")
	if err != nil {
		// A freshly-initialized repo (daemon just booted, nothing committed yet)
		// has no HEAD — that is empty history, not a failure.
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
	b.WriteString("_Generated, read-only — Cognosis rewrites this file on every compile pass and daemon start; " +
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
			fmt.Fprintf(&b, "## %s — %s\n\n", c.When, c.Subject)
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

// Show returns a path's content at a given ref — the read half of
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
// anything is written. Old commits are not bound by today's rules — a note
// predating the UUIDv7 id requirement is the concrete case — and without this
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

// PurgePath erases a path from every commit in the history repo — the one
// sanctioned history rewrite, reserved for hard-delete (erasure that leaves
// the content recoverable from history isn't erasure). There is no remote to
// coordinate with, which is what makes this tractable.
func (h *History) PurgePath(ctx context.Context, relPath string) error {
	const op = "vault.History.PurgePath"

	// filter-branch refuses outright on a dirty working tree ("Cannot rewrite
	// branches: You have unstaged changes"), and this repo is dirty most of the
	// time: history.md is generated and rewritten on every daemon boot and
	// compile pass. Committing first is what the daemon does routinely, and it
	// preserves the pending drift rather than discarding it — the alternative,
	// stashing across a history rewrite, leaves the stash pointing at commits
	// that no longer exist.
	//
	// Retried, because committing once is not enough. The vault has live
	// external writers — the daemon regenerates history.md, and an open
	// Obsidian rewrites .obsidian/workspace.json continuously — so the tree can
	// be dirtied again in the milliseconds between the commit and the rewrite.
	// Observed exactly that: the pre-purge commit landed and filter-branch
	// still refused.
	//
	// A few attempts covers the ordinary case. If an editor is writing
	// continuously this still gives up rather than looping, and it gives up
	// having destroyed nothing — the caller runs this before it touches the
	// file, the row or the log.
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
				"could not rewrite history after %d attempts: something keeps writing to the vault "+
					"(an open editor, or the daemon). Close it and retry — nothing has been deleted: %v",
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
// favour of the out-of-tree filter-repo, but it ships with git itself — no new
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
