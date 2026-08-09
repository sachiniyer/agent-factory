package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func TestInstanceData_ProjectsConsumedRelocationClaimUntilUse(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "worktree")
	require.NoError(t, os.Mkdir(source, 0o755))
	info, err := os.Stat(source)
	require.NoError(t, err)
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("worktree stat did not expose directory identity")
	}

	gw, err := git.NewGitWorktreeFromStorage(
		root, source, "claim-checkpoint", "af/claim-checkpoint", "", false, true,
	)
	require.NoError(t, err)
	alternate := filepath.Join(root, "old-candidate")
	require.NoError(t, gw.RestoreRelocationRecovery(git.RelocationRecovery{
		State:         git.RelocationRecoveryMoveUnknown,
		AlternatePath: alternate,
		IdentityKnown: true,
		Device:        uint64(stat.Dev),
		Inode:         uint64(stat.Ino),
		FileType:      uint32(stat.Mode & syscall.S_IFMT),
	}))

	inst, err := NewInstance(InstanceOptions{Title: "claim-checkpoint", Path: root, Program: "claude"})
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	_, err = inst.ClaimWorktreeRelocationForRetry()
	require.NoError(t, err)

	recovery := inst.ToInstanceData().Worktree.RelocationRecovery
	if recovery == nil {
		t.Fatal("a shutdown checkpoint taken while a consumed claim is still in use must retain its identity")
	}
	if !recovery.IdentityKnown || recovery.Inode != uint64(stat.Ino) || recovery.AlternatePath != alternate {
		t.Fatalf("checkpointed claim = %+v, want inode %d and alternate %s", recovery, stat.Ino, alternate)
	}
}

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
				State:         git.RelocationRecoveryMoveUnknown,
				AlternatePath: filepath.Join(root, "original-source"),
				IdentityKnown: true,
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

func TestFromInstanceData_RelocationRecoveryLoadsRunningSessionInert(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_relocation_recovery_agent"
	shellName := agentName + shellTmuxSuffix
	data := deadInstanceData(t, Running, agentName, shellName)
	data.Worktree.RelocationRecovery = &GitWorktreeRelocationRecoveryData{
		State: git.RelocationRecoveryStalled,
	}

	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	previousRestore := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = previousRestore })

	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	require.False(t, restored.Started(),
		"an unresolved relocation identity must keep a running session inert on reload")
	require.Equal(t, 0, newSessions,
		"reload must not reconnect or respawn tmux against an unresolved worktree path")
	require.NotNil(t, restored.ToInstanceData().Worktree.RelocationRecovery,
		"the inert reload must retain the recovery record for a bounded retry")
}
