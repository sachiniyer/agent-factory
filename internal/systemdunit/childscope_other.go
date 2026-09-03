//go:build !linux

package systemdunit

import (
	"errors"
	"os/exec"
)

// launchd has no shared service cgroup and no systemd transient scopes. Keep the
// existing direct child launch on Darwin and other non-Linux platforms, and let
// every scope-name derivation come back empty so callers record no handle and
// take today's path.
var ErrManagerUnavailable = errors.New("systemd user manager is unavailable")

func NewBoundChildCommand(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...)
}

func NewUnboundScopeCommand(_ string, name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...)
}

func UnboundScopeArgv(_ string, name string, args ...string) (string, []string) {
	return name, args
}

func HookScopeUnitPrefix(string) string { return "" }

func NewHookScopeGeneration() string { return "" }

func HookScopeUnit(string, string, int) string { return "" }

func RunningHookScopes(...string) ([]string, error) { return nil, nil }

func StopHookScopes(...string) error { return nil }

func StopScopeUnits(...string) error { return nil }

func RunningHookLaunchers(...string) ([]HookLauncher, error) { return nil, nil }
