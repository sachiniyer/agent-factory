//go:build linux

package tmux

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/internal/afhome"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/internal/systemdunit"
	"github.com/sachiniyer/agent-factory/log"
)

const (
	dedicatedServerExecMarker = "__af-tmux-server"
	dedicatedServerLogEnv     = "AF_TMUX_SERVER_LOG"
	dedicatedServerLogName    = "tmux-server.log"
	dedicatedServerUnitEnv    = "AF_TMUX_SERVER_UNIT"
	serverProbeTimeout        = 250 * time.Millisecond
	tmuxVersionProbeTimeout   = 2 * time.Second
	serverStartupTimeout      = 2 * time.Second
	serverScopeKillTimeout    = 2 * time.Second
	// serverProbeMaxAttempts bounds the liveness probe's retry budget before
	// the gate falls through to the relaunch path. A single-shot 250 ms probe
	// collapsing a transient timeout against a live server into "no server" is
	// the root cause this gate guards against; a bounded retry — the same
	// shape #3371 gave the -V probe — lets a transient stall recover and adopt
	// the pre-existing server without reaching the relaunch.
	serverProbeMaxAttempts = 3
	// serverProbeRetryInterval paces the liveness probe's retries so a
	// transient stall has a chance to lift before the next attempt.
	serverProbeRetryInterval = 50 * time.Millisecond
	// attachFailureGrace is how long the readiness loop waits for the wrapper
	// to exit before stamping a dedicated record when the gate could not
	// definitively prove no server was running. A wrapper whose `tmux -D` is
	// attach-failing against a pre-existing server exits within tens of ms
	// (measured: ~18–48 ms); a wrapper that legitimately started the server
	// stays alive. The grace lets the readiness loop tell the two apart and
	// adopt the pre-existing server rather than stamping the already-exiting
	// wrapper's unit as the host. Only the non-conclusive-gate path waits —
	// the definitive-no case has no pre-existing server to race, so it pays
	// no grace.
	attachFailureGrace = 250 * time.Millisecond
)

var (
	daemonServerConfigMu sync.RWMutex
	daemonServerHome     string
	serverBootstrapMu    sync.Mutex
	noStartWarningOnce   sync.Once
	noStartProbeMu       sync.Mutex
	noStartProbeCached   bool
	noStartProbeSupports bool
)

// ConfigureDaemonServer gives the tmux launch path the AF home where the
// dedicated server wrapper keeps its bounded stderr log. RunDaemon installs
// this before restore and removes it on exit; ordinary CLI processes never
// configure a bootstrapper and retain their historical direct launch.
func ConfigureDaemonServer(home string) func() {
	if absolute, err := filepath.Abs(home); err == nil {
		home = absolute
	}
	daemonServerConfigMu.Lock()
	previous := daemonServerHome
	resetNoStartProbeCache()
	daemonServerHome = home
	daemonServerConfigMu.Unlock()
	return func() {
		daemonServerConfigMu.Lock()
		resetNoStartProbeCache()
		daemonServerHome = previous
		daemonServerConfigMu.Unlock()
	}
}

func resetNoStartProbeCache() {
	noStartProbeMu.Lock()
	noStartProbeCached = false
	noStartProbeSupports = false
	noStartProbeMu.Unlock()
}

func configuredDaemonServerHome() (string, bool) {
	daemonServerConfigMu.RLock()
	defer daemonServerConfigMu.RUnlock()
	return daemonServerHome, daemonServerHome != ""
}

// EnsureDaemonServer starts tmux before any daemon-owned session spawn. The
// foreground server lives in its own named systemd scope, independent of every
// session launch and of the daemon service itself. tmux -D both preserves its
// stderr and disables exit-empty for the server it creates.
func EnsureDaemonServer() error {
	if !systemdunit.RunningDaemonProcess() {
		return nil
	}
	home, configured := configuredDaemonServerHome()
	if !configured {
		return fmt.Errorf("dedicated tmux server bootstrap is not configured")
	}
	return ensureDedicatedServer(home)
}

