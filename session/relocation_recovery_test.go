package session

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestInstanceData_RoundTripsInterruptedRelocationRecovery(t *testing.T) {
	createdByUs := true
	root := t.TempDir()
	data := InstanceData{
		ID:       "relocation-recovery",
		Title:    "archived",
		Status:   Archived,
		Liveness: LiveArchived,
		Worktree: GitWorktreeData{
			RepoPath:          filepath.Join(root, "repo"),
			WorktreePath:      filepath.Join(root, "archive-destination"),
			SessionName:       "archived",
			BranchName:        "af/archived",
			BranchCreatedByUs: &createdByUs,
			RelocationRecovery: &GitWorktreeRelocationRecoveryData{
				AlternatePath: filepath.Join(root, "original-source"),
				Device:        17,
				Inode:         23,
				FileType:      0o040000,
			},
		},
	}

	payload, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var loaded InstanceData
	if err := json.Unmarshal(payload, &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	instance, err := FromInstanceData(loaded)
	if err != nil {
		t.Fatalf("FromInstanceData: %v", err)
	}
	roundTrip := instance.ToInstanceData().Worktree.RelocationRecovery
	if roundTrip == nil {
		t.Fatal("interrupted relocation recovery handle was dropped across storage reload")
	}
	if *roundTrip != *data.Worktree.RelocationRecovery {
		t.Fatalf("relocation recovery = %+v, want %+v", *roundTrip, *data.Worktree.RelocationRecovery)
	}
}
