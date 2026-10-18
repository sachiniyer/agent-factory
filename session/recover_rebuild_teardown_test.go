package session

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An arm checkpoint that fails leaves the in-memory flag true — and the NEXT
// matching SetRebuildTeardownPending(true) must retry the durable write
// rather than treating the equal value as backed. Otherwise the second
// attempt rebuilds on top of storage that still reads pending=false: a daemon
// exit after that rebuild commits reloads a row that cannot tell the teardown
// is owed (#5371 review).
func TestRecover_RebuildTeardownRetriesFailedArmCheckpoint(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	repoRoot := initTempGitRepo(t)
	gitOut(t, repoRoot, "config", "user.email", "test@test.com")
	gitOut(t, repoRoot, "config", "user.name", "test")
	gitOut(t, repoRoot, "commit", "--allow-empty", "-m", "initial")
	const branch = "af/lost-recover-armretry"
	gitOut(t, repoRoot, "branch", branch)

	const agentName = "af_recover_armretry"
	const conversationID = "019f386f-7206-7fc2-803b-f7045e07a242"
	worktreePath := filepath.Join(t.TempDir(), "repo-lost-recover-armretry")
	branchCreatedByUs := true
	data := InstanceData{
		Title:    "lost-recover-armretry",
		Path:     repoRoot,
		Branch:   branch,
		Program:  tmux.ProgramCodex,
		Status:   Lost,
		TmuxName: agentName,
		Tabs: []TabData{
			{
				Name:     agentTabName,
				Kind:     TabKindAgent,
				TmuxName: agentName,
				Conversation: &AgentConversationData{
					Agent:       tmux.ProgramCodex,
					ID:          conversationID,
					CaptureKind: ConversationCaptureCodexRollout,
				},
			},
		},
		Worktree: GitWorktreeData{
			RepoPath:          repoRoot,
			WorktreePath:      worktreePath,
			SessionName:       "lost-recover-armretry",
			BranchName:        branch,
			BranchCreatedByUs: &branchCreatedByUs,
		},
	}

	var newSessions int
	var spawns []string
	exec := deadPaneAwareExec(map[string]bool{},
		recordingExec(map[string]bool{}, &newSessions, &spawns))
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	require.Equal(t, Lost, restored.GetStatus())

	var armCalls int
	var failArm bool
	restored.SetRebuildTeardownNotify(func(inst *Instance) error {
		if inst.RebuildTeardownPending() {
			armCalls++
			if failArm {
				return errors.New("persist backend down")
			}
		}
		return nil
	})

	// Attempt 1: the arm checkpoint refuses, so the whole recover refuses —
	// nothing may rebuild on top of an obligation that is not durable yet.
	failArm = true
	require.Error(t, restored.Recover(), "a failed arm checkpoint refuses the rebuild, it does not proceed unrecorded")
	assert.Equal(t, Lost, restored.GetStatus())
	assert.Equal(t, 1, armCalls)

	// Attempt 2: the flag is still true in memory, but its value was never
	// made durable — the matching arm call must fire the notifier again
	// BEFORE this attempt's rebuild commits, not skip it as already-backed.
	failArm = false
	require.NoError(t, restored.Recover())
	assert.Equal(t, 2, armCalls,
		"the retry must re-checkpoint the owed arm before rebuilding — the equal in-memory value is not the durable one")
	assert.Equal(t, Running, restored.GetStatus())
	assert.Equal(t, 1, newSessions)
}

