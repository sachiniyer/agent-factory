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

// These tests pin the #7558 in-verb refusal added to the `--daemon` branch of
// root's RunE: `af --daemon` starts the daemon process on THIS machine, so a
// remote target named EXPLICITLY on the invocation (--daemon-url or
// AF_DAEMON_URL) refuses through requireLocalDaemonStart, naming the target —
// where it used to be silently accepted and dropped. A SAVED target never
// refuses: the daemon is always local and a saved target is a client
// preference, so the start logs one INFO line naming it and proceeds.
//
// That split is what keeps the supervisor-owned launches — the autostart
// unit's `ExecStart=<path> --daemon` (daemon/autostart.go's
// systemdAutostartUnit/launchdAutostartPlist: argv is the path plus --daemon,
// and the Environment=/EnvironmentVariables entries carry only the marker,
// PATH, SHELL and an optional AGENT_FACTORY_HOME), rollback's
// startPreviousDaemon and the committed handoff's startArmedDaemonUnderOwner
// (daemon/upgrade_recovery.go, daemon/upgrade_forward.go — both reach
// startDaemonChild, whose argv is `<exec> --daemon` and whose cmd.Env is left
// unset so it inherits the recovery actor's environment), and the probation
// candidate's `<exec> --daemon --upgrade-transaction <id>` — working in a
// home that happens to hold a saved target. The recovery actor itself runs
// under a recovery unit that renders no environment at all
// (internal/upgradetxn/recovery_job.go), so none of these launches passes the
// flag or the variable, and none of them meets the refusal.
//
// The runDaemon/runDaemonForUpgrade seams keep the tests from binding the
// real control/HTTP sockets.

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

// daemonLog reads the daemon log file the RunE's log.Initialize(daemonFlag)
// routed to $AGENT_FACTORY_HOME/agent-factory.log. The RunE's deferred
// log.Close has run by the time this is called, so the file is complete.
func daemonLog(t *testing.T, home string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, "agent-factory.log"))
	require.NoError(t, err, "log.Initialize should have created the daemon log in the sandboxed home")
	return string(data)
}

// TestDaemonFlagRefusesExplicitTargetFromFlag: a --daemon-url FLAG on the
// invocation is the operator saying which daemon they mean — af --daemon
// refuses it, naming the target, and never reaches runDaemon. No config.toml
// is written: the refusal must precede LoadConfig (pinned explicitly in
// TestDaemonFlagRefusalPrecedesConfigLoad).
func TestDaemonFlagRefusesExplicitTargetFromFlag(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("AF_DAEMON_URL", "")
	snapshotTargetState(t)
	apiclient.FlagDaemonURL = "http://flag.invalid:8443"
	runCalls, _, _, _ := snapshotDaemonState(t)
	daemonFlag = true

	err := rootCmd.RunE(&cobra.Command{}, nil)
	require.Error(t, err)
	for _, want := range []string{
		"af --daemon",
		"local-only",
		"starts the daemon process on this machine",
		"the remote daemon at http://flag.invalid:8443",
		"--daemon-url/AF_DAEMON_URL",
		"run af --daemon on the daemon host",
	} {
		require.Contains(t, err.Error(), want)
	}
	require.Equal(t, 0, *runCalls, "a refusal must not reach runDaemon")
}

// TestDaemonFlagRefusesExplicitTargetFromEnv: the AF_DAEMON_URL env var is
// explicit the same way the flag is — an exported target on this invocation —
// so it refuses identically.
func TestDaemonFlagRefusesExplicitTargetFromEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("AF_DAEMON_URL", "http://box.invalid:8443")
	snapshotTargetState(t)
	runCalls, _, _, _ := snapshotDaemonState(t)
	daemonFlag = true

	err := rootCmd.RunE(&cobra.Command{}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "af --daemon")
	require.Contains(t, err.Error(), "the remote daemon at http://box.invalid:8443")
	require.Equal(t, 0, *runCalls, "a refusal must not reach runDaemon")
}

// TestDaemonFlagSavedTargetStartsAndLogsIt: a SAVED target alone never
// refuses af --daemon — the daemon is always local and a saved target is a
// client preference. The start reaches runDaemon and the daemon log carries
// the one INFO line naming the saved target it ignored.
func TestDaemonFlagSavedTargetStartsAndLogsIt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	writeConfig(t, home)
	t.Setenv("AF_DAEMON_URL", "")
	snapshotTargetState(t)
	require.NoError(t, apiclient.SaveTarget(apiclient.SavedTarget{URL: "http://saved.invalid:8443"}))
	runCalls, upgradeCalls, lastCfg, _ := snapshotDaemonState(t)
	daemonFlag = true

	require.NoError(t, rootCmd.RunE(&cobra.Command{}, nil))
	require.Equal(t, 1, *runCalls, "a saved target alone must not refuse the daemon start")
	require.Equal(t, 0, *upgradeCalls)
	require.NotNil(t, *lastCfg, "runDaemon must receive the loaded config")

	out := daemonLog(t, home)
	require.Contains(t, out, "INFO:")
	require.Contains(t, out, "saved remote target http://saved.invalid:8443")
}

