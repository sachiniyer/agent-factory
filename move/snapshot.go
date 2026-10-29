// Package move implements the core of "move a live local worktree session to a
// sandbox" (#5571 W7.9): the worktree snapshot, the operator move-script
// contract, and the all-or-nothing orchestration. It performs no daemon or
// storage I/O itself — every effect on a session row, a sandbox or an agent
// goes through the Steps seam, so the sequencing and the abort semantics are
// testable in isolation.
package move

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// cleanupTimeout bounds the snapshot ref's removal, which must run even when the
// move's own context is already cancelled.
const cleanupTimeout = 30 * time.Second

// snapshotAfterRef is a test seam, called once the snapshot ref exists.
var snapshotAfterRef = func() {}

// SnapshotRef returns the ref the snapshot commit is bundled under, keyed per
// session. Everything under refs/ except refs/worktree/, refs/bisect/ and
// refs/rewritten/ is shared by every worktree of the repo, so a fixed name
// would let two concurrent moves clobber each other.
func SnapshotRef(sessionKey string) string {
	return "refs/af-move/" + sessionKey + "/snapshot"
}

const snapshotMessage = "af: move snapshot (uncommitted + untracked work)"

// Snapshot is a self-contained, transferable copy of a worktree: the branch
// history plus every uncommitted and untracked (non-ignored) file.
type Snapshot struct {
	// Branch is the session branch (refs/heads/<Branch>) as it was at HEAD.
	Branch string
	// Head is the branch tip commit — the committed state.
	Head string
	// Tree is the snapshot's working-tree tree: what the destination's working
	// tree must hash to before the move may report success.
	Tree string
	// Commit is the snapshot commit: a child of Head whose tree is the full
	// working tree. Equal to Head when the worktree was clean.
	Commit string
	// Dirty reports whether Commit differs from Head, i.e. whether the sandbox
	// must overlay uncommitted work after seeding the branch.
	Dirty bool
	// Ref is the ref name the snapshot commit carries inside the bundle.
	Ref string
	// BundlePath is a git bundle carrying Branch and Ref. The caller
	// owns it and removes it with Cleanup.
	BundlePath string
}

// Cleanup removes the bundle.
func (s *Snapshot) Cleanup() {
	if s != nil && s.BundlePath != "" {
		_ = os.Remove(s.BundlePath)
	}
}

// OverlayCommand is the shell snippet that, run inside the seeded sandbox
// workspace (cwd = the worktree on the branch), makes the working tree equal
// the snapshot while leaving HEAD at the branch tip and nothing staged, so the
// uncommitted work arrives uncommitted.
func (s *Snapshot) OverlayCommand() string {
	if !s.Dirty {
		return "true"
	}
	return fmt.Sprintf("git read-tree -u --reset %s && git reset -q", s.Commit)
}

