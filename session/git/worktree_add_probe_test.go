package git

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAnnotateWorktreeAddError_DiskFull — the #5295 play-test failure: `git
// worktree add` dies with ENOSPC trapped inside *exec.ExitError stderr text,
// but a data-write probe against the same filesystem fails with a REAL errno.
// The joined error must carry BOTH the git failure and ENOSPC so the
// lost-restore classifier reads it as environmental.
func TestAnnotateWorktreeAddError_DiskFull(t *testing.T) {
	old := writeSpaceProbe
	writeSpaceProbe = func(string) error {
		return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
	}
	defer func() { writeSpaceProbe = old }()

	g := &GitWorktree{worktreePath: filepath.Join(t.TempDir(), "wt")}
	gitErr := fmt.Errorf("git command failed: could not write (%w)", exitErrorWithStderr(t, "No space left on device"))
	out := g.annotateWorktreeAddError(fmt.Errorf("failed to create worktree from branch x: %w", gitErr))
	if !errors.Is(out, syscall.ENOSPC) {
		t.Fatalf("disk-full probe must put ENOSPC in the chain: %v", out)
	}
	if !errors.Is(out, gitErr) {
		t.Fatalf("joined error dropped the original git failure: %v", out)
	}
}

// TestAnnotateWorktreeAddError_FreeSpace — a non-disk git failure on a
// writable filesystem returns UNCHANGED: the real probe succeeds, no errno is
// invented, and the structural classifier sees exactly the error it had.
func TestAnnotateWorktreeAddError_FreeSpace(t *testing.T) {
	dir := t.TempDir()
	g := &GitWorktree{worktreePath: filepath.Join(dir, "wt")}
	gitErr := fmt.Errorf("git command failed: ref already exists (%w)", exitErrorWithStderr(t, "fatal: branch exists"))
	out := g.annotateWorktreeAddError(fmt.Errorf("failed to create worktree from branch x: %w", gitErr))
	if errors.Is(out, syscall.ENOSPC) {
		t.Fatalf("non-disk failure on free space must not acquire ENOSPC: %v", out)
	}
	if !errors.Is(out, gitErr) {
		t.Fatalf("original git failure lost: %v", out)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, ".af-add-probe-*")); len(matches) != 0 {
		t.Fatalf("successful probe left litter: %v", matches)
	}
}

// TestAnnotateWorktreeAddError_ProbeWriteFails pins the byte-write half: a
// create that succeeds but a WRITE that fails is still evidence (tmpfs inode
// budget vs data budget — a zero-byte file is not proof of space).
func TestAnnotateWorktreeAddError_ProbeWriteFails(t *testing.T) {
	old := writeSpaceProbe
	writeSpaceProbe = func(string) error {
		return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.EDQUOT}
	}
	defer func() { writeSpaceProbe = old }()

	g := &GitWorktree{worktreePath: filepath.Join(t.TempDir(), "wt")}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	if !errors.Is(out, syscall.EDQUOT) {
		t.Fatalf("quota-class probe failure must carry EDQUOT: %v", out)
	}
}

// TestAnnotateWorktreeAddError_NonSpaceProbeFailure — when the probe itself
// fails for a NON-space reason (parent vanished), the joined error carries
// that errno honestly: ENOENT is not in the environmental errno class, so the
// git failure keeps its structural classification.
func TestAnnotateWorktreeAddError_NonSpaceProbeFailure(t *testing.T) {
	old := writeSpaceProbe
	writeSpaceProbe = func(string) error {
		return &os.PathError{Op: "open", Path: ".af-add-probe-x", Err: syscall.ENOENT}
	}
	defer func() { writeSpaceProbe = old }()

	g := &GitWorktree{worktreePath: filepath.Join(t.TempDir(), "wt")}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	if errors.Is(out, syscall.ENOSPC) {
		t.Fatalf("ENOENT probe must not read as ENOSPC: %v", out)
	}
	if !errors.Is(out, syscall.ENOENT) {
		t.Fatalf("joined error should still carry the probe's real errno: %v", out)
	}
}

// TestAnnotateGitWriteError_CommonDirProbe — the worktree parent is writable
// but the repository's Git common dir sits on a FULL second filesystem
// (#5295 review): `worktree add` registers under worktrees/<name>/ there, and
// a probe of only the checkout parent would miss it. The common-dir probe's
// errno must still land in the chain.
func TestAnnotateGitWriteError_CommonDirProbe(t *testing.T) {
	old := writeSpaceProbe
	writeSpaceProbe = func(dir string) error {
		if dir == "/common" {
			return &os.PathError{Op: "write", Path: dir + "/.af-add-probe-x", Err: syscall.ENOSPC}
		}
		return nil
	}
	defer func() { writeSpaceProbe = old }()
	oldCommon := gitCommonDir
	gitCommonDir = func(*GitWorktree) (string, error) { return "/common", nil }
	defer func() { gitCommonDir = oldCommon }()

	g := &GitWorktree{worktreePath: filepath.Join(t.TempDir(), "wt")}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed (exit status 128)"))
	if !errors.Is(out, syscall.ENOSPC) {
		t.Fatalf("full common dir must put ENOSPC in the chain even when the worktree parent writes fine: %v", out)
	}
}

