package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/log"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingExec wraps countingExec and additionally captures every
// new-session command string, so tests can assert on the spawned program.
func recordingExec(alive map[string]bool, newSessions *int, spawns *[]string) cmd_test.MockCmdExec {
	inner := countingExec(alive, newSessions)
	return cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if strings.Contains(cmd.String(), "new-session") {
				*spawns = append(*spawns, cmd.String())
			}
			return inner.Run(cmd)
		},
		OutputFunc: inner.Output,
	}
}

// deadPaneAwareExec teaches an executor to answer the pane-row reads the
// waiting teardown path (CloseAndWaitForPaneExit) makes — list-panes and
// display-message — instead of countingExec's blanket "content", which parses
// as an invalid pane pid and turns every teardown confirmation into a
// refusal. Sessions in `existing` (kept in step with kill/new-session) answer
// with a held dead-pane row; absent ones answer with tmux's exit-1
// "can't find session" diagnostic so absence classifies as determinate empty.
func deadPaneAwareExec(existing map[string]bool, inner cmd_test.MockCmdExec) cmd_test.MockCmdExec {
	state := map[string]bool{}
	for k, v := range existing {
		state[k] = v
	}
	var missing *exec.ExitError
	if err := exec.Command("sh", "-c", "exit 1").Run(); err != nil {
		missing, _ = err.(*exec.ExitError)
	}
	nameOf := func(cmd *exec.Cmd) string {
		// `-t` names the target for list-panes/display-message/kill-session;
		// `-s` names it only for new-session — for list-panes `-s` is a
		// flagless "session format" switch that must not eat the next arg.
		takeValue := !strings.Contains(cmd.String(), "list-panes")
		for i, a := range cmd.Args {
			switch {
			case a == "-t" && i+1 < len(cmd.Args):
				return strings.TrimSuffix(strings.TrimPrefix(cmd.Args[i+1], "="), ":")
			case a == "-s" && takeValue && i+1 < len(cmd.Args):
				return strings.TrimSuffix(strings.TrimPrefix(cmd.Args[i+1], "="), ":")
			case strings.HasPrefix(a, "-t="):
				return strings.TrimSuffix(strings.TrimPrefix(a, "-t="), ":")
			case strings.HasPrefix(a, "-s=") && takeValue:
				return strings.TrimSuffix(strings.TrimPrefix(a, "-s="), ":")
			}
		}
		return ""
	}
	return cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			if err := inner.Run(cmd); err != nil {
				return err
			}
			s := cmd.String()
			switch {
			case strings.Contains(s, "new-session"):
				state[nameOf(cmd)] = true
			case strings.Contains(s, "kill-session"):
				delete(state, nameOf(cmd))
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			s := cmd.String()
			if !strings.Contains(s, "list-panes") && !strings.Contains(s, "display-message") {
				return inner.Output(cmd)
			}
			name := nameOf(cmd)
			if state[name] {
				return []byte("999999|1|\n"), nil
			}
			if missing == nil {
				return nil, errors.New("can't find session: " + name)
			}
			missing.Stderr = []byte("can't find session: " + name)
			return nil, missing
		},
	}
}

// lostInstanceForRecover loads a Lost instance through the production path
// (FromInstanceData → Start(false) → #970 guard, no re-spawn) with an
// EXISTING worktree directory, ready for Recover.
func lostInstanceForRecover(t *testing.T, agentName, shellName string, exec cmd_test.MockCmdExec) *Instance {
	t.Helper()
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	data := deadInstanceData(t, Lost, agentName, shellName)
	require.NoError(t, os.MkdirAll(data.Worktree.WorktreePath, 0755))
	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	require.Equal(t, Lost, restored.GetStatus())
	return restored
}

// TestRecover_RespawnsLostSession: the daemon's explicit Recover (#1108 PR 2)
// re-spawns a Lost instance's tmux session in its worktree — the operation the
// #970 guard forbids at load time — and flips the instance Running like a
// fresh create. The spawned program must carry the resolved-program injection
// (#1132 choke-point) exactly once.
func TestRecover_RespawnsLostSession(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_recover_agent"
	shellName := agentName + tmuxTabSeparator + shellTabName
	var newSessions int
	var spawns []string
	exec := recordingExec(map[string]bool{}, &newSessions, &spawns)
	restored := lostInstanceForRecover(t, agentName, shellName, exec)

	require.NoError(t, restored.Recover())

	assert.Greater(t, newSessions, 0, "Recover must re-spawn the missing tmux session")
	assert.Equal(t, Running, restored.GetStatus(),
		"a recovered session is booting its program: Running, like a fresh create")
	assert.True(t, restored.TabAlive(0), "the agent session must exist server-side after Recover")

	// The #1132 choke-point injected claude's --plugin-dir into the spawn —
	// exactly once, recomputed from the clean persisted Program (repeated
	// attempts must never accumulate flags).
	require.NotEmpty(t, spawns)
	agentSpawn := spawns[0]
	assert.Equal(t, 1, strings.Count(agentSpawn, "--plugin-dir"),
		"resolved-program injection must appear exactly once in the spawn: %s", agentSpawn)
	require.NotEmpty(t, restored.RuntimeProgram(),
		"Recover respawns a real process, so it must record the command it launched (#5066)")
	assert.Equal(t, resolveLaunchProgramForInstance(restored).command, restored.RuntimeProgram(),
		"the recorded command must be the respawn's resolved base, not the bare persisted Program")
}

func TestRecover_UnverifiedReattachClearsRuntimeProgram(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_recover_recreated_same_name"
	shellName := agentName + tmuxTabSeparator + shellTabName
	var newSessions int
	exec := countingExec(map[string]bool{agentName: true, shellName: true}, &newSessions)
	restored := lostInstanceForRecover(t, agentName, shellName, exec)
	restored.setRuntimeProgram("/old/claude")
	stale := restored.ObserveRuntimeProgram()

	require.NoError(t, restored.Recover())
	require.Zero(t, newSessions, "the fixture must exercise name-only reattachment, not verified respawn")
	require.Empty(t, restored.RuntimeProgram(),
		"a recovered tmux name cannot prove that the pane is the runtime whose command was persisted")
	require.False(t, restored.RuntimeProgramEvidenceCurrent(stale))
}

