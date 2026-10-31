package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

const poisonFlagged = "codex -c check_for_update_on_startup=false"

// fakeCodexOnPath places an executable named "codex" on PATH and returns its
// path. Manual account swaps preflight the resolved launch command at the
// worktree, so the codex binary must be discoverable even though the mock
// tmux exec never actually runs it.
func fakeCodexOnPath(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	codexPath := filepath.Join(binDir, "codex")
	require.NoError(t, os.WriteFile(codexPath, []byte("#!/bin/sh\n"), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return codexPath
}

// flaggedPaneSwapInstanceForRespawn is flaggedPaneSwapOsInstance with mock
// tmux dependencies so RespawnForAccountSwap can drive a real (mock-backed)
// new-session spawn. It builds a limit-blocked, automatic-swap codex session
// whose agent pane already carries af's own injected codex startup-update-check
// flag — recorded with its generatedArgs declaration — and whose durable
// runtime-program evidence is empty (the pane-fallback state). extraAccounts
// names additional codex accounts to register alongside the default "work".
func flaggedPaneSwapInstanceForRespawn(t *testing.T, extraAccounts ...string) (
	*Instance, string, *int, *[]string,
) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{tmux.ProgramCodex: tmux.ProgramCodex}
	require.NoError(t, config.SaveConfig(cfg))
	target := registerAccount(t, tmux.ProgramCodex, "work")
	for _, name := range extraAccounts {
		registerAccount(t, tmux.ProgramCodex, name)
	}
	inst := accountSwapTestInstanceWithProgram(tmux.ProgramCodex)
	inst.Path = initTempGitRepo(t)
	gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)

	var newSessions int
	var spawns []string
	executor := recordingExec(map[string]bool{}, &newSessions, &spawns)
	pty := persistPtyFactory{t: t, cmdExec: executor}

	ts := tmux.NewTmuxSessionWithDeps(inst.Title, poisonFlagged, pty, executor)
	setLaunchProgram(ts, poisonFlagged, sessionenv.AccountLaunchProof{
		GeneratedArgs: []string{"-c", "check_for_update_on_startup=false"},
	})
	inst.mu.Lock()
	inst.Tabs = []*Tab{newAgentTab(ts)}
	inst.runtimeProgram = ""
	if inst.runtimePID != 0 || inst.runtimeStartID != 0 {
		inst.runtimeEvidenceGeneration.Add(1)
		inst.runtimePID = 0
		inst.runtimeStartID = 0
	}
	inst.mu.Unlock()
	require.Empty(t, inst.RuntimeProgram(), "the pane-fallback state has no runtime-program evidence")
	require.Equal(t, poisonFlagged, inst.ResolvedPaneProgram(),
		"the pane's frozen command carries af's injected flag")
	require.Equal(t, []string{"-c", "check_for_update_on_startup=false"}, inst.ResolvedPaneGeneratedArgs(),
		"the pane still records the declaration af authored for the injected flag")
	return inst, target, &newSessions, &spawns
}

// poisonRuntimeProgramWithFlaggedBase simulates the durable evidence an
// earlier version's respawn recorded before the clean-base fix: that respawn
// boundary (respawnWithConversation → setRuntimeLaunch(declarationBase, ts))
// wrote the flagged prepared.base as RuntimeProgram, although RuntimeProgram's
// contract holds the clean override-resolved base command. A row last respawned
// by such a version reaches the seeding gate with runtimeEstablished true and
// RuntimeProgram equal to the pane's frozen program, so the seeding-path change
// (which still fires in that state) is what keeps its next swap from being
// refused for af's own flag. The mock exec cannot answer a pane root probe, so
// the record carries the command alone (pid=0, startID=0), exactly the "pane
// query that cannot answer records the command alone" contract.
func poisonRuntimeProgramWithFlaggedBase(t *testing.T, inst *Instance) {
	t.Helper()
	require.Equal(t, poisonFlagged, inst.ResolvedPaneProgram(),
		"poison seeds from the flagged pane program the earlier version's respawn recorded")
	inst.mu.Lock()
	inst.setRuntimeEvidenceLocked(poisonFlagged, 0, 0)
	inst.mu.Unlock()
	require.Equal(t, poisonFlagged, inst.RuntimeProgram(),
		"the poisoned runtime evidence is the flagged pane program")
}

