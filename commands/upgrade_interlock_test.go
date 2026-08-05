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

// journalAt builds a journal whose preserved previous binary really exists —
// that artifact is what the interlock protects, and a journal without it is a
// transaction with no rollback left (see
// TestActiveUpgrade_MissingRollbackArtifactAllowsTheInstall).
func journalAt(t *testing.T, phase upgradetxn.Phase) upgradetxn.Journal {
	t.Helper()
	previous := filepath.Join(t.TempDir(), ".af-upgrade-previous")
	require.NoError(t, os.WriteFile(previous, []byte("preserved previous binary"), 0o755))
	return upgradetxn.Journal{
		ID:                 "upgrade-abc123",
		ToVersion:          "1.0.300",
		Phase:              phase,
		PreviousBinaryPath: previous,
	}
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
			stubUpgradeJournal(t, journalAt(t, phase), nil)

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
			stubUpgradeJournal(t, journalAt(t, phase), nil)
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

// A loadable journal is not proof there is still a rollback to protect.
// upgradetxn.Load validates recorded paths, digests, and lock metadata — not the
// artifact contents — so a half-cleaned or failed transaction can leave a journal
// that loads while its preserved previous binary is gone. Blocking then protects
// nothing and only stands between the user and a working `af upgrade`.
func TestActiveUpgrade_MissingRollbackArtifactAllowsTheInstall(t *testing.T) {
	upgradeHome(t)
	journal := journalAt(t, upgradetxn.PhaseCandidateValidating)
	require.NoError(t, os.Remove(journal.PreviousBinaryPath))
	stubUpgradeJournal(t, journal, nil)

	require.Nil(t, activeUpgradeOwningExecutable(),
		"with the preserved previous binary gone the rollback is already impossible; blocking protects nothing")
}

// But only a POSITIVE absence downgrades the block. An inconclusive stat is not
// evidence the artifact is gone, and the block it keeps is overridable anyway.
func TestActiveUpgrade_InconclusiveArtifactStatKeepsTheBlock(t *testing.T) {
	upgradeHome(t)
	journal := journalAt(t, upgradetxn.PhaseCandidateValidating)
	// A path whose parent is a regular file: stat fails ENOTDIR, not ErrNotExist.
	blocked := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(blocked, []byte("x"), 0o644))
	journal.PreviousBinaryPath = filepath.Join(blocked, "previous")
	stubUpgradeJournal(t, journal, nil)

	require.NotNil(t, activeUpgradeOwningExecutable(),
		"an inconclusive stat must not be read as a missing rollback artifact")
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
	stubUpgradeJournal(t, journalAt(t, upgradetxn.PhaseCandidateStarting), nil)

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
	stubUpgradeJournal(t, journalAt(t, upgradetxn.PhaseCandidateValidating), nil)

	target := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(target, []byte("old binary"), 0o755))

	require.NoError(t, writeExecutableInPlace(target, []byte("new binary"), true, "--"+ignoreActiveUpgradeFlag))

	on, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "new binary", string(on), "an explicit override must install")
}

// The path a maintainer actually runs: `af upgrade` on a box where a daemon is
// serving. It must refuse before spending a download, name the override, and —
// the thing that actually matters — leave the executable it is running on
// untouched.
func TestRunUpgrade_RefusesDuringALiveTransaction(t *testing.T) {
	upgradeHome(t)
	stubUpgradeJournal(t, journalAt(t, upgradetxn.PhaseDaemonStopped), nil)

	installed := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(installed, []byte("the binary in use"), 0o755))
	originalExec := osExecutableFn
	osExecutableFn = func() (string, error) { return installed, nil }
	t.Cleanup(func() { osExecutableFn = originalExec })

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

	on, readErr := os.ReadFile(installed)
	require.NoError(t, readErr)
	require.Equal(t, "the binary in use", string(on),
		"af upgrade must not clobber the executable while a transaction owns it")
}

// The other half, and the more important one for a box where `af upgrade` is run
// often: with no transaction in flight, `af upgrade` installs exactly as it
// always has. The interlock must be invisible on the path people actually use.
func TestRunUpgrade_InstallsNormallyWithNoTransaction(t *testing.T) {
	upgradeHome(t)

	installed := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(installed, []byte("old binary"), 0o755))
	originalExec := osExecutableFn
	osExecutableFn = func() (string, error) { return installed, nil }
	t.Cleanup(func() { osExecutableFn = originalExec })

	originalDownload := downloadBinaryFn
	downloadBinaryFn = func(string, time.Duration) ([]byte, error) {
		return []byte("new binary"), nil
	}
	t.Cleanup(func() { downloadBinaryFn = originalDownload })

	originalIgnore := upgradeIgnoreActiveUpgrade
	upgradeIgnoreActiveUpgrade = false
	t.Cleanup(func() { upgradeIgnoreActiveUpgrade = originalIgnore })

	require.NoError(t, runUpgrade(io.Discard, io.Discard, "http://example.invalid/af.tar.gz", true))

	on, err := os.ReadFile(installed)
	require.NoError(t, err)
	require.Equal(t, "new binary", string(on),
		"with no transaction the in-place upgrade must behave exactly as before")
}