func TestRespawnForAccountSwap_StartsFreshConversation(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	// The scoped default-command that proves restored panes launch inside the
	// replacement account is written by the daemon alone (#4731); this test
	// stands in for it.
	t.Cleanup(tmux.SetLaunchCapabilityOwnerForTest(true))
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{tmux.ProgramClaude: "claude"}
	require.NoError(t, config.SaveConfig(cfg))
	_, err := agentaccount.Register(home, tmux.ProgramClaude, "work")
	require.NoError(t, err)

	const agentName = "af_account_swap_agent"
	var newSessions int
	var spawns []string
	restored := lostInstanceForRecover(t, agentName, agentName+tmuxTabSeparator+shellTabName,
		recordingExec(map[string]bool{}, &newSessions, &spawns))
	restored.mu.Lock()
	restored.Tabs[1].Kind = TabKindProcess
	restored.Tabs[1].Command = "git status --short"
	restored.Tabs[1].tmux.SetProgram("git status --short")
	restored.mu.Unlock()
	restored.Path = initTempGitRepo(t)
	restored.SetLimitReached(time.Time{})
	require.NoError(t, restored.BeginLimitResume())
	require.NoError(t, restored.ValidateAccountSwap("work"))
	_, err = restored.SelectAccountAutomatically("", "work")
	require.NoError(t, err)

	require.NoError(t, restored.RespawnForAccountSwap())
	require.NotEmpty(t, spawns)
	agentSpawn := spawns[0]
	require.NotContains(t, agentSpawn, "--continue",
		"the previous account's conversation store must never be resumed under the replacement account")
	require.Contains(t, agentSpawn, "--session-id",
		"the replacement claude identity must receive an explicit fresh conversation")
	require.Contains(t, strings.Join(spawns, "\n"), "__af-session-env-exec-account-environment",
		"restored process panes must launch inside the replacement account environment")
	assert.Equal(t, "claude", restored.RuntimeProgram(),
		"an account-swap respawn launches a replacement process and must record its resolved command (#5066)")
}

// A relocation probe that reaches its deadline latches the worktree as stalled
// even though it has not yet created two pathname candidates. The Lost loop must
// honor that latch before os.Stat or rebuild touches the same filesystem: those
// unbounded calls can wedge the daemon's sole status/self-healing goroutine.
//
// Since #5295 the refusal is not the first answer: Recover first tries the
// designed consumer — the bounded relocation-claim machinery — to resolve the
// record itself. The refusal below therefore comes one step later, when the
// claim's own bounded identity probe cannot answer either (injected here as
// the deadline a still-stalled filesystem would produce). The wedge property
// the latch exists for is unchanged: nothing unbounded ever touches the
// filesystem.
func TestRecover_RefusesWorktreeAfterRelocationProbeTimeout(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_recover_relocation_stall"
	shellName := agentName + tmuxTabSeparator + shellTabName
	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	restored := lostInstanceForRecover(t, agentName, shellName, exec)

	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "git"), []byte("#!/bin/sh\nexec sleep 300\n"), 0o755))
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Cleanup(sessiongit.SetLocalGitTimeoutForTest(100 * time.Millisecond))

	worktree, err := restored.GetGitWorktree()
	require.NoError(t, err)
	moveErr := worktree.MoveWorktree(filepath.Join(t.TempDir(), "archive"))
	require.ErrorIs(t, moveErr, context.DeadlineExceeded,
		"the setup must trip the bounded pre-move probe")
	recovery, hasRecovery := worktree.GetRelocationRecovery()
	require.True(t, hasRecovery,
		"every latched relocation stall must materialize recovery state; absent cannot mean safe")
	assert.Equal(t, sessiongit.RelocationRecoveryStalled, recovery.State)
	storedRecovery := restored.ToInstanceData().Worktree.RelocationRecovery
	require.NotNil(t, storedRecovery, "the stall latch must survive the daemon's durable snapshot")
	assert.Equal(t, sessiongit.RelocationRecoveryStalled, storedRecovery.State)

	// The filesystem is still not answering: the claim's bounded identity
	// probe of the recorded path hits the same deadline the pre-move probe
	// did, so resolution fails closed and the refusal stands — carrying the
	// state-unknown sentinel the retry loop classifies as environmental.
	primary, _, _ := worktree.RelocationSnapshot()
	t.Cleanup(sessiongit.SetRelocationIdentityErrorForTest(primary, context.DeadlineExceeded))

	err = restored.Recover()
	require.Error(t, err, "Lost recovery must refuse a worktree whose filesystem still cannot answer")
	assert.Contains(t, err.Error(), "unresolved worktree recovery state")
	assert.ErrorIs(t, err, sessiongit.ErrRelocateStateUnknown)
	assert.Equal(t, 0, newSessions, "the stalled worktree must be refused before any agent spawn")
}

// TestRecover_ResolvesClaimStaleWorktreeRelocation is the #5295 mechanism:
// an interrupted relocation (an ENOSPC-cut archive) leaves a durable
// claim_stale record naming which directory IS the worktree. Recover must
// resolve it through the bounded claim machinery and respawn — the session
// heals itself without an unrelated archive consuming the record first.
func TestRecover_ResolvesClaimStaleWorktreeRelocation(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_recover_claim_stale"
	shellName := agentName + tmuxTabSeparator + shellTabName
	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	restored := lostInstanceForRecover(t, agentName, shellName, exec)

	worktree, err := restored.GetGitWorktree()
	require.NoError(t, err)
	worktreePath, _, _ := worktree.RelocationSnapshot()

	// The record an ENOSPC-interrupted archive leaves: identity known, no
	// second candidate — exactly what RestoreRelocationRecovery reads back
	// after a daemon restart.
	var st syscall.Stat_t
	require.NoError(t, syscall.Stat(worktreePath, &st))
	require.NoError(t, worktree.RestoreRelocationRecovery(sessiongit.RelocationRecovery{
		State:         sessiongit.RelocationRecoveryClaimStale,
		IdentityKnown: true,
		Device:        uint64(st.Dev),
		Inode:         uint64(st.Ino),
		FileType:      uint32(st.Mode & syscall.S_IFMT),
	}))

	require.NoError(t, restored.Recover())

	assert.Equal(t, Running, restored.GetStatus())
	assert.Greater(t, newSessions, 0, "Recover must respawn once the record resolves")
	_, hasRecovery := worktree.GetRelocationRecovery()
	assert.False(t, hasRecovery, "a resolved record must be consumed, not left to refuse the next operation")
}

