package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// unreadableDayDir creates a day directory in home's store that the walk
// cannot list, and restores it at cleanup.
func unreadableDayDir(t *testing.T, home string, day time.Time) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root lists any directory")
	}
	dir := filepath.Dir(filepath.Join(home, codexRolloutRelDay(carryOtherThreadID, day)))
	writeCarryFile(t, home, codexRolloutRelDay(carryOtherThreadID, day), "{}\n")
	require.NoError(t, os.Chmod(dir, 0))
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
}

// A thread/revert rollout is minted after the rollout it reverts, so the
// re-check before the copy, which runs under the swap's global fences, walks
// only the part of each store that can hold one: days before the thread's own
// rollout, less the recorder's clock lookback, are never opened (#7152 P3-2).
func TestCarryRevertCheckSkipsDaysBeforeTheThreadBegan(t *testing.T) {
	inst, source, target, original := backtrackedCarryInstance(t)
	require.NoError(t, inst.ValidateAccountSwap("work"))
	require.NotNil(t, inst.accountSwapLaunch.carry)

	// Unreadable history from long before the thread, in both homes.
	unreadableDayDir(t, source, time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local))
	unreadableDayDir(t, target, time.Date(2026, 1, 2, 0, 0, 0, 0, time.Local))

	require.NoError(t, inst.CarryAccountSwapConversation())
	require.NotNil(t, inst.accountSwapLaunch.carry, "fallback %q", inst.accountSwapLaunch.carryFallback)
	require.Equal(t, codexRollout(inst.GetWorktreePath()), readCarryFile(t, filepath.Join(target, original)))
}

// Control: a day the thread's revert rollout could be filed under still fails
// closed when it cannot be listed.
func TestCarryRevertCheckStillFailsClosedWhereARevertCouldBe(t *testing.T) {
	inst, source, target, _ := backtrackedCarryInstance(t)
	require.NoError(t, inst.ValidateAccountSwap("work"))
	require.NotNil(t, inst.accountSwapLaunch.carry)

	unreadableDayDir(t, source, time.Date(2026, 9, 18, 0, 0, 0, 0, time.Local))
	require.NoError(t, inst.CarryAccountSwapConversation())
	requireFreshCodexPlan(t, inst, "af could not enumerate the Codex rollout store")
	require.Empty(t, codexRolloutsNaming(t, target, carryTestID))
}

// Control: Codex publishes a compressed copy before it removes the plain
// rollout, and writes a compressed rollout back out as plain when it is
// resumed, so the thread's own rollout beside its .zst is one thread, not a
// backtrack (#7152 P3-4).
func TestCarryTakesAThreadBesideItsOwnCompressedCopy(t *testing.T) {
	inst, source, target, original := backtrackedCarryInstance(t)
	writeCarryFile(t, source, original+".zst", "\x28\xb5\x2f\xfd")

	require.NoError(t, inst.ValidateAccountSwap("work"))
	plan := inst.accountSwapLaunch
	require.NotNil(t, plan.carry, "fallback %q", plan.carryFallback)
	require.Equal(t, original, plan.carry.transcript)

	require.NoError(t, inst.CarryAccountSwapConversation())
	require.NotNil(t, inst.accountSwapLaunch.carry, "fallback %q", inst.accountSwapLaunch.carryFallback)
	require.FileExists(t, filepath.Join(target, original))
}
