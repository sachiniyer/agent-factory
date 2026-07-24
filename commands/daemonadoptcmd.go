package commands

import (
	"fmt"
	"io"
	"time"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"

	"github.com/spf13/cobra"
)

// `af daemon adopt` is #2168 Phase 5's missing recovery verb. The incident left
// a detached, unsupervised daemon serving the control socket while the installed
// systemd/launchd unit sat enabled-but-inactive, and no first-class af command
// could hand supervision back: `systemctl --user restart` found the socket
// already served, exited 0, and the unit stayed inactive. adopt performs the
// one operation that reclaims it — stop the detached daemon, start the installed
// unit in its place, and verify the unit now owns the process answering Ping.

// adoptVerifyGrace bounds how long adopt waits for the freshly started unit to
// answer Ping AND for the service manager to report it as the owner of that
// responder. A unit started Type=simple is "active" as soon as it forks, before
// the daemon binds its socket, so a single check races the bind and the
// MainPID becoming queryable. Package vars so tests can shorten the failure
// path; adoptVerifyPoll is the retry cadence, mirroring shutdownComplete* .
var (
	adoptVerifyGrace = 5 * time.Second
	adoptVerifyPoll  = 50 * time.Millisecond
)

// Indirection points so the adopt orchestration can be tested without touching
// the host's real service manager, config dir, or daemon. resolveSupervisionOwnerFn
// derives the owner from the installed unit's baked home (#2168 Phase 3);
// daemonStopFn SIGTERMs the detached daemon named in daemon.pid. The remaining
// collaborators (daemonHealthFn, daemonStatusSupervisionFn, restartAutostartUnitFn,
// waitForShutdownCompletionFn, configDirFn) are shared with the status and
// restart paths.
var (
	resolveSupervisionOwnerFn = daemon.ResolveSupervisionOwner
	daemonStopFn              = daemon.StopDaemon
)

var daemonAdoptCmd = &cobra.Command{
	Use:   "adopt",
	Short: "Hand a detached daemon back to the installed autostart unit",
	Long: `Reclaim supervision of the background daemon for the installed autostart unit.

When a daemon is running detached from its unit — an ad-hoc child a live af
spawned, which the service manager can no longer restart — adopt stops that
daemon and starts the installed unit in its place, then verifies the unit now
owns the process answering the control socket. Live sessions keep running in
tmux; the new supervised daemon re-adopts persisted session state on startup,
exactly as 'af daemon restart' does.

It needs an installed unit that serves this home ('af daemon install'); with no
such unit there is nothing to adopt the daemon into. If the installed unit
already owns the running daemon, adopt reports that and changes nothing.`,
	Args: cobra.NoArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()
		return runDaemonAdopt(cmd.OutOrStdout())
	},
}