// TakeSnapshot snapshots the worktree at worktree on branch into a bundle
// written under dir. It never modifies the worktree, its index, the branch, or
// any shared ref: the working tree is captured through a throwaway index file.
// .gitignored files are deliberately not captured — they are the move
// script's job.
func TakeSnapshot(ctx context.Context, worktree, branch, sessionKey, dir string) (*Snapshot, error) {
	if strings.TrimSpace(branch) == "" {
		return nil, fmt.Errorf("move snapshot: session has no branch")
	}
	if !validSessionKey(sessionKey) {
		return nil, fmt.Errorf("move snapshot: invalid session key %q", sessionKey)
	}
	ref := SnapshotRef(sessionKey)
	head, err := git(ctx, worktree, nil, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("move snapshot: branch %q not found in %s: %w", branch, worktree, err)
	}
	if err := refuseShallow(ctx, worktree); err != nil {
		return nil, err
	}
	if err := refuseDirtySubmodules(ctx, worktree); err != nil {
		return nil, err
	}
	gitDir, err := git(ctx, worktree, nil, "rev-parse", "--git-dir")
	if err != nil {
		return nil, err
	}
	if !filepath.IsAbs(gitDir) {
		gitDir = filepath.Join(worktree, gitDir)
	}
	tmpIdx, err := os.CreateTemp(gitDir, "af-move-index-*")
	if err != nil {
		return nil, fmt.Errorf("move snapshot: cannot create temporary index: %w", err)
	}
	tmpIdx.Close()
	defer os.Remove(tmpIdx.Name())
	// An empty file is not a valid index; start from the real one so unchanged
	// paths are not re-hashed, then fold the whole working tree in.
	realIdx, err := git(ctx, worktree, nil, "rev-parse", "--path-format=absolute", "--git-path", "index")
	if err != nil {
		return nil, err
	}
	if data, rerr := os.ReadFile(realIdx); rerr == nil {
		if werr := os.WriteFile(tmpIdx.Name(), data, 0o600); werr != nil {
			return nil, werr
		}
	} else if rerr := os.Remove(tmpIdx.Name()); rerr != nil {
		return nil, rerr
	}
	env := []string{"GIT_INDEX_FILE=" + tmpIdx.Name()}
	realGitlinks, err := gitlinks(ctx, worktree, nil)
	if err != nil {
		return nil, err
	}
	// Flags that hide a modified file from `git add -A` live in the copied index.
	if err := clearHidingFlags(ctx, worktree, env); err != nil {
		return nil, err
	}
	if _, err := git(ctx, worktree, env, "add", "-A"); err != nil {
		return nil, fmt.Errorf("move snapshot: staging the working tree failed: %w", err)
	}
	if err := refuseEmbeddedRepos(ctx, worktree, env, realGitlinks); err != nil {
		return nil, err
	}
	if err := verifyStaged(ctx, worktree, env); err != nil {
		return nil, err
	}
	tree, err := git(ctx, worktree, env, "write-tree")
	if err != nil {
		return nil, fmt.Errorf("move snapshot: writing the tree failed: %w", err)
	}
	headTree, err := git(ctx, worktree, nil, "rev-parse", head+"^{tree}")
	if err != nil {
		return nil, err
	}
	commit := head
	if tree != headTree {
		commit, err = git(ctx, worktree, []string{
			"GIT_AUTHOR_NAME=af", "GIT_AUTHOR_EMAIL=af@localhost",
			"GIT_COMMITTER_NAME=af", "GIT_COMMITTER_EMAIL=af@localhost",
		}, "commit-tree", tree, "-p", head, "-m", snapshotMessage)
		if err != nil {
			return nil, fmt.Errorf("move snapshot: writing the snapshot commit failed: %w", err)
		}
	}

	// The snapshot commit is dangling; bundling needs a ref, per session, for the
	// bundle's lifetime only. The empty old value makes creation fail rather than
	// overwrite a ref another move (or a crashed earlier one) left behind.
	if _, err := git(ctx, worktree, nil, "update-ref", ref, commit, ""); err != nil {
		return nil, fmt.Errorf("move snapshot: %s already exists (another move of this session in progress, or one that crashed; remove it with `git update-ref -d %s`): %w", ref, ref, err)
	}
	// Removed on every exit, cancellation included: the move's own context may be
	// done, and a ref left behind would fail every later move of this session.
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		_, _ = git(cctx, worktree, nil, "update-ref", "-d", ref)
	}()
	snapshotAfterRef()
	f, err := os.CreateTemp(dir, "af-move-*.bundle")
	if err != nil {
		return nil, err
	}
	f.Close()
	if _, err := git(ctx, worktree, nil, "bundle", "create", f.Name(), "refs/heads/"+branch, ref); err != nil {
		os.Remove(f.Name())
		return nil, fmt.Errorf("move snapshot: bundling failed: %w", err)
	}
	return &Snapshot{Branch: branch, Head: head, Tree: tree, Commit: commit, Dirty: commit != head, Ref: ref, BundlePath: f.Name()}, nil
}

func git(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	out, err := gitRaw(ctx, dir, env, args...)
	return strings.TrimSpace(out), err
}

// gitStdin runs git with stdin.
func gitStdin(ctx context.Context, dir string, env []string, in *strings.Reader, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = in
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(stderr.String()), err)
	}
	return string(out), nil
}

// gitRaw is git without trimming, for NUL-separated output.
func gitRaw(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(stderr.String()), err)
	}
	return string(out), nil
}