// The launch path stands down without touching the shared throttle window or
// spending a download — and never blocks or errors at the launch.
func TestAutoUpdateOnLaunch_StandsDownDuringALiveTransaction(t *testing.T) {
	home := upgradeHome(t)
	stubUpgradeJournal(t, journalAt(t, upgradetxn.PhaseCandidateStarting), nil)

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

// The interlock's check and write must be atomic against a transaction publish.
// Checking and then writing is a time-of-check-to-time-of-use window on its own:
// a transaction published in between is invisible to a check that already
// passed. Both sides take upgradetxn's preparation lock, so this proves the
// installer actually holds it — a Prepare racing the swap blocks until the swap
// is done, rather than interleaving with it.
func TestWriteExecutableInPlace_HoldsThePreparationLockAgainstPrepare(t *testing.T) {
	home := upgradeHome(t)

	target := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(target, []byte("old binary"), 0o755))

	// Hold the same lock a Prepare would, then prove the swap cannot proceed
	// while it is held.
	holding := make(chan struct{})
	release := make(chan struct{})
	held := make(chan error, 1)
	go func() {
		held <- upgradetxn.WithInstallLock(home, target, func() error {
			close(holding)
			<-release
			return nil
		})
	}()
	<-holding

	done := make(chan error, 1)
	go func() {
		done <- writeExecutableInPlace(target, []byte("new binary"), false, "--"+ignoreActiveUpgradeFlag)
	}()

	select {
	case <-done:
		t.Fatal("the in-place swap proceeded while the preparation lock was held; check and write are not serialised against Prepare")
	case <-time.After(250 * time.Millisecond):
	}

	on, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "old binary", string(on), "nothing may be written while the lock is held")

	close(release)
	require.NoError(t, <-held)
	require.NoError(t, <-done)

	on, err = os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "new binary", string(on), "and the swap completes once the lock is free")
}

// Broken lock storage must never block an install. <home>/upgrade left as a file
// says nothing about whether a transaction exists, and refusing here would make
// `af upgrade` fail on every invocation with --ignore-active-upgrade powerless,
// because the override lives inside the swap that never runs.
func TestWriteExecutableInPlace_BrokenLockStorageStillInstalls(t *testing.T) {
	home := upgradeHome(t)
	// Not a directory: WithInstallLock cannot take a lock under it.
	require.NoError(t, os.WriteFile(filepath.Join(home, "upgrade"), []byte("not a directory"), 0o644))

	target := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(target, []byte("old binary"), 0o755))

	require.NoError(t, writeExecutableInPlace(target, []byte("new binary"), false, "--"+ignoreActiveUpgradeFlag),
		"broken lock storage must not stand between a user and their upgrade")

	on, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "new binary", string(on))
}

// ...but the journal check still applies on that unlocked path. Failing to take
// the lock is not a bypass of the guard, only of the serialisation.
func TestWriteExecutableInPlace_BrokenLockStorageStillRefusesALiveTransaction(t *testing.T) {
	home := upgradeHome(t)
	require.NoError(t, os.WriteFile(filepath.Join(home, "upgrade"), []byte("not a directory"), 0o644))
	stubUpgradeJournal(t, journalAt(t, upgradetxn.PhaseCandidateValidating), nil)

	target := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(target, []byte("old binary"), 0o755))

	err := writeExecutableInPlace(target, []byte("new binary"), false, "--"+ignoreActiveUpgradeFlag)
	var blocked *blockedInPlaceInstallError
	require.ErrorAs(t, err, &blocked, "an unlockable install must still honour the journal")

	on, readErr := os.ReadFile(target)
	require.NoError(t, readErr)
	require.Equal(t, "old binary", string(on))
}

// The override has to work on the unlocked path too, or the escape hatch is
// exactly as unreachable as the bug this fixes.
func TestWriteExecutableInPlace_BrokenLockStorageHonoursTheOverride(t *testing.T) {
	home := upgradeHome(t)
	require.NoError(t, os.WriteFile(filepath.Join(home, "upgrade"), []byte("not a directory"), 0o644))
	stubUpgradeJournal(t, journalAt(t, upgradetxn.PhaseCandidateValidating), nil)

	target := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(target, []byte("old binary"), 0o755))

	require.NoError(t, writeExecutableInPlace(target, []byte("new binary"), true, "--"+ignoreActiveUpgradeFlag))

	on, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "new binary", string(on))
}

