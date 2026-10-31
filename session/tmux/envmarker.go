package tmux

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/log"
)

// Ancestry env markers (#1104). Every process spawned inside an af tmux pane
// inherits these variables, and /proc/<pid>/environ preserves a process's
// *initial* environment — so even after a leaked child is orphaned and
// reparented to init, `af doctor` can still prove which session (and which
// agent-factory home) spawned it. Without the marker that ancestry is
// unrecoverable, which is exactly what made the 2026-07-03 `yes` orphans
// ambiguous.
const (
	// EnvMarkerSession holds the sanitized tmux session name (af_...).
	EnvMarkerSession = "AF_SESSION"
	// EnvMarkerHome holds the agent-factory config dir that owns the
	// session, so doctor never confuses sessions from another install or a
	// test's temp home with its own.
	EnvMarkerHome = "AF_HOME"
	// EnvMarkerGeneration distinguishes successive tmux sessions that reuse
	// the same sanitized name and Agent Factory home. AF_SESSION proves the
	// reusable name, not which process generation carried it (#3309).
	EnvMarkerGeneration = "AF_SESSION_GEN"
	// EnvMarkerInstance holds the stable instance id of the create attempt whose
	// launch options started this session (InstanceOptions.ID). The id is minted
	// and, for a cron fire, recorded durably before the spawn, so a match proves
	// THIS attempt's spawn created the session — which no name, home or clock can
	// (#6258). Only the agent session carries it.
	EnvMarkerInstance = "AF_INSTANCE_ID"
	// envMarkerTestguardRun carries a test process's stable ownership identity
	// across per-test AGENT_FACTORY_HOME overrides. It is deliberately private:
	// production consumers authorize ownership with AF_HOME, while testguard uses
	// the same string literal to diagnose test leaks without importing this package.
	envMarkerTestguardRun = "AF_TESTGUARD_RUN"
)

// PanePidMarker is the pane-scoped user option that records a pane's identity
// bundle — "<pane_pid>:<deaths>:<pane_tty>" — while it is alive. tmux ≥3.8
// stops reporting pane_pid once a pane's root exits (#5319), but user options
// live on the pane struct, which remain-on-exit preserves — so teardown can
// keep naming the dead root by the pid it had while alive. The launch shim
// stamps the bundle itself (sessionenv stamps — the shim IS the pane root),
// which keeps the record current even across a user's commandless
// respawn-pane, and ApplyRemainOnExit stamps still-alive pre-stamp panes as a
// heal. The deaths, tty-name and @af_pane_cmd proofs are what let the reader
// distrust a stamp a respawn carried into a new incarnation; what keeps a
// respawn from running under the stale record at all is the shim's own
// restamp-or-die (sessionenv).
// Measured on tmux 3.8: a stamped pane answers `#{@af_pane_pid}` after
// death while `#{pane_pid}` comes back empty. On tmux older than 3.8
// pane_pid itself survives death, so a missing stamp loses nothing.
const PanePidMarker = sessionenv.PanePidUserOption

// paneDeathsMarker is the pane-died counter the freshness check compares the
// stamp's deaths field against; paneCommandMarker pins the stamped
// incarnation's pane_start_command for the byte-equality check. Both are
// defined and documented in sessionenv beside the shim that writes them.
const (
	paneDeathsMarker  = sessionenv.PaneDeathsUserOption
	paneCommandMarker = sessionenv.PaneCommandUserOption
)

// panePidStampArgs heals the pane identity record on an already-existing
// pane: `-o` never overwrites a stamp that is already there (a pane that died
// before the heal would re-expand #{pane_pid} to EMPTY on tmux ≥3.8 and
// clobber its own good record), and `-F` expands the live pane_pid for a
// still-running, pre-stamp pane. The pane-died hook is installed too — a
// healed pane otherwise has no deaths counter, and its eventual death would
// leave the stamp's freshness unprovable. The hook installs under a
// dedicated array key (sessionenv.PaneDeathsHookName) so it neither clobbers
// nor is clobbered by an operator's own pane-died hook.
//
// paneTarget is any pane target: `=name:` resolves to the session's active
// pane, and a `%N` id targets a specific pane exactly.
func panePidStampArgs(paneTarget string) []string {
	return []string{
		"set-option", "-o", "-F", "-p", "-t", paneTarget, PanePidMarker, sessionenv.PanePidStampFormat, ";",
		"set-option", "-o", "-F", "-p", "-t", paneTarget, paneCommandMarker, "#{pane_start_command}", ";",
		"set-hook", "-p", "-t", paneTarget, sessionenv.PaneDeathsHookName, sessionenv.PaneDeathsHookCommand,
	}
}