func ensureDedicatedServer(home string) error {
	serverBootstrapMu.Lock()
	defer serverBootstrapMu.Unlock()

	// Latch the prior-generation verdict before this bootstrap can overwrite
	// the residue it is read from: the wrapper below rewrites the
	// server-death marker when its tmux exits, so a record written after a
	// failed launch would otherwise read only this generation's own marker
	// (#6610).
	_ = priorDaemonGenerationEvidence(home)

	// Liveness gate. A single-shot 250 ms probe collapsing a transient
	// timeout against a live server into "no server" is the root cause of the
	// evidence corruption this gate guards against — relaunching a wrapper on
	// that guess makes the wrapper's `tmux -D` a headless attach against the
	// still-healthy server, fails instantly, and (without the wrapper's
	// post-exit check and the readiness loop's adopt-on-exit below) stamps a
	// false `tmux-server-death.json` plus a hosting-unit record naming the
	// already-exited wrapper's scope (#5429/#6339/#6610).
	//
	// Distinguish a definitive answer from a transient miss — the same
	// treatment #3371 gave the -V probe — and retry briefly before accepting
	// "no server": a transient stall that recovers within the retry budget
	// adopts the pre-existing server here, never reaching the relaunch path.
	// "I could not look" never becomes "it is gone": a non-conclusive probe
	// falls through to the relaunch path, where the wrapper's post-exit
	// check and the readiness loop's adopt-on-exit catch the residual case.
	gateRunning, gateDefinite := probeServerDefinitively()
	if gateRunning {
		// Do not mutate a foreign or legacy server. Its ordinary exit-empty
		// policy lets it retire after its last session, at which point a later
		// spawn replaces it with the dedicated server below. The record must
		// still be rewritten — this early return is how a restart that adopts
		// an existing server used to leave the previous generation's record in
		// place, letting doctor pin its reads to a socket the daemon no longer
		// uses (#6339).
		recordAdoptedServer(home)
		return nil
	}
	// gateProvedNoServer is true only when the probe DEFINITIVELY said no
	// server is running (the authoritative no-server diagnostic
	// NoServerRunning already trusts elsewhere). A non-conclusive probe (a
	// transient stall that survived the retry budget) leaves it false, so the
	// readiness loop knows a server that answers after launch MIGHT be a
	// pre-existing one the gate missed — and stamps adopt rather than
	// attributing the answering server to the just-launched wrapper.
	gateProvedNoServer := gateDefinite
	// afhome.MkdirAll, not os.MkdirAll: the argument IS the AF home, so this is
	// the most direct resurrection there is — and it runs on any spawn where the
	// dedicated server is not up, which is exactly the state the comment above
	// describes after exit-empty retires it (#3850). afhome rather than
	// config.MkdirAllUnderAFHome because config imports this package.
	if err := afhome.MkdirAll(home, 0o700); err != nil {
		return fmt.Errorf("create AF home for tmux server log: %w", err)
	}
	logPath := filepath.Join(home, dedicatedServerLogName)
	if err := secureExistingServerLog(logPath); err != nil {
		return fmt.Errorf("secure existing tmux server log: %w", err)
	}
	// Apply the configured bound before systemd-run gets a direct append fd. The
	// long-lived helper reopens the same path through NewLiveRotatingFile and
	// owns write-time rotation after startup.
	if writer, err := log.NewRotatingFile(logPath, 0o600); err != nil {
		return fmt.Errorf("open bounded tmux server log: %w", err)
	} else if err := writer.Close(); err != nil {
		return fmt.Errorf("prepare bounded tmux server log: %w", err)
	}
	launchLog, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open tmux server launch log: %w", err)
	}
	if err := launchLog.Chmod(0o600); err != nil {
		_ = launchLog.Close()
		return fmt.Errorf("secure tmux server launch log: %w", err)
	}
	// A fresh unit name per launch, never the deterministic one (#6180). A scope
	// stays loaded for as long as ANY process remains in its cgroup, so after the
	// server died a leftover process kept the old name taken and the next
	// systemd-run failed with "Unit … was already loaded or has a fragment file".
	// Stopping the old unit instead would kill those leftovers (measured: stop
	// kills the whole control group), which is not ours to do.
	unit := newDedicatedServerUnit()
	cmd := dedicatedServerScopeCommand(logPath, unit)
	cmd.Stdout = launchLog
	cmd.Stderr = launchLog
	if err := cmd.Start(); err != nil {
		_ = launchLog.Close()
		return fmt.Errorf("start dedicated tmux server scope: %w", err)
	}
	_ = launchLog.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	deadline := time.NewTimer(serverStartupTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	// wrapperExited disambiguates the readiness probe's success branch. A
	// dedicated-server wrapper's `tmux -D` is a FOREGROUND server, so while the
	// wrapper's own tmux serves the socket the wrapper STAYS ALIVE. The wrapper
	// exiting before the readiness probe succeeds therefore means it did NOT
	// start the answering server — a pre-existing server won the race (a
	// concurrent launcher that succeeded, or a transient probe miss at the
	// gate that left a healthy server running and made this wrapper a headless
	// attach against it that failed instantly). Recording that pre-existing
	// server as `dedicated` to the just-launched, already-exited unit's scope
	// is the harm: doctor then reports a hosting unit that only ever contained
	// a client (#5429/#6339/#6610). Treat an answered probe once the wrapper
	// has exited as an adopt, preserving any prior dedicated claim on this
	// same socket rather than stamping the freshly-launched dead unit as the
	// host.
	//
	// When the gate could not definitively prove no server was running
	// (gateProvedNoServer is false — a transient miss that survived the retry
	// budget), the answering server MIGHT be a pre-existing one the gate
	// missed, and the wrapper's `tmux -D` may still be on its way to
	// attach-failing against it. Wait briefly for the wrapper to exit before
	// stamping: a wrapper whose `tmux -D` is attach-failing exits within tens
	// of ms, while a wrapper that legitimately started the server stays alive.
	// The gate's definitive-no case needs no grace — a server that comes up
	// after a provably-empty socket is ours.
	wrapperExited := false
	var wrapperErr error
	for {
		if tmuxServerRunning() {
			// Drain a wrapper exit that may have arrived while the probe was in
			// flight. A nil channel never selects in a non-blocking receive,
			// so this is a no-op once done has been consumed. The drained
			// error is not needed here: this block returns once it decides
			// adopt-vs-dedicated, and the launch-failure error path below
			// reads wrapperErr only on the next iteration after the main
			// select's done-branch sets it.
			if !wrapperExited {
				select {
				case <-done:
					wrapperExited = true
					done = nil
				default:
				}
			}
			// When the gate was non-conclusive, give a brief grace for the
			// wrapper's attach-failure exit to arrive before deciding: the
			// wrapper's `tmux -D` attach-fails in tens of ms against a
			// pre-existing server, while a legitimate launch stays alive.
			if !wrapperExited && !gateProvedNoServer {
				select {
				case <-done:
					wrapperExited = true
					done = nil
				case <-time.After(attachFailureGrace):
					// Wrapper survived the grace — it is running our server.
				}
			}
			if wrapperExited {
				// The wrapper already exited, so it cannot have started the
				// answering server: a dedicated-server wrapper's `tmux -D`
				// stays alive WHILE it serves, so an answering probe WHEN the
				// wrapper is already dead is by a pre-existing server that won
				// the race. Preserve the prior dedicated claim (if any) rather
				// than stamp the now-dead unit as the host.
				recordAdoptedServer(home)
			} else {
				dedicatedServerReady(home, unit)
			}
			return nil
		}
		if wrapperExited && wrapperErr != nil {
			// The wrapper failed AND no server is answering — a genuine launch
			// failure on an empty socket. A concurrent launcher that succeeded
			// would answer the probe and exit via the adopt branch above; the
			// absence leaves the wrapper's failure as the only thing to report.
			return fmt.Errorf("dedicated tmux server scope exited before readiness: %w", wrapperErr)
		}
		select {
		case err := <-done:
			// The wrapper exited (cleanly — runDedicatedServer bailed on a
			// pre-existing server it detected, or with an error — an attach
			// failure or a genuine launch crash). Whatever server answers the
			// probe after this belongs to that race; re-probe at the top of the
			// loop and adopt it rather than attribute it to the just-exited
			// unit. With a non-nil error and no server answering, the
			// wrapperExited branch above returns the launch-failure error.
			wrapperExited = true
			wrapperErr = err
			done = nil
		case <-ticker.C:
		case <-deadline.C:
			timeoutErr := fmt.Errorf("dedicated tmux server did not become ready within %s", serverStartupTimeout)
			if done != nil {
				if stopErr := stopAndReapDedicatedScope(cmd, unit, done); stopErr != nil {
					return fmt.Errorf("%w (failed to stop its scope: %v)", timeoutErr, stopErr)
				}
			}
			return timeoutErr
		}
	}
}

