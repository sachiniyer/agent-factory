package main

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/task"

	"github.com/spf13/cobra"
)

// The daemon is the single always-on host for task schedules (cron and watch
// scripts) and autoyes mode (#782). It starts automatically whenever af runs
// and an enabled task exists; `af daemon install` additionally registers a
// user-level autostart unit so schedules survive logouts and reboots without
// ever opening af.

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Manage the background daemon that schedules tasks",
	Long: `The agent-factory daemon runs task cron schedules in-process, supervises
watch-task scripts, and drives autoyes mode. It starts automatically when you
run af and at least one enabled task exists. Install it as a user-level
autostart unit (systemd user service on Linux, launchd agent on macOS) so
tasks keep firing after reboots, even when af is never opened.`,
}

var daemonInstallCmd = &cobra.Command{
	Use:   "install",
	Short: "Register the daemon to start automatically at login",
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		unitPath, err := daemon.InstallAutostart()
		if err != nil {
			return err
		}
		fmt.Printf("daemon autostart installed: %s\n", unitPath)
		fmt.Println("the daemon now starts at login and keeps task schedules running across reboots")
		return nil
	},
}

var daemonUninstallCmd = &cobra.Command{
	Use:   "uninstall",
	Short: "Remove the daemon autostart unit",
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		unitPath, err := daemon.UninstallAutostart()
		if err != nil {
			return err
		}
		if unitPath == "" {
			fmt.Println("no daemon autostart unit is installed")
			return nil
		}
		fmt.Printf("daemon autostart removed: %s\n", unitPath)
		fmt.Println("the daemon still starts on demand whenever you run af with enabled tasks")
		return nil
	},
}

func init() {
	daemonCmd.AddCommand(daemonInstallCmd)
	daemonCmd.AddCommand(daemonUninstallCmd)
}

// respawnDaemonFn is indirected so upgrade/auto-update tests can observe the
// respawn without launching a real daemon.
var respawnDaemonFn = respawnDaemonAfterUpgrade

// Indirection points so respawnDaemonAfterUpgrade tests can observe which
// branch ran without touching the real systemctl/launchctl or spawning a
// daemon process.
var (
	autostartInstalledFn        = daemon.AutostartInstalled
	restartAutostartUnitFn      = daemon.RestartAutostartUnit
	ensureDaemonFn              = daemon.EnsureDaemon
	waitForShutdownCompletionFn = daemon.WaitForShutdownCompletion
)

// respawnDaemonAfterUpgrade restores the daemon that the upgrade/auto-update
// path just stopped. The Shutdown RPC is a clean exit, and the autostart unit
// uses Restart=on-failure (deliberately — Restart=always would make the
// daemon unstoppable via RPC), so the service manager will not bring the
// daemon back on its own. When the unit is installed, restart it through
// systemctl/launchctl so the daemon stays supervised instead of being demoted
// to an ad-hoc child that dies with the session and skips the next reboot
// (#796). Without a unit, or when the service manager call fails, spawn an
// ad-hoc daemon.
//
// Both branches respawn unconditionally: callers only reach this function
// after stopping a running daemon, and that daemon may have been serving
// autoyes mode with zero enabled tasks. Gating the fallback on enabled tasks
// left autoyes-only users without a daemon until the next af run (#813). The
// task gate belongs only on the cold-start path (ensureDaemonForTasks), where
// nothing was running and "no enabled tasks" means there is nothing to start.
func respawnDaemonAfterUpgrade() {
	// The Shutdown RPC acks before the daemon tears down, so the old daemon's
	// control socket can still answer pings here. Respawning into that window
	// makes EnsureDaemon — or the unit-restarted daemon's own startup ping
	// guard — mistake the dying daemon for a live one and skip the spawn,
	// leaving no daemon at all once it exits (#854). Wait for the socket to
	// die first; the SIGTERM fallback already waited for process exit, so the
	// wait returns immediately on that path. On timeout, warn and respawn
	// anyway: a spawn skipped against a wedged daemon is no worse than not
	// trying, and the next af invocation retries.
	if err := waitForShutdownCompletionFn(); err != nil {
		log.WarningLog.Printf("post-upgrade respawn: %v; respawning anyway, but the new daemon may see the old one as alive and exit — run af again if schedules stay dark", err)
	}
	if autostartInstalledFn() {
		err := restartAutostartUnitFn()
		if err == nil {
			log.InfoLog.Printf("restarted the daemon autostart unit from the new binary")
			return
		}
		log.WarningLog.Printf("failed to restart the daemon autostart unit; falling back to an ad-hoc daemon: %v", err)
	}
	if err := ensureDaemonFn(); err != nil {
		log.ErrorLog.Printf("failed to respawn daemon after upgrade: %v", err)
	}
}

// ensureDaemonForTasks starts the daemon when any enabled task exists, so
// cron schedules are evaluated even if the user never toggles autoyes.
// Failures are logged rather than surfaced: the TUI is fully usable without
// the daemon, and the next af invocation retries.
//
// The enabled-task gate is correct here and only here: this is the cold-start
// path (af launch), where no daemon was previously running. The post-upgrade
// respawn path must not use it — see respawnDaemonAfterUpgrade (#813).
func ensureDaemonForTasks() {
	tasks, err := task.LoadTasks()
	if err != nil {
		log.WarningLog.Printf("failed to load tasks while ensuring daemon: %v", err)
		return
	}
	for _, t := range tasks {
		if t.Enabled {
			if err := daemon.EnsureDaemon(); err != nil {
				log.ErrorLog.Printf("failed to start daemon for scheduled tasks: %v", err)
			}
			return
		}
	}
}