// newSessionGeneration mints the process-generation identity stamped into one
// tmux launch. It is a package var so tests can make exact argv assertions. The
// timestamp fallback keeps session creation available if the entropy read fails;
// uniqueness, not secrecy, is the requirement.
var newSessionGeneration = func() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return fmt.Sprintf("ts-%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(value[:])
}

// newSessionEnvSupportedOverride forces the `-e` support probe's answer in
// tests (mock executors can't answer a real `tmux -V`); nil probes the real
// tmux binary.
var newSessionEnvSupportedOverride *bool

// SetNewSessionEnvSupportForTest pins the tmux capability probe for tests whose
// mock executor cannot answer the otherwise host-dependent `tmux -V` check.
// The returned restore function must be deferred or registered with Cleanup.
func SetNewSessionEnvSupportForTest(supported bool) func() {
	previous := newSessionEnvSupportedOverride
	newSessionEnvSupportedOverride = &supported
	return func() { newSessionEnvSupportedOverride = previous }
}

// sessionEnvFlags returns the `-e VAR=value` arguments for `tmux
// new-session`, or nil when the running tmux predates `-e` (added in 3.2) —
// passing it there would fail session creation outright, which is far worse
// than a missing diagnostic marker.
func sessionEnvFlags(sanitizedName, generation, instanceID string) []string {
	if !SessionEnvSupported() {
		return nil
	}
	flags := []string{
		"-e", EnvMarkerSession + "=" + sanitizedName,
		"-e", EnvMarkerGeneration + "=" + generation,
	}
	if home, err := afHomeDir(); err == nil {
		flags = append(flags, "-e", EnvMarkerHome+"="+home)
	}
	if instanceID != "" {
		flags = append(flags, "-e", EnvMarkerInstance+"="+instanceID)
	}
	if testRun := os.Getenv(envMarkerTestguardRun); testRun != "" {
		flags = append(flags, "-e", envMarkerTestguardRun+"="+testRun)
	}
	return flags
}

// SessionHomeMarker reads a tmux session's AF_HOME ownership marker with the
// three-valued contract the answer actually has: (home, true, nil) when tmux
// answered and the session carries one, ("", false, nil) when tmux answered and
// it carries NONE, and a non-nil error when tmux did not answer at all — which
// leaves ownership UNKNOWN and must never be read as "belongs to nobody".
//
// Exported for `af doctor` (#2874), which had grown its own one-line version
// that collapsed the last two cases into "no marker". That is the difference
// between "this session claims no home" and "I could not ask", and doctor spends
// the answer on whether a temp home may be rm -rf'd. Two implementations of a
// distinction this sharp is how the first one drifts; there is now one.
func SessionHomeMarker(cmdExec cmd.Executor, sanitizedName string) (home string, present bool, err error) {
	return sessionHomeMarker(cmdExec, sanitizedName)
}

// SessionGenerationMarker reads a tmux session's AF_SESSION_GEN generation
// marker with the same three-valued contract as SessionHomeMarker: (gen, true,
// nil) when tmux answered and the session carries one, ("", false, nil) when
// tmux answered and it carries none (a pre-generation pane or tmux < 3.2), and a
// non-nil error when tmux did not answer — ownership remains UNKNOWN.
//
// Used by adopt to bind the verified generation identity through the
// verification-to-track window: if the session is replaced between the marker
// read and the track call, the generation will differ and adoption fails closed.
func SessionGenerationMarker(cmdExec cmd.Executor, sanitizedName string) (generation string, present bool, err error) {
	return sessionEnvMarker(cmdExec, sanitizedName, EnvMarkerGeneration)
}

// SessionEnvVar reads a named variable from a tmux session's environment with
// the same three-valued contract as SessionHomeMarker: (value, true, nil) when
// tmux answered and the session carries the variable, ("", false, nil) when tmux
// answered and it carries no such variable, and a non-nil error when tmux did
// not answer — so the caller cannot mistake "absent" for "unknown".
//
// Used by the legacy-pane migration check to read the agent's credential-root
// variable (e.g. CODEX_HOME) and verify account identity before refusing.
func SessionEnvVar(cmdExec cmd.Executor, sanitizedName, varName string) (value string, present bool, err error) {
	return sessionEnvMarker(cmdExec, sanitizedName, varName)
}