func stopAndReapDedicatedScope(cmd *exec.Cmd, unit string, done <-chan error) error {
	// Killing only the systemd-run client can strand its child in the transient
	// scope. Target the exact deterministic unit so every process in the failed
	// bootstrap attempt is killed before the fail-open session launch proceeds.
	ctx, cancel := context.WithTimeout(context.Background(), serverScopeKillTimeout)
	defer cancel()
	unit += ".scope"
	out, scopeErr := exec.CommandContext(
		ctx, "systemctl", "--user", "kill", "--kill-who=all", "--signal=KILL", unit,
	).CombinedOutput()

	killErr := cmd.Process.Kill()
	// Stdout/stderr are direct files, not pipes, so SIGKILL makes Wait return
	// promptly. Always receive the result: that both reaps the process and lets
	// the goroutine terminate before fail-open returns to session creation.
	<-done
	if killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
		return fmt.Errorf("kill launcher: %w", killErr)
	}
	if scopeErr != nil {
		detail := strings.TrimSpace(string(out))
		if detail != "" {
			return fmt.Errorf("kill %s: %w: %s", unit, scopeErr, detail)
		}
		return fmt.Errorf("kill %s: %w", unit, scopeErr)
	}
	return nil
}

func secureExistingServerLog(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("refusing non-regular path with mode %s", info.Mode())
	}
	return os.Chmod(path, 0o600)
}