// reenterLimitResumeFence lowers any held OpRespawning fence, re-parks the
// instance at the usage-limit wall, and raises the fence again — the sequence
// the scheduler runs between two swap episodes.
func reenterLimitResumeFence(t *testing.T, inst *Instance) {
	t.Helper()
	inst.EndLimitResume()
	inst.SetLimitReached(time.Time{})
	require.NoError(t, inst.BeginLimitResume())
}

// TestPoisonReproEndToEndRespawnRecordsCleanBase is the end-to-end reproduction:
// after a successful pane-fallback account swap, the swap's own respawn
// boundary records the clean override-resolved base (RuntimeProgram's
// contract), not the flagged pane program the proof was declared against — so
// the NEXT automatic swap on that session succeeds through the ordinary
// extension path rather than being refused for af's own injected flag as an
// undeclared argument.
//
// Before the fix, the respawn recorded the flagged prepared base as
// RuntimeProgram: the next swap reached the seeding gate with
// runtimeEstablished true, and (before the seeding-gate change) the gate
// refused to fire, recomputing the proof from the flagged base and rejecting
// af's own `-c check_for_update_on_startup=false` as a user argument.
func TestPoisonReproEndToEndRespawnRecordsCleanBase(t *testing.T) {
	inst, _, newSessions, spawns := flaggedPaneSwapInstanceForRespawn(t, "next")

	// 1. The first pane-fallback swap succeeds: the seeding gate vouches for
	//    af's flag from the pane's recorded declaration.
	require.NoError(t, inst.ValidateAccountSwap("work"),
		"the first pane-fallback swap succeeds via the pane declaration seeding")
	plan := cloneAccountSwapLaunchPlan(inst.accountSwapLaunch)
	require.NotNil(t, plan)
	require.Equal(t, poisonFlagged, plan.base,
		"the swap base is the flagged pane program the fallback selected")
	require.Equal(t, []string{"-c", "check_for_update_on_startup=false"}, plan.proof.GeneratedArgs,
		"the proof declares af's flag from the pane's recording")

	// 2. Commit the replacement identity and respawn.
	_, err := inst.SelectAccountAutomatically("", "work")
	require.NoError(t, err)
	require.NoError(t, inst.RespawnForAccountSwap(),
		"the replacement pane set starts under the selected account")
	require.Greater(t, *newSessions, 0, "the respawn spawned a fresh agent pane")

	// 3. The respawn recorded the CLEAN override-resolved base as RuntimeProgram
	//    (its contract), not the flagged pane program the proof was declared
	//    against: a same-agent pane-fallback swap's proof base carries af's own
	//    injected flag, and recording that would poison every later reader of
	//    RuntimeProgram.
	require.Equal(t, tmux.ProgramCodex, inst.RuntimeProgram(),
		"the respawn records the clean override-resolved base, not the flagged pane program")
	require.Equal(t, poisonFlagged, inst.ResolvedPaneProgram(),
		"the pane still carries the flagged program")
	require.Equal(t, []string{"-c", "check_for_update_on_startup=false"}, inst.ResolvedPaneGeneratedArgs(),
		"the pane still records the declaration for the flag")
	launched := false
	for _, s := range *spawns {
		if strings.Contains(s, "check_for_update_on_startup=false") {
			launched = true
			break
		}
	}
	require.True(t, launched, "the respawn spawned the flagged codex program")

	// 4. The next swap keeps succeeding through the ordinary extension path:
	//    RuntimeProgram is the clean base derived from the launched command (the
	//    flagged pane program minus af's declared flag), which differs from that
	//    program, so the seeding gate does not fire and the proof describes the
	//    injected flag as an ordinary trailing extension (the #5479 control shape).
	reenterLimitResumeFence(t, inst)
	require.NoError(t, inst.ValidateAccountSwap("next"),
		"after a pane-fallback swap the next automatic swap must keep succeeding")
	nextPlan := cloneAccountSwapLaunchPlan(inst.accountSwapLaunch)
	require.NotNil(t, nextPlan)
	require.Equal(t, tmux.ProgramCodex, nextPlan.base,
		"the next swap's base is the clean runtime program the respawn recorded")
	require.Equal(t, []string{"-c", "check_for_update_on_startup=false"}, nextPlan.proof.GeneratedArgs,
		"the next swap's proof describes the injected flag as an ordinary trailing extension")
}