// sessionHomeMarker reads the AF_HOME ancestry marker from a tmux session's
// environment (stamped via `new-session -e` at creation). A false present value
// means tmux answered and the session carries no marker — created by a pre-marker
// build or by a tmux older than 3.2, which cannot set session environment at
// creation. A non-nil error means tmux did not answer authoritatively, so the
// caller must keep ownership UNKNOWN rather than treating the marker as absent.
//
// Bounded by tmuxCommandTimeout (#2099): CleanupSessions calls this once per
// discovered session, so an unbounded stall on any one of them wedges the whole
// sweep — and `af reset` runs that sweep synchronously in a short-lived CLI
// process, where the only way out is ^C. The targeted query is the only output
// trusted as the marker: unfiltered environment output can render a newline in
// an unrelated value as a forged-looking AF_HOME line. If the targeted query
// fails, an unfiltered query is used only to prove that the session answered and
// the named variable was absent. If neither query answers, ownership remains
// unknown and cleanup decides whether the exact session has since vanished.
func sessionHomeMarker(cmdExec cmd.Executor, sanitizedName string) (home string, present bool, err error) {
	return sessionEnvMarker(cmdExec, sanitizedName, EnvMarkerHome)
}

// sessionEnvMarker binds the query, response parser, and absence classifier to
// one marker name so readers cannot accidentally classify a different variable.
func sessionEnvMarker(cmdExec cmd.Executor, sanitizedName, marker string) (string, bool, error) {
	return sessionEnvMarkerAt(cmdExec, exactTarget(sanitizedName), sanitizedName, marker)
}

// sessionEnvMarkerAt is sessionEnvMarker against an explicit tmux target: the
// exact-name form for a name lookup, or a "$N" session id to read the
// environment of one probed incarnation rather than whatever the name resolves
// to by the time the command runs (#6258). sanitizedName only labels errors.
func sessionEnvMarkerAt(cmdExec cmd.Executor, target, sanitizedName, marker string) (string, bool, error) {
	ctx, cancel := tmuxTimeoutContext()
	out, markerErr := outputTmuxBoundedWith(ctx, cmdExec, "show-environment", "-t", target, marker)
	markerTimedOut := ctx.Err() != nil
	cancel()
	if markerErr == nil {
		line := strings.TrimSuffix(strings.TrimSuffix(string(out), "\n"), "\r")
		if line == "-"+marker {
			return "", false, nil
		}
		if strings.ContainsAny(line, "\r\n") {
			return "", false, fmt.Errorf("read %s marker for tmux session %s: malformed multiline response", marker, sanitizedName)
		}
		value, ok := strings.CutPrefix(line, marker+"=")
		if !ok {
			return "", false, fmt.Errorf("read %s marker for tmux session %s: malformed response %q", marker, sanitizedName, line)
		}
		return value, true, nil
	}
	if markerTimedOut {
		return "", false, fmt.Errorf("%w: show-environment %s after %s", ErrTmuxTimeout, sanitizedName, tmuxCommandTimeout)
	}
	if !missingSessionEnvMarker(markerErr, marker) {
		return "", false, fmt.Errorf("read %s marker for tmux session %s: %w", marker, sanitizedName, markerErr)
	}

	// The targeted result itself identified an absent variable. Ask for the
	// environment without consuming its ambiguous contents: success confirms the
	// session answered and the marker alone was absent. A generic targeted error
	// never reaches this fallback, even if this second command would succeed.
	allCtx, allCancel := tmuxTimeoutContext()
	_, allErr := outputTmuxBoundedWith(allCtx, cmdExec, "show-environment", "-t", target)
	allTimedOut := allCtx.Err() != nil
	allCancel()
	if allErr == nil {
		return "", false, nil
	}
	if allTimedOut {
		return "", false, fmt.Errorf("%w: show-environment %s after %s", ErrTmuxTimeout, sanitizedName, tmuxCommandTimeout)
	}

	return "", false, fmt.Errorf("read %s marker for tmux session %s: targeted query: %v; environment query: %w",
		marker, sanitizedName, markerErr, allErr)
}

// missingSessionEnvMarker recognizes tmux's explicit absent-variable answer
// from the TARGETED query. Exit status alone is insufficient: a transient
// wrapper/server failure may also be nonzero, and a later command succeeding
// cannot retroactively determine why the first failed.
func missingSessionEnvMarker(err error, marker string) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) &&
		strings.TrimSpace(string(exitErr.Stderr)) == "unknown variable: "+marker
}

var (
	newSessionEnvMu        sync.Mutex
	newSessionEnvCached    bool
	newSessionEnvSupported bool
)

// resetNewSessionEnvProbeCache clears the cached `-e` support probe so the next
// call re-probes `tmux -V`. Tests that install a tmux shim on PATH use this to
// start from an unprobed state; production never calls it (the cache is valid
// for the process lifetime once a probe succeeds).
func resetNewSessionEnvProbeCache() {
	newSessionEnvMu.Lock()
	newSessionEnvCached = false
	newSessionEnvSupported = false
	newSessionEnvMu.Unlock()
}