func tmuxServerRunning() bool {
	ctx, cancel := context.WithTimeout(context.Background(), serverProbeTimeout)
	defer cancel()
	return exec.CommandContext(ctx, "tmux", tmuxArgv("show-options", "-gv", "exit-empty")...).Run() == nil
}

// probeServerDefinitively retries the liveness probe until it reaches a
// definitive answer or exhausts a bounded retry budget — the same shape #3371
// gave the -V probe, applied to the liveness check that was the odd one out
// since #3314.
//
// A single-shot 250 ms tmuxServerRunning collapses a transient timeout against
// a live server into "no server", which licenses the dedicated-server
// bootstrap's relaunch on a guess — and relaunching a wrapper whose `tmux -D`
// becomes a headless attach against the still-healthy server is what corrupts
// the post-mortem evidence (#5429/#6339/#6610): the wrapper's exit stamps a
// false `tmux-server-death.json` and the readiness loop stamps the
// already-exited wrapper's scope as the hosting unit. This helper keeps the
// package's standing rule — "a failed read is not an empty result" (see
// NoServerRunning) — intact by distinguishing a definitive answer from a
// non-conclusive probe.
//
// Returns (running, definite):
//   - running == true only when the probe succeeded — a server is up.
//   - definite == true when the probe REPLIED at all: success, or tmux's
//     authoritative "no server running on <socket>" / socket-absent diagnostic
//     (the same definitive empty NoServerRunning already trusts elsewhere).
//   - definite == false when the probe could not tell — a tripped deadline,
//     an unknown diagnostic, an exec failure. A non-conclusive probe after the
//     retry budget returns definite == false; callers must NOT act on that as
//     "no server" — it is "I could not tell", never "it is gone".
func probeServerDefinitively() (running, definite bool) {
	for attempt := 0; attempt < serverProbeMaxAttempts; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), serverProbeTimeout)
		// Output (not Run) so the stderr tmux writes for a definitive
		// "no server running" diagnostic is captured into the ExitError and
		// NoServerRunning can classify it — Run discards stderr to /dev/null,
		// leaving the diagnostic empty and every exit-1 failure
		// unclassifiable.
		cmd := exec.CommandContext(ctx, "tmux", tmuxArgv("show-options", "-gv", "exit-empty")...)
		_, err := cmd.Output()
		ctxErr := ctx.Err()
		cancel()
		if err == nil {
			return true, true
		}
		if ctxErr == nil && NoServerRunning(err) {
			return false, true
		}
		// Non-conclusive (tripped deadline or unknown diagnostic): retry within
		// the budget before deciding. "I could not look" is neither "it is
		// gone" nor "it survived".
		if attempt+1 < serverProbeMaxAttempts {
			time.Sleep(serverProbeRetryInterval)
		}
	}
	return false, false
}

