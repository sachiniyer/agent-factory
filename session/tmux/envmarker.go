package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/cmd"
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
)

// newSessionEnvSupportedOverride forces the `-e` support probe's answer in
// tests (mock executors can't answer a real `tmux -V`); nil probes the real
// tmux binary.
var newSessionEnvSupportedOverride *bool

// sessionEnvFlags returns the `-e VAR=value` arguments for `tmux
// new-session`, or nil when the running tmux predates `-e` (added in 3.2) —
// passing it there would fail session creation outright, which is far worse
// than a missing diagnostic marker.
func sessionEnvFlags(sanitizedName string) []string {
	supported := tmuxSupportsNewSessionEnv
	if newSessionEnvSupportedOverride != nil {
		supported = func() bool { return *newSessionEnvSupportedOverride }
	}
	if !supported() {
		return nil
	}
	flags := []string{"-e", EnvMarkerSession + "=" + sanitizedName}
	if home, err := afHomeDir(); err == nil {
		flags = append(flags, "-e", EnvMarkerHome+"="+home)
	}
	return flags
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
// unknown and cleanup stops before destructive reset work.
func sessionHomeMarker(cmdExec cmd.Executor, sanitizedName string) (home string, present bool, err error) {
	ctx, cancel := tmuxTimeoutContext()
	out, markerErr := outputTmuxBoundedWith(ctx, cmdExec, "show-environment", "-t", exactTarget(sanitizedName), EnvMarkerHome)
	markerTimedOut := ctx.Err() != nil
	cancel()
	if markerErr == nil {
		line := strings.TrimSuffix(strings.TrimSuffix(string(out), "\n"), "\r")
		if line == "-"+EnvMarkerHome {
			return "", false, nil
		}
		if strings.ContainsAny(line, "\r\n") {
			return "", false, fmt.Errorf("read %s ownership marker for tmux session %s: malformed multiline response", EnvMarkerHome, sanitizedName)
		}
		home, ok := strings.CutPrefix(line, EnvMarkerHome+"=")
		if !ok {
			return "", false, fmt.Errorf("read %s ownership marker for tmux session %s: malformed response %q", EnvMarkerHome, sanitizedName, line)
		}
		return home, true, nil
	}
	if markerTimedOut {
		return "", false, fmt.Errorf("%w: show-environment %s after %s", ErrTmuxTimeout, sanitizedName, tmuxCommandTimeout)
	}
	if !missingSessionEnvMarker(markerErr) {
		return "", false, fmt.Errorf("read %s ownership marker for tmux session %s: %w", EnvMarkerHome, sanitizedName, markerErr)
	}

	// The targeted result itself identified an absent variable. Ask for the
	// environment without consuming its ambiguous contents: success confirms the
	// session answered and the marker alone was absent. A generic targeted error
	// never reaches this fallback, even if this second command would succeed.
	allCtx, allCancel := tmuxTimeoutContext()
	_, allErr := outputTmuxBoundedWith(allCtx, cmdExec, "show-environment", "-t", exactTarget(sanitizedName))
	allTimedOut := allCtx.Err() != nil
	allCancel()
	if allErr == nil {
		return "", false, nil
	}
	if allTimedOut {
		return "", false, fmt.Errorf("%w: show-environment %s after %s", ErrTmuxTimeout, sanitizedName, tmuxCommandTimeout)
	}

	return "", false, fmt.Errorf("read %s ownership marker for tmux session %s: targeted query: %v; environment query: %w",
		EnvMarkerHome, sanitizedName, markerErr, allErr)
}

// missingSessionEnvMarker recognizes tmux's explicit absent-variable answer
// from the TARGETED query. Exit status alone is insufficient: a transient
// wrapper/server failure may also be nonzero, and a later command succeeding
// cannot retroactively determine why the first failed.
func missingSessionEnvMarker(err error) bool {
	var exitErr *exec.ExitError
	return errors.As(err, &exitErr) &&
		strings.TrimSpace(string(exitErr.Stderr)) == "unknown variable: "+EnvMarkerHome
}

var (
	newSessionEnvOnce      sync.Once
	newSessionEnvSupported bool
)

// tmuxSupportsNewSessionEnv probes `tmux -V` once per process. Unparseable
// versions ("openbsd-7.4", exotic builds) conservatively report false.
//
// Bounded like every other tmux command in this package (#2099), though the
// hazard here is narrower and worth naming honestly: `tmux -V` is answered by the
// CLIENT and never contacts the server, so a wedged SERVER — the failure mode
// #2099/#2105 are about — cannot stall it. What can is a hanging tmux BINARY (a
// wrapper script, a stalled network filesystem holding the executable). That is
// defensive rather than reproduced, but the blast radius justifies the three
// lines: this sits behind a sync.Once on the session-CREATE path, so a single
// hang would wedge session creation for the entire process lifetime and every
// later caller would block on the Once rather than retry.
func tmuxSupportsNewSessionEnv() bool {
	newSessionEnvOnce.Do(func() {
		ctx, cancel := tmuxTimeoutContext()
		defer cancel()
		out, err := outputTmuxBoundedWith(ctx, cmd.MakeExecutor(), "-V")
		if err != nil {
			return
		}
		newSessionEnvSupported = versionSupportsNewSessionEnv(strings.TrimSpace(string(out)))
	})
	return newSessionEnvSupported
}

// versionSupportsNewSessionEnv parses a `tmux -V` string ("tmux 3.4",
// "tmux 3.3a", "tmux next-3.6", "tmux master") and reports whether
// `new-session -e` (tmux >= 3.2) is available.
func versionSupportsNewSessionEnv(version string) bool {
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
	return major > 3 || (major == 3 && minor >= 2)
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