// TestRecover_SettlePersistsBeforeSpawn is the #5295-review regression: the
// respawn's settle is in-memory only, so the daemon installs a persist
// callback that MUST fire before the spawn — otherwise a daemon exit in the
// settle→boundary window reloads the stale record and strands the recovery.
// A notify failure must abort the spawn the same way.
func TestRecover_SettlePersistsBeforeSpawn(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_recover_settle_notify"
	shellName := agentName + tmuxTabSeparator + shellTabName
	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	restored := lostInstanceForRecover(t, agentName, shellName, exec)

	worktree, err := restored.GetGitWorktree()
	require.NoError(t, err)
	worktreePath, _, _ := worktree.RelocationSnapshot()

	var st syscall.Stat_t
	require.NoError(t, syscall.Stat(worktreePath, &st))
	require.NoError(t, worktree.RestoreRelocationRecovery(sessiongit.RelocationRecovery{
		State:         sessiongit.RelocationRecoveryClaimStale,
		IdentityKnown: true,
		Device:        uint64(st.Dev),
		Inode:         uint64(st.Ino),
		FileType:      uint32(st.Mode & syscall.S_IFMT),
	}))

	var notified int
	var spawnedAtNotify = -1
	restored.SetRelocationSettleNotify(func(*Instance) error {
		notified++
		spawnedAtNotify = newSessions
		return nil
	})
	require.NoError(t, restored.Recover())
	assert.Equal(t, 1, notified, "the settle persist must fire exactly once")
	assert.Equal(t, 0, spawnedAtNotify, "the notify must run BEFORE the spawn proceeds")

	// The same edge must fail closed: a notify error aborts the attempt
	// before any spawn.
	var newSessions2 int
	exec2 := countingExec(map[string]bool{}, &newSessions2)
	restored2 := lostInstanceForRecover(t, agentName+"2", shellName+"2", exec2)
	worktree2, err := restored2.GetGitWorktree()
	require.NoError(t, err)
	wp2, _, _ := worktree2.RelocationSnapshot()
	var st2 syscall.Stat_t
	require.NoError(t, syscall.Stat(wp2, &st2))
	require.NoError(t, worktree2.RestoreRelocationRecovery(sessiongit.RelocationRecovery{
		State:         sessiongit.RelocationRecoveryClaimStale,
		IdentityKnown: true,
		Device:        uint64(st2.Dev),
		Inode:         uint64(st2.Ino),
		FileType:      uint32(st2.Mode & syscall.S_IFMT),
	}))
	restored2.SetRelocationSettleNotify(func(*Instance) error {
		return fmt.Errorf("disk full")
	})
	err = restored2.Recover()
	require.Error(t, err, "a persist failure must fail the attempt, not spawn ahead of the durable clear")
	assert.Contains(t, err.Error(), "not yet durable")
	assert.Equal(t, 0, newSessions2, "no spawn may proceed on top of a non-durable settle")

	// #5295 review: the failed persist leaves the durable record in place, so
	// the in-memory fence MUST be re-armed — the next retry re-enters the
	// settle→persist checkpoint instead of skipping it and spawning on top of
	// the stale durable record.
	_, hasRecovery := worktree2.GetRelocationRecovery()
	assert.True(t, hasRecovery, "a failed settle-persist must re-arm the record so the next retry re-attempts the durable clear")

	var notified2 int
	restored2.SetRelocationSettleNotify(func(*Instance) error {
		notified2++
		return nil
	})
	require.NoError(t, restored2.Recover(), "a retry after the re-armed fence must settle and persist again")
	assert.Equal(t, 1, notified2, "the retry must re-enter the settle→persist checkpoint")
	assert.Equal(t, 2, newSessions2, "the retry's spawn proceeds only behind a durable clear")
	_, hasRecovery = worktree2.GetRelocationRecovery()
	assert.False(t, hasRecovery, "the settled record is consumed once durable")
}

// TestRecover_UnresolvableClaimKeepsRecordAndRefuses is the fail-closed half:
// when neither recorded path still holds the claimed identity — the worktree
// was replaced or deleted behind the record — Recover must refuse and KEEP
// the durable evidence rather than consuming it, so a later archive, kill,
// or identity-qualified retry still sees what was left.
func TestRecover_UnresolvableClaimKeepsRecordAndRefuses(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_recover_claim_gone"
	shellName := agentName + tmuxTabSeparator + shellTabName
	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	restored := lostInstanceForRecover(t, agentName, shellName, exec)

	worktree, err := restored.GetGitWorktree()
	require.NoError(t, err)

	// An identity no directory answers: a stale claim whose captured
	// (device, inode) belongs to nothing at either recorded path.
	require.NoError(t, worktree.RestoreRelocationRecovery(sessiongit.RelocationRecovery{
		State:         sessiongit.RelocationRecoveryClaimStale,
		IdentityKnown: true,
		Device:        ^uint64(0),
		Inode:         ^uint64(0),
		FileType:      uint32(syscall.S_IFDIR),
	}))

	err = restored.Recover()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "unresolved worktree recovery state")
	assert.ErrorIs(t, err, sessiongit.ErrRelocateStateUnknown,
		"a bare identity-mismatch verdict is definitive: it carries no errno, so the restore loop keeps it on the structural budget")
	assert.Equal(t, 0, newSessions, "no spawn may happen while the directory identity is unestablished")
	_, hasRecovery := worktree.GetRelocationRecovery()
	assert.True(t, hasRecovery, "the durable record must survive a failed resolution for the next operation")
}