func tmuxSupportsNoStart() (bool, error) {
	noStartProbeMu.Lock()
	defer noStartProbeMu.Unlock()
	if noStartProbeCached {
		return noStartProbeSupports, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), tmuxVersionProbeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, "tmux", tmuxArgv("-V")...).Output()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, fmt.Errorf("tmux -V timed out after %s: %w", tmuxVersionProbeTimeout, ctxErr)
		}
		return false, fmt.Errorf("tmux -V: %w", err)
	}

	noStartProbeSupports = tmuxVersionAtLeast(strings.TrimSpace(string(out)), 3, 3)
	noStartProbeCached = true
	return noStartProbeSupports, nil
}

func dedicatedServerScopeCommand(logPath, unit string) *exec.Cmd {
	// /proc/<pid>/exe avoids systemd-run's ${NAME} argv expansion without
	// requiring a new-systemd-only flag. The daemon remains alive throughout
	// this readiness barrier, so the proc link is stable until the helper execs.
	executable := fmt.Sprintf("/proc/%d/exe", os.Getpid())
	args := []string{
		"--user", "--scope", "--quiet", "--collect",
		"--unit=" + unit,
		"--property=KillMode=control-group",
		"--", executable, dedicatedServerExecMarker,
	}
	cmd := exec.Command("systemd-run", args...)
	// Match the old first-session launch's credential boundary: a new tmux
	// server must not snapshot the daemon's ambient secrets into its global
	// environment. Later session clients import only their approved names.
	serverEnv := sessionenv.Filter(os.Environ(), "", nil)
	cmd.Env = replaceEnvironment(replaceEnvironment(serverEnv, dedicatedServerLogEnv, logPath), dedicatedServerUnitEnv, unit)
	return cmd
}

// dedicatedServerScopeName is the stable per-socket prefix of every dedicated
// server unit; newDedicatedServerUnit appends a per-launch suffix.
func dedicatedServerScopeName() string {
	sum := sha256.Sum256([]byte(DefaultSocketPath()))
	return fmt.Sprintf("agent-factory-tmux-server-%x", sum[:6])
}

