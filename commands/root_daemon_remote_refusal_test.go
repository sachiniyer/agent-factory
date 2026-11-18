package commands

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

// These tests pin the W9.4 in-verb refusal added to the `--daemon` branch of
// root's RunE: `af --daemon` starts the daemon process on THIS machine, so
// under a remote target (--daemon-url / AF_DAEMON_URL / a saved target) it
// refuses through requireLocalTarget instead of silently dropping the target
// and serving a local daemon on the wrong machine — the same invariant the
// sibling verbs (`af daemon restart`, `af token show`, …) already enforce
// centrally. The bare TUI form of the root command stays classRouted, so the
// central guard does not fire; this is the matching in-verb refusal.
//
// All four target sources are covered (flag, env, saved), plus the
// upgrade-transaction recovery exemption and the routed-TUI non-regression,
// and the seams (runDaemon / runDaemonForUpgrade) keep the tests from binding
// the real control/HTTP sockets.

// snapshotTargetState captures and restores every package-level input the
// remote-target resolver reads (apiclient.FlagDaemonURL / FlagDaemonToken) so
// one test's target never leaks into another's RunE. t.Setenv already handles
// AF_DAEMON_URL / AF_DAEMON_TOKEN / AGENT_FACTORY_HOME.
func snapshotTargetState(t *testing.T) {
	t.Helper()
	prevURL, prevToken := apiclient.FlagDaemonURL, apiclient.FlagDaemonToken
	t.Cleanup(func() {
		apiclient.FlagDaemonURL = prevURL
		apiclient.FlagDaemonToken = prevToken
	})
	apiclient.FlagDaemonURL = ""
	apiclient.FlagDaemonToken = ""
}

// snapshotDaemonState captures and restores the daemon-branch inputs (daemonFlag,
// upgradeTransactionFlag) and the runDaemon / runDaemonForUpgrade seams. A test
// drives rootCmd.RunE directly, so cobra never writes these for us.
func snapshotDaemonState(t *testing.T) (runDaemonCalls *int, runUpgradeCalls *int, lastCfg **config.Config, lastTxID *string) {
	t.Helper()
	prevDaemon, prevTx := daemonFlag, upgradeTransactionFlag
	prevRun, prevRunUp := runDaemon, runDaemonForUpgrade
	t.Cleanup(func() {
		daemonFlag = prevDaemon
		upgradeTransactionFlag = prevTx
		runDaemon = prevRun
		runDaemonForUpgrade = prevRunUp
	})
	runCalls, upgradeCalls := 0, 0
	var cfg *config.Config
	var txID string
	runDaemon = func(c *config.Config) error {
		runCalls++
		cfg = c
		return nil
	}
	runDaemonForUpgrade = func(c *config.Config, id string) error {
		upgradeCalls++
		cfg = c
		txID = id
		return nil
	}
	return &runCalls, &upgradeCalls, &cfg, &txID
}

// writeConfig writes a minimal valid config.toml into the given AF home so the
// daemon branch's config.LoadConfig succeeds.
func writeConfig(t *testing.T, home string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("auto_update = false\n"), 0600))
}

// TestDaemonFlagRefusesRemoteTargetFromEnv: with AF_DAEMON_URL set and --daemon,
// root's RunE refuses through requireLocalTarget and never reaches
// runDaemon. No config.toml is written: the refusal must precede LoadConfig
// (pinned explicitly in TestDaemonFlagRefusalPrecedesConfigLoad).
func TestDaemonFlagRefusesRemoteTargetFromEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("AF_DAEMON_URL", "http://box.invalid:8443")
	snapshotTargetState(t)
	runCalls, _, _, _ := snapshotDaemonState(t)
	daemonFlag = true

	err := rootCmd.RunE(&cobra.Command{}, nil)
	require.Error(t, err)
	for _, want := range []string{
		"af --daemon",
		"local-only",
		"starts the daemon process on this machine",
		"the remote daemon at http://box.invalid:8443",
		"--daemon-url/AF_DAEMON_URL",
		"af target clear",
		"run af --daemon on the daemon host",
	} {
		require.Contains(t, err.Error(), want)
	}
	require.Equal(t, 0, *runCalls, "a refusal must not reach runDaemon")
}

// TestDaemonFlagRefusesRemoteTargetFromFlag: the --daemon-url FLAG (not env)
// also triggers the refusal — the resolver the guard shares with the real
// client treats flag > env > saved, so a flag-supplied target is named in the
// refusal the same way an exported env is.
func TestDaemonFlagRefusesRemoteTargetFromFlag(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("AF_DAEMON_URL", "")
	snapshotTargetState(t)
	apiclient.FlagDaemonURL = "http://flag.invalid:8443"
	runCalls, _, _, _ := snapshotDaemonState(t)
	daemonFlag = true

	err := rootCmd.RunE(&cobra.Command{}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "af --daemon")
	require.Contains(t, err.Error(), "the remote daemon at http://flag.invalid:8443")
	require.Equal(t, 0, *runCalls)
}