// TestGitCommonDir_RelativeRepoPath is the #5295-review regression: a restored
// record can carry a relative repoPath, which runGitLocalCommand resolves
// against daemonLaunchCwd — but a relative --git-common-dir result then has to
// join onto that RESOLVED repo dir, not the bare relative repoPath (which would
// leave the probe path relative to the daemon's post-chdir AF home and probe an
// unrelated filesystem).
func TestGitCommonDir_RelativeRepoPath(t *testing.T) {
	sandboxHome(t)
	root := t.TempDir()
	repoDir := filepath.Join(root, "repo")
	require.NoError(t, exec.Command("git", "init", repoDir).Run())

	old := daemonLaunchCwd
	daemonLaunchCwd = root
	defer func() { daemonLaunchCwd = old }()

	dir, err := gitCommonDir(&GitWorktree{repoPath: "repo"})
	require.NoError(t, err)
	require.True(t, filepath.IsAbs(dir),
		"a relative repoPath must resolve against the launch cwd, not the daemon's post-chdir cwd")
	assert.Equal(t, filepath.Join(repoDir, ".git"), dir)
}

// TestAnnotateGitWriteError_CommonDirUnresolvable — when `rev-parse
// --git-common-dir` itself fails with a resource errno (a fork under
// RLIMIT_NPROC), that failure is evidence too and stays in the chain.
func TestAnnotateGitWriteError_CommonDirUnresolvable(t *testing.T) {
	old := writeSpaceProbe
	writeSpaceProbe = func(string) error { return nil }
	defer func() { writeSpaceProbe = old }()
	oldCommon := gitCommonDir
	gitCommonDir = func(*GitWorktree) (string, error) {
		return "", &os.PathError{Op: "fork/exec", Path: "git", Err: syscall.EAGAIN}
	}
	defer func() { gitCommonDir = oldCommon }()

	g := &GitWorktree{worktreePath: filepath.Join(t.TempDir(), "wt")}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	if !errors.Is(out, syscall.EAGAIN) {
		t.Fatalf("unresolvable common dir must join its own errno: %v", out)
	}
}

// TestAnnotateWorktreeAddError_DestinationStatError — a stat failure on the
// destination that is not plain ENOENT (a dead mount reports ETIMEDOUT; an
// unsearchable parent reports EACCES/ENOTDIR) is probe evidence the git
// subprocess could never carry, and must join the annotated error rather
// than be discarded — otherwise mount stalls charge the structural budget
// (#5295 review).
func TestAnnotateWorktreeAddError_DestinationStatError(t *testing.T) {
	old := writeSpaceProbe
	writeSpaceProbe = func(string) error { return nil }
	defer func() { writeSpaceProbe = old }()
	oldCommon := gitCommonDir
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }
	defer func() { gitCommonDir = oldCommon }()

	// worktreePath sits beneath a regular FILE: os.Stat fails ENOTDIR.
	parent := filepath.Join(t.TempDir(), "a-file")
	require.NoError(t, os.WriteFile(parent, []byte("x"), 0o600))
	g := &GitWorktree{worktreePath: filepath.Join(parent, "wt")}

	out := g.annotateWorktreeAddError(errors.New("git worktree add failed (exit status 128)"))
	require.True(t, errors.Is(out, syscall.ENOTDIR),
		"the destination stat failure must be joined as evidence: %v", out)
}

// TestAnnotateWorktreeAddError_DestinationAbsentIsSilent — a simply-missing
// destination is the normal pre-creation answer, not evidence: the stat skip
// must not add noise to the chain.
func TestAnnotateWorktreeAddError_DestinationAbsentIsSilent(t *testing.T) {
	old := writeSpaceProbe
	writeSpaceProbe = func(string) error { return nil }
	defer func() { writeSpaceProbe = old }()
	oldCommon := gitCommonDir
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }
	defer func() { gitCommonDir = oldCommon }()

	g := &GitWorktree{worktreePath: filepath.Join(t.TempDir(), "wt")}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.False(t, errors.Is(out, syscall.ENOENT),
		"plain ENOENT is the expected pre-creation state, not evidence: %v", out)
}

// The probe machinery itself (real writes, the O_TMPFILE anonymous inode,
// the named fallback, and the bounded flight) is tested in internal/fsprobe.