// A conclusive-but-blind close ("session already gone") proves nothing about
// what the vanished session spawned: tmux forgot the ancestry and the
// AF_SESSION marker sweep is vacuous for a survivor that carries none. The
// rebuild teardown aggregates that blindness and owes the worktree ONE
// occupancy check before replacements spawn — a detached process with its cwd
// inside the deleted inode must keep the row Lost and retryable, not watch a
// replacement start next to it. teardownTabs' gate, same shape (#5371 review).
func TestRecover_RebuiltTeardownOccupancyGateBlocksDetachedSurvivor(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the occupancy evidence under test is the /proc cwd scan")
	}
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	repoRoot := initTempGitRepo(t)
	gitOut(t, repoRoot, "config", "user.email", "test@test.com")
	gitOut(t, repoRoot, "config", "user.name", "test")
	gitOut(t, repoRoot, "commit", "--allow-empty", "-m", "initial")
	const branch = "af/lost-recover-occupant"
	gitOut(t, repoRoot, "branch", branch)

	const agentName = "af_recover_occupant"
	worktreePath := filepath.Join(t.TempDir(), "repo-lost-recover-occupant")

	// A detached survivor: a process holding a cwd INSIDE what will become the
	// rebuilt path, with no AF_SESSION marker — invisible to the marker sweep,
	// visible only to the occupancy scan. Create the dir, chdir the process
	// into it, then delete it so its /proc/<pid>/cwd reads "<path> (deleted)".
	require.NoError(t, os.MkdirAll(worktreePath, 0755))
	survivor := exec.Command("sleep", "60")
	survivor.Dir = worktreePath
	require.NoError(t, survivor.Start())
	t.Cleanup(func() { _ = survivor.Process.Kill() })
	require.NoError(t, os.RemoveAll(worktreePath))
	cwd, err := os.Readlink("/proc/" + strconv.Itoa(survivor.Process.Pid) + "/cwd")
	require.NoError(t, err)
	require.True(t, strings.HasSuffix(cwd, " (deleted)"),
		"the planted survivor's cwd must be the deleted inode, got %q", cwd)

	branchCreatedByUs := true
	data := InstanceData{
		Title:    "lost-recover-occupant",
		Path:     repoRoot,
		Branch:   branch,
		Program:  tmux.ProgramCodex,
		Status:   Lost,
		TmuxName: agentName,
		Tabs: []TabData{
			{Name: agentTabName, Kind: TabKindAgent, TmuxName: agentName},
		},
		Worktree: GitWorktreeData{
			RepoPath:          repoRoot,
			WorktreePath:      worktreePath,
			SessionName:       "lost-recover-occupant",
			BranchName:        branch,
			BranchCreatedByUs: &branchCreatedByUs,
		},
	}

	// The agent session is ALREADY gone — its close will be conclusive-blind
	// rather than observed. In the alive map it is absent.
	var newSessions int
	var spawns []string
	cmdExec := deadPaneAwareExec(map[string]bool{},
		recordingExec(map[string]bool{}, &newSessions, &spawns))
	pty := persistPtyFactory{t: t, cmdExec: cmdExec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, cmdExec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	require.Equal(t, Lost, restored.GetStatus())
	restored.SetRebuildTeardownNotify(func(*Instance) error { return nil })

	err = restored.Recover()
	require.Error(t, err,
		"a detached survivor in the deleted inode must refuse the teardown — reporting, not spawning next to it")
	assert.Contains(t, err.Error(), "still working inside",
		"the refusal must name the occupancy evidence, not a generic teardown failure")
	assert.Equal(t, Lost, restored.GetStatus())
	assert.Equal(t, 0, newSessions, "no replacement may spawn while an occupant is unaccounted for")
	assert.True(t, restored.RebuildTeardownPending(),
		"the obligation stays armed — the row is retryable, exactly like teardownTabs' ErrPaneMayBeLive")
}

// While the rebuild teardown is owed, user-facing runtime mutations must
// refuse: a spawned tab or a handoff binds a NEW runtime inside the worktree,
// and the pending teardown snapshots names, not vintages — it would kill the
// fresh runtime together with the genuinely stale ones. The owning loops
// (limit resume, lost recover) stay admitted — they are who retires the
// obligation (#5371 review).
func TestRebuildTeardownPending_FencesRuntimeMutations(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	data := deadInstanceData(t, Running, "af_pending_mut", "af_pending_mut"+tmuxTabSeparator+shellTabName)
	data.Liveness = LiveLimitReached
	data.RebuildTeardownPending = true

	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	require.True(t, restored.Started(), "the pending row loads materialized for its owner loop")
	assert.Equal(t, LiveLimitReached, restored.GetLiveness())

	err = restored.ValidateRuntimeAction(RuntimeActionHandoff)
	require.Error(t, err, "a handoff would bind a fresh runtime the pending teardown then kills")
	assert.Contains(t, err.Error(), "teardown")

	err = restored.TabSpawnBlocked()
	require.Error(t, err, "a spawned tab is a fresh binding inside the worktree the teardown owns")
	assert.Contains(t, err.Error(), "teardown")

	// The owners stay admitted: the limit scheduler consumes the obligation
	// through RuntimeActionResumeLimit, exactly the loop this row is for.
	require.NoError(t, restored.ValidateRuntimeAction(RuntimeActionResumeLimit),
		"the owning retry must remain eligible — fencing it would strand the obligation")
}
