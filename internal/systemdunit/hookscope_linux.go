//go:build linux

package systemdunit

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// ErrManagerUnavailable reports that the systemd user manager could not be
// reached at all — systemctl is missing, or there is no user bus. It is
// deliberately distinct from "the manager answered and listed nothing": a
// caller that cannot tell those apart turns a failed read into an empty result.
//
// It does NOT mean the scopes are gone. Unreachable-from-here and gone are
// different facts: a client with no XDG_RUNTIME_DIR, or one that lost the bus,
// gets this while the manager and every scope in it keep running. Callers on a
// path that is about to touch the tree must treat it as UNKNOWN and fail closed;
// it is typed only so an error message can say which of the two it was.
var ErrManagerUnavailable = errors.New("systemd user manager is unavailable")

const (
	hookScopeListTimeout = 10 * time.Second
	hookScopeStopTimeout = 30 * time.Second
)

// hookLauncherSettleTimeout bounds how long the sweep waits for a launcher it
// found to become a scope it can stop, and hookLauncherPollInterval is how often
// it re-asks. The wait is normally microseconds — the window closes when systemd
// answers StartTransientUnit — so this budget is for the pathological case: a
// launcher wedged on a bus that never replies. Waiting it out and then FAILING
// is the point; returning success at any moment in the window is the defect.
//
// Vars, not consts, only so a test can collapse the clock. Production never
// reassigns them.
var (
	hookLauncherSettleTimeout = 15 * time.Second
	hookLauncherPollInterval  = 100 * time.Millisecond
)

func systemctlUser(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "systemctl", append([]string{"--user", "--no-pager"}, args...)...)
	// CommandContext kills only the immediate process; without a delay Output
	// can still block on an inherited pipe held by a child systemctl forked.
	cmd.WaitDelay = 2 * time.Second
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err == nil {
		return out, nil
	}
	if errors.Is(err, exec.ErrNotFound) {
		return nil, fmt.Errorf("%w: %v", ErrManagerUnavailable, err)
	}
	message := stderr.String()
	for _, marker := range []string{
		"Failed to connect to bus",
		"Failed to get D-Bus connection",
		"No medium found",
		"Failed to connect to user scope bus",
	} {
		if strings.Contains(message, marker) {
			return nil, fmt.Errorf("%w: %s", ErrManagerUnavailable, strings.TrimSpace(message))
		}
	}
	return nil, fmt.Errorf("systemctl --user %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(message))
}

// RunningHookScopes lists the scope units still active under prefix — the hook
// runs that outlived the daemon generation that started them. An empty prefix
// means the session never entered a scope, which is today's behaviour and needs
// no manager round trip at all.
func RunningHookScopes(prefixes ...string) ([]string, error) {
	present := nonEmpty(prefixes)
	if len(present) == 0 {
		return nil, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookScopeListTimeout)
	defer cancel()
	// No --all: list-units then reports only units that are loaded AND active,
	// which is exactly "a hook is still running in here". --collect already
	// garbage-collects a scope the moment its last process exits.
	args := []string{"list-units", "--type=scope", "--plain", "--no-legend", "--"}
	for _, prefix := range present {
		args = append(args, prefix+"-*.scope")
	}
	out, err := systemctlUser(ctx, args...)
	if err != nil {
		return nil, err
	}
	var units []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		// UNIT LOAD ACTIVE SUB DESCRIPTION…
		if len(fields) < 3 || !strings.HasSuffix(fields[0], ".scope") {
			continue
		}
		if fields[2] == "inactive" {
			continue
		}
		if hasHookPrefix(fields[0], present) {
			units = append(units, fields[0])
		}
	}
	return units, nil
}