// TestRecover_RefusesRepairOwedRelocationRecord pins the repair boundary:
// a two-candidate record (move_unknown — a timed-out `git worktree move`)
// can owe `git worktree repair` and submodule gitdir repair, work that
// belongs to the relocation-capable consumers. Recover must refuse WITHOUT
// touching the claim machinery: spawning into an un-repaired worktree, or
// settling the record that would drive the missing repair, both fail.
func TestRecover_RefusesRepairOwedRelocationRecord(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_recover_move_unknown"
	shellName := agentName + tmuxTabSeparator + shellTabName
	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	restored := lostInstanceForRecover(t, agentName, shellName, exec)

	worktree, err := restored.GetGitWorktree()
	require.NoError(t, err)
	worktreePath, _, _ := worktree.RelocationSnapshot()

	// The record a timed-out move leaves: two identity-qualified candidates.
	var st syscall.Stat_t
	require.NoError(t, syscall.Stat(worktreePath, &st))
	require.NoError(t, worktree.RestoreRelocationRecovery(sessiongit.RelocationRecovery{
		State:         sessiongit.RelocationRecoveryMoveUnknown,
		AlternatePath: filepath.Join(t.TempDir(), "moved-candidate"),
		IdentityKnown: true,
		Device:        uint64(st.Dev),
		Inode:         uint64(st.Ino),
		FileType:      uint32(st.Mode & syscall.S_IFMT),
	}))

	err = restored.Recover()
	require.Error(t, err, "a repair-owed record must refuse until a relocation-capable consumer resolves it")
	assert.Contains(t, err.Error(), "unresolved worktree recovery state")
	assert.Equal(t, 0, newSessions, "no spawn may happen while a move's repair obligation is unestablished")
	recovery, hasRecovery := worktree.GetRelocationRecovery()
	require.True(t, hasRecovery, "the move record must stay durable for the repair-capable consumer")
	assert.Equal(t, sessiongit.RelocationRecoveryMoveUnknown, recovery.State)
}

// TestRecover_RetryAfterFailureInjectsFlagsOnce: a failed first attempt (the
// outage still biting) must leave the instance retryable — still Lost, tmux
// refs intact — and the eventual successful spawn must still carry the
// injected flags exactly once.
func TestRecover_RetryAfterFailureInjectsFlagsOnce(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_recover_retry"
	shellName := agentName + tmuxTabSeparator + shellTabName
	var newSessions, ptyNewSessions int
	var spawns []string
	exec := recordingExec(map[string]bool{}, &newSessions, &spawns)

	// First new-session fails (server still hostile), later ones succeed.
	pty := failFirstNewSessionPty{t: t, cmdExec: exec, count: &ptyNewSessions}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	data := deadInstanceData(t, Lost, agentName, shellName)
	require.NoError(t, os.MkdirAll(data.Worktree.WorktreePath, 0755))
	restored, err := FromInstanceData(data)
	require.NoError(t, err)

	require.Error(t, restored.Recover(), "first attempt must surface the spawn failure")
	assert.Equal(t, Lost, restored.GetStatus(), "a failed recover leaves the session Lost")
	assert.True(t, restored.Started(), "a failed recover must keep the row killable")

	require.NoError(t, restored.Recover(), "retry must succeed once the server behaves")
	assert.Equal(t, Running, restored.GetStatus())
	for _, spawn := range spawns {
		if strings.Contains(spawn, agentName) && !strings.Contains(spawn, tmuxTabSeparator+shellTabName) {
			assert.Equal(t, 1, strings.Count(spawn, "--plugin-dir"),
				"every attempt recomputes injection from the clean Program: %s", spawn)
		}
	}
}

// TestRecover_FailsWithoutWorktree: a deleted worktree is the expected
// permanent-failure shape; Recover must name it (the restore loop's
// escalation log leans on this) and leave the session Lost and killable.
func TestRecover_FailsWithoutWorktree(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_recover_nowt"
	shellName := agentName + tmuxTabSeparator + shellTabName
	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	// Worktree path deliberately absent.
	data := deadInstanceData(t, Lost, agentName, shellName)
	require.NoError(t, os.RemoveAll(data.Worktree.WorktreePath))
	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	restored.SetRebuildTeardownNotify(func(*Instance) error { return nil })
	restored.SetRebuildTeardownRuntimesStop(func(*Instance) error { return nil })

	err = restored.Recover()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "worktree", "the failure must name the missing worktree")
	assert.Equal(t, 0, newSessions, "no spawn may happen without a worktree")
	assert.Equal(t, Lost, restored.GetStatus())
}

// TestRecover_FailsWithNonDirectoryWorktree: a regular file squatting on the
// persisted worktree path is not rebuildable — rebuild must never delete data
// af did not create — and stats fine, so it would otherwise skip the rebuild
// branch and retry into the tmux spawn-dir refusal until give-up (#5172 review).
// Recover must instead fail fast naming the cause, leave the session Lost, and
// never attempt a spawn.
func TestRecover_FailsWithNonDirectoryWorktree(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_recover_filewt"
	shellName := agentName + tmuxTabSeparator + shellTabName
	var newSessions int
	exec := countingExec(map[string]bool{}, &newSessions)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	// Worktree path exists but is a regular file, not a directory.
	data := deadInstanceData(t, Lost, agentName, shellName)
	require.NoError(t, os.RemoveAll(data.Worktree.WorktreePath))
	require.NoError(t, os.WriteFile(data.Worktree.WorktreePath, []byte("squatter"), 0o644))
	restored, err := FromInstanceData(data)
	require.NoError(t, err)

	err = restored.Recover()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a directory", "the failure must name the squatting file, not a generic spawn error")
	var wtErr *WorktreeUnavailableError
	assert.ErrorAs(t, err, &wtErr, "the permanent-failure shape the restore loop escalates on")
	assert.Equal(t, 0, newSessions, "no spawn may happen when the worktree path is not a directory")
	assert.Equal(t, Lost, restored.GetStatus())
}

