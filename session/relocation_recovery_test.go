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
	ID          string   `json:"id,omitempty"`
	Title       string   `json:"title"`
	Path        string   `json:"path"`
	Branch      string   `json:"branch"`
	Status      Status   `json:"status"`
	Liveness    Liveness `json:"liveness,omitempty"`
	Program     string   `json:"program"`
	BackendType string   `json:"backend_type,omitempty"`
	// StartupStateUnknown is an existing v1.0.228 fail-closed load fence. The
	// current writer sets it while recovery is unresolved so rollback cannot
	// reconnect a runtime through a path the old reader cannot qualify.
	StartupStateUnknown bool                  `json:"startup_state_unknown,omitempty"`
	Worktree            v10228GitWorktreeData `json:"worktree"`
	Tabs                []TabData             `json:"tabs,omitempty"`
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

// v10244WorktreeData is the immediately preceding release's recovery wire
// shape. It understands relocation states, but intentionally has no additive
// cleanup_lifecycle field and therefore must receive a recognized fail-closed
// State when it reads a record written by this version.
type v10244WorktreeData struct {
	RepoPath           string                        `json:"repo_path"`
	WorktreePath       string                        `json:"worktree_path"`
	ExternalWorktree   bool                          `json:"external_worktree,omitempty"`
	BranchCreatedByUs  *bool                         `json:"branch_created_by_us,omitempty"`
	RelocationRecovery *v10244RelocationRecoveryData `json:"relocation_recovery,omitempty"`
}

type v10244RelocationRecoveryData struct {
	State                       git.RelocationRecoveryState `json:"state,omitempty"`
	AlternatePath               string                      `json:"alternate_path"`
	IdentityKnown               bool                        `json:"identity_known,omitempty"`
	Device                      uint64                      `json:"device"`
	Inode                       uint64                      `json:"inode"`
	FileType                    uint32                      `json:"file_type"`
	OriginalExternalWorktree    *bool                       `json:"original_external_worktree,omitempty"`
	OriginalBranchCreatedByUs   *bool                       `json:"original_branch_created_by_us,omitempty"`
	OriginalStartupStateUnknown *bool                       `json:"original_startup_state_unknown,omitempty"`
}

