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
	"strings"
)

// SnapshotRef is the private ref the snapshot commit is bundled under. It lives
// outside refs/heads and refs/stash so nothing in the source repo — and no
// sibling worktree sharing it — can observe or consume it.
const SnapshotRef = "refs/af-move/snapshot"

const snapshotMessage = "af: move snapshot (uncommitted + untracked work)"

// Snapshot is a self-contained, transferable copy of a worktree: the branch
// history plus every uncommitted and untracked (non-ignored) file.
type Snapshot struct {
	// Branch is the session branch (refs/heads/<Branch>) as it was at HEAD.
	Branch string
	// Head is the branch tip commit — the committed state.
	Head string
	// Commit is the snapshot commit: a child of Head whose tree is the full
	// working tree. Equal to Head when the worktree was clean.
	Commit string
	// Dirty reports whether Commit differs from Head, i.e. whether the sandbox
	// must overlay uncommitted work after seeding the branch.
	Dirty bool
	// BundlePath is a git bundle carrying Branch and SnapshotRef. The caller
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
func TakeSnapshot(ctx context.Context, worktree, branch, dir string) (*Snapshot, error) {
	if strings.TrimSpace(branch) == "" {
		return nil, fmt.Errorf("move snapshot: session has no branch")
	}
	head, err := git(ctx, worktree, nil, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("move snapshot: branch %q not found in %s: %w", branch, worktree, err)
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
	if _, err := git(ctx, worktree, env, "add", "-A"); err != nil {
		return nil, fmt.Errorf("move snapshot: staging the working tree failed: %w", err)
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

	// The snapshot commit is dangling; bundling needs a ref. Name it in the
	// private namespace for the bundle's lifetime only.
	if _, err := git(ctx, worktree, nil, "update-ref", SnapshotRef, commit); err != nil {
		return nil, err
	}
	defer git(ctx, worktree, nil, "update-ref", "-d", SnapshotRef) //nolint:errcheck
	f, err := os.CreateTemp(dir, "af-move-*.bundle")
	if err != nil {
		return nil, err
	}
	f.Close()
	if _, err := git(ctx, worktree, nil, "bundle", "create", f.Name(), "refs/heads/"+branch, SnapshotRef); err != nil {
		os.Remove(f.Name())
		return nil, fmt.Errorf("move snapshot: bundling failed: %w", err)
	}
	return &Snapshot{Branch: branch, Head: head, Commit: commit, Dirty: commit != head, BundlePath: f.Name()}, nil
}

func git(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %s: %w", strings.Join(args, " "), strings.TrimSpace(stderr.String()), err)
	}
	return strings.TrimSpace(string(out)), nil
}