// RunningHookPrefixes reports which of prefixes still have a hook run in
// flight. It is the READ half of what StopHookScopes does: the same two oracles,
// consulted in the same order, with no stop and no wait.
//
// The order is load-bearing for the same reason it is there. A launcher that
// registers between the two reads becomes a unit the second read still sees;
// units-first would let exactly that hook fall through the gap between them and
// be reported as finished (#3667).
//
// One call answers for every prefix, which is why it takes a set: the caller is
// a daemon restoring its whole fleet, and one manager round trip plus one /proc
// walk for all of them beats one pair per session on the path that gates
// readiness (#3682).
//
// An oracle that cannot be read is an error, never an empty answer. That
// includes one process the launcher oracle cannot classify
// (*proctree.ArgvUnreadableError, #7239): it might be a launcher mid-execve,
// so "none" is not an answer this read can give over it.
//
// # Which callers fail closed and which fail open
//
// This is the FAIL-CLOSED read. Its callers act only on err == nil with an
// empty answer, and hold on an error, as they do for an unreachable manager:
//   - hooks_progress_adopt.go resumes the remaining hooks;
//   - hooks_progress_resume.go steps over a claimed entry;
//   - hooks_progress_failure.go records a disappeared claimant as failed;
//   - hooks_progress_retention_plan.go reclaims journals.
//
// Each of those, on a false "none", would let a command recorded as failed run
// alongside the hooks resumed after it, which is the ordering #2770 and #3667
// exist for (#7242 review R2-1). The trade-off is stated, not hidden: while a
// process of ours stays argv-less, which a large exit does for under a second
// and a blanked argv does for good, they wait.
//
// The two FAIL-OPEN readers, the restored session's hook probe and its adopted
// watcher (hooks_adopt.go), use RunningHookPrefixesFailOpen instead.
func RunningHookPrefixes(prefixes ...string) ([]string, error) {
	present := nonEmpty(prefixes)
	if len(present) == 0 {
		return nil, nil
	}
	launchers, err := RunningHookLaunchers(present...)
	if err != nil {
		return nil, err
	}
	units, err := RunningHookScopes(present...)
	if err != nil {
		return nil, err
	}
	return liveHookPrefixes(present, units, launchers), nil
}

// RunningHookPrefixesFailOpen is RunningHookPrefixes for the readers that fail
// OPEN: the restored session's hook probe and its adopted watcher
// (hooks_adopt.go). On an error they report no hooks in flight, because
// guessing "running" would hold the readiness budget and a task's teardown
// against a session nothing can resolve. The safety ordering is not theirs: it
// belongs to StopHookScopes and the fail-closed callers above.
//
// Failing open is why one process the launcher oracle cannot classify is NOT an
// error here. An error would erase everything this read DID establish. An
// adopted watcher would call a hook finished on one poll that landed while some
// process of ours was exiting, and stop watching for good. So that unknown
// degrades this read to the units the manager lists plus the launchers the scan
// found (#7242 review R2-1). A process table or manager that cannot be read is
// still an error.
func RunningHookPrefixesFailOpen(prefixes ...string) ([]string, error) {
	present := nonEmpty(prefixes)
	if len(present) == 0 {
		return nil, nil
	}
	launchers, err := RunningHookLaunchers(present...)
	var unclassified *proctree.ArgvUnreadableError
	if err != nil && !errors.As(err, &unclassified) {
		return nil, err
	}
	units, err := RunningHookScopes(present...)
	if err != nil {
		return nil, err
	}
	return liveHookPrefixes(present, units, launchers), nil
}

// liveHookPrefixes attributes the units and launchers found back to each prefix
// that owns one.
func liveHookPrefixes(present, units []string, launchers []HookLauncher) []string {
	names := make([]string, 0, len(units)+len(launchers))
	names = append(names, units...)
	for _, launcher := range launchers {
		names = append(names, launcher.Unit)
	}
	live := make([]string, 0, len(present))
	for _, prefix := range present {
		if hasAnyHookPrefix(names, prefix) {
			live = append(live, prefix)
		}
	}
	return live
}

// hasAnyHookPrefix reports whether any of names belongs to this one prefix. The
// per-prefix answer is what lets one batched read be attributed back to the
// session that owns each scope.
func hasAnyHookPrefix(names []string, prefix string) bool {
	own := []string{prefix}
	for _, name := range names {
		if hasHookPrefix(name, own) {
			return true
		}
	}
	return false
}

func nonEmpty(values []string) []string {
	present := make([]string, 0, len(values))
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			present = append(present, trimmed)
		}
	}
	return present
}