// Taking the lock must not restyle a directory the installer did not create. An
// AF home pointed at a broad user directory would otherwise have a routine
// `af upgrade` change the mode of an unrelated upgrade/ folder.
func TestWriteExecutableInPlace_LeavesAnExistingUpgradeDirectoryUntouched(t *testing.T) {
	home := upgradeHome(t)
	existing := filepath.Join(home, "upgrade")
	require.NoError(t, os.Mkdir(existing, 0o755))
	keep := filepath.Join(existing, "user-file")
	require.NoError(t, os.WriteFile(keep, []byte("mine"), 0o644))

	target := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(target, []byte("old binary"), 0o755))
	require.NoError(t, writeExecutableInPlace(target, []byte("new binary"), false, "--"+ignoreActiveUpgradeFlag))

	info, err := os.Stat(existing)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o755), info.Mode().Perm(),
		"an in-place upgrade must not chmod a directory it did not create")

	contents, err := os.ReadFile(keep)
	require.NoError(t, err)
	require.Equal(t, "mine", string(contents))
}

// One `af` binary can serve many AF homes, but a transaction is home-scoped
// while the executable is not. An upgrade run under a DIFFERENT home would find
// no journal of its own and rename over the very binary another home's
// transaction is preserving. upgradetxn stages its artifacts next to the
// executable, so the executable's own directory is where every home's
// transaction is visible.
func TestWriteExecutableInPlace_RefusesAnotherHomesStagedUpgrade(t *testing.T) {
	upgradeHome(t) // this home has no transaction at all

	dir := t.TempDir()
	target := filepath.Join(dir, "af")
	require.NoError(t, os.WriteFile(target, []byte("old binary"), 0o755))
	// What upgradetxn.binaryArtifactPaths stages beside the executable.
	staged := filepath.Join(dir, ".af.af-upgrade-upgrade-otherhome.previous")
	require.NoError(t, os.WriteFile(staged, []byte("preserved by another home"), 0o755))

	err := writeExecutableInPlace(target, []byte("new binary"), false, "--"+ignoreActiveUpgradeFlag)
	var blocked *blockedInPlaceInstallError
	require.ErrorAs(t, err, &blocked,
		"a transaction staged over this executable by another home must still block the swap")
	require.Contains(t, err.Error(), "upgrade-otherhome")
	require.Contains(t, err.Error(), staged, "the message must name the artifact so a leftover can be cleared")

	on, readErr := os.ReadFile(target)
	require.NoError(t, readErr)
	require.Equal(t, "old binary", string(on))
}

// The override still applies to the cross-home case — including for a leftover
// artifact from an interrupted cleanup, which is the realistic way a user meets
// this block.
func TestWriteExecutableInPlace_OverrideBeatsAnotherHomesStagedUpgrade(t *testing.T) {
	upgradeHome(t)

	dir := t.TempDir()
	target := filepath.Join(dir, "af")
	require.NoError(t, os.WriteFile(target, []byte("old binary"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, ".af.af-upgrade-upgrade-leftover.previous"), []byte("x"), 0o755))

	require.NoError(t, writeExecutableInPlace(target, []byte("new binary"), true, "--"+ignoreActiveUpgradeFlag))

	on, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "new binary", string(on))
}

// Only artifacts belonging to THIS executable count. A sibling binary's upgrade,
// or an unrelated dotfile, must not block.
func TestForeignUpgradeStagingOver_OnlyMatchesThisExecutable(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "af")
	require.NoError(t, os.WriteFile(target, []byte("binary"), 0o755))

	for _, name := range []string{
		".other.af-upgrade-upgrade-abc.previous", // a different executable
		".af.af-upgrade-upgrade-abc.candidate",   // the candidate, not the rollback input
		".af.af-upgrade-.previous",               // no transaction id
		"af.af-upgrade-upgrade-abc.previous",     // not the dotted prefix
		"unrelated",
	} {
		require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o644))
	}

	require.Nil(t, foreignUpgradeStagingOver(target),
		"only a preserved-previous artifact for THIS executable may block it")

	require.NoError(t, os.WriteFile(filepath.Join(dir, ".af.af-upgrade-upgrade-real.previous"), []byte("x"), 0o755))
	found := foreignUpgradeStagingOver(target)
	require.NotNil(t, found)
	require.Equal(t, "upgrade-real", found.ID)
}

// The in-place swap is serialised against a DIFFERENT AF home's transaction, not
// just its own home's. One `af` binary serves many homes, so a per-home lock
// excludes nothing over the binary they share; the interlock passes the resolved
// executable so the lock is keyed to the thing actually contended.
func TestWriteExecutableInPlace_SerialisesAgainstAnotherHomesLock(t *testing.T) {
	upgradeHome(t) // this installer's home

	target := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(target, []byte("old binary"), 0o755))

	// Another AF home entirely, holding the lock over the same executable.
	otherHome := t.TempDir()
	held := make(chan struct{})
	release := make(chan struct{})
	otherDone := make(chan error, 1)
	go func() {
		otherDone <- upgradetxn.WithInstallLock(otherHome, target, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	<-held

	done := make(chan error, 1)
	go func() {
		done <- writeExecutableInPlace(target, []byte("new binary"), false, "--"+ignoreActiveUpgradeFlag)
	}()
	select {
	case <-done:
		t.Fatal("the swap proceeded while another AF home held the executable lock")
	case <-time.After(250 * time.Millisecond):
	}

	on, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, "old binary", string(on))

	close(release)
	require.NoError(t, <-otherDone)
	require.NoError(t, <-done)
}
