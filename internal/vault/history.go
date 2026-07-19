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
// several goroutines commit to the one vault repo -- the write pipeline per
// write, the compile pass, restore_note, and the reconciliation watcher -- and
// two concurrent index writers otherwise race on .git/index.lock. A single
// process-wide lock (History instances are cheap, per-call values sharing one
// on-disk repo) is the simplest correct guard.
var gitIndexMu sync.Mutex

// Vault history: an auto-managed, local-only git repository inside the
// vault -- recovery net, not staleness signal. No remote, no branches; the
// operator never has to know it's there. Implemented by shelling out to the
// system git binary (dependency ledger: less code and risk than go-git for
// five subcommands).

// History wraps the vault's git repo. Zero value is unusable; use NewHistory.
type History struct {
	dir string // vault root (worktree)
}

func NewHistory(vaultDir string) *History { return &History{dir: vaultDir} }

func (h *History) git(ctx context.Context, args ...string) (string, error) {
	return h.gitWithIndex(ctx, "", args...)
}

// gitWithIndex is git with GIT_INDEX_FILE pointed at indexFile, or the repo's
// real index when indexFile is empty.
func (h *History) gitWithIndex(ctx context.Context, indexFile string, args ...string) (string, error) {
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
	if indexFile != "" {
		cmd.Env = append(cmd.Env, "GIT_INDEX_FILE="+indexFile)
	}
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
// It is belt to CommitAll's braces rather than the primary mechanism -- the
// pathspec there already prevents these from being committed. This exists so
// `git status` in the vault is quiet for a human, and so the files do not read
// as "untracked, probably should be added".
//
// .obsidian is not ignored wholesale: app.json, appearance.json and
// core-plugins.json are genuine user configuration worth versioning. Only the
// two that Obsidian rewrites continuously are listed, matching Obsidian's own
// guidance for shared vaults.
const seedGitignore = `# Generated from git log on every boot and compile -- regenerable, and tracking
# it would make the dashboard list its own commits as restorable.
history.md

# Obsidian rewrites these continuously while it is open. The rest of
# .obsidian/ is real configuration and stays tracked.
.obsidian/workspace.json
.obsidian/graph.json
`

// CommitAll commits the paths Cognosis owns with the given message. A clean
// tree is not an error -- reconciliation may find no drift to record.
//
// Two properties have to hold at once, and they pull in opposite directions.
//
// **Only owned paths.** This was `git add -A` plus a bare `git commit`, so
// anything any tool left in the vault directory became part of the knowledge
// audit trail: on a real vault, 22% of commits touched no note at all, and some
// carried "watch: <note>.md edited out-of-band" subjects while containing only
// editor state. Scoping the `add` fixes what Cognosis stages; it does nothing
// about what is *already* in the index, and a bare `git commit` records the
// whole index.
//
// **A snapshot, not a live read.** The obvious scoped commit,
// `git commit -- <paths>`, is a *partial commit*: git takes those paths from the
// working tree at commit time, ignoring the index. Vault file writes are not
// under gitIndexMu -- only the git calls are -- so a concurrent writer's file can
// land between this add and this commit. Under a partial commit it is swept into
// *this* message, and its own CommitAll then finds nothing left to record and
// commits nothing. The write is misattributed and its history entry is lost.
//
// So the commit is assembled in a scratch index instead: read HEAD into it,
// stage the owned paths, and write that tree directly. The snapshot is taken at
// the `add`, so a file landing afterwards is simply not in this commit and stays
// pending for its own writer -- while the real index, and anything another party
// has staged in it, is never read or modified.
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

	// A fresh vault has no commits yet: EnsureRepo runs `git init` and stops, so
	// the first CommitAll is the root commit and there is no HEAD to read or to
	// parent onto.
	head, haveHead, err := h.headCommit(ctx)
	if err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}

	idx, err := os.CreateTemp(filepath.Join(h.dir, ".git"), "cognosis-index-*")
	if err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	tmpIndex := idx.Name()
	_ = idx.Close()
	// git wants to create this file itself; an existing empty file is not a
	// valid index.
	_ = os.Remove(tmpIndex)
	defer func() { _ = os.Remove(tmpIndex) }()

	if haveHead {
		if _, err := h.gitWithIndex(ctx, tmpIndex, "read-tree", head); err != nil {
			return cogerr.E(op, cogerr.Internal, err)
		}
	}
	// `--` then the paths: everything after is a pathspec, never a rev. `-A`
	// within a pathspec still records deletions inside those paths.
	add := append([]string{"add", "-A", "--"}, paths...)
	if _, err := h.gitWithIndex(ctx, tmpIndex, add...); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	// The window this whole design exists to close: another writer's file can
	// land here, after the snapshot and before the commit. nil in production.
	if testHookAfterStage != nil {
		testHookAfterStage()
	}
	tree, err := h.gitWithIndex(ctx, tmpIndex, "write-tree")
	if err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	tree = strings.TrimSpace(tree)

	// Emptiness check by tree identity: if staging the owned paths reproduced
	// HEAD's tree, there is nothing to record. This is exact, where the old
	// `git status` check could be fooled by an untracked foreign file.
	if haveHead {
		headTree, err := h.git(ctx, "rev-parse", head+"^{tree}")
		if err != nil {
			return cogerr.E(op, cogerr.Internal, err)
		}
		if strings.TrimSpace(headTree) == tree {
			return nil
		}
	} else if tree == emptyTreeHash {
		return nil
	}

	commitArgs := []string{"commit-tree", tree, "-m", message}
	if haveHead {
		commitArgs = append(commitArgs, "-p", head)
	}
	commit, err := h.git(ctx, commitArgs...)
	if err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	commit = strings.TrimSpace(commit)
	// Updating HEAD resolves the symbolic ref, so this moves the current branch
	// and creates it when unborn.
	if _, err := h.git(ctx, "update-ref", "HEAD", commit); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}

	// Bring the real index up to the commit for the owned paths only. Without
	// this every owned file reads as staged-modified against the new HEAD, which
	// would leave the tree permanently dirty and block PurgePath's filter-branch.
	// Anything another party staged outside these paths is left untouched.
	if _, err := h.git(ctx, add...); err != nil {
		return cogerr.E(op, cogerr.Internal, err)
	}
	return nil
}

