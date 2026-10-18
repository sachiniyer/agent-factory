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
	"time"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
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
	restored.SetRebuildTeardownRuntimesStop(func(*Instance) error { return nil })

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
	restored.SetRebuildTeardownRuntimesStop(func(*Instance) error { return nil })

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

// The pending gate must hold for EVERY liveness write, not only the poll's
// ObserveLiveness: SetLimitReachedAtEpoch is the poll's other door — a stale
// pane idling on a usage-limit banner — and ClearLimitReached is the resume
// path's. Both must coerce to LiveLost while the obligation stands, or the
// row drops out of the retry loop with the teardown still owed (#5371
// review). The account-level wall evidence still records — attribution is
// not the thing being fenced; the row's liveness is.
func TestRebuildTeardownPending_HoldsLimitObservationsLost(t *testing.T) {
	notify := func(*Instance) error { return nil }

	i := &Instance{Title: "pending-limit", liveness: LiveLost}
	i.SetRebuildTeardownNotify(notify)
	require.NoError(t, i.SetRebuildTeardownPending(true))
	epoch := i.LifecycleView().StateEpoch

	resetAt := time.Now().Add(time.Hour)
	require.True(t, i.SetLimitReachedAtEpoch(resetAt, epoch),
		"the limit observation applies its account evidence even under the gate")
	assert.Equal(t, LiveLost, i.GetLiveness(),
		"a limit banner from the stale pane must not lift the row out of Lost")

	// The unconditional twin takes the same door.
	i.SetLimitReached(resetAt.Add(time.Hour))
	assert.Equal(t, LiveLost, i.GetLiveness())

	// And the resume-path clear on a pending limit-parked row: it may retire
	// the wall's fields but must not write the row off Lost either.
	limitParked := &Instance{Title: "pending-limit-parked", liveness: LiveLimitReached,
		limitAgent: "claude", limitAccount: "work", limitResetAt: resetAt}
	limitParked.SetRebuildTeardownNotify(notify)
	require.NoError(t, limitParked.SetRebuildTeardownPending(true))
	limitParked.ClearLimitReached()
	assert.Equal(t, LiveLost, limitParked.GetLiveness(),
		"ClearLimitReached on a pending row must not resurrect it as Running")
	assert.True(t, limitParked.limitResetAt.IsZero())
}

// The teardown owes everything a session owns, not only what tmux carries:
// the supervised VS Code editor is a daemon-owned process rooted at the
// worktree with no tab.tmux and no AF_SESSION marker, and a post-worktree
// hook runner is the same shape. The teardown must run the installed stop
// BEFORE the occupancy gate — an editor still holding the deleted inode is
// the occupant that gate would otherwise refuse as a stranger — and a failed
// or missing stop must leave the row Lost, the obligation armed, and nothing
// spawned (#5371 review).
func TestRecover_RebuiltTeardownStopsSupervisedRuntimes(t *testing.T) {
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
	const branch = "af/lost-recover-runtimes"
	gitOut(t, repoRoot, "branch", branch)

	const agentName = "af_recover_runtimes"
	worktreePath := filepath.Join(t.TempDir(), "repo-lost-recover-runtimes")

	// The daemon-owned editor stand-in: a process holding a cwd inside what
	// will become the rebuilt path. The runtimes-stop callback's job is to end
	// it; the occupancy gate must then see the worktree clear.
	require.NoError(t, os.MkdirAll(worktreePath, 0755))
	editor := exec.Command("sleep", "60")
	editor.Dir = worktreePath
	require.NoError(t, editor.Start())
	editorKilled := false
	t.Cleanup(func() {
		if !editorKilled {
			_ = editor.Process.Kill()
		}
	})
	require.NoError(t, os.RemoveAll(worktreePath))

	branchCreatedByUs := true
	data := InstanceData{
		Title:    "lost-recover-runtimes",
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
			SessionName:       "lost-recover-runtimes",
			BranchName:        branch,
			BranchCreatedByUs: &branchCreatedByUs,
		},
	}

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
	restored.SetRebuildTeardownNotify(func(*Instance) error { return nil })

	var stopCalls int
	var pendingAtStop bool
	restored.SetRebuildTeardownRuntimesStop(func(inst *Instance) error {
		stopCalls++
		pendingAtStop = inst.RebuildTeardownPending()
		// The stop does what the daemon's does: end the supervised process.
		_ = editor.Process.Kill()
		editorKilled = true
		_ = editor.Wait()
		return nil
	})

	require.NoError(t, restored.Recover())
	assert.Equal(t, 1, stopCalls)
	assert.True(t, pendingAtStop,
		"the runtimes stop runs while the obligation is still owed — before discharge")
	assert.Equal(t, Running, restored.GetStatus())
	assert.Equal(t, 1, newSessions,
		"the worktree must answer occupancy clear only because the editor was stopped first")
}