// tmuxSupportsNewSessionEnv probes `tmux -V` and caches the answer for the
// process lifetime — but only on SUCCESS. A transient probe failure (a one-shot
// fork/exec error, a flaky wrapper script exiting nonzero on a single
// invocation) is NOT cached: the next call re-probes, so a recovered tmux is
// picked up instead of permanently downgrading the daemon to "tmux < 3.2".
// Unparseable versions ("openbsd-7.4", exotic builds) are a successful probe
// that conservatively reports false, and ARE cached — the version is
// deterministic, not transient.
//
// This mirrors the sibling tmuxSupportsNoStart probe in server_scope_linux.go,
// which deliberately avoids caching on error for the same reason
// (TestNoStartProbeFailureDoesNotDowngradeAndIsRetried pins that intent there).
// The earlier sync.Once here latched false on any error, permanently blinding
// cleanup, adoption, orphan reaping, and doctor to every session created after
// a single hiccup.
//
// Bounded like every other tmux command in this package (#2099), though the
// hazard here is narrower and worth naming honestly: `tmux -V` is answered by the
// CLIENT and never contacts the server, so a wedged SERVER — the failure mode
// #2099/#2105 are about — cannot stall it. What can is a hanging tmux BINARY (a
// wrapper script, a stalled network filesystem holding the executable). That is
// defensive rather than reproduced, but the blast radius justifies the bound:
// without it a single hang would wedge session creation for the entire process
// lifetime and every later caller would block on the mutex rather than retry.
func tmuxSupportsNewSessionEnv() bool {
	newSessionEnvMu.Lock()
	defer newSessionEnvMu.Unlock()
	if newSessionEnvCached {
		return newSessionEnvSupported
	}
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	out, err := outputTmuxBoundedWith(ctx, cmd.MakeExecutor(), "-V")
	if err != nil {
		log.WarningLog.Printf("tmux -V probe failed; ancestry env markers disabled until a successful probe: %v", err)
		return false
	}
	newSessionEnvSupported = versionSupportsNewSessionEnv(strings.TrimSpace(string(out)))
	newSessionEnvCached = true
	return newSessionEnvSupported
}

func versionSupportsNewSessionEnv(version string) bool {
	return tmuxVersionAtLeast(version, 3, 2)
}

// tmuxVersionAtLeast parses a `tmux -V` string ("tmux 3.4", "tmux 3.3a",
// "tmux next-3.6", "tmux master") for feature gates tied to a release.
func tmuxVersionAtLeast(version string, wantMajor, wantMinor int) bool {
	v := strings.TrimPrefix(version, "tmux ")
	v = strings.TrimPrefix(v, "next-")
	if v == "master" {
		// Development builds trail no released feature this old.
		return true
	}
	parts := strings.SplitN(v, ".", 2)
	if len(parts) != 2 {
		return false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	// Strip any suffix after the minor digits ("3a" -> "3", "5-rc" -> "5").
	minorStr := parts[1]
	end := 0
	for end < len(minorStr) && minorStr[end] >= '0' && minorStr[end] <= '9' {
		end++
	}
	if end == 0 {
		return false
	}
	minor, err := strconv.Atoi(minorStr[:end])
	if err != nil {
		return false
	}
	return major > wantMajor || (major == wantMajor && minor >= wantMinor)
}

// afHomeDir mirrors config.GetConfigDir — $AGENT_FACTORY_HOME (tilde-expanded)
// or ~/.agent-factory. Duplicated because config imports session/tmux, so
// this package cannot import config without a cycle (same trade-off as
// internal/testguard, which documents the same mirroring).
func afHomeDir() (string, error) {
	if envDir := os.Getenv("AGENT_FACTORY_HOME"); envDir != "" {
		if envDir == "~" || strings.HasPrefix(envDir, "~/") {
			home, err := os.UserHomeDir()
			if err != nil {
				return "", err
			}
			return filepath.Join(home, strings.TrimPrefix(envDir[1:], "/")), nil
		}
		return envDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".agent-factory"), nil
}

// SessionEnvSupported reports whether af can stamp session identity through
// new-session -e. Identity consumers must not trust inherited markers when the
// launch path cannot replace them. Shares the cached probe and test override.
func SessionEnvSupported() bool {
	if newSessionEnvSupportedOverride != nil {
		return *newSessionEnvSupportedOverride
	}
	return tmuxSupportsNewSessionEnv()
}