type v10244InstanceData struct {
	StartupStateUnknown bool               `json:"startup_state_unknown,omitempty"`
	Worktree            v10244WorktreeData `json:"worktree"`
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
	require.ErrorContains(t, err, "rollback safety metadata is missing",
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
		State:             git.RelocationRecoveryCleanupReady,
		IdentityKnown:     true,
		Device:            uint64(stat.Dev),
		Inode:             uint64(stat.Ino),
		FileType:          uint32(stat.Mode & syscall.S_IFMT),
		CleanupGeneration: "0123456789abcdef0123456789abcdef",
	}))
	inst, err := NewInstance(InstanceOptions{Title: "archived", Path: root, Program: "claude"})
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	inst.SetStatusForTest(Archived)

	payload, err := json.Marshal(inst.ToInstanceData().ForStorage())
	require.NoError(t, err)
	require.Contains(t, string(payload), `"relocation_recovery"`,
		"precondition: the current writer must emit the additive recovery record")
	require.Contains(t, string(payload), `"cleanup_generation"`,
		"precondition: the non-reusable cleanup identity must survive the rollback projection")
	require.Contains(t, string(payload), `"cleanup_lifecycle":"cleanup_ready"`,
		"the new lifecycle must be additive rather than an unknown State value")

	var previous v10244InstanceData
	require.NoError(t, json.Unmarshal(payload, &previous))
	require.NotNil(t, previous.Worktree.RelocationRecovery)
	require.Equal(t, git.RelocationRecoveryClaimStale, previous.Worktree.RelocationRecovery.State,
		"the immediately preceding reader must receive a state its decoder accepts")
	require.True(t, previous.Worktree.RelocationRecovery.IdentityKnown)
	previousRecovery := previous.Worktree.RelocationRecovery
	require.NotNil(t, previousRecovery.OriginalExternalWorktree)
	require.NotNil(t, previousRecovery.OriginalBranchCreatedByUs)
	require.NotNil(t, previousRecovery.OriginalStartupStateUnknown)
	// Model v1.0.244's load and its repo-gone restore consuming claim_stale.
	// Even after that old restore clears the record, cleanup ownership must stay
	// inert rather than reverting to the current binary's real ownership values.
	previous.Worktree.ExternalWorktree = *previousRecovery.OriginalExternalWorktree
	previous.Worktree.BranchCreatedByUs = previousRecovery.OriginalBranchCreatedByUs
	previous.StartupStateUnknown = *previousRecovery.OriginalStartupStateUnknown
	previous.Worktree.RelocationRecovery = nil
	require.True(t, previous.Worktree.ExternalWorktree,
		"a downgraded retry must still treat the archive as user-owned")
	require.False(t, *previous.Worktree.BranchCreatedByUs,
		"a downgraded retry must not authorize branch deletion")
	require.True(t, previous.StartupStateUnknown,
		"a downgraded retry must remain inert after consuming the compatibility record")

	var legacy v10228InstanceData
	require.NoError(t, json.Unmarshal(payload, &legacy),
		"v1.0.228 used encoding/json without DisallowUnknownFields")
	require.Equal(t, worktreePath, legacy.Worktree.WorktreePath)
	require.Equal(t, "af/archived", legacy.Worktree.BranchName)
	require.True(t, legacy.StartupStateUnknown,
		"rollback must keep the session inert because v1.0.228 cannot qualify the recovery path")
	require.True(t, legacy.Worktree.ExternalWorktree,
		"rollback must make v1.0.228's destructive worktree cleanup a no-op")
	require.NotNil(t, legacy.Worktree.BranchCreatedByUs)
	require.False(t, *legacy.Worktree.BranchCreatedByUs,
		"rollback must not authorize v1.0.228 to delete the branch")

	var current InstanceData
	require.NoError(t, json.Unmarshal(payload, &current))
	restored, err := FromInstanceData(current)
	require.NoError(t, err)
	restoredRecovery, ok := restored.gitWorktree.GetRelocationRecovery()
	require.True(t, ok)
	require.Equal(t, git.RelocationRecoveryCleanupReady, restoredRecovery.State,
		"the current reader must recover the additive cleanup lifecycle")
	require.False(t, restored.StartupStateUnknown(),
		"the current reader must remove the rollback-only inert fence")
	require.False(t, restored.IsExternalWorktree(),
		"the current reader must restore the actual linked-worktree ownership")
	require.True(t, restored.gitWorktree.BranchCreatedByUs(),
		"the current reader must restore the actual branch provenance")
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
	externalWorktree := false
	startupStateUnknown := false
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
				State:                       git.RelocationRecoveryMoveUnknown,
				AlternatePath:               filepath.Join(root, "original-source"),
				IdentityKnown:               true,
				Device:                      17,
				Inode:                       23,
				FileType:                    0o040000,
				OriginalExternalWorktree:    &externalWorktree,
				OriginalBranchCreatedByUs:   &createdByUs,
				OriginalStartupStateUnknown: &startupStateUnknown,
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
	require.Equal(t, data.Worktree.RelocationRecovery, roundTrip)
}

func TestFromInstanceData_RelocationRecoveryLoadsRunningSessionInert(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_relocation_recovery_agent"
	shellName := agentName + shellTmuxSuffix
	data := deadInstanceData(t, Running, agentName, shellName)
	originalExternalWorktree := data.Worktree.ExternalWorktree
	originalBranchCreatedByUs := false
	if data.Worktree.BranchCreatedByUs != nil {
		originalBranchCreatedByUs = *data.Worktree.BranchCreatedByUs
	}
	originalStartupStateUnknown := data.StartupStateUnknown
	data.Worktree.RelocationRecovery = &GitWorktreeRelocationRecoveryData{
		State:                       git.RelocationRecoveryStalled,
		OriginalExternalWorktree:    &originalExternalWorktree,
		OriginalBranchCreatedByUs:   &originalBranchCreatedByUs,
		OriginalStartupStateUnknown: &originalStartupStateUnknown,
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