func TestRecover_RebuiltTeardownRefusesWhenRuntimesStopFails(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	repoRoot := initTempGitRepo(t)
	gitOut(t, repoRoot, "config", "user.email", "test@test.com")
	gitOut(t, repoRoot, "config", "user.name", "test")
	gitOut(t, repoRoot, "commit", "--allow-empty", "-m", "initial")
	const branch = "af/lost-recover-stopfail"
	gitOut(t, repoRoot, "branch", branch)

	const agentName = "af_recover_stopfail"
	worktreePath := filepath.Join(t.TempDir(), "repo-lost-recover-stopfail")
	branchCreatedByUs := true
	data := InstanceData{
		Title:    "lost-recover-stopfail",
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
			SessionName:       "lost-recover-stopfail",
			BranchName:        branch,
			BranchCreatedByUs: &branchCreatedByUs,
		},
	}

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

	// A failed stop: an editor the supervisor cannot confirm dead must hold
	// the row Lost with the obligation armed — spawning beside it is the
	// exposure this gate exists against.
	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	restored.SetRebuildTeardownNotify(func(*Instance) error { return nil })
	stopErr := errors.New("editor reap timeout")
	restored.SetRebuildTeardownRuntimesStop(func(*Instance) error { return stopErr })

	err = restored.Recover()
	require.ErrorIs(t, err, stopErr)
	assert.Equal(t, Lost, restored.GetStatus())
	assert.True(t, restored.RebuildTeardownPending(),
		"the obligation stays armed — the next attempt retries the stop")
	assert.Equal(t, 0, newSessions)

	// A MISSING install fails closed the same way: reaching the teardown
	// without the callback means an entry point was missed. The obligation is
	// still owed from the attempt above, so the retry refuses before it
	// closes anything.
	restored.SetRebuildTeardownRuntimesStop(nil)
	err = restored.Recover()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no supervised-runtime stop")
	assert.Equal(t, Lost, restored.GetStatus())
	assert.Equal(t, 0, newSessions)
}

