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

// respawnDaemonForTasksFn is indirected so upgrade/auto-update tests can
// observe the respawn without launching a real daemon.
var respawnDaemonForTasksFn = respawnDaemonAfterUpgrade

// Indirection points so respawnDaemonAfterUpgrade tests can observe which
// branch ran without touching the real systemctl/launchctl or spawning a
// daemon process.
var (
	autostartInstalledFn   = daemon.AutostartInstalled
	restartAutostartUnitFn = daemon.RestartAutostartUnit
	ensureDaemonForTasksFn = ensureDaemonForTasks
)

// respawnDaemonAfterUpgrade restores the daemon that the upgrade/auto-update
// path just stopped. The Shutdown RPC is a clean exit, and the autostart unit
// uses Restart=on-failure (deliberately — Restart=always would make the
// daemon unstoppable via RPC), so the service manager will not bring the
// daemon back on its own. When the unit is installed, restart it through
// systemctl/launchctl so the daemon stays supervised instead of being demoted
// to an ad-hoc child that dies with the session and skips the next reboot
// (#796). The unit runs unconditionally at login, so this branch is not gated
// on enabled tasks — it restores exactly what was running before the upgrade.
// Without a unit, or when the service manager call fails, fall back to the
// task-gated ad-hoc spawn (the pre-#796 behavior).
func respawnDaemonAfterUpgrade() {
	if autostartInstalledFn() {
		err := restartAutostartUnitFn()
		if err == nil {
			log.InfoLog.Printf("restarted the daemon autostart unit from the new binary")
			return
		}
		log.WarningLog.Printf("failed to restart the daemon autostart unit; falling back to an ad-hoc daemon: %v", err)
	}
	ensureDaemonForTasksFn()
}

// ensureDaemonForTasks starts the daemon when any enabled task exists, so
// cron schedules are evaluated even if the user never toggles autoyes.
// Failures are logged rather than surfaced: the TUI is fully usable without
// the daemon, and the next af invocation retries.
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