// TestDaemonFlagUnreadableSavedTargetStillStarts: a saved target that fails
// closed (here: group/other-readable, which LoadSavedTarget refuses) fails
// CLIENTS closed, but a daemon start has no wrong host to act on — it warns
// and proceeds, so a corrupt client-target.json can never wedge a
// supervisor's restart of a daemon it never aimed at.
func TestDaemonFlagUnreadableSavedTargetStillStarts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	writeConfig(t, home)
	t.Setenv("AF_DAEMON_URL", "")
	snapshotTargetState(t)
	require.NoError(t, apiclient.SaveTarget(apiclient.SavedTarget{URL: "http://saved.invalid:8443"}))
	path, err := apiclient.SavedTargetPath()
	require.NoError(t, err)
	require.NoError(t, os.Chmod(path, 0o644)) // LoadSavedTarget refuses group/other-readable
	runCalls, _, _, _ := snapshotDaemonState(t)
	daemonFlag = true

	require.NoError(t, rootCmd.RunE(&cobra.Command{}, nil))
	require.Equal(t, 1, *runCalls, "a broken saved target must not refuse the daemon start")
	require.Contains(t, daemonLog(t, home), "ignores the saved remote target")
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

// TestDaemonFlagUpgradeTransactionGetsNoExemption: the hidden
// --upgrade-transaction flag is NOT an exemption — it is a string flag an
// operator can pass, not supervisor authentication. A probationary candidate
// launched with an explicit target refuses exactly like a plain start; the
// real candidate's spawn (daemon's startDaemonChild) inherits an env the
// recovery unit renders with no AF_DAEMON_URL, so it never meets the refusal.
func TestDaemonFlagUpgradeTransactionGetsNoExemption(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	writeConfig(t, home)
	t.Setenv("AF_DAEMON_URL", "http://box.invalid:8443")
	snapshotTargetState(t)
	runCalls, upgradeCalls, _, _ := snapshotDaemonState(t)
	daemonFlag = true
	upgradeTransactionFlag = "tx-1234"

	err := rootCmd.RunE(&cobra.Command{}, nil)
	require.Error(t, err)
	require.Contains(t, err.Error(), "af --daemon")
	require.Contains(t, err.Error(), "the remote daemon at http://box.invalid:8443")
	require.Equal(t, 0, *runCalls)
	require.Equal(t, 0, *upgradeCalls)
}

// TestDaemonFlagSupervisorLaunchesCarryNoExplicitTarget: every launch that a
// supervisor or the upgrade machinery drives produces an argv/env shape with
// no explicit target — no --daemon-url flag, no AF_DAEMON_URL in the
// environment — so in a home that happens to hold a saved target each one
// still starts the local daemon (and logs the ignored saved target). The
// argv/env each row simulates is exactly what the cited launch site emits:
//
//   - the systemd unit's ExecStart=<path> --daemon (daemon/autostart.go's
//     systemdAutostartUnit; its Environment= lines carry only the marker,
//     PATH, SHELL and an optional AGENT_FACTORY_HOME);
//   - rollback's startPreviousDaemon (daemon/upgrade_recovery.go), which is
//     either the same unit start or an ad-hoc startDaemonChild spawn — argv
//     `<exec> --daemon`, cmd.Env unset;
//   - the committed handoff's startArmedDaemonUnderOwner
//     (daemon/upgrade_forward.go) — the same two paths;
//   - the probation candidate's launchCandidateDaemon — argv
//     `<exec> --daemon --upgrade-transaction <id>`.
//
// The recovery actor that drives the last three runs under a recovery unit
// that renders no environment (internal/upgradetxn/recovery_job.go), and a
// daemon started under this build was itself refused had its start carried
// AF_DAEMON_URL — so the variable cannot ride down through the inherited
// environment either.
func TestDaemonFlagSupervisorLaunchesCarryNoExplicitTarget(t *testing.T) {
	for _, launch := range []struct {
		name  string
		txID  string // non-empty simulates the --upgrade-transaction argv member
		entry string // the launch site, for failure messages
	}{
		{name: "systemd unit ExecStart", entry: "daemon/autostart.go systemdAutostartUnit"},
		{name: "rollback startPreviousDaemon", entry: "daemon/upgrade_recovery.go"},
		{name: "handoff startArmedDaemonUnderOwner", entry: "daemon/upgrade_forward.go"},
		{name: "probation candidate", txID: "tx-4242", entry: "daemon/upgrade_forward.go launchCandidateDaemon"},
	} {
		t.Run(launch.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("AGENT_FACTORY_HOME", home)
			writeConfig(t, home)
			t.Setenv("AF_DAEMON_URL", "")
			snapshotTargetState(t)
			require.NoError(t, apiclient.SaveTarget(apiclient.SavedTarget{URL: "http://saved.invalid:8443"}))
			runCalls, upgradeCalls, _, _ := snapshotDaemonState(t)
			daemonFlag = true
			upgradeTransactionFlag = launch.txID

			require.NoError(t, rootCmd.RunE(&cobra.Command{}, nil), launch.entry)
			if launch.txID != "" {
				require.Equal(t, 1, *upgradeCalls, launch.entry)
				require.Equal(t, 0, *runCalls, launch.entry)
			} else {
				require.Equal(t, 1, *runCalls, launch.entry)
				require.Equal(t, 0, *upgradeCalls, launch.entry)
			}
			require.Contains(t, daemonLog(t, home), "saved remote target http://saved.invalid:8443", launch.entry)
		})
	}
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