// emptyTreeHash is git's fixed hash for a tree with no entries -- what
// write-tree returns when nothing owned exists to stage.
const emptyTreeHash = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// testHookAfterStage runs between staging the scratch index and writing its
// tree. Nil outside tests.
//
// A seam rather than a sleep: the interleaving it exposes is the one defect in
// this file that a single-threaded test cannot see, and it shipped once
// precisely because every test called CommitAll start-to-finish.
var testHookAfterStage func()

// headCommit resolves HEAD, reporting false rather than an error when the
// branch is unborn.
func (h *History) headCommit(ctx context.Context) (string, bool, error) {
	out, err := h.git(ctx, "rev-parse", "--verify", "--quiet", "HEAD")
	if err != nil {
		// `--quiet` makes an unborn HEAD exit 1 with no output, which is the
		// fresh-vault case rather than a failure.
		if strings.TrimSpace(out) == "" {
			return "", false, nil
		}
		return "", false, err
	}
	return strings.TrimSpace(out), true, nil
}

// ownedPaths lists the vault-relative paths the history repo records: the
// processing stages, the append-only lifecycle log, and the root index.
//
// Derived from vault.Stages() rather than restated, so adding a stage cannot
// silently leave it unversioned.
//
// The two other reserved names are treated differently, and the split is not
// arbitrary. history.md is generated from git log on every boot, so committing
// it makes the dashboard cite its own churn as restorable -- it is deliberately
// absent. index.md is reserved and *validated* but never written by Cognosis:
// it carries the vault's okf_version declaration, which is the one field that
// says how everything else should be read. Leaving it unversioned meant a
// format declaration could change or vanish with nothing in the history saying
// so.
func ownedPaths() []string {
	out := make([]string, 0, len(Stages())+2)
	for _, st := range Stages() {
		out = append(out, string(st))
	}
	return append(out, "log.md", "index.md")
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

// Log returns the one-line history for a vault-relative path (newest first) --
// backs `cognosis vault history`.