// StopHookScopes stops every hook scope still alive under prefix and PROVES the
// stop rather than assuming it: it re-lists afterwards and reports the units
// that are still there. Callers on the rebuild/remove path must fail closed on
// the error, exactly as they already do for an unjoined in-process hook — a
// survivor holding the old tree is the hazard the ordering exists to prevent.
//
// It consults TWO oracles, because one of them can be silent while a hook is
// still coming. A unit's absence is not evidence on its own: for the interval
// between systemd-run's execve and its StartTransientUnit reply the scope does
// not exist yet, and a daemon that died in that window (KillMode=process) left
// the launcher alive and unscoped. RunningHookLaunchers finds it, and a launcher
// found means "a hook survives" — so this WAITS for it to become a stoppable
// scope instead of reporting the tree clear (#3667).
//
// Neither oracle may fail quietly. A systemctl that cannot reach the manager and
// a process table that cannot be read are both UNKNOWN, and both come back as an
// error the caller must refuse on.
//
// One process of ours that the launcher oracle cannot CLASSIFY is waited on
// like a launcher it found (#7239). It might be a launcher mid-execve, so it
// cannot be read as absent. But it is far more often a process mid-exit: a
// single-threaded process reads present and argv-less for its whole
// address-space teardown, measured at 114 ms for 1 GiB, and archive runs this
// sweep exactly while the session's own processes are exiting. So it holds the
// sweep within the same settle budget, and is refused on (as a
// *HookLauncherUnclassifiedError) only if it outlives the budget.
func StopHookScopes(prefixes ...string) error {
	present := nonEmpty(prefixes)
	if len(present) == 0 {
		return nil
	}
	// The settle budget measures WAITING FOR REGISTRATION, so time spent
	// stopping scopes the sweep could already name is subtracted from it. That
	// work is progress, and it is bounded by hookScopeStopTimeout — 30s, an
	// order of magnitude above the budget — so charging it here would let a
	// single slow `systemctl stop` exhaust the budget before the first re-sweep:
	// the sweep would give up without ever having waited for the launcher, and
	// say it had waited the budget while doing it.
	start := time.Now()
	var stopping time.Duration
	for {
		// The LAUNCHER oracle is read first, and the order is load-bearing. A
		// launcher that registers between the two reads turns into a unit the
		// list below still sees; reading the units first would let exactly that
		// hook fall through the gap between them, seen by neither.
		launchers, err := RunningHookLaunchers(present...)
		// A process the oracle cannot classify is held like a found launcher,
		// below. Any other failure is a /proc that cannot be read: refuse.
		var unclassified *proctree.ArgvUnreadableError
		if err != nil && !errors.As(err, &unclassified) {
			return err
		}
		units, err := RunningHookScopes(present...)
		if err != nil {
			return err
		}
		if len(units) > 0 {
			stopStart := time.Now()
			stopErr := StopScopeUnits(units...)
			if stopErr == nil {
				units, stopErr = RunningHookScopes(present...)
			}
			stopping += time.Since(stopStart)
			if stopErr != nil {
				return stopErr
			}
		}
		if len(launchers) == 0 && unclassified == nil {
			if len(units) == 0 {
				return nil
			}
			return fmt.Errorf("hook scopes %s are still active after stop", strings.Join(units, ", "))
		}
		// A launcher is alive, so a hook survives even though its scope does not
		// exist yet — and it cannot be stopped by name until it does. WAIT for it
		// rather than proceeding: it is about to exec the operator's command with
		// the cwd this caller is on its way to rebuild or move (#3667).
		//
		// Waiting rather than killing also keeps this inside the authority the
		// sweep already had. What it ends is the SCOPE, through the manager, once
		// the manager admits the scope exists.
		waited := time.Since(start) - stopping
		if waited >= hookLauncherSettleTimeout {
			// A launcher it FOUND is the stronger, session-specific reason, so it
			// names the refusal even when an unknown was also standing.
			if len(launchers) == 0 && unclassified != nil {
				return &HookLauncherUnclassifiedError{Unclassified: unclassified, Waited: waited}
			}
			return &HookLauncherUnregisteredError{Launchers: launchers, Waited: waited}
		}
		time.Sleep(hookLauncherPollInterval)
	}
}