func TestRecover_RebuildsMissingWorktreeFromExistingBranchAndResumesRecordedConversation(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	repoRoot := initTempGitRepo(t)
	gitOut(t, repoRoot, "config", "user.email", "test@test.com")
	gitOut(t, repoRoot, "config", "user.name", "test")
	gitOut(t, repoRoot, "commit", "--allow-empty", "-m", "initial")
	const branch = "af/lost-recover"
	gitOut(t, repoRoot, "branch", branch)

	const agentName = "af_recover_branch"
	shellName := agentName + tmuxTabSeparator + shellTabName
	const conversationID = "019f386f-7206-7fc2-803b-f7045e07a242"
	worktreePath := filepath.Join(t.TempDir(), "repo-lost-recover")
	branchCreatedByUs := true
	data := InstanceData{
		Title:    "lost-recover",
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
		},
		Worktree: GitWorktreeData{
			RepoPath:          repoRoot,
			WorktreePath:      worktreePath,
			SessionName:       "lost-recover",
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
	restored.SetRebuildTeardownNotify(func(*Instance) error { return nil })
	restored.SetRebuildTeardownRuntimesStop(func(*Instance) error { return nil })
	_, statErr := os.Stat(worktreePath)
	require.True(t, os.IsNotExist(statErr), "test setup should start with a missing worktree")

	require.NoError(t, restored.Recover())

	assert.Equal(t, Running, restored.GetStatus())
	assert.True(t, restored.TabAlive(0), "Recover must re-spawn the agent after rebuilding the worktree")
	require.DirExists(t, worktreePath)
	assert.Equal(t, branch, gitOut(t, worktreePath, "rev-parse", "--abbrev-ref", "HEAD"))

	var agentSpawn string
	for _, spawn := range spawns {
		if strings.Contains(spawn, agentName) && !strings.Contains(spawn, tmuxTabSeparator+shellTabName) {
			agentSpawn = spawn
			break
		}
	}
	require.NotEmpty(t, agentSpawn, "expected to record the agent new-session command")
	assert.Contains(t, agentSpawn, " resume "+conversationID,
		"Recover must resume the recorded Codex conversation, not the latest one")
	assert.NotContains(t, agentSpawn, "resume --last")

	saved := restored.ToInstanceData()
	require.NotNil(t, saved.Worktree.BranchCreatedByUs)
	assert.True(t, *saved.Worktree.BranchCreatedByUs,
		"rebuilding from the surviving branch must preserve original branch ownership")
}

func TestRecover_RebuildsBranchGoneWorktreeFromRecordedBaseAndResumesRecordedConversation(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	repoRoot := initTempGitRepo(t)
	gitOut(t, repoRoot, "config", "user.email", "test@test.com")
	gitOut(t, repoRoot, "config", "user.name", "test")
	gitOut(t, repoRoot, "commit", "--allow-empty", "-m", "base")
	baseSHA := gitOut(t, repoRoot, "rev-parse", "HEAD")
	require.NoError(t, os.WriteFile(filepath.Join(repoRoot, "later.txt"), []byte("later\n"), 0644))
	gitOut(t, repoRoot, "add", "later.txt")
	gitOut(t, repoRoot, "commit", "-m", "later")
	require.NotEqual(t, baseSHA, gitOut(t, repoRoot, "rev-parse", "HEAD"))

	const branch = "af/branch-gone"
	const agentName = "af_recover_branch_gone"
	shellName := agentName + tmuxTabSeparator + shellTabName
	const conversationID = "019f386f-7206-7fc2-803b-f7045e07a242"
	worktreePath := filepath.Join(t.TempDir(), "repo-branch-gone")
	branchCreatedByUs := false
	data := InstanceData{
		Title:    "branch-gone",
		Path:     repoRoot,
		Branch:   branch,
		Program:  tmux.ProgramClaude,
		Status:   Lost,
		TmuxName: agentName,
		Tabs: []TabData{
			{
				Name:     agentTabName,
				Kind:     TabKindAgent,
				TmuxName: agentName,
				Conversation: &AgentConversationData{
					Agent:       tmux.ProgramClaude,
					ID:          conversationID,
					CaptureKind: ConversationCaptureInjected,
				},
			},
			{Name: shellTabName, Kind: TabKindShell, TmuxName: shellName},
		},
		Worktree: GitWorktreeData{
			RepoPath:          repoRoot,
			WorktreePath:      worktreePath,
			SessionName:       "branch-gone",
			BranchName:        branch,
			BaseCommitSHA:     baseSHA,
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
	restored.SetRebuildTeardownNotify(func(*Instance) error { return nil })
	restored.SetRebuildTeardownRuntimesStop(func(*Instance) error { return nil })

	require.NoError(t, restored.Recover())

	assert.Equal(t, Running, restored.GetStatus())
	require.DirExists(t, worktreePath)
	assert.Equal(t, branch, gitOut(t, worktreePath, "rev-parse", "--abbrev-ref", "HEAD"))
	assert.Equal(t, baseSHA, gitOut(t, worktreePath, "rev-parse", "HEAD"),
		"branch-gone recovery should prefer the recorded base over current master")

	var agentSpawn string
	for _, spawn := range spawns {
		if strings.Contains(spawn, agentName) && !strings.Contains(spawn, tmuxTabSeparator+shellTabName) {
			agentSpawn = spawn
			break
		}
	}
	require.NotEmpty(t, agentSpawn, "expected to record the agent new-session command")
	assert.Contains(t, agentSpawn, "--resume "+conversationID)
	assert.NotContains(t, agentSpawn, "--continue")

	saved := restored.ToInstanceData()
	require.NotNil(t, saved.Worktree.BranchCreatedByUs)
	assert.True(t, *saved.Worktree.BranchCreatedByUs,
		"a branch recreated from recorded base is now owned by Agent Factory")
	assert.Equal(t, baseSHA, saved.Worktree.BaseCommitSHA)
}

func TestRecover_BranchGoneWithoutRecordedConversationDoesNotFreshDispatch(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	repoRoot := initTempGitRepo(t)
	gitOut(t, repoRoot, "config", "user.email", "test@test.com")
	gitOut(t, repoRoot, "config", "user.name", "test")
	gitOut(t, repoRoot, "commit", "--allow-empty", "-m", "base")
	baseSHA := gitOut(t, repoRoot, "rev-parse", "HEAD")

	const branch = "af/no-conversation"
	const agentName = "af_recover_no_conversation"
	shellName := agentName + tmuxTabSeparator + shellTabName
	worktreePath := filepath.Join(t.TempDir(), "repo-no-conversation")
	branchCreatedByUs := true
	data := InstanceData{
		Title:    "no-conversation",
		Path:     repoRoot,
		Branch:   branch,
		Program:  tmux.ProgramCodex,
		Status:   Lost,
		TmuxName: agentName,
		Tabs: []TabData{
			{Name: agentTabName, Kind: TabKindAgent, TmuxName: agentName},
			{Name: shellTabName, Kind: TabKindShell, TmuxName: shellName},
		},
		Worktree: GitWorktreeData{
			RepoPath:          repoRoot,
			WorktreePath:      worktreePath,
			SessionName:       "no-conversation",
			BranchName:        branch,
			BaseCommitSHA:     baseSHA,
			BranchCreatedByUs: &branchCreatedByUs,
		},
	}

	var newSessions int
	// deadPaneAwareExec so the stale-pane teardown — which now runs BEFORE the
	// rebuild — can confirm the absent sessions dead (blind) instead of
	// choking on countingExec's unparseable list-panes reply (#5371 review).
	tmuxExec := deadPaneAwareExec(map[string]bool{}, countingExec(map[string]bool{}, &newSessions))
	pty := persistPtyFactory{t: t, cmdExec: tmuxExec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, tmuxExec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	restored.SetRebuildTeardownNotify(func(*Instance) error { return nil })
	restored.SetRebuildTeardownRuntimesStop(func(*Instance) error { return nil })

	err = restored.Recover()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "recorded conversation id")
	assert.Equal(t, 0, newSessions, "branch-gone recovery must not dispatch a fresh agent without exact resume")
	_, statErr := os.Stat(worktreePath)
	assert.True(t, os.IsNotExist(statErr), "worktree must remain absent when exact conversation resume is unavailable")
	assert.Error(t, exec.Command("git", "-C", repoRoot, "show-ref", "--verify", "refs/heads/"+branch).Run(),
		"branch must not be recreated without exact conversation resume")
	assert.Equal(t, Lost, restored.GetStatus())
}

// TestRecover_RefusesNonLostAndTombstoned: Recover is for Lost sessions only —
// a live session must never be re-spawned over (adopt, never clobber), and a
// tombstoned record's only future is having its kill finished.
func TestRecover_RefusesNonLostAndTombstoned(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const agentName = "af_recover_guard"
	shellName := agentName + tmuxTabSeparator + shellTabName
	var newSessions int
	exec := countingExec(map[string]bool{agentName: true, shellName: true}, &newSessions)
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	live, err := FromInstanceData(deadInstanceData(t, Running, agentName, shellName))
	require.NoError(t, err)
	require.Error(t, live.Recover(), "a non-Lost session must be refused")

	tombstoned := lostInstanceForRecover(t, "af_recover_guard2", "af_recover_guard2"+tmuxTabSeparator+shellTabName,
		countingExec(map[string]bool{}, &newSessions))
	tombstoned.MarkUserKilled()
	require.Error(t, tombstoned.Recover(), "a tombstoned session must be refused")
}

// A committed rebuild whose stale-pane teardown is undischarged must keep the
// row Lost against ANY positive observation: a live answer can still be the
// pane bound to the deleted inode, which a status poll cannot tell apart from
// a fresh one. Lifting the row to Running/Ready would drop it out of the
// restore loop and strand the teardown (#5371 review: P1 live-status gate).
func TestTransition_RebuildTeardownPendingHoldsObservedLivenessLost(t *testing.T) {
	i := &Instance{Title: "pending", liveness: LiveLost}
	i.SetRebuildTeardownNotify(func(*Instance) error { return nil })
	require.NoError(t, i.SetRebuildTeardownPending(true))

	for _, observed := range []Liveness{LiveRunning, LiveReady, LiveLimitReached, LiveDead, LiveArchived} {
		require.NoError(t, i.Transition(ObserveLiveness(observed)))
		assert.Equal(t, LiveLost, i.GetLiveness(),
			"observed %v must not lift a pending-teardown row out of Lost", observed)
	}
	// A Lost observation is the poll's own mark — it still applies.
	require.NoError(t, i.Transition(ObserveLiveness(LiveLost)))
	assert.Equal(t, LiveLost, i.GetLiveness())

	// Even a liveness that slipped to Running before the flag was set is
	// pulled back: pending means the live answer is unverifiable.
	running := &Instance{Title: "pending-running", liveness: LiveRunning}
	running.SetRebuildTeardownNotify(func(*Instance) error { return nil })
	require.NoError(t, running.SetRebuildTeardownPending(true))
	require.NoError(t, running.Transition(ObserveLiveness(LiveReady)))
	assert.Equal(t, LiveLost, running.GetLiveness(),
		"a pending row must not keep a positive liveness it cannot verify")

	// Once the obligation is discharged, observations apply again.
	require.NoError(t, i.SetRebuildTeardownPending(false))
	require.NoError(t, i.Transition(ObserveLiveness(LiveRunning)))
	assert.Equal(t, LiveRunning, i.GetLiveness())
}

// The durable contract behind the flag: every transition must land on disk
// through the installed notify before the dependent step runs, and a refused
// write leaves the flag SET — the obligation may exist either way (unrecorded
// commit or unrecorded discharge), so "set" is the only honest post-error
// state (#5371 review: P2 durable discharge).
func TestSetRebuildTeardownPending_CheckpointContract(t *testing.T) {
	var transitions []bool
	i := &Instance{Title: "ckpt"}
	i.SetRebuildTeardownNotify(func(inst *Instance) error {
		transitions = append(transitions, inst.RebuildTeardownPending())
		return nil
	})
	require.NoError(t, i.SetRebuildTeardownPending(true))
	require.NoError(t, i.SetRebuildTeardownPending(false))
	assert.Equal(t, []bool{true, false}, transitions,
		"the notify must observe the flag already in its new state")

	failure := errors.New("disk wedged")
	i.SetRebuildTeardownNotify(func(*Instance) error { return failure })
	require.ErrorIs(t, i.SetRebuildTeardownPending(true), failure)
	assert.True(t, i.RebuildTeardownPending(),
		"a failed set-checkpoint keeps the flag: the rebuild committed either way")
	require.ErrorIs(t, i.SetRebuildTeardownPending(false), failure)
	assert.True(t, i.RebuildTeardownPending(),
		"a failed clear-checkpoint restores the flag: the discharge never landed")
}

// The clear-checkpoint must precede the first replacement spawn, or a daemon
// exit in the window between them reloads `true` and kills the fresh panes
// (#5371 review). The notify sees zero new-session calls at clear time, and a
// refused clear leaves the row Lost with nothing spawned.
func TestRecover_RebuildTeardownClearPrecedesRespawn(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	repoRoot := initTempGitRepo(t)
	gitOut(t, repoRoot, "config", "user.email", "test@test.com")
	gitOut(t, repoRoot, "config", "user.name", "test")
	gitOut(t, repoRoot, "commit", "--allow-empty", "-m", "initial")
	const branch = "af/lost-recover-clear"
	gitOut(t, repoRoot, "branch", branch)

	const agentName = "af_recover_clearckpt"
	const conversationID = "019f386f-7206-7fc2-803b-f7045e07a242"
	worktreePath := filepath.Join(t.TempDir(), "repo-lost-recover-clear")
	branchCreatedByUs := true
	data := InstanceData{
		Title:    "lost-recover-clear",
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
			SessionName:       "lost-recover-clear",
			BranchName:        branch,
			BranchCreatedByUs: &branchCreatedByUs,
		},
	}

	var newSessions int
	var spawns []string
	exec := deadPaneAwareExec(map[string]bool{agentName: true},
		recordingExec(map[string]bool{agentName: true}, &newSessions, &spawns))
	pty := persistPtyFactory{t: t, cmdExec: exec}
	prev := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
	}
	t.Cleanup(func() { restoreTmuxSession = prev })

	restored, err := FromInstanceData(data)
	require.NoError(t, err)
	require.Equal(t, Lost, restored.GetStatus())

	clearSpawnCount := -1
	var notifyCalls int
	restored.SetRebuildTeardownNotify(func(inst *Instance) error {
		notifyCalls++
		if !inst.RebuildTeardownPending() {
			clearSpawnCount = newSessions
		}
		return nil
	})
	restored.SetRebuildTeardownRuntimesStop(func(*Instance) error { return nil })
	require.NoError(t, restored.Recover())
	require.Equal(t, 2, notifyCalls, "set at rebuild-commit + clear after confirmed teardown")
	assert.Equal(t, 0, clearSpawnCount,
		"the cleared obligation must be durable before any replacement launches")
	assert.Equal(t, Running, restored.GetStatus())
	assert.Equal(t, 1, newSessions, "the agent respawned after the checkpointed clear")
}

// The teardown obligation must be durable BEFORE the rebuilt directory is:
// armed after the rebuild commits, a daemon exit in the gap reloads a present
// worktree beside a false pending flag — `rebuilt` re-derives false from
// os.Stat, the teardown gate skips, and a surviving stale pane reattaches to
// the deleted inode. The notify observes the arm; the worktree must not yet
// exist when it fires (#5371 review).
func TestRecover_RebuildTeardownArmsBeforeRebuildCommits(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	repoRoot := initTempGitRepo(t)
	gitOut(t, repoRoot, "config", "user.email", "test@test.com")
	gitOut(t, repoRoot, "config", "user.name", "test")
	gitOut(t, repoRoot, "commit", "--allow-empty", "-m", "initial")
	const branch = "af/lost-recover-arm"
	gitOut(t, repoRoot, "branch", branch)

	const agentName = "af_recover_arm"
	const conversationID = "019f386f-7206-7fc2-803b-f7045e07a242"
	worktreePath := filepath.Join(t.TempDir(), "repo-lost-recover-arm")
	branchCreatedByUs := true
	data := InstanceData{
		Title:    "lost-recover-arm",
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
			SessionName:       "lost-recover-arm",
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

	var dirExistedAtArm bool
	restored.SetRebuildTeardownNotify(func(inst *Instance) error {
		if inst.RebuildTeardownPending() {
			_, statErr := os.Stat(worktreePath)
			dirExistedAtArm = statErr == nil
		}
		return nil
	})
	restored.SetRebuildTeardownRuntimesStop(func(*Instance) error { return nil })
	require.NoError(t, restored.Recover())

	assert.False(t, dirExistedAtArm,
		"the teardown obligation must be armed while the worktree is still missing — "+
			"armed after the commit, a daemon exit in the gap reloads pending=false beside a present directory")
	assert.Equal(t, Running, restored.GetStatus())
	require.DirExists(t, worktreePath)
}

// A pending tab-cleanup handle owns a tmux session that is no longer on the
// roster — the rebuild teardown must retire it through the WAITING close
// (pane process tree observed exited) before any replacement spawns, because
// Close's asynchronous reaper can still be running in the deleted worktree
// when the new panes launch (#5371 review).
func TestRecover_RebuiltTeardownWaitsOnPendingTabCleanup(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	repoRoot := initTempGitRepo(t)
	gitOut(t, repoRoot, "config", "user.email", "test@test.com")
	gitOut(t, repoRoot, "config", "user.name", "test")
	gitOut(t, repoRoot, "commit", "--allow-empty", "-m", "initial")
	const branch = "af/lost-recover-handle"
	gitOut(t, repoRoot, "branch", branch)

	const agentName = "af_recover_handle"
	const staleHandleName = "af_recover_handle__oldtab"
	const conversationID = "019f386f-7206-7fc2-803b-f7045e07a242"
	worktreePath := filepath.Join(t.TempDir(), "repo-lost-recover-handle")
	branchCreatedByUs := true
	data := InstanceData{
		Title:    "lost-recover-handle",
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
			SessionName:       "lost-recover-handle",
			BranchName:        branch,
			BranchCreatedByUs: &branchCreatedByUs,
		},
	}

	var newSessions int
	var spawns, commands []string
	inner := deadPaneAwareExec(map[string]bool{agentName: true, staleHandleName: true},
		recordingExec(map[string]bool{agentName: true, staleHandleName: true}, &newSessions, &spawns))
	exec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			commands = append(commands, cmd.String())
			return inner.Run(cmd)
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			commands = append(commands, cmd.String())
			return inner.Output(cmd)
		},
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
	restored.SetPendingTabCleanupForTest([]TabCleanupData{{TabID: "oldtab", TmuxName: staleHandleName}})

	require.NoError(t, restored.Recover())

	// panePID's display-message runs only on the CloseAndWaitForPaneExit path:
	// it is the observable difference between "kill-session fired, reaper left
	// running" and "the leftover's process tree was observed gone". The handle
	// then retires only after that confirmed teardown.
	assert.True(t, commandIncludesSession(commands, "display-message", staleHandleName),
		"the pending cleanup handle must be torn down through the pane-exit wait path, not Close's async reaper")
	assert.Empty(t, restored.PendingTabCleanup(),
		"the handle retires once its session is confirmed gone")
	assert.Equal(t, Running, restored.GetStatus())
}