// newDedicatedServerUnit names one launch's scope. Unique per launch so a unit a
// previous server left loaded can never block this one (#6180).
func newDedicatedServerUnit() string {
	return dedicatedServerScopeName() + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// dedicatedServerReady records that sessions are hosted by the dedicated server.
func dedicatedServerReady(home, unit string) {
	socket := DefaultSocketPath()
	log.InfoLog.Printf("dedicated tmux server is running in systemd unit %s.scope on socket %s", unit, socket)
	recordServerState(home, ServerState{Mode: ServerModeDedicated, Socket: socket, Unit: unit, At: time.Now()})
}

// recordAdoptedServer rewrites the server-state record when the daemon adopts
// an already-running server instead of launching its own (#6339). Without this
// write a restart that adopted a server returned from ensureDedicatedServer
// leaving the PREVIOUS generation's record in place — stamped with a dead
// pid's authorship and possibly naming a socket this daemon no longer uses,
// which doctor then trusted as current when it armed process kills. The
// record's socket always reflects the socket this daemon generation resolved;
// when the adopted socket is still the one a previous generation dedicated,
// the dedicated claim is preserved rather than rewritten as adopted, since
// the running server is almost certainly that dedicated server — and the same
// holds for a same-socket fallback decision, whose reason a rewrite to
// "adopted" would hide from doctor while its bootstrap failure is still
// unresolved.
func recordAdoptedServer(home string) {
	socket := DefaultSocketPath()
	st := ServerState{Mode: ServerModeAdopted, Socket: socket, At: time.Now()}
	if prev, ok, err := ReadServerState(home); err == nil && ok && prev.Socket == socket {
		st.Mode, st.Unit, st.Reason = prev.Mode, prev.Unit, prev.Reason
	}
	recordServerState(home, st)
}

func replaceEnvironment(environ []string, name, value string) []string {
	prefix := name + "="
	out := make([]string, 0, len(environ)+1)
	for _, entry := range environ {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return append(out, prefix+value)
}

func withoutEnvironment(environ []string, name string) []string {
	prefix := name + "="
	out := make([]string, 0, len(environ))
	for _, entry := range environ {
		if !strings.HasPrefix(entry, prefix) {
			out = append(out, entry)
		}
	}
	return out
}

// HandleDedicatedServerExec consumes the private wrapper invocation before
// Cobra. It stays alive beside foreground tmux, feeding stdout/stderr into the
// same bounded rotation policy as Agent Factory's other long-lived logs.
func HandleDedicatedServerExec() {
	if len(os.Args) != 2 || os.Args[1] != dedicatedServerExecMarker {
		return
	}
	if err := runDedicatedServer(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "af: dedicated tmux server exited: %v\n", err)
		os.Exit(1)
	}
	os.Exit(0)
}

func runDedicatedServer() error {
	logPath := os.Getenv(dedicatedServerLogEnv)
	if logPath == "" {
		return fmt.Errorf("tmux server log path is missing")
	}
	if err := secureExistingServerLog(logPath); err != nil {
		return fmt.Errorf("secure existing tmux server log: %w", err)
	}
	writer, err := log.NewLiveRotatingFile(logPath, 0o600)
	if err != nil {
		return fmt.Errorf("open bounded tmux server log: %w", err)
	}
	defer writer.Close()
	_, _ = fmt.Fprintf(writer, "%s af tmux server starting in foreground\n", time.Now().Format(time.RFC3339))

	unit := os.Getenv(dedicatedServerUnitEnv)
	cmd := exec.Command("tmux", tmuxArgv("-D")...)
	cmd.Env = withoutEnvironment(withoutEnvironment(os.Environ(), dedicatedServerLogEnv), dedicatedServerUnitEnv)
	cmd.Stdout = writer
	cmd.Stderr = writer
	started := time.Now()
	err = cmd.Run()
	if err != nil {
		// tmux -D failed. Before stamping a server-death marker, re-probe the
		// socket: if a tmux server is still answering there, this was a headless
		// attach against a pre-existing server — the gate's probe missed it —
		// not a server death. Stamping a death marker for an attach failure
		// corrupts the post-mortem evidence (#5429/#6339/#6610): the marker
		// overwrites any genuine prior death record, names a unit that only
		// ever contained a client, and is what doctor then reports as "the
		// dedicated tmux server ended … with exit code 1 after 0s". The wrapper
		// owns nothing that died, so it records nothing and exits cleanly; the
		// readiness loop observes the clean exit and records the pre-existing
		// server as adopted rather than stamping this unit as its host.
		//
		// A definitive "no server running" answer is the only shape that
		// licenses the marker — the same invariant NoServerRunning enforces
		// elsewhere in this package. A non-conclusive probe (a sustained stall)
		// falls through to recordServerDeath, matching the historic behavior
		// for a genuine launch crash this probe cannot disprove.
		if running, _ := probeServerDefinitively(); running {
			_, _ = fmt.Fprintf(writer, "%s af tmux server did not start: %v; a server is already running on this socket, so this was a headless attach failure, not a server death — no death marker recorded\n",
				time.Now().Format(time.RFC3339), err)
			return nil
		}
		recordServerDeath(filepath.Dir(logPath), unit, started, err)
		_, _ = fmt.Fprintf(writer, "%s af tmux server exited: %v\n", time.Now().Format(time.RFC3339), err)
		return err
	}
	recordServerDeath(filepath.Dir(logPath), unit, started, err)
	_, _ = fmt.Fprintf(writer, "%s af tmux server exited cleanly\n", time.Now().Format(time.RFC3339))
	return nil
}

// recordServerDeath leaves the marker #5429 asked for: what this wrapper saw when
// tmux ended. The cause of a signal's sender is not knowable here; the exit code
// or signal, the uptime and the unit narrow it, and the server log carries the
// rest.
func recordServerDeath(home, unit string, started time.Time, runErr error) {
	death := ServerDeath{
		At:            time.Now(),
		Unit:          unit,
		Socket:        DefaultSocketPath(),
		UptimeSeconds: time.Since(started).Seconds(),
	}
	if runErr != nil {
		death.Error = runErr.Error()
		death.ExitCode = -1
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			death.ExitCode = exitErr.ExitCode()
			if status, ok := exitErr.Sys().(syscall.WaitStatus); ok && status.Signaled() {
				death.Signal = status.Signal().String()
			}
		}
	}
	_ = writeJSONFile(filepath.Join(home, serverDeathFileName), death)
}

