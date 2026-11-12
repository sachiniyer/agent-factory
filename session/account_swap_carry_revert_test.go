package session

import (
	"io/fs"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// A Codex backtrack (Esc-Esc) calls thread/revert. The thread keeps its id and
// continues in a new rollout, rollout-<ts>-<thread>_<rollout>.jsonl, which
// references a prefix of the original. The original stays on disk, frozen at
// the revert. Which of the two `codex resume <thread>` opens is decided by
// Codex's own state database (codex-rs thread-store thread_rollout_resolver),
// so a carry that copies the original alone resumes the conversation the user
// backtracked away from (#7141).

const (
	// carryRevertRolloutID is the rollout id thread/revert mints: a UUIDv7,
	// distinct from the thread id it keeps.
	carryRevertRolloutID = "019a3b4c-5d6e-7f80-9a1b-2c3d4e5f6a7b"
	// carryOtherThreadID is a second thread in the same store.
	carryOtherThreadID = "0a0b0c0d-1e2f-4a3b-8c4d-5e6f7a8b9c0d"

	revertCarrySourceReason = "it was backtracked in Codex and continues in a thread/revert rollout, which af does not carry yet"
	revertCarryTargetReason = "the new account already holds a thread/revert rollout of this conversation, which Codex would resume instead of the carried one"
)

// codexRevertRolloutRel is where thread/revert files thread's new rollout:
// the day it was minted, named for the thread and then the rollout.
func codexRevertRolloutRel(thread, rollout string, day time.Time, suffix string) string {
	return filepath.Join("sessions", day.Format("2006"), day.Format("01"), day.Format("02"),
		"rollout-"+day.Format("2006-01-02")+"T04-05-06-"+thread+"_"+rollout+".jsonl"+suffix)
}

// codexRolloutsNaming lists every file under home's sessions store whose
// name mentions id, relative to home.
func codexRolloutsNaming(t *testing.T, home, id string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(filepath.Join(home, "sessions"), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.Contains(strings.ToLower(d.Name()), strings.ToLower(id)) {
			rel, relErr := filepath.Rel(home, path)
			require.NoError(t, relErr)
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		require.ErrorIs(t, err, fs.ErrNotExist)
	}
	return out
}

// backtrackedCarryInstance is a limit-blocked Codex session on the registered
// "old" account whose recorded thread has its original rollout there. It
// returns the instance, the old and new account homes, and the original's
// path relative to either.
func backtrackedCarryInstance(t *testing.T) (*Instance, string, string, string) {
	t.Helper()
	carryAmbientProviderHomes(t)
	inst, target := carrySwapInstance(t, tmux.ProgramCodex)
	source := registerAccount(t, tmux.ProgramCodex, "old")
	inst.Account, inst.accountAutoSelected = "old", true
	recordOutgoingConversation(t, inst, tmux.ProgramCodex, carryTestID)
	original := codexRolloutRel(carryTestID)
	writeCarryFile(t, source, original, codexRollout(inst.GetWorktreePath()))
	return inst, source, target, original
}

// requireFreshCodexPlan asserts the planned replacement starts a new
// conversation for reason, rather than resuming any thread.
func requireFreshCodexPlan(t *testing.T, inst *Instance, reason string) {
	t.Helper()
	plan := inst.accountSwapLaunch
	require.Nil(t, plan.carry)
	require.Equal(t, reason, plan.carryFallback)
	require.Equal(t, "codex -c check_for_update_on_startup=false", plan.program)
	require.NotContains(t, plan.program, "resume")
	require.Equal(t, HandoffConversation{CarryFailure: reason}, inst.PreparedAccountSwapConversation())
}

func TestAccountSwapDoesNotCarryTheOriginalOfABacktrackedCodexThread(t *testing.T) {
	for _, tc := range []struct {
		name   string
		suffix string
	}{
		{name: "revert rollout", suffix: ""},
		// Codex's opt-in compression keeps the name and appends .zst.
		{name: "compressed revert rollout", suffix: ".zst"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inst, source, target, _ := backtrackedCarryInstance(t)
			writeCarryFile(t, source, codexRevertRolloutRel(carryTestID, carryRevertRolloutID,
				time.Date(2026, 9, 17, 0, 0, 0, 0, time.Local), tc.suffix), codexRollout(inst.GetWorktreePath()))

			require.NoError(t, inst.ValidateAccountSwap("work"))
			requireFreshCodexPlan(t, inst, revertCarrySourceReason)

			require.NoError(t, inst.CarryAccountSwapConversation())
			requireFreshCodexPlan(t, inst, revertCarrySourceReason)
			require.Empty(t, codexRolloutsNaming(t, target, carryTestID),
				"the pre-revert original must not reach the new account")
		})
	}
}

// The plan is made while the outgoing runtime still runs, and the copy only
// after it stops, so a backtrack in between must still stop the carry.
func TestAccountSwapCarryRechecksForABacktrackAfterTheRuntimeStops(t *testing.T) {
	inst, source, target, _ := backtrackedCarryInstance(t)
	require.NoError(t, inst.ValidateAccountSwap("work"))
	require.NotNil(t, inst.accountSwapLaunch.carry, "nothing was backtracked when the swap was planned")

	writeCarryFile(t, source, codexRevertRolloutRel(carryTestID, carryRevertRolloutID,
		time.Date(2026, 9, 17, 0, 0, 0, 0, time.Local), ""), codexRollout(inst.GetWorktreePath()))
	require.NoError(t, inst.CarryAccountSwapConversation(), "a refused copy is a stated fresh start, not a failed swap")

	requireFreshCodexPlan(t, inst, revertCarrySourceReason)
	require.Empty(t, codexRolloutsNaming(t, target, carryTestID),
		"the pre-revert original must not reach the new account")
}

// The new account's Codex would resolve the thread to a revert rollout it
// already holds, not to the original af copies in beside it.
func TestAccountSwapDoesNotCarryIntoAnAccountHoldingARevertOfTheThread(t *testing.T) {
	inst, _, target, _ := backtrackedCarryInstance(t)
	revert := codexRevertRolloutRel(carryTestID, carryRevertRolloutID,
		time.Date(2026, 9, 17, 0, 0, 0, 0, time.Local), "")
	writeCarryFile(t, target, revert, "{}\n")

	require.NoError(t, inst.ValidateAccountSwap("work"))
	requireFreshCodexPlan(t, inst, revertCarryTargetReason)

	require.NoError(t, inst.CarryAccountSwapConversation())
	requireFreshCodexPlan(t, inst, revertCarryTargetReason)
	require.Equal(t, []string{revert}, codexRolloutsNaming(t, target, carryTestID),
		"the carry must neither copy the original in nor touch the account's own rollout")
}

// Control: other threads' revert rollouts say nothing about this one, even a
// name that carries this thread's id in the rollout position.
func TestAccountSwapCarriesBesideAnotherThreadsRevertRollouts(t *testing.T) {
	inst, source, target, original := backtrackedCarryInstance(t)
	day := time.Date(2026, 9, 17, 0, 0, 0, 0, time.Local)
	writeCarryFile(t, source, codexRevertRolloutRel(carryOtherThreadID, carryRevertRolloutID, day, ""), "{}\n")
	writeCarryFile(t, source, codexRevertRolloutRel(carryOtherThreadID, carryTestID, day.Add(24*time.Hour), ""), "{}\n")
	writeCarryFile(t, target, codexRevertRolloutRel(carryOtherThreadID, carryTestID, day, ""), "{}\n")

	require.NoError(t, inst.ValidateAccountSwap("work"))
	plan := inst.accountSwapLaunch
	require.NotNil(t, plan.carry)
	require.Empty(t, plan.carryFallback)
	require.Equal(t, original, plan.carry.transcript)

	require.NoError(t, inst.CarryAccountSwapConversation())
	require.Equal(t, codexRollout(inst.GetWorktreePath()), readCarryFile(t, filepath.Join(target, original)))
}

// Control: once a carry has committed, the replacement runs in the new account
// and a backtrack there is that account's own thread/revert, which its Codex
// resolves through its own state. Restart recovery still resumes the thread.
func TestCommittedCarryResumesAThreadTheNewAccountBacktracked(t *testing.T) {
	inst, _, target, original := backtrackedCarryInstance(t)
	require.NoError(t, inst.ValidateAccountSwap("work"))
	require.NoError(t, inst.CarryAccountSwapConversation())
	_, err := inst.SelectAccountAutomatically("old", "work")
	require.NoError(t, err)
	require.Equal(t, carryTestID, inst.ToInstanceData().PendingAccountSwap.CarriedConversationID)

	writeCarryFile(t, target, codexRevertRolloutRel(carryTestID, carryRevertRolloutID,
		time.Date(2026, 9, 17, 0, 0, 0, 0, time.Local), ""), codexRollout(inst.GetWorktreePath()))

	require.NoError(t, inst.ValidateAccountSwap("work"))
	plan := inst.accountSwapLaunch
	require.NotNil(t, plan.carry)
	require.Empty(t, plan.carryFallback)
	require.Equal(t, original, plan.carry.transcript)
	require.Equal(t, "codex -c check_for_update_on_startup=false resume "+carryTestID, plan.program)
}
