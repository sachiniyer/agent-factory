package session

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/sachiniyer/agent-factory/session/git"
)

func TestLocalBackendKill_PostCommitRelocationRefusalIsUnknown(t *testing.T) {
	root := t.TempDir()
	worktree := filepath.Join(root, "archived")
	if err := os.Mkdir(worktree, 0o755); err != nil {
		t.Fatalf("create archived worktree: %v", err)
	}
	info, err := os.Stat(worktree)
	if err != nil {
		t.Fatalf("stat archived worktree: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("archived worktree stat has no syscall identity")
	}
	gw, err := git.NewGitWorktreeFromStorage(
		filepath.Join(root, "missing-repo"), worktree, "post-commit", "af/post-commit", "", false, true,
	)
	if err != nil {
		t.Fatalf("restore worktree handle: %v", err)
	}
	if err := gw.RestoreRelocationRecovery(git.RelocationRecovery{
		State:         git.RelocationRecoveryClaimStale,
		IdentityKnown: true,
		Device:        uint64(stat.Dev),
		Inode:         uint64(stat.Ino),
		FileType:      uint32(stat.Mode & syscall.S_IFMT),
	}); err != nil {
		t.Fatalf("restore unresolved cleanup recovery: %v", err)
	}
	claim, err := gw.ClaimRelocationSource()
	if err != nil {
		t.Fatalf("claim unresolved cleanup recovery: %v", err)
	}
	if err := gw.PrepareRelocationClaimForCleanup(claim); err != nil {
		t.Fatalf("prepare cleanup-ready recovery: %v", err)
	}
	if err := os.Rename(worktree, worktree+"-original"); err != nil {
		t.Fatalf("move archive after pre-tombstone admission: %v", err)
	}
	if err := os.Mkdir(worktree, 0o755); err != nil {
		t.Fatalf("install same-path replacement: %v", err)
	}
	instance := &Instance{Title: "post-commit", gitWorktree: gw}

	err = (&LocalBackend{}).Kill(instance)
	if !TeardownStateUnknown(err) {
		t.Fatalf("a post-tombstone relocation refusal must retain the durable row as unknown; got %v", err)
	}
}