func newTmuxServerCommandAfterEnsure(serverErr error, args ...string) (*exec.Cmd, bool) {
	if !systemdunit.RunningDaemonProcess() {
		return exec.Command("tmux", tmuxArgv(args...)...), false
	}
	if home, configured := configuredDaemonServerHome(); configured {
		serverReady := serverErr == nil || tmuxServerRunning()
		if serverReady {
			supportsNoStart, probeErr := tmuxSupportsNoStart()
			if probeErr != nil {
				log.WarningLog.Printf("tmux -V probe failed: %v; using -N no-autostart client", probeErr)
				return exec.Command("tmux", tmuxArgv(append([]string{"-N"}, args...)...)...), false
			}
			if supportsNoStart {
				return exec.Command("tmux", tmuxArgv(append([]string{"-N"}, args...)...)...), false
			}
			noStartWarningOnce.Do(func() {
				log.WarningLog.Printf("tmux client lacks a verified -N no-autostart flag; using a per-session systemd scope compatibility fallback")
			})
		} else {
			socket := DefaultSocketPath()
			log.WarningLog.Printf("dedicated tmux server launch failed; falling back to a per-session systemd scope on socket %s: %v", socket, serverErr)
			recordServerState(home, ServerState{Mode: ServerModeFallback, Socket: socket, Reason: serverErr.Error(), At: time.Now()})
		}
	}

	scopeArgs := []string{"--user", "--scope", "--quiet", "--collect", "--", "tmux"}
	scopeArgs = append(scopeArgs, args...)
	return exec.Command("systemd-run", scopeArgs...), true
}
