package daemon

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
)

func TestGhostWorktreeRemovable_RestoresOwnershipBeforeRollbackFence(t *testing.T) {
	root := t.TempDir()
	archive := filepath.Join(root, "archive")
	if err := os.Mkdir(archive, 0o755); err != nil {
		t.Fatalf("create archived worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(archive, "work.txt"), []byte("user work"), 0o644); err != nil {
		t.Fatalf("write archived worktree: %v", err)
	}
	info, err := os.Stat(archive)
	if err != nil {
		t.Fatalf("stat archived worktree: %v", err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("archived worktree stat has no syscall identity")
	}
	originalExternal := false
	originalBranchCreated := true
	originalStartupUnknown := false
	projectedBranchCreated := false
	data := &session.InstanceData{
		Worktree: session.GitWorktreeData{
			RepoPath:          filepath.Join(root, "missing-repo"),
			WorktreePath:      archive,
			SessionName:       "ghost",
			BranchName:        "af/ghost",
			ExternalWorktree:  true,
			BranchCreatedByUs: &projectedBranchCreated,
			RelocationRecovery: &session.GitWorktreeRelocationRecoveryData{
				State:                       git.RelocationRecoveryCleanupStalled,
				IdentityKnown:               true,
				Device:                      uint64(stat.Dev),
				Inode:                       uint64(stat.Ino),
				FileType:                    uint32(stat.Mode & syscall.S_IFMT),
				OriginalExternalWorktree:    &originalExternal,
				OriginalBranchCreatedByUs:   &originalBranchCreated,
				OriginalStartupStateUnknown: &originalStartupUnknown,
			},
		},
	}

	if !ghostWorktreeRemovable(data) {
		t.Fatal("the rollback-safe external-worktree projection was read before the recovery record restored AF ownership")
	}
	restored, err := data.RestoreRelocationRecoveryOriginals()
	if err != nil {
		t.Fatalf("restore original ghost ownership: %v", err)
	}
	if restored.Worktree.BranchCreatedByUs == nil || !*restored.Worktree.BranchCreatedByUs {
		t.Fatal("ghost recovery did not restore original branch provenance")
	}
	state, err, _ := ghostCleanupWorktree(data, "ghost", nil)
	if err != nil || state != git.CleanupSettled {
		t.Fatalf("ghost cleanup did not retry the restored identity-qualified claim; state=%v err=%v", state, err)
	}
	if _, err := os.Stat(archive); !os.IsNotExist(err) {
		t.Fatalf("ghost cleanup left the claimed archive behind: %v", err)
	}
}
