package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
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

// TestWriteSpaceProbe_RealWrite — the production probe against a real writable
// dir writes a byte and cleans up after itself.
func TestWriteSpaceProbe_RealWrite(t *testing.T) {
	dir := t.TempDir()
	if err := writeSpaceProbe(dir); err != nil {
		t.Fatalf("real probe on a writable dir failed: %v", err)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, ".af-add-probe-*")); len(matches) != 0 {
		t.Fatalf("probe file left behind: %v", matches)
	}
}