// TestDaemonFlagRefusesRemoteTargetFromSavedTarget: a saved client target
// (the lowest-precedence source) is also honoured, so an operator who ran
// `af target set` once and later starts a daemon is refused the same way.
func TestDaemonFlagRefusesRemoteTargetFromSavedTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("AF_DAEMON_URL", "")
	snapshotTargetState(t)
	require.NoError(t, apiclient.SaveTarget(apiclient.SavedTarget{URL: "http://saved.invalid:8443"}))
	runCalls, _, _, _ := snapshotDaemonState(t)
	daemonFlag = true

	err := rootCmd.RunE(&cobra.Command{}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "af --daemon")
	require.Contains(t, err.Error(), "the remote daemon at http://saved.invalid:8443")
	require.Equal(t, 0, *runCalls)
}

// TestDaemonFlagRefusalPrecedesConfigLoad: the refusal is upstream of
// config.LoadConfig. With an unparseable config.toml the daemon branch would
// return a load error if it reached LoadConfig; the refusal winning instead
// proves the guard is first in the `--daemon` branch.
func TestDaemonFlagRefusalPrecedesConfigLoad(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	require.NoError(t, os.WriteFile(filepath.Join(home, "config.toml"), []byte("= this is not valid toml =\n"), 0600))
	t.Setenv("AF_DAEMON_URL", "http://box.invalid:8443")
	snapshotTargetState(t)
	_, _, _, _ = snapshotDaemonState(t)
	daemonFlag = true

	err := rootCmd.RunE(&cobra.Command{}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "af --daemon")
	require.NotContains(t, err.Error(), "config.toml")
	require.NotContains(t, err.Error(), "parse")
}

// TestDaemonFlagAllowedWithoutRemoteTarget: with no remote target configured
// the refusal does not fire and runDaemon runs with the loaded config — the
// happy path of an operator (or the autostart unit) starting the local daemon.
// No false refusal, no stub skew.
func TestDaemonFlagAllowedWithoutRemoteTarget(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	writeConfig(t, home)
	t.Setenv("AF_DAEMON_URL", "")
	snapshotTargetState(t)
	runCalls, upgradeCalls, lastCfg, _ := snapshotDaemonState(t)
	daemonFlag = true

	require.NoError(t, rootCmd.RunE(&cobra.Command{}, nil))
	require.Equal(t, 1, *runCalls, "runDaemon must be reached once when no remote target is set")
	require.Equal(t, 0, *upgradeCalls, "RunDaemonForUpgrade must not be hit without --upgrade-transaction")
	require.NotNil(t, *lastCfg, "runDaemon must receive the loaded config")
}

// TestDaemonFlagUpgradeTransactionExemptFromRemoteRefusal: the internal
// `--upgrade-transaction` recovery invocation is exempt. StartCandidate execs
// `af --daemon --upgrade-transaction <id>` on the daemon host, and the child
// inherits a pass-through env that may carry a target an operator of this
// machine happened to export; refusing there would break an in-flight upgrade
// on the very host the target is about. The exemption lands runDaemonForUpgrade
// (not runDaemon), carrying the transaction id, and only when the flag is set.
func TestDaemonFlagUpgradeTransactionExemptFromRemoteRefusal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	writeConfig(t, home)
	t.Setenv("AF_DAEMON_URL", "http://box.invalid:8443")
	snapshotTargetState(t)
	runCalls, upgradeCalls, lastCfg, lastTxID := snapshotDaemonState(t)
	daemonFlag = true
	upgradeTransactionFlag = "tx-1234"

	require.NoError(t, rootCmd.RunE(&cobra.Command{}, nil))
	require.Equal(t, 1, *upgradeCalls, "RunDaemonForUpgrade must run despite the remote target (exempt path)")
	require.Equal(t, 0, *runCalls, "the upgrade path must not call the non-upgrade runDaemon")
	require.NotNil(t, *lastCfg)
	require.Equal(t, "tx-1234", *lastTxID)
}

// TestDaemonFlagRefusalDoesNotFireForRoutedTUI: the refusal is keyed on
// daemonFlag, not on the root command path, so the bare TUI (the classRouted
// form of this same verb) under a remote target still launches in registry
// mode and is untouched by the new guard. This re-pins the routed-TUI
// behaviour of root_remote_launch_test.go against the new in-verb guard.
func TestDaemonFlagRefusalDoesNotFireForRoutedTUI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	writeConfig(t, home)
	t.Setenv("AF_DAEMON_URL", "http://box.invalid:8443")
	snapshotTargetState(t)
	runCalls, _, _, _ := snapshotDaemonState(t)

	oldRun, oldTasks := runLaunchApp, launchEnsureDaemonForTasks
	t.Cleanup(func() { runLaunchApp = oldRun; launchEnsureDaemonForTasks = oldTasks })
	mounted := false
	runLaunchApp = func(_ context.Context, _, _ string, _ *config.RepoContext) error {
		mounted = true
		return nil
	}
	launchEnsureDaemonForTasks = func() {}

	// daemonFlag defaults to false unless a prior test left it set; the
	// snapshotDaemonState restore above keeps it false here.
	daemonFlag = false

	require.NoError(t, rootCmd.RunE(&cobra.Command{}, nil))
	require.True(t, mounted, "the routed TUI must launch under a remote target")
	require.Equal(t, 0, *runCalls, "the TUI path must never reach runDaemon, remote or not")
}
