package commands

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
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
watch-task scripts, drives autoyes mode, and serves the bundled web UI.

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
require_token = true demands a bearer token ('af token show') from network
peers; on the default loopback listener same-host callers stay exempt, so the
UI keeps opening with no login on this machine. Add require_loopback_token =
true to require the token from localhost as well.
Note that 'af agent-server' does not serve the web UI: it is the headless
per-workspace backend a daemon drives on a remote machine.

Clients reach the daemon over a local Unix socket by default. To drive one from
another machine, either ssh to that host and run 'af' there, or give listen_addr
a routable address and point a client at it with the persistent --daemon-url and
--token flags. A routable listener is allowed with the token off, but af warns
once at daemon start: with require_token = false anyone who can reach the
address drives your agents, so set require_token = true unless you trust the
network. That listener speaks plain HTTP either way, so put it behind a reverse
proxy or a private network (Tailscale/VPN) if you need TLS.
Full guide: https://sachiniyer.github.io/agent-factory/remote-http-auth/

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
	// ExposureWarning is non-empty when the config on disk serves the control API
	// unauthenticated on a network address (#2090) — an ALLOWED posture since
	// #2168 Phase 0, so this reports it rather than predicting a failure.
	//
	// It replaces cannot_start_reason, which named a dead end that no longer
	// exists: the daemon starts and serves in this configuration now, so a field
	// meaning "it cannot start" could only ever have been wrong. omitempty keeps
	// the JSON byte-identical for every consumer whose posture is safe, which is
	// every consumer on the default config.
	ExposureWarning string `json:"exposure_warning,omitempty"`
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
	// Read-only (LoadConfigReadOnly materializes and converts nothing, so `af
	// daemon status` never writes config as a side effect). Reported whether or
	// not a daemon is running: an exposed listener is most worth saying when it is
	// actually being served. This reads the config on disk, which a long-running
	// daemon may predate — reporting the posture the RUNNING daemon booted with
	// needs a Ping that carries it (#2168 Phase 4).
	if load, err := config.LoadConfigReadOnly(); err == nil {
		info.ExposureWarning = config.ListenerExposureNotice(load.Config)
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
		// The on-demand promise is unconditional again: since #2168 Phase 0 there
		// is no config the daemon refuses to start under, so there is no posture
		// that makes this line a lie.
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
	if info.ExposureWarning != "" {
		fmt.Fprintf(w, "  warning:        %s\n", info.ExposureWarning)
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
// daemon process. autostartUnitServesHomeFn (reset.go) is shared.
var (
	autostartInstalledFn        = daemon.AutostartInstalled
	restartAutostartUnitFn      = daemon.RestartAutostartUnit
	ensureDaemonFromPathFn      = daemon.EnsureDaemonFromPath
	waitForShutdownCompletionFn = daemon.WaitForShutdownCompletion
	autostartUnitExecPathFn     = daemon.AutostartUnitExecPath
	configDirFn                 = config.GetConfigDir
)

// respawnResult reports the ways a respawn can succeed and still leave the
// user worse off than they think, so callers can say so instead of assuming
// the restart landed the way they asked. Both were log-only warnings printed
// underneath an unqualified "Restarted the running daemon from the new binary"
// (#1947). The zero value is the good outcome: the daemon came back, on the
// binary we just wrote, however it was started.
type respawnResult struct {
	// UnitErr is set when a unit restart was attempted and failed, so the
	// daemon was demoted to the ad-hoc fallback: running and on the new
	// binary, but unsupervised — it dies with the session and skips the next
	// login. Nil when no unit was restarted because none serves this home,
	// which is not a degradation.
	UnitErr error
	// UnitGateErr is set when we could not TELL whether the installed unit
	// serves this home, so we conservatively did not restart it and spawned an
	// ad-hoc daemon instead. That is the safe call, but it is a real
	// degradation the user has to hear about: if the unit was in fact theirs,
	// their supervised daemon just became an unsupervised one. Distinct from
	// the quiet, correct "no unit serves this home" case, which is not an
	// error and must stay silent.
	UnitGateErr error
	// StaleUnitExec is the binary the restarted unit launches, set only when
	// that is NOT the binary the upgrade just wrote. The unit bakes its
	// program path at install time, so a second install on the box brings the
	// OLD image straight back up — the restart lands, and changes nothing.
	StaleUnitExec string
}

// restartPhase names the step of the shutdown-then-respawn sequence that
// failed. The two failures are OPPOSITE states and must never share a message:
//
//   - restartPhaseShutdown: the old daemon would not stop, so it is STILL
//     RUNNING THE OLD BINARY. That is #1947's own symptom — the upgrade did not
//     reach the daemon.
//   - restartPhaseRespawn: the old daemon stopped but no new one came up, so
//     NOTHING IS RUNNING. Task schedules, watch scripts and autoyes are all
//     stopped until something starts a daemon.
//
// "Still on the old code" and "no daemon at all" need opposite remedies, and
// telling them apart by reading the wrapped error's text is exactly the
// guessing this PR exists to delete. The phase is carried, not inferred.
type restartPhase int

const (
	// restartPhaseNone: nothing failed. The zero value, valid only alongside a
	// nil error.
	restartPhaseNone restartPhase = iota
	restartPhaseShutdown
	restartPhaseRespawn
)

// restartOutcome is the whole story of a shutdown-then-respawn: how the old
// daemon was stopped, and how (or whether) a new one came back.
type restartOutcome struct {
	Shutdown daemon.ShutdownResult
	// Respawned is false when no daemon was running, so nothing was respawned.
	Respawned bool
	Respawn   respawnResult
	// FailedPhase is restartPhaseNone unless the accompanying error is
	// non-nil, and names which half of the sequence broke.
	FailedPhase restartPhase
}

// restartDaemonFromPath keeps the (result, error) shape the auto-update path
// and `af daemon restart` are written against. Callers that must report on the
// restart's fidelity — `af upgrade` — use restartDaemonFromPathDetailed.
func restartDaemonFromPath(execPath string) (daemon.ShutdownResult, error) {
	outcome, err := restartDaemonFromPathDetailed(execPath)
	return outcome.Shutdown, err
}

func restartDaemonFromPathDetailed(execPath string) (restartOutcome, error) {
	result, shutdownErr := requestDaemonShutdownFn()
	outcome := restartOutcome{Shutdown: result}
	if shutdownErr != nil {
		outcome.FailedPhase = restartPhaseShutdown
		return outcome, fmt.Errorf("failed to stop running daemon: %w", shutdownErr)
	}
	if result == daemon.ShutdownNoDaemon {
		return outcome, nil
	}
	respawn, err := respawnDaemonFn(execPath)
	outcome.Respawn = respawn
	if err != nil {
		outcome.FailedPhase = restartPhaseRespawn
		return outcome, fmt.Errorf("failed to restart daemon: %w", err)
	}
	outcome.Respawned = true
	return outcome, nil
}

// unitRestartTarget decides whether the post-upgrade respawn may restart the
// installed autostart unit, and reports the binary that unit would launch.
//
// "A unit file exists" and "that unit is the daemon I just stopped" are
// different questions (#1916/#1919, and #1950 for this path). The unit bakes
// its AGENT_FACTORY_HOME at install time, so `AGENT_FACTORY_HOME=/tmp/sandbox
// af upgrade` gating on existence alone restarts the developer's REAL daemon
// and returns — leaving the sandbox it was actually upgrading with no daemon
// at all. Anything short of proof that the unit serves this home means we do
// not touch it and spawn an ad-hoc daemon for the home in front of us.
// gateErr is returned (not just logged) when the decision could not be made:
// skipping the unit is the safe call, but it silently costs a supervised
// daemon its supervision, and this whole path exists because degradations that
// only reach the log are degradations nobody fixes.
func unitRestartTarget() (useUnit bool, unitExec string, gateErr error) {
	configDir, err := configDirFn()
	if err != nil {
		return false, "", fmt.Errorf("cannot resolve the config dir to check whether the autostart unit serves this home: %w", err)
	}
	serves, installed, err := autostartUnitServesHomeFn(configDir)
	if err != nil {
		return false, "", fmt.Errorf("cannot tell whether the autostart unit serves %s: %w", configDir, err)
	}
	if !installed || !serves {
		// The ordinary answer for an ad-hoc install, and for any upgrade run
		// against a home the installed unit does not serve. Not an error.
		return false, "", nil
	}
	// Best-effort, and log-only on purpose: the unit restart below is the
	// authority on whether the daemon came back, and it is about to run either
	// way. An unreadable program path costs only the staleness check — no
	// behavior changes — so it does not warrant a line the user must act on.
	unitExec, _, err = autostartUnitExecPathFn()
	if err != nil {
		log.WarningLog.Printf("post-upgrade respawn: cannot read the autostart unit's program path, so cannot check it launches the upgraded binary: %v", err)
	}
	return true, unitExec, nil
}

// staleUnitExec reports the unit's program path when it is NOT the binary the
// upgrade just wrote, and "" when they agree (or when it cannot be told).
// Both sides are resolved through symlinks first: the unit is routinely
// installed with a symlinked path (~/.local/bin/af) while the upgrade resolves
// to the real file, and those are the same binary.
func staleUnitExec(unitExec, upgradedPath string) string {
	if unitExec == "" {
		return ""
	}
	if canonicalExec(unitExec) == canonicalExec(upgradedPath) {
		return ""
	}
	return unitExec
}

// canonicalExec resolves p through symlinks, falling back to the input when it
// cannot be resolved (the unit may name a binary that no longer exists — which
// is itself worth reporting as a mismatch rather than swallowing).
func canonicalExec(p string) string {
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

// respawnDaemonAfterUpgrade restores the daemon that the upgrade/auto-update
// path just stopped. The Shutdown RPC is a clean exit, and the autostart unit
// uses Restart=on-failure (deliberately — Restart=always would make the
// daemon unstoppable via RPC), so the service manager will not bring the
// daemon back on its own. When the unit is installed AND SERVES THIS HOME,
// restart it through systemctl/launchctl so the daemon stays supervised
// instead of being demoted to an ad-hoc child that dies with the session and
// skips the next reboot (#796). Without such a unit, or when the service
// manager call fails, spawn an ad-hoc daemon.
//
// The returned respawnResult says which of those actually happened. Callers
// must report a degraded respawn rather than printing plain success over it:
// silently demoting to an ad-hoc daemon under a "Restarted the running daemon"
// line is half of #1947.
//
// Both branches respawn unconditionally: callers only reach this function
// after stopping a running daemon, and that daemon may have been serving
// autoyes mode with zero enabled tasks. Gating the fallback on enabled tasks
// left autoyes-only users without a daemon until the next af run (#813). The
// task gate belongs only on the cold-start path (ensureDaemonForTasks), where
// nothing was running and "no enabled tasks" means there is nothing to start.
func respawnDaemonAfterUpgrade(execPath string) (respawnResult, error) {
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
	useUnit, unitExec, gateErr := unitRestartTarget()
	if gateErr != nil {
		log.WarningLog.Printf("post-upgrade respawn: not restarting the autostart unit: %v", gateErr)
	}
	if useUnit {
		err := restartAutostartUnitFn()
		if err == nil {
			log.InfoLog.Printf("restarted the daemon autostart unit from the new binary")
			return respawnResult{StaleUnitExec: staleUnitExec(unitExec, execPath)}, nil
		}
		unitErr = err
		log.WarningLog.Printf("failed to restart the daemon autostart unit; falling back to an ad-hoc daemon: %v", err)
	}
	if err := ensureDaemonFromPathFn(execPath); err != nil {
		log.ErrorLog.Printf("failed to respawn daemon after upgrade: %v", err)
		if unitErr != nil {
			return respawnResult{UnitErr: unitErr, UnitGateErr: gateErr},
				fmt.Errorf("unit restart failed: %w; ad-hoc fallback failed: %v", unitErr, err)
		}
		return respawnResult{UnitGateErr: gateErr}, err
	}
	return respawnResult{UnitErr: unitErr, UnitGateErr: gateErr}, nil
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
