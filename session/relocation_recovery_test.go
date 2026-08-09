package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// v10228InstanceData is the persisted subset needed to pin the rollback wire
// contract. v1.0.228's GitWorktreeData ended at BranchCreatedByUs: it had no
// relocation_recovery field, so encoding/json must ignore that additive field
// when a current record is read after a rollback.
type v10228InstanceData struct {
	ID          string                `json:"id,omitempty"`
	Title       string                `json:"title"`
	Path        string                `json:"path"`
	Branch      string                `json:"branch"`
	Status      Status                `json:"status"`
	Liveness    Liveness              `json:"liveness,omitempty"`
	Program     string                `json:"program"`
	BackendType string                `json:"backend_type,omitempty"`
	Worktree    v10228GitWorktreeData `json:"worktree"`
	Tabs        []TabData             `json:"tabs,omitempty"`
}

type v10228GitWorktreeData struct {
	RepoPath          string `json:"repo_path"`
	WorktreePath      string `json:"worktree_path"`
	SessionName       string `json:"session_name"`
	BranchName        string `json:"branch_name"`
	BaseCommitSHA     string `json:"base_commit_sha"`
	ExternalWorktree  bool   `json:"external_worktree,omitempty"`
	BranchCreatedByUs *bool  `json:"branch_created_by_us,omitempty"`
}

func TestInstanceData_LoadsV10228WorktreeWithoutInventingRecovery(t *testing.T) {
	createdByUs := true
	root := t.TempDir()
	legacy := v10228InstanceData{
		ID:       "v10228-recovery-compat",
		Title:    "archived",
		Path:     root,
		Branch:   "af/archived",
		Status:   Archived,
		Liveness: LiveArchived,
		Program:  "claude",
		Worktree: v10228GitWorktreeData{
			RepoPath:          filepath.Join(root, "repo"),
			WorktreePath:      filepath.Join(root, "archive"),
			SessionName:       "archived",
			BranchName:        "af/archived",
			BranchCreatedByUs: &createdByUs,
		},
	}

	payload, err := json.Marshal(legacy)
	require.NoError(t, err)
	var current InstanceData
	require.NoError(t, json.Unmarshal(payload, &current))
	require.Nil(t, current.Worktree.RelocationRecovery,
		"a v1.0.228 record has no recovery field; absence must remain absence")

	restored, err := FromInstanceData(current)
	require.NoError(t, err)
	require.Equal(t, legacy.Worktree.WorktreePath, restored.GetWorktreePath())
	require.False(t, restored.gitWorktree.HasUnresolvedRelocation())

	emptyPayload := strings.Replace(
		string(payload),
		`"branch_created_by_us":true`,
		`"branch_created_by_us":true,"relocation_recovery":{}`,
		1,
	)
	require.NotEqual(t, string(payload), emptyPayload, "fixture insertion must succeed")
	var explicitEmpty InstanceData
	require.NoError(t, json.Unmarshal([]byte(emptyPayload), &explicitEmpty))
	require.NotNil(t, explicitEmpty.Worktree.RelocationRecovery,
		"an explicit empty record must not collapse into absence")
	_, err = FromInstanceData(explicitEmpty)
	require.ErrorContains(t, err, "move recovery alternate path is empty",
		"zero-valued recovery state must fail closed rather than authorize the path")
}

func TestInstanceData_CurrentRecoveryRecordIsReadableByV10228Shape(t *testing.T) {
	root := t.TempDir()
	worktreePath := filepath.Join(root, "archive")
	require.NoError(t, os.Mkdir(worktreePath, 0o755))
	info, err := os.Stat(worktreePath)
	require.NoError(t, err)
	stat, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)

	gw, err := git.NewGitWorktreeFromStorage(
		filepath.Join(root, "repo"), worktreePath, "archived", "af/archived", "", false, true,
	)
	require.NoError(t, err)
	require.NoError(t, gw.RestoreRelocationRecovery(git.RelocationRecovery{
		State:         git.RelocationRecoveryClaimStale,
		IdentityKnown: true,
		Device:        uint64(stat.Dev),
		Inode:         uint64(stat.Ino),
		FileType:      uint32(stat.Mode & syscall.S_IFMT),
	}))
	inst, err := NewInstance(InstanceOptions{Title: "archived", Path: root, Program: "claude"})
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	inst.SetStatusForTest(Archived)

	payload, err := json.Marshal(inst.ToInstanceData().ForStorage())
	require.NoError(t, err)
	require.Contains(t, string(payload), `"relocation_recovery"`,
		"precondition: the current writer must emit the additive recovery record")

	var legacy v10228InstanceData
	require.NoError(t, json.Unmarshal(payload, &legacy),
		"v1.0.228 used encoding/json without DisallowUnknownFields")
	require.Equal(t, worktreePath, legacy.Worktree.WorktreePath)
	require.Equal(t, "af/archived", legacy.Worktree.BranchName)
	require.NotNil(t, legacy.Worktree.BranchCreatedByUs)
	require.True(t, *legacy.Worktree.BranchCreatedByUs)
}

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
