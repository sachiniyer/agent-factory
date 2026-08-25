package git

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// A final-component symlink must be refused before CleanupRegisteredOnly
// follows it. On a stalled FUSE/NFS target, os.Stat never returns and wedges
// the session operation lock; even on a healthy target, following the link
// crosses the ownership boundary that registered-only cleanup is meant to
// protect.
func TestCleanupRegisteredOnly_RejectsFinalSymlinkWithoutFollowing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}

	gw, worktreePath := worktreeForCleanup(t)
	target := t.TempDir()
	protected := filepath.Join(target, "keep.txt")
	if err := os.WriteFile(protected, []byte("keep me"), 0o644); err != nil {
		t.Fatalf("seed protected target: %v", err)
	}
	if err := os.RemoveAll(worktreePath); err != nil {
		t.Fatalf("replace recorded worktree: %v", err)
	}
	if err := os.Symlink(target, worktreePath); err != nil {
		t.Fatalf("install replacement symlink: %v", err)
	}

	state, err := gw.CleanupRegisteredOnly()

	if state != CleanupStateUnknown {
		t.Fatalf("final-component symlink cleanup state = %v, want unknown", state)
	}
	if err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("cleanup must refuse the final-component symbolic link before following it, got: %v", err)
	}
	info, lstatErr := os.Lstat(worktreePath)
	if lstatErr != nil {
		t.Fatalf("replacement symlink must be retained: %v", lstatErr)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("replacement path mode = %v, want symbolic link", info.Mode())
	}
	contents, readErr := os.ReadFile(protected)
	if readErr != nil {
		t.Fatalf("protected target must remain readable: %v", readErr)
	}
	if string(contents) != "keep me" {
		t.Fatalf("protected target changed to %q", contents)
	}
}