// The pending gate fences NEW transactions, not the committed manual
// transaction's own retry — the scheduled BeginManualAccountSwap retry and
// the explicit ValidateHandoffRuntimeAction retry are the respawn paths that
// FINISH the teardown. A limit-parked committed swap wedged at the rebuild
// teardown must reach its respawn, or the durable transaction is stranded
// for good (#5371 review).
func TestRebuildTeardownPending_CommittedSwapRetryAdmitted(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	data := deadInstanceData(t, Running, "af_swap_retry", "af_swap_retry"+tmuxTabSeparator+shellTabName)
	data.Liveness = LiveLimitReached
	data.RebuildTeardownPending = true
	data.Account = "personal"
	data.Program = "claude"
	data.PendingAccountSwap = &AccountSwapData{
		Manual: true, From: "work", To: "personal",
	}

	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	require.True(t, restored.RebuildTeardownPending())

	// The committed transaction's own retry — both spellings — is admitted to
	// the respawn that discharges the obligation.
	require.NoError(t, restored.ValidateHandoffRuntimeAction("claude", "personal"),
		"the committed manual swap's own retry must reach the teardown's respawn")
	require.NoError(t, restored.BeginManualAccountSwap(),
		"the scheduled retry must reach the same respawn")

	// A request naming a DIFFERENT account is a new transaction and stays
	// fenced — the pending gate still does its job for everything but the
	// owner.
	other := deadInstanceData(t, Running, "af_swap_other", "af_swap_other"+tmuxTabSeparator+shellTabName)
	other.Liveness = LiveLimitReached
	other.RebuildTeardownPending = true
	other.Account = "personal"
	other.Program = "claude"
	other.PendingAccountSwap = &AccountSwapData{
		Manual: true, From: "work", To: "personal",
	}
	restored2, err := FromInstanceData(other)
	require.NoError(t, err)
	err = restored2.ValidateHandoffRuntimeAction("claude", "work")
	require.Error(t, err, "a foreign target is a new transaction, not the committed retry")
}

// The rebuild teardown must cover EVERY tmux name the row binds, not only the
// agent's: a sibling that stayed live — for example, one reattached at load
// before a different sibling's respawn was refused — is a pane in the deleted
// inode too, and reattaching it binds the same stale process the rebuild was
// built to retire. Two live shells here stand in for "any number of live
// siblings" (#5371 review, carried from #5376).
func TestRecover_RebuiltTeardownKillsEverySurvivingSibling(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	repoRoot := initTempGitRepo(t)
	gitOut(t, repoRoot, "config", "user.email", "test@test.com")
	gitOut(t, repoRoot, "config", "user.name", "test")
	gitOut(t, repoRoot, "commit", "--allow-empty", "-m", "initial")
	const branch = "af/lost-recover-siblings"
	gitOut(t, repoRoot, "branch", branch)

	const agentName = "af_recover_siblings"
	shellName := agentName + tmuxTabSeparator + shellTabName
	shell2Name := agentName + tmuxTabSeparator + "term-2"
	const conversationID = "019f386f-7206-7fc2-803b-f7045e07a242"
	worktreePath := filepath.Join(t.TempDir(), "repo-lost-recover-siblings")
	branchCreatedByUs := true
	data := InstanceData{
		Title:    "lost-recover-siblings",
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
			{Name: shellTabName, Kind: TabKindShell, TmuxName: shellName},
			{Name: "term-2", Kind: TabKindShell, TmuxName: shell2Name},
		},
		Worktree: GitWorktreeData{
			RepoPath:          repoRoot,
			WorktreePath:      worktreePath,
			SessionName:       "lost-recover-siblings",
			BranchName:        branch,
			BranchCreatedByUs: &branchCreatedByUs,
		},
	}

	// All three sessions are the live survivors in the deleted inode.
	var newSessions int
	var spawns, kills []string
	inner := deadPaneAwareExec(map[string]bool{agentName: true, shellName: true, shell2Name: true},
		recordingExec(map[string]bool{agentName: true, shellName: true, shell2Name: true}, &newSessions, &spawns))
	exec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "kill-session") {
				kills = append(kills, cmd.String())
			}
			return inner.Run(cmd)
		},
		OutputFunc: inner.Output,
	}
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	require.Equal(t, Lost, restored.GetStatus())
	restored.SetRebuildTeardownNotify(func(*Instance) error { return nil })
	restored.SetRebuildTeardownRuntimesStop(func(*Instance) error { return nil })

	require.NoError(t, restored.Recover())
	assert.Equal(t, Running, restored.GetStatus())

	killed := strings.Join(kills, "\n")
	for _, name := range []string{agentName, shellName, shell2Name} {
		assert.Contains(t, killed, name,
			"every bound survivor in the deleted inode must be killed before respawn, not only the agent")
	}
	assert.Equal(t, 3, newSessions, "agent + both shells must be freshly spawned inside the rebuilt worktree")
}