// Every respawn entry point that can reach the rebuild teardown must install
// the durable checkpoint the transitions fire. With none installed, a
// transition that skipped persistence would be invisible to a restart — so
// the obligation itself is refused rather than allowed to go unrecorded
// (#5371 review).
func TestRecover_RebuildTeardownRefusesWithoutPersistCallback(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	repoRoot := initTempGitRepo(t)
	gitOut(t, repoRoot, "config", "user.email", "test@test.com")
	gitOut(t, repoRoot, "config", "user.name", "test")
	gitOut(t, repoRoot, "commit", "--allow-empty", "-m", "initial")
	const branch = "af/lost-recover-nonotify"
	gitOut(t, repoRoot, "branch", branch)

	const agentName = "af_recover_nonotify"
	const conversationID = "019f386f-7206-7fc2-803b-f7045e07a242"
	worktreePath := filepath.Join(t.TempDir(), "repo-lost-recover-nonotify")
	branchCreatedByUs := true
	data := InstanceData{
		Title:    "lost-recover-nonotify",
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
			SessionName:       "lost-recover-nonotify",
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

	err = restored.Recover()
	require.Error(t, err,
		"without the durable checkpoint the rebuild must not arm a silently-unrecorded obligation")
	assert.Contains(t, err.Error(), "durable checkpoint")
	assert.Equal(t, Lost, restored.GetStatus())
	assert.Equal(t, 0, newSessions)
	assert.True(t, restored.RebuildTeardownPending(),
		"the obligation stays armed — a restart must find it set, matching the unrecorded reality")
}

// A row that still owes the post-rebuild teardown must not rebind its tmux
// sessions on load: every bound name answers inside the deleted inode, and
// Start(false) would reattach the stale pane before the teardown owner
// retries — regardless of the persisted liveness (#5371 review). The row
// loads materialized (started) so its owner stays eligible, with the
// obligation and liveness intact.
func TestFromInstanceData_RebuildTeardownPendingLoadsWithoutRestoringTmux(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	repoRoot := initTempGitRepo(t)
	gitOut(t, repoRoot, "config", "user.email", "test@test.com")
	gitOut(t, repoRoot, "config", "user.name", "test")
	gitOut(t, repoRoot, "commit", "--allow-empty", "-m", "initial")
	const branch = "af/pending-load"
	gitOut(t, repoRoot, "branch", branch)

	const agentName = "af_pending_load"
	worktreePath := filepath.Join(t.TempDir(), "repo-pending-load")
	require.NoError(t, os.MkdirAll(worktreePath, 0755))
	branchCreatedByUs := true

	for _, tc := range []struct {
		name     string
		status   Status
		liveness Liveness
		want     Liveness
	}{
		{name: "lost", status: Lost, want: LiveLost},
		{name: "limit", liveness: LiveLimitReached, want: LiveLimitReached},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var newSessions int
			var spawns, commands []string
			inner := deadPaneAwareExec(map[string]bool{agentName: true},
				recordingExec(map[string]bool{agentName: true}, &newSessions, &spawns))
			exec := cmd_test.MockCmdExec{
				RunFunc: func(cmd *exec.Cmd) error {
					commands = append(commands, cmd.String())
					return inner.Run(cmd)
				},
				OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
					commands = append(commands, cmd.String())
					return inner.Output(cmd)
				},
			}
			pty := persistPtyFactory{t: t, cmdExec: exec}
			prev := restoreTmuxSession
			restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
				return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
			}
			t.Cleanup(func() { restoreTmuxSession = prev })

			data := InstanceData{
				Title:                  "pending-load-" + tc.name,
				Path:                   repoRoot,
				Branch:                 branch,
				Program:                tmux.ProgramCodex,
				Status:                 tc.status,
				Liveness:               tc.liveness,
				TmuxName:               agentName,
				RebuildTeardownPending: true,
				Tabs: []TabData{
					{Name: agentTabName, Kind: TabKindAgent, TmuxName: agentName},
				},
				Worktree: GitWorktreeData{
					RepoPath:          repoRoot,
					WorktreePath:      worktreePath,
					SessionName:       "pending-load-" + tc.name,
					BranchName:        branch,
					BranchCreatedByUs: &branchCreatedByUs,
				},
			}

			restored, err := FromInstanceData(data)
			require.NoError(t, err)
			assert.True(t, restored.Started(),
				"the pending row must stay materialized so its owner loop remains eligible")
			assert.Equal(t, tc.want, restored.GetLiveness(),
				"the persisted liveness is preserved for the loop that owns the retry")
			assert.True(t, restored.RebuildTeardownPending())
			assert.Empty(t, commands,
				"a pending-teardown row must not touch tmux on load — its names answer inside the deleted inode")
		})
	}
}