// validSessionKey accepts only characters that are safe in a ref component.
func validSessionKey(k string) bool {
	if k == "" || len(k) > 128 {
		return false
	}
	for _, r := range k {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return false
		}
	}
	return true
}

// refuseDirtySubmodules fails when an initialized submodule has uncommitted or
// untracked content: the snapshot records only the submodule's commit pointer,
// so that work would be silently dropped by the move.
func refuseDirtySubmodules(ctx context.Context, worktree string) error {
	out, err := git(ctx, worktree, nil, "status", "--porcelain", "--untracked-files=normal", "--ignore-submodules=none")
	if err != nil {
		return err
	}
	subs, err := git(ctx, worktree, nil, "ls-files", "--stage")
	if err != nil {
		return err
	}
	gitlinks := map[string]bool{}
	for _, l := range strings.Split(subs, "\n") {
		if strings.HasPrefix(l, "160000 ") {
			if i := strings.IndexByte(l, '\t'); i >= 0 {
				gitlinks[l[i+1:]] = true
			}
		}
	}
	if len(gitlinks) == 0 {
		return nil
	}
	for _, l := range strings.Split(out, "\n") {
		// git() trims the output, so the first line may have lost its leading
		// status space; split on the first gap rather than a fixed offset.
		parts := strings.SplitN(strings.TrimSpace(l), " ", 2)
		if len(parts) == 2 && gitlinks[strings.TrimSpace(parts[1])] {
			return fmt.Errorf("move snapshot: submodule %q has uncommitted changes that a move cannot carry; commit them inside the submodule first", strings.TrimSpace(parts[1]))
		}
	}
	return nil
}

// refuseShallow fails for a shallow source: its bundle lacks the ancestors past
// the shallow boundary, so the destination's fetch of it would fail after the
// move reported success. Refusing up front costs nothing.
func refuseShallow(ctx context.Context, worktree string) error {
	out, err := git(ctx, worktree, nil, "rev-parse", "--is-shallow-repository")
	if err != nil {
		return err
	}
	if out == "true" {
		return fmt.Errorf("move snapshot: the repository is a shallow clone, so its history cannot be carried to a sandbox; run `git fetch --unshallow` in %s and move again", worktree)
	}
	return nil
}

// CheckShallow is refuseShallow for callers outside a snapshot (the dry run).
func CheckShallow(ctx context.Context, worktree string) error { return refuseShallow(ctx, worktree) }

// clearHidingFlags clears assume-unchanged and skip-worktree on every index
// entry whose file exists on disk, in the temporary index only. Those flags make
// `git add -A` skip a modified file, so the snapshot would silently hold its old
// content. A skip-worktree entry whose file is absent (a sparse checkout) keeps
// its flag: clearing it would stage a deletion of something the user never removed.
func clearHidingFlags(ctx context.Context, worktree string, env []string) error {
	out, err := gitRaw(ctx, worktree, env, "ls-files", "-v", "-z")
	if err != nil {
		return err
	}
	var paths []string
	for _, rec := range strings.Split(out, "\x00") {
		if len(rec) < 3 {
			continue
		}
		tag, path := rec[0], rec[2:]
		if !(tag >= 'a' && tag <= 'z') && tag != 'S' {
			continue
		}
		if _, err := os.Lstat(filepath.Join(worktree, path)); err == nil {
			paths = append(paths, path)
		}
	}
	if len(paths) == 0 {
		return nil
	}
	// One flag per invocation: update-index applies only one of them when both
	// are given.
	for _, flag := range []string{"--no-assume-unchanged", "--no-skip-worktree"} {
		in := strings.NewReader(strings.Join(paths, "\x00") + "\x00")
		if _, err := gitStdin(ctx, worktree, env, in, "update-index", flag, "-z", "--stdin"); err != nil {
			return fmt.Errorf("move snapshot: clearing index flags that hide modified files failed: %w", err)
		}
	}
	return nil
}

// gitlinks returns the gitlink paths of the index selected by env.
func gitlinks(ctx context.Context, worktree string, env []string) (map[string]bool, error) {
	out, err := gitRaw(ctx, worktree, env, "ls-files", "--stage", "-z")
	if err != nil {
		return nil, err
	}
	links := map[string]bool{}
	for _, rec := range strings.Split(out, "\x00") {
		if strings.HasPrefix(rec, "160000 ") {
			if i := strings.IndexByte(rec, '\t'); i >= 0 {
				links[rec[i+1:]] = true
			}
		}
	}
	return links, nil
}

