package commands

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/upgradetxn"
	"github.com/stretchr/testify/require"
)

var errNotAValidJournal = errors.New("validate active upgrade journal: invalid")

// stubUpgradeJournal points the interlock's loader seam at a fixed journal, so
// each phase's policy is testable. upgradetxn.Load validates a journal against
// the preserved binaries, their digests, and the recovery lock on disk, so a
// journal per phase cannot be hand-forged — only Prepare produces a valid one.
// TestActiveUpgrade_ReadsARealPreparedTransaction covers the real loader, so the
// seam cannot drift away from it unnoticed.
func stubUpgradeJournal(t *testing.T, journal upgradetxn.Journal, err error) {
	t.Helper()
	original := loadUpgradeJournal
	loadUpgradeJournal = func(string) (upgradetxn.Journal, error) { return journal, err }
	t.Cleanup(func() { loadUpgradeJournal = original })
}

func journalAt(phase upgradetxn.Phase) upgradetxn.Journal {
	return upgradetxn.Journal{ID: "upgrade-abc123", ToVersion: "1.0.300", Phase: phase}
}

func upgradeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	resolved, err := config.GetConfigDir()
	require.NoError(t, err)
	return resolved
}

// The universal case on every box today: no transaction has ever been created,
// so the interlock is invisible and the in-place installers behave exactly as
// before. If this regresses, the interlock has started blocking real upgrades.
func TestActiveUpgrade_NoJournalAllowsTheInstall(t *testing.T) {
	upgradeHome(t)
	require.Nil(t, activeUpgradeOwningExecutable(),
		"with no upgrade transaction the in-place installer must be unaffected")
}

// The real loader, against a genuinely Prepare'd transaction — proof that the
// seam above is wired to production and that a real journal blocks.
func TestActiveUpgrade_ReadsARealPreparedTransaction(t *testing.T) {
	home := upgradeHome(t)
	executable := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(executable, []byte("previous-af-binary"), 0o755))

	id := "upgrade-" + strings.Repeat("a", 32)
	_, err := upgradetxn.Prepare(upgradetxn.Plan{
		ID:             id,
		HomeDir:        home,
		ExecutablePath: executable,
		FromVersion:    "1.0.100",
		ToVersion:      "1.0.300",
		Candidate:      []byte("candidate-af-binary"),
		Daemon: upgradetxn.DaemonSnapshot{
			WasRunning: true,
			BootID:     "boot",
			Owner:      upgradetxn.DaemonOwner{Kind: upgradetxn.SupervisionAdHoc},
		},
		RecoveryJob: upgradetxn.RecoveryJob{Kind: upgradetxn.RecoveryJobDetached},
	})
	require.NoError(t, err, "the test needs a real, valid transaction to be meaningful")

	active := activeUpgradeOwningExecutable()
	require.NotNil(t, active, "a real prepared transaction must block an in-place swap")
	require.Equal(t, id, active.ID)
	require.Equal(t, "1.0.300", active.ToVersion)
	require.Equal(t, string(upgradetxn.PhasePrepared), active.Phase)
}

// Every phase where an activation is still in flight must block: a swap there
// destroys the rollback the transaction depends on.
func TestActiveUpgrade_InFlightPhasesBlockTheInstall(t *testing.T) {
	for _, phase := range []upgradetxn.Phase{
		upgradetxn.PhasePrepared,
		upgradetxn.PhaseSupervisorReady,
		upgradetxn.PhaseDaemonStopping,
		upgradetxn.PhaseDaemonStopped,
		upgradetxn.PhaseCandidateInstalled,
		upgradetxn.PhaseCandidateStarting,
		upgradetxn.PhaseCandidateValidating,
		upgradetxn.PhaseRollingBack,
		upgradetxn.PhaseRollbackRestored,
		upgradetxn.PhasePreviousStarting,
		upgradetxn.PhasePreviousValidating,
		// Not terminal, and the one that matters most: rollback_failed is the
		// circuit-breaker state that retains every recovery artifact, so it is
		// exactly when an unwitting overwrite does the most damage.
		upgradetxn.PhaseRollbackFailed,
	} {
		t.Run(string(phase), func(t *testing.T) {
			upgradeHome(t)
			stubUpgradeJournal(t, journalAt(phase), nil)

			active := activeUpgradeOwningExecutable()
			require.NotNil(t, active, "phase %s is in flight and must block an install", phase)
			require.Equal(t, "upgrade-abc123", active.ID)
			require.Equal(t, string(phase), active.Phase)
		})
	}
}

// Terminal phases are cleanup only — the activation is already decided and there
// is no rollback left to corrupt — so they must not block. Blocking here would
// strand `af upgrade` behind a journal nobody is going to act on.
func TestActiveUpgrade_TerminalPhasesAllowTheInstall(t *testing.T) {
	for _, phase := range []upgradetxn.Phase{
		upgradetxn.PhaseCommitted,
		upgradetxn.PhaseRolledBack,
		upgradetxn.PhaseAborted,
	} {
		t.Run(string(phase), func(t *testing.T) {
			upgradeHome(t)
			stubUpgradeJournal(t, journalAt(phase), nil)
			require.Nil(t, activeUpgradeOwningExecutable(),
				"phase %s is cleanup only and must not block an install", phase)
		})
	}
}