// A process tab whose tmux session already vanished is not "stopped by the
// rebuild": nothing was observed and nothing was terminated, so the tab must
// stay inert rather than claim a stopped outcome this recovery did not
// produce (#5371 review).
func TestRecover_RebuiltTeardownAbsentProcessTabStaysInert(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	repoRoot := initTempGitRepo(t)
	gitOut(t, repoRoot, "config", "user.email", "test@test.com")
	gitOut(t, repoRoot, "config", "user.name", "test")
	gitOut(t, repoRoot, "commit", "--allow-empty", "-m", "initial")
	const branch = "af/lost-recover-proctab"
	gitOut(t, repoRoot, "branch", branch)

	const agentName = "af_recover_proctab"
	const procName = "af_recover_proctab__process"
	const conversationID = "019f386f-7206-7fc2-803b-f7045e07a242"
	worktreePath := filepath.Join(t.TempDir(), "repo-lost-recover-proctab")
	branchCreatedByUs := true
	data := InstanceData{
		Title:    "lost-recover-proctab",
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
			{Name: processTabName, Kind: TabKindProcess, TmuxName: procName},
		},
		Worktree: GitWorktreeData{
			RepoPath:          repoRoot,
			WorktreePath:      worktreePath,
			SessionName:       "lost-recover-proctab",
			BranchName:        branch,
			BranchCreatedByUs: &branchCreatedByUs,
		},
	}

	// The agent's session is the stale survivor; the process tab's session is
	// already gone — absent from the alive map.
	var newSessions int
	var spawns []string
	exec := deadPaneAwareExec(map[string]bool{agentName: true},
		recordingExec(map[string]bool{agentName: true}, &newSessions, &spawns))
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

	var procTab *Tab
	for _, tab := range restored.Tabs {
		if tab.Kind == TabKindProcess {
			procTab = tab
		}
	}
	require.NotNil(t, procTab)
	assert.Nil(t, procTab.Exit,
		"an already-absent process session invents no outcome — absence is not evidence of how the command ended")
}