// HookLauncherUnregisteredError reports launchers that were still alive and
// still unregistered when the sweep gave up, and how long it actually spent
// WAITING for them.
//
// Waited is carried rather than only formatted into the message because it is a
// claim the sweep has to get right: the settle budget is for a launcher that
// will not register, and time spent stopping a scope the sweep could already
// name is progress rather than waiting.
type HookLauncherUnregisteredError struct {
	Launchers []HookLauncher
	Waited    time.Duration
}

// HookLauncherUnclassifiedError reports a process of ours that the launcher
// oracle still could not classify when the sweep's settle budget ran out, and
// how long the sweep spent WAITING for it (#7239). Such a process might be a
// launcher mid-execve. Usually it is a process mid-exit or one starting up, and
// that resolves well inside the budget, so the error means one that did not.
//
// The oracle scans every process of this user on the box, not just this
// session's, so the process may have nothing to do with the session being torn
// down. The message says so, and it does not claim any scope failed to stop
// (#7242 review R2-3).
type HookLauncherUnclassifiedError struct {
	Unclassified *proctree.ArgvUnreadableError
	Waited       time.Duration
}

func (e *HookLauncherUnclassifiedError) Error() string {
	return fmt.Sprintf(
		"pid %d, a process of this user that may be unrelated to this session, stayed unclassifiable for %s "+
			"of the hook sweep's wait: its command line could not be read, so it could not be ruled out as a "+
			"hook launcher. Retry; if it persists, inspect it with `ps -o pid,stat,lstart,args -p %d`: %v",
		e.Unclassified.PID, e.Waited.Round(time.Millisecond), e.Unclassified.PID, e.Unclassified)
}

// DescribeHookSweepRefusal says, for a caller's refusal message, what made a
// StopHookScopes sweep refuse. what names the scopes the caller was clearing,
// such as "a previous run's hook scopes (af-hook-x-*)". That is either "<what>
// could not be stopped", or, when the refusal was an unclassifiable process
// (#7242 review R2-3), that such a process of this user, possibly unrelated to
// this session, could not be ruled out as one of their launchers.
func DescribeHookSweepRefusal(err error, what string) string {
	var unclassified *HookLauncherUnclassifiedError
	if errors.As(err, &unclassified) {
		return fmt.Sprintf("a process of this user (pid %d), possibly unrelated to this session, "+
			"could not be ruled out as a launcher for %s", unclassified.Unclassified.PID, what)
	}
	return what + " could not be stopped"
}

func (e *HookLauncherUnclassifiedError) Unwrap() error { return e.Unclassified }

func (e *HookLauncherUnregisteredError) Error() string {
	return fmt.Sprintf(
		"hook launcher%s %s did not register within %s of waiting, so a hook is still about to start in this worktree",
		plural(len(e.Launchers)), describeHookLaunchers(e.Launchers), e.Waited.Round(time.Millisecond))
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// describeHookLaunchers names the pids and the scopes they are about to claim,
// so an operator reading the refusal can find the process rather than being told
// only that one exists.
func describeHookLaunchers(launchers []HookLauncher) string {
	parts := make([]string, 0, len(launchers))
	for _, launcher := range launchers {
		parts = append(parts, fmt.Sprintf("pid %d (%s)", launcher.PID, launcher.Unit))
	}
	return strings.Join(parts, ", ")
}

// StopScopeUnits stops named scopes. A unit that is already gone is not an
// error: --collect means a finished scope is unloaded, and "not loaded" is the
// success case for a teardown call, not a failure to report.
func StopScopeUnits(units ...string) error {
	present := nonEmpty(units)
	if len(present) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), hookScopeStopTimeout)
	defer cancel()
	_, err := systemctlUser(ctx, append([]string{"stop", "--"}, present...)...)
	if err == nil {
		return nil
	}
	if strings.Contains(err.Error(), "not loaded") {
		return nil
	}
	return err
}