func runDaemonAdopt(w io.Writer) error {
	configDir, err := configDirFn()
	if err != nil {
		return fmt.Errorf("cannot resolve the config dir to find the daemon autostart unit: %w", err)
	}
	owner, err := resolveSupervisionOwnerFn(configDir)
	if err != nil {
		// Fail-closed. An unreadable or unparseable unit resolves to OwnerUnknown,
		// and if the service manager cannot be consulted we could neither prove the
		// running daemon unsupervised nor start the unit — so we do not touch a
		// possibly-healthy daemon on missing evidence.
		return fmt.Errorf("cannot determine the daemon supervision owner for this home, so refusing to adopt: %w", err)
	}
	if owner != daemon.OwnerUnit {
		return fmt.Errorf("no installed autostart unit serves this home, so there is nothing to hand the daemon to — run `af daemon install` first")
	}

	// If a daemon is already serving and the installed unit already owns it,
	// there is nothing to adopt: do not cycle a healthy supervised daemon.
	h := daemonHealthFn()
	stoppedDetached := false
	if h.PingErr == nil {
		var (
			alreadyOwned bool
			cannotTell   error
		)
		daemon.ServingDaemonSupervised(h, daemonStatusSupervisionFn()).Match(
			func() { alreadyOwned = true },
			func() {},
			func() {},
			func(cause error) { cannotTell = cause },
		)
		if alreadyOwned {
			fmt.Fprintf(w, "daemon already adopted: the installed unit owns the responding daemon (pid %d)\n", h.ServingPID)
			return nil
		}
		if cannotTell != nil {
			// Fail-closed: we could not prove the running daemon is unsupervised.
			// Displacing it on missing evidence risks killing a healthy supervised
			// daemon, so refuse and point at the diagnostic instead.
			return fmt.Errorf("cannot confirm whether the running daemon is already supervised, so refusing to displace it: %w — run `af doctor` for detail", cannotTell)
		}
		// A detached, unsupervised daemon is serving. Stop it and wait for its
		// control socket to go quiet so the unit's fresh daemon acquires the freed
		// singleton lock instead of exiting against a still-live socket (#854).
		if _, err := daemonStopFn(); err != nil {
			return fmt.Errorf("failed to stop the unsupervised daemon before adopting: %w", err)
		}
		if err := waitForShutdownCompletionFn(); err != nil {
			return fmt.Errorf("the unsupervised daemon did not release the control socket, so the installed unit cannot take it over: %w", err)
		}
		stoppedDetached = true
	}

	// Start the daemon under the installed unit. RestartAutostartUnit clears
	// Phase 1's crash-loop backstop (reset-failed) and starts a fresh supervised
	// daemon — an explicit adopt is exactly the recovery path meant to clear it.
	if err := restartAutostartUnitFn(); err != nil {
		return fmt.Errorf("failed to start the installed autostart unit: %w", err)
	}

	answer, verified := verifyAdoption()
	var adoptErr error
	answer.Match(
		func() {
			if stoppedDetached {
				fmt.Fprintf(w, "daemon adopted: stopped the unsupervised daemon and handed supervision to the installed unit (pid %d)\n", verified.ServingPID)
			} else {
				fmt.Fprintf(w, "daemon adopted: the installed unit now supervises the daemon (pid %d)\n", verified.ServingPID)
			}
		},
		func() {
			adoptErr = fmt.Errorf("started the installed unit, but pid %d is answering while the unit is not reported as its owner — run `af doctor` to inspect supervision", verified.ServingPID)
		},
		func() {
			adoptErr = fmt.Errorf("started the installed unit, but the service manager has no record of it owning the responder — run `af doctor` to inspect supervision")
		},
		func(cause error) {
			adoptErr = fmt.Errorf("started the installed unit, but could not confirm it owns the responding daemon: %w — run `af doctor`", cause)
		},
	)
	return adoptErr
}

// verifyAdoption polls until the responding daemon is the installed unit's, or
// the grace elapses. A negative answer (the responder is not the unit's) or an
// undetermined one (the daemon has not answered Ping yet, or the manager has not
// yet reported MainPID) is retried, because both settle in the seconds after a
// unit start: only a persistent answer at the deadline is reported. It returns
// the final supervision answer and the last health snapshot so the caller can
// name the exact responding pid.
func verifyAdoption() (daemon.ProbeAnswer, daemon.HealthStatus) {
	deadline := time.Now().Add(adoptVerifyGrace)
	for {
		h := daemonHealthFn()
		var answer daemon.ProbeAnswer
		if h.PingErr != nil {
			answer = daemon.Undetermined(fmt.Errorf("the installed unit's daemon did not answer Ping: %w", h.PingErr))
		} else {
			answer = daemon.ServingDaemonSupervised(h, daemonStatusSupervisionFn())
		}
		adopted := false
		answer.Match(func() { adopted = true }, func() {}, func() {}, func(error) {})
		if adopted || !time.Now().Before(deadline) {
			return answer, h
		}
		time.Sleep(adoptVerifyPoll)
	}
}

func init() {
	daemonCmd.AddCommand(daemonAdoptCmd)
}