// The polarity that matters. A journal that cannot be read or validated must NOT
// block: refusing on unreadable evidence would let one corrupt file disable
// `af upgrade` permanently, with no upgrade-transaction repair in `af doctor` to
// send the user to — an unoverridable block produced by an inference.
func TestActiveUpgrade_UnreadableJournalAllowsTheInstall(t *testing.T) {
	t.Run("real corrupt journal on disk", func(t *testing.T) {
		home := upgradeHome(t)
		dir := filepath.Join(home, "upgrade")
		require.NoError(t, os.MkdirAll(dir, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "active.json"), []byte("{not json"), 0o644))

		require.Nil(t, activeUpgradeOwningExecutable(),
			"a corrupt journal must not permanently disable af upgrade")
	})

	t.Run("journal fails validation", func(t *testing.T) {
		upgradeHome(t)
		stubUpgradeJournal(t, upgradetxn.Journal{}, errNotAValidJournal)
		require.Nil(t, activeUpgradeOwningExecutable(),
			"an unvalidatable journal is inconclusive, not proof of a live upgrade")
	})
}

// A missing home is not a live transaction.
func TestActiveUpgrade_MissingHomeAllowsTheInstall(t *testing.T) {
	home := upgradeHome(t)
	require.NoError(t, os.RemoveAll(home))
	require.Nil(t, activeUpgradeOwningExecutable())
}

// The guarded writer is the actual interlock: both installers go through it, so
// it must refuse rather than write when a transaction is live.
func TestWriteExecutableInPlace_RefusesDuringALiveTransaction(t *testing.T) {
	upgradeHome(t)
	stubUpgradeJournal(t, journalAt(upgradetxn.PhaseCandidateStarting), nil)

	target := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(target, []byte("old binary"), 0o755))

	err := writeExecutableInPlace(target, []byte("new binary"), false, "--"+ignoreActiveUpgradeFlag)
	require.Error(t, err)

	var blocked *blockedInPlaceInstallError
	require.ErrorAs(t, err, &blocked)
	require.Contains(t, err.Error(), "upgrade-abc123")
	require.Contains(t, err.Error(), "--"+ignoreActiveUpgradeFlag,
		"a refusal must name its override; a block a user cannot get past is a brick")

	on, readErr := os.ReadFile(target)
	require.NoError(t, readErr)
	require.Equal(t, "old binary", string(on), "the executable must be untouched")
}

// And it must actually write when nothing owns the executable — otherwise every
// test above would pass on a writer that refuses unconditionally.
func TestWriteExecutableInPlace_WritesWhenNoTransactionIsActive(t *testing.T) {
	upgradeHome(t)

	target := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(target, []byte("old binary"), 0o755))

	require.NoError(t, writeExecutableInPlace(target, []byte("new binary"), false, "--"+ignoreActiveUpgradeFlag))

	on, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "new binary", string(on))

	info, err := os.Stat(target)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm(), "the swapped binary must stay executable")
}

// The override is honoured. An interlock a user cannot get past would be its own
// hazard — a safeguard must not be the thing that strands someone on a broken
// binary.
func TestWriteExecutableInPlace_OverrideInstallsAnyway(t *testing.T) {
	upgradeHome(t)
	stubUpgradeJournal(t, journalAt(upgradetxn.PhaseCandidateValidating), nil)

	target := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(target, []byte("old binary"), 0o755))

	require.NoError(t, writeExecutableInPlace(target, []byte("new binary"), true, "--"+ignoreActiveUpgradeFlag))

	on, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "new binary", string(on), "an explicit override must install")
}

// `af upgrade` refuses before spending a download, and the refusal names the
// override.
func TestRunUpgrade_RefusesDuringALiveTransaction(t *testing.T) {
	upgradeHome(t)
	stubUpgradeJournal(t, journalAt(upgradetxn.PhaseDaemonStopped), nil)

	downloaded := false
	originalDownload := downloadBinaryFn
	downloadBinaryFn = func(string, time.Duration) ([]byte, error) {
		downloaded = true
		return []byte("new binary"), nil
	}
	t.Cleanup(func() { downloadBinaryFn = originalDownload })

	originalIgnore := upgradeIgnoreActiveUpgrade
	upgradeIgnoreActiveUpgrade = false
	t.Cleanup(func() { upgradeIgnoreActiveUpgrade = originalIgnore })

	err := runUpgrade(io.Discard, io.Discard, "http://example.invalid/af.tar.gz", true)
	require.Error(t, err)
	require.Contains(t, err.Error(), "upgrade-abc123")
	require.Contains(t, err.Error(), "--"+ignoreActiveUpgradeFlag)
	require.False(t, downloaded, "the refusal must come before the download, not after it")
}

// The launch path stands down without touching the shared throttle window or
// spending a download — and never blocks or errors at the launch.
func TestAutoUpdateOnLaunch_StandsDownDuringALiveTransaction(t *testing.T) {
	home := upgradeHome(t)
	stubUpgradeJournal(t, journalAt(upgradetxn.PhaseCandidateStarting), nil)

	checked := false
	originalFetch := fetchLatestReleaseTagFn
	fetchLatestReleaseTagFn = func(string, time.Duration) (string, error) {
		checked = true
		return "v1.0.400", nil
	}
	t.Cleanup(func() { fetchLatestReleaseTagFn = originalFetch })

	originalTTY := stdoutIsTTYFn
	stdoutIsTTYFn = func() bool { return true }
	t.Cleanup(func() { stdoutIsTTYFn = originalTTY })

	autoUpdateOnLaunch(&config.Config{AutoUpdate: true, UpdateChannel: config.UpdateChannelStable})

	require.False(t, checked, "a launch during an upgrade must not even resolve a release")
	_, err := os.Stat(filepath.Join(home, "last_update_check"))
	require.True(t, os.IsNotExist(err),
		"standing down must not consume the shared 6h window; the next launch re-evaluates")
}