// TestPoisonReproNextManualSwapSucceedsWithPoisonedRuntimeEvidence confirms the
// manual swap path (ValidateManualAccountSwap) is not refused either after a
// pane-fallback swap poisons RuntimeProgram. A same-agent manual swap routes
// through the same seeding gate as the automatic one.
func TestPoisonReproNextManualSwapSucceedsWithPoisonedRuntimeEvidence(t *testing.T) {
	fakeCodexOnPath(t)
	inst, _ := flaggedPaneSwapInstance(t)
	registerAccount(t, tmux.ProgramCodex, "next")

	require.NoError(t, inst.ValidateAccountSwap("work"))
	poisonRuntimeProgramWithFlaggedBase(t, inst)
	reenterLimitResumeFence(t, inst)

	// A same-agent (account-only) manual swap: the operator asked to move to
	// "next" without changing the running agent.
	program, crossAgent := inst.ManualAccountSwapProgram("", true)
	require.False(t, crossAgent, "an account-only manual swap is same-agent")
	require.NotEmpty(t, program)
	require.NoError(t, inst.ValidateManualAccountSwap("next", "", false),
		"the next manual swap after a poisoned RuntimeProgram must succeed")
}

// TestPoisonReproSeedingRefusesUndeclaredUserArg is the negative test for the
// relaxed seeding gate: the gate fires (RuntimeProgram matches the pane's frozen
// program, the state an earlier version's poisoned respawn left behind), but a
// pane whose recorded program carries a user-supplied argument af did not declare
// must still be refused. Only af's own declared flags may pass.
//
// The pane's frozen program carries the operator's --dangerous-flag after af's own
// injected startup-update flag, and the recorded GeneratedArgs vouch only for
// af's own words. The seeding gate fires because RuntimeProgram matches the pane,
// but the proof's base-suffix check (GenerateAccountLaunchProof's accountBaseWords
// match against trustedBaseArgs) sees the user flag where af's declaration expects
// its own — the proof fails closed and ValidateAccountCommand refuses the swap.
func TestPoisonReproSeedingRefusesUndeclaredUserArg(t *testing.T) {
	carryAmbientProviderHomes(t)
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{tmux.ProgramCodex: tmux.ProgramCodex}
	require.NoError(t, config.SaveConfig(cfg))
	registerAccount(t, tmux.ProgramCodex, "work")
	inst := accountSwapTestInstanceWithProgram(tmux.ProgramCodex)
	inst.Path = initTempGitRepo(t)
	gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)

	// A pane whose frozen program carries a user-supplied argument af did not
	// declare: the recorded GeneratedArgs vouch only for af's own startup-update
	// flag, not the operator's --dangerous-flag. RuntimeProgram holds that same
	// flagged program — the state an earlier version's poisoned respawn left
	// behind — so the seeding gate fires.
	const userFlagged = "codex -c check_for_update_on_startup=false --dangerous-flag"
	ts := tmux.NewTmuxSession(inst.Title, userFlagged)
	setLaunchProgram(ts, userFlagged, sessionenv.AccountLaunchProof{
		GeneratedArgs: []string{"-c", "check_for_update_on_startup=false"},
	})
	inst.mu.Lock()
	inst.Tabs = []*Tab{newAgentTab(ts)}
	inst.setRuntimeEvidenceLocked(userFlagged, 0, 0)
	inst.mu.Unlock()
	require.Equal(t, userFlagged, inst.RuntimeProgram(),
		"RuntimeProgram matches the pane's frozen program: the seeding gate fires")
	require.Equal(t, userFlagged, inst.ResolvedPaneProgram(),
		"the pane carries a user-supplied flag af did not declare")

	// The seeding gate fires (RuntimeProgram == ResolvedPaneProgram), but the
	// proof's base suffix does not match af's recorded declaration: the base
	// ends in the user's --dangerous-flag where af's declaration expects its own
	// -c flag, so the proof fails closed and the account boundary refuses the
	// swap. Only af's own declared flags may pass.
	require.Error(t, inst.ValidateAccountSwap("work"),
		"the seeding gate must not vouch for a user-supplied argument af did not declare")
}

