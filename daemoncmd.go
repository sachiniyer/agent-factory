package main

import (
	"fmt"
	"os"

	"github.com/sachiniyer/agent-factory/apiproto"
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

// daemonStatusJSON switches `af daemon status` to the shared {data,error}
// envelope. Local to this command (like `af api`'s --json) — there is no
// bare-vs-envelope legacy to preserve.
var daemonStatusJSON bool

// daemonStatusInfo is the read-only liveness/topology snapshot printed by
// `af daemon status`. It is derived entirely from daemon.Health() (the same
// no-spawn probe `af doctor` uses) plus an os.Stat of the HTTP socket, so the
// command never dials or starts a daemon and needs no new RPC.
type daemonStatusInfo struct {
	Running           bool   `json:"running"`
	ControlSocket     string `json:"control_socket"`
	ControlSocketFile bool   `json:"control_socket_file"`
	HTTPSocket        string `json:"http_socket"`
	HTTPSocketFile    bool   `json:"http_socket_file"`
	PID               int    `json:"pid"`
	PIDVerified       bool   `json:"pid_verified"`
	AutostartUnit     bool   `json:"autostart_unit"`
	BinaryStale       bool   `json:"binary_stale"`
}

// collectDaemonStatus builds a daemonStatusInfo from the read-only health
// probe. httpSocketPath is resolved separately (its own path helper) and
// stat'd; a missing config dir leaves the field empty rather than erroring, so
// status still reports the control-plane facts.
func collectDaemonStatus() daemonStatusInfo {
	h := daemon.Health()
	info := daemonStatusInfo{
		Running:           h.PingErr == nil,
		ControlSocket:     h.SocketPath,
		ControlSocketFile: h.SocketExists,
		PID:               h.PIDFilePID,
		PIDVerified:       h.PIDVerified,
		AutostartUnit:     h.AutostartUnit,
		BinaryStale:       h.BinaryDeleted,
	}
	if httpPath, err := daemon.DaemonHTTPSocketPath(); err == nil {
		info.HTTPSocket = httpPath
		if _, err := os.Stat(httpPath); err == nil {
			info.HTTPSocketFile = true
		}
	}
	return info
}

// printDaemonStatusHuman renders the snapshot as a short human report mirroring
// the wording `af doctor` uses for the daemon check.
func printDaemonStatusHuman(cmd *cobra.Command, info daemonStatusInfo) {
	w := cmd.OutOrStdout()
	if info.Running {
		fmt.Fprintln(w, "daemon: running")
	} else {
		fmt.Fprintln(w, "daemon: not running (starts on demand when you run af with an enabled task)")
	}
	fmt.Fprintf(w, "  control socket: %s (%s)\n", info.ControlSocket, presence(info.ControlSocketFile))
	if info.HTTPSocket != "" {
		fmt.Fprintf(w, "  http socket:    %s (%s)\n", info.HTTPSocket, presence(info.HTTPSocketFile))
	}
	if info.PID > 0 {
		pidNote := "unverified"
		if info.PIDVerified {
			pidNote = "verified"
		}
		fmt.Fprintf(w, "  pid:            %d (%s)\n", info.PID, pidNote)
	} else {
		fmt.Fprintln(w, "  pid:            (no daemon.pid on disk)")
	}
	if info.AutostartUnit {
		fmt.Fprintln(w, "  autostart:      installed")
	} else {
		fmt.Fprintln(w, "  autostart:      not installed (`af daemon install` to keep schedules running across reboots)")
	}
	if info.BinaryStale {
		fmt.Fprintf(w, "  warning:        pid %d is running a binary since replaced on disk — restart the daemon to pick up the new version\n", info.PID)
	}
}

// presence is the file-present/absent label shared by the two socket lines.
func presence(exists bool) string {
	if exists {
		return "present"
	}
	return "absent"
}

var daemonStatusCmd = &cobra.Command{
	Use:   "status",
	Short: "Report daemon liveness, sockets, pid, and autostart",
	Long: `Print a read-only snapshot of the background daemon: whether it is responding
on the control socket, the control and HTTP socket paths (and whether their
files are present), the recorded pid and whether it is a verified af daemon,
whether the autostart unit is installed, and whether the running daemon is on a
since-replaced binary.

It never contacts a paused daemon in a way that spawns one and never starts the
daemon — it uses the same no-spawn health probe as af doctor. Use --json for a
machine-readable form wrapped in the shared {data,error} envelope.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		info := collectDaemonStatus()
		if daemonStatusJSON {
			return apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(info))
		}
		printDaemonStatusHuman(cmd, info)
		return nil
	},
}

func init() {
	daemonStatusCmd.Flags().BoolVar(&daemonStatusJSON, "json", false,
		"Emit the status as JSON wrapped in the {data,error} envelope")
	daemonCmd.AddCommand(daemonInstallCmd)
	daemonCmd.AddCommand(daemonUninstallCmd)
	daemonCmd.AddCommand(daemonStatusCmd)
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
