//go:build linux

package systemdunit

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
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
func StopHookScopes(prefixes ...string) error {
	present := nonEmpty(prefixes)
	if len(present) == 0 {
		return nil
	}
	deadline := time.Now().Add(hookLauncherSettleTimeout)
	for {
		// The LAUNCHER oracle is read first, and the order is load-bearing. A
		// launcher that registers between the two reads turns into a unit the
		// list below still sees; reading the units first would let exactly that
		// hook fall through the gap between them, seen by neither.
		launchers, err := RunningHookLaunchers(present...)
		if err != nil {
			return err
		}
		units, err := RunningHookScopes(present...)
		if err != nil {
			return err
		}
		if len(units) > 0 {
			if err := StopScopeUnits(units...); err != nil {
				return err
			}
			if units, err = RunningHookScopes(present...); err != nil {
				return err
			}
		}
		if len(launchers) == 0 {
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
		if !time.Now().Before(deadline) {
			return fmt.Errorf(
				"hook launcher%s %s did not register within %s, so a hook is still about to start in this worktree",
				plural(len(launchers)), describeHookLaunchers(launchers), hookLauncherSettleTimeout)
		}
		time.Sleep(hookLauncherPollInterval)
	}
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