// refuseEmbeddedRepos fails when staging turned an untracked nested repository
// into a bare gitlink. The snapshot would carry a pointer to a commit the
// destination does not have, and none of the nested files; carrying them as
// plain files would drop the nested history, which is work too.
func refuseEmbeddedRepos(ctx context.Context, worktree string, env []string, before map[string]bool) error {
	after, err := gitlinks(ctx, worktree, env)
	if err != nil {
		return err
	}
	var embedded []string
	for p := range after {
		if !before[p] {
			embedded = append(embedded, p)
		}
	}
	if len(embedded) > 0 {
		sort.Strings(embedded)
		return fmt.Errorf("move snapshot: %s is a git repository nested inside the worktree and not tracked by it; a move cannot carry its files or its history. Commit it as a submodule, move it out of the worktree, or add it to .gitignore and carry it with the move script",
			strings.Join(embedded, ", "))
	}
	return nil
}

// verifyStaged proves the snapshot index covers the working tree: after staging,
// nothing tracked differs from disk and nothing untracked-and-unignored remains.
// Anything left is work the snapshot would drop, so the move refuses.
func verifyStaged(ctx context.Context, worktree string, env []string) error {
	diff, err := gitRaw(ctx, worktree, env, "diff", "--name-only", "-z")
	if err != nil {
		return err
	}
	others, err := gitRaw(ctx, worktree, env, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return err
	}
	var left []string
	for _, l := range strings.Split(diff+"\x00"+others, "\x00") {
		if l != "" {
			left = append(left, l)
		}
	}
	if len(left) > 0 {
		sort.Strings(left)
		if len(left) > 5 {
			left = append(left[:5], "…")
		}
		return fmt.Errorf("move snapshot: the snapshot does not cover the working tree (%s); refusing to move work it would lose", strings.Join(left, ", "))
	}
	return nil
}

// VerifyCommand is the shell snippet that, run in the destination workspace,
// prints the tree its working tree hashes to (untracked ignored files excluded,
// tracked ones kept, as in the snapshot). CheckTree compares the output.
func (s *Snapshot) VerifyCommand() string {
	// Built the way the snapshot was: start from the destination's own index (so
	// tracked files stay in the set even when they match an ignore rule), clear the
	// assume-unchanged and skip-worktree flags it carries for files present on disk
	// (they make add -A skip a file the move script changed, so a stale destination
	// would verify), then fold the working tree in with add -A. A flagged file that
	// is absent keeps its flag, as in clearHidingFlags: clearing it would stage a
	// deletion nobody made. Paths travel NUL-terminated end to end (ls-files -z,
	// xargs -0, update-index -z), so quoting never reaches the shell and the
	// existence test and the clear see exactly the same list.
	return `d=$(mktemp -d) && idx=$(git rev-parse --path-format=absolute --git-path index) && top=$(git rev-parse --show-toplevel) && ` +
		`{ [ ! -f "$idx" ] || cp "$idx" "$d/index"; } && ` +
		`{ cd "$top" && git ls-files -v -z | xargs -0 sh -c 'for l in "$@"; do case "$l" in [a-zS]\ *) p=${l#??}; if [ -e "$p" ] || [ -L "$p" ]; then printf "%s\0" "$p"; fi;; esac; done' sh > "$d/hidden"; } && ` +
		`export GIT_INDEX_FILE="$d/index" && git update-index --no-assume-unchanged -z --stdin < "$d/hidden" && git update-index --no-skip-worktree -z --stdin < "$d/hidden" && ` +
		`git add -A && git write-tree; rc=$?; rm -rf "$d"; exit $rc`
}

// CheckTree reports whether got, the output of VerifyCommand at the destination,
// is the snapshot's tree.
func (s *Snapshot) CheckTree(got string) error {
	if got = strings.TrimSpace(got); got != s.Tree {
		return fmt.Errorf("move verification: the destination's working tree is %q, not the snapshot's %q; the source was not released", got, s.Tree)
	}
	return nil
}
