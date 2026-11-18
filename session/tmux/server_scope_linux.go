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
	"github.com/sachiniyer/agent-factory/internal/execgroup"
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

	switch probeDedicatedServer() {
	case dedicatedServerPresent:
		// Do not mutate a foreign or legacy server. Its ordinary exit-empty
		// policy lets it retire after its last session, at which point a later
		// spawn replaces it with the dedicated server below. The record must
		// still be rewritten — this early return is how a restart that adopts
		// an existing server used to leave the previous generation's record in
		// place, letting doctor pin its reads to a socket the daemon no longer
		// uses (#6339).
		recordAdoptedServer(home)
		return nil
	case dedicatedServerUnknown:
		// A read that did not answer is not a licence to launch a second
		// server on the socket. The old Run()==nil probe collapsed this case
		// into absence, so a one-off stall relaunched a wrapper over a healthy
		// server: tmux -D only attach-failed against it, and the wrapper's
		// unconditional recordServerDeath overwrote the real marker with a
		// false one while recordServerState credited a dead scope (#7557).
		// Refusing here leaves the spawn to its loud fallback instead.
		return fmt.Errorf("tmux server presence could not be determined; refusing to launch a second server")
	}
	// dedicatedServerAbsent — tmux's own diagnostic said definitively nobody
	// is on the socket, so the wrapper below binds a fresh server.
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
	for {
		if probeDedicatedServer() == dedicatedServerPresent {
			dedicatedServerReady(home, unit)
			return nil
		}
		select {
		case launchErr := <-done:
			done = nil
			if probeDedicatedServer() == dedicatedServerPresent {
				// A concurrent AF home may have won the same socket after this
				// launcher failed — and once `done` delivers, the answering
				// server is provably not the tmux -D this scope ran. Prefer the
				// observed healthy server to a false startup refusal, but record
				// it as adopted: crediting this scope's already-exited unit is
				// how a false "dedicated" claim reached doctor (#7557).
				recordAdoptedServer(home)
				return nil
			}
			if launchErr != nil {
				return fmt.Errorf("dedicated tmux server scope exited before readiness: %w", launchErr)
			}
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

// dedicatedServerVerdict is the bootstrap's read of the dedicated server's
// socket. The verdict is PASSIVE — read from tmux's own diagnostic alone, so
// it sends no signal, reads no process table, and does not depend on what
// other servers may be running for this uid.
type dedicatedServerVerdict int

const (
	// dedicatedServerPresent: a server answered the probe.
	dedicatedServerPresent dedicatedServerVerdict = iota
	// dedicatedServerAbsent: tmux's diagnostic said definitively there is no
	// server — ECONNREFUSED (a socket is there, nobody listens) or ENOENT
	// (no socket at all). ENOENT counts as definitive here even though
	// NoServerRunning treats it as ambiguous, because the question differs:
	// this verdict gates a LAUNCH, and `tmux -D` on an absent path binds a
	// fresh socket rather than attaching to a server orphaned from an
	// unlinked one. The process-table scan and the SIGUSR1 reclaim probe
	// that make NoServerRunning's ENOENT verdict safe have nothing to buy
	// here, so this path never runs them.
	dedicatedServerAbsent
	// dedicatedServerUnknown: the read did not answer — a tripped deadline,
	// a killed probe, or a diagnostic tmux gives no definitive meaning to.
	// A failed read is not evidence of absence.
	dedicatedServerUnknown
)

// probeDedicatedServer asks the dedicated server's socket, ONCE and inside
// the probe bound, whether a server answers. Every tmux read on this path
// carries the package's full bound — boundedTmuxCommand puts the client in
// its own process group with WaitDelay, so a wedged answer is killed rather
// than parked on. A live server answers in this one attempt; a cold start
// pays no retry and no grace.
//
// The deadline is checked BEFORE the error is classified: a probe killed on
// the bound is unknown even when its error string happens to parse, and a
// WaitDelay-normalized nil is never mistaken for an answer the deadline cut
// off. Anything else that is not an exit-1 diagnostic is likewise unknown —
// never "no server" — which is what keeps a one-off stall from relaunching a
// wrapper over a healthy server and stamping a false death marker (#7557).
func probeDedicatedServer() dedicatedServerVerdict {
	ctx, cancel := context.WithTimeout(context.Background(), serverProbeTimeout)
	c := boundedTmuxCommand(ctx, "show-options", "-gv", "exit-empty")
	_, err := c.Output()
	execgroup.Reap(c)
	err = normalizeWaitDelay(err)
	timedOut := ctx.Err() != nil
	cancel()
	switch {
	case timedOut:
		return dedicatedServerUnknown
	case err == nil:
		return dedicatedServerPresent
	}
	diagnostic, exitOne := tmuxExitOneDiagnostic(err)
	if !exitOne {
		return dedicatedServerUnknown
	}
	switch classifyNoServerDiagnostic(diagnostic) {
	case serverRefusedConnection, socketAbsent:
		return dedicatedServerAbsent
	}
	return dedicatedServerUnknown
}

func tmuxServerRunning() bool {
	return probeDedicatedServer() == dedicatedServerPresent
}

func tmuxSupportsNoStart() (bool, error) {
	noStartProbeMu.Lock()
	defer noStartProbeMu.Unlock()
	if noStartProbeCached {
		return noStartProbeSupports, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), tmuxVersionProbeTimeout)
	defer cancel()
	c := boundedTmuxCommand(ctx, "-V")
	out, err := c.Output()
	execgroup.Reap(c)
	err = normalizeWaitDelay(err)
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
	// Recheck the socket before binding it: the launcher committed on a
	// definitive absent verdict, but a concurrent AF home may have won the
	// socket in between, and `tmux -D` against a server that already answers
	// is a failed attach — exiting after one is not a server death (#7557).
	if probeDedicatedServer() == dedicatedServerPresent {
		_, _ = fmt.Fprintf(writer, "%s af tmux server not launched: the socket already has a server\n", time.Now().Format(time.RFC3339))
		return nil
	}
	cmd := exec.Command("tmux", tmuxArgv("-D")...)
	cmd.Env = withoutEnvironment(withoutEnvironment(os.Environ(), dedicatedServerLogEnv), dedicatedServerUnitEnv)
	cmd.Stdout = writer
	cmd.Stderr = writer
	started := time.Now()
	err = cmd.Run()
	// The death marker is evidence for doctor's post-mortem, so it is written
	// only on a definitive no-server read: a socket that still answers means
	// this tmux -D attached to somebody else's server and nothing owned by
	// this wrapper died, and a read that did not answer proves nothing either
	// way — a non-definitive exit never writes the marker (#7557).
	if probeDedicatedServer() == dedicatedServerAbsent {
		recordServerDeath(filepath.Dir(logPath), unit, started, err)
	}
	if err != nil {
		_, _ = fmt.Fprintf(writer, "%s af tmux server exited: %v\n", time.Now().Format(time.RFC3339), err)
		return err
	}
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
		serverReady := serverErr == nil || probeDedicatedServer() == dedicatedServerPresent
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