// TestPoisonReproSeedingDoesNotWidenAdmission confirms the fix does not widen
// what the account boundary admits: when RuntimeProgram holds a command that is
// NOT the pane's frozen program (the clean base of the #5479 control test), the
// seeding gate still does not fire, and the swap keeps succeeding through the
// ordinary extension path — exactly as before.
func TestPoisonReproSeedingDoesNotWidenAdmission(t *testing.T) {
	carryAmbientProviderHomes(t)
	cfg := config.DefaultConfig()
	cfg.ProgramOverrides = map[string]string{tmux.ProgramCodex: tmux.ProgramCodex}
	require.NoError(t, config.SaveConfig(cfg))
	registerAccount(t, tmux.ProgramCodex, "work")
	registerAccount(t, tmux.ProgramCodex, "next")
	inst := accountSwapTestInstanceWithProgram(tmux.ProgramCodex)
	inst.Path = initTempGitRepo(t)
	gw, err := sessiongit.NewGitWorktreeFromStorage(inst.Path, inst.Path, inst.Title, "main", "", false, true)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)

	// The control: RuntimeProgram holds the clean base ("codex"), while the
	// pane carries the flagged program. The two differ, so the seeding gate
	// must not fire regardless of the fix.
	ts := tmux.NewTmuxSession(inst.Title, poisonFlagged)
	setLaunchProgram(ts, poisonFlagged, sessionenv.AccountLaunchProof{
		GeneratedArgs: []string{"-c", "check_for_update_on_startup=false"},
	})
	inst.mu.Lock()
	inst.Tabs = []*Tab{newAgentTab(ts)}
	inst.setRuntimeEvidenceLocked(tmux.ProgramCodex, 0, 0)
	inst.mu.Unlock()
	require.Equal(t, tmux.ProgramCodex, inst.RuntimeProgram(),
		"the control base is the clean runtime program")
	require.Equal(t, poisonFlagged, inst.ResolvedPaneProgram(),
		"the pane carries the flagged program, which differs from the clean base")

	// RuntimeProgram (the clean base) does NOT match the pane, so the second
	// swap routes through the ordinary extension path (base == "codex", final
	// gets the flag appended) and succeeds without the seeding.
	require.NoError(t, inst.ValidateAccountSwap("work"))
	reenterLimitResumeFence(t, inst)
	require.NoError(t, inst.ValidateAccountSwap("next"),
		"a clean runtime base that differs from the pane keeps succeeding through the extension path")
	plan := cloneAccountSwapLaunchPlan(inst.accountSwapLaunch)
	require.NotNil(t, plan)
	require.Equal(t, tmux.ProgramCodex, plan.base,
		"the swap base is the clean runtime program, not the flagged pane")
	require.Equal(t, []string{"-c", "check_for_update_on_startup=false"}, plan.proof.GeneratedArgs,
		"a clean base describes the injected flag as an ordinary trailing extension")
}
