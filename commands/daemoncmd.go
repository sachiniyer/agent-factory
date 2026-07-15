package commands

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/task"

	"github.com/spf13/cobra"
)

// The daemon is the single always-on host for task schedules (cron and watch
// scripts) and autoyes mode (#782), and serves the web UI. Launching the TUI
// starts it: the cold start reads session state through the daemon
// (coldStartFromSnapshot -> withDaemonHTTP -> daemon.EnsureDaemon), and autoyes
// and ensureDaemonForTasks cover the other root-path cases. `af daemon install`
// registers a user-level autostart unit so schedules and the web UI survive
// logouts and reboots without ever opening af.

var daemonCmd = &cobra.Command{
	Use:   "daemon",
	Short: "Manage the background daemon: serves the web UI and schedules tasks",
	Long: `The agent-factory daemon runs task cron schedules in-process, supervises
watch-task scripts, drives autoyes mode, and serves the bundled WEB UI.

The web UI is part of the daemon — there is no separate web command — so it is
served whenever the daemon is running. Running af starts one: the TUI reads
session state through the daemon and spawns it if none is up, so simply opening
af serves the web UI. Autoyes mode and any enabled task start one too. Only
standalone commands that never talk to the daemon (such as 'af config list')
leave it down.

With af running, open:

    http://localhost:8443

It needs no token by default, so the page connects as soon as it loads. Set
listen_addr to change the address (or to "" to turn the web server off).
require_token = true demands a bearer token ('af token show') from NETWORK
peers; on the default loopback listener same-host callers stay exempt, so the
UI keeps opening with no login on this machine. Add require_loopback_token =
true to require the token from localhost as well.
Note 'af agent-server' does NOT serve the web UI: it is the headless
per-workspace backend a daemon drives on a remote machine.

Install the daemon as a user-level autostart unit (systemd user service on
Linux, launchd agent on macOS) so tasks keep firing and the web UI stays up
after reboots, even when af is never opened:

    af daemon install`,
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

// daemonRestartQuiet suppresses the "no daemon" no-op line. The install
// scripts use this so fresh installs stay quiet while real restarts still
// print a confirmation.
var daemonRestartQuiet bool

var daemonRestartCmd = &cobra.Command{
	Use:   "restart",
	Short: "Restart the running daemon without stopping live sessions",
	Long: `Restart the background daemon if one is running. Live sessions keep running
in tmux; the new daemon re-adopts persisted session state on startup. If no
daemon is running, this command exits successfully without starting one.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		execPath, err := osExecutableFn()
		if err != nil {
			return fmt.Errorf("failed to find current executable: %w", err)
		}
		resolvedPath, err := filepath.EvalSymlinks(execPath)
		if err != nil {
			return fmt.Errorf("failed to resolve executable path: %w", err)
		}

		result, err := restartDaemonFromPath(resolvedPath)
		if err != nil {
			return err
		}

		w := cmd.OutOrStdout()
		switch result {
		case daemon.ShutdownNoDaemon:
			if !daemonRestartQuiet {
				fmt.Fprintln(w, "no running daemon to restart")
			}
		case daemon.ShutdownViaSIGTERM:
			fmt.Fprintln(w, "daemon restarted (stopped old daemon via SIGTERM fallback)")
		default:
			fmt.Fprintln(w, "daemon restarted")
		}
		return nil
	},
}

func init() {
	daemonStatusCmd.Flags().BoolVar(&daemonStatusJSON, "json", false,
		"Emit the status as JSON wrapped in the {data,error} envelope")
	daemonRestartCmd.Flags().BoolVar(&daemonRestartQuiet, "quiet", false,
		"Suppress output when no daemon is running")
	daemonCmd.AddCommand(daemonInstallCmd)
	daemonCmd.AddCommand(daemonRestartCmd)
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
	ensureDaemonFromPathFn      = daemon.EnsureDaemonFromPath
	waitForShutdownCompletionFn = daemon.WaitForShutdownCompletion
)

func restartDaemonFromPath(execPath string) (daemon.ShutdownResult, error) {
	result, shutdownErr := requestDaemonShutdownFn()
	if shutdownErr != nil {
		return result, fmt.Errorf("failed to stop running daemon: %w", shutdownErr)
	}
	if result == daemon.ShutdownNoDaemon {
		return result, nil
	}
	if err := respawnDaemonFn(execPath); err != nil {
		return result, fmt.Errorf("failed to restart daemon: %w", err)
	}
	return result, nil
}

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
func respawnDaemonAfterUpgrade(execPath string) error {
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
	var unitErr error
	if autostartInstalledFn() {
		err := restartAutostartUnitFn()
		if err == nil {
			log.InfoLog.Printf("restarted the daemon autostart unit from the new binary")
			return nil
		}
		unitErr = err
		log.WarningLog.Printf("failed to restart the daemon autostart unit; falling back to an ad-hoc daemon: %v", err)
	}
	if err := ensureDaemonFromPathFn(execPath); err != nil {
		log.ErrorLog.Printf("failed to respawn daemon after upgrade: %v", err)
		if unitErr != nil {
			return fmt.Errorf("unit restart failed: %w; ad-hoc fallback failed: %v", unitErr, err)
		}
		return err
	}
	return nil
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
