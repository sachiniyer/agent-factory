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

// poisonRuntimeProgramWithFlaggedBase simulates the durable evidence a
// successful pane-fallback swap's respawn records: the swap prepared its launch
// against the pane's flagged frozen program, and the respawn boundary
// (respawnWithConversation → setRuntimeLaunch(declarationBase, ts)) writes that
// flagged prepared.base as RuntimeProgram. The mock exec cannot answer a pane
// root probe, so the record carries the command alone (pid=0, startID=0), exactly
// the "pane query that cannot answer records the command alone" contract.
func poisonRuntimeProgramWithFlaggedBase(t *testing.T, inst *Instance) {
	t.Helper()
	require.Equal(t, poisonFlagged, inst.ResolvedPaneProgram(),
		"poison seeds from the flagged pane program the swap respawn recorded")
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

// TestPoisonReproEndToEndRespawnRecordsFlaggedBase is the end-to-end reproduction:
// after a successful pane-fallback account swap, the swap's own respawn boundary
// records af's flag-carrying pane program as durable RuntimeProgram evidence,
// and the NEXT automatic swap on that session must keep succeeding — not be
// refused for af's own injected flag as an undeclared argument.
//
// Before the fix, the second swap was refused: RuntimeProgram being non-empty
// routed the plan off the pane-declaration seeding gate, so the proof recomputed
// from the flagged base declared nothing and ValidateAccountCommand rejected
// af's own `-c check_for_update_on_startup=false` as a user argument.
func TestPoisonReproEndToEndRespawnRecordsFlaggedBase(t *testing.T) {
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

	// 3. The respawn poisoned RuntimeProgram: it recorded the flagged prepared
	//    base (the pane program that carries af's injected flag) as durable
	//    runtime evidence, although RuntimeProgram's contract holds the clean
	//    override-resolved base command.
	require.Equal(t, poisonFlagged, inst.RuntimeProgram(),
		"the swap respawn records the flagged prepared base as RuntimeProgram (the poison)")
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

	// 4. The next swap must keep succeeding exactly as the first did. Before the
	//    fix this was refused with "could not be proven safe" naming af's own flag.
	reenterLimitResumeFence(t, inst)
	require.NoError(t, inst.ValidateAccountSwap("next"),
		"after a pane-fallback swap the next automatic swap must keep succeeding")
	nextPlan := cloneAccountSwapLaunchPlan(inst.accountSwapLaunch)
	require.NotNil(t, nextPlan)
	require.Equal(t, poisonFlagged, nextPlan.base,
		"the next swap's base is still the flagged pane program (now via RuntimeProgram)")
	require.Equal(t, []string{"-c", "check_for_update_on_startup=false"}, nextPlan.proof.GeneratedArgs,
		"the next swap's proof still declares af's flag from the pane's recording")
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
