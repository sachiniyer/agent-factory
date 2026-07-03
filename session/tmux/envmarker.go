package tmux

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

var (
	newSessionEnvOnce      sync.Once
	newSessionEnvSupported bool
)

// tmuxSupportsNewSessionEnv probes `tmux -V` once per process. Unparseable
// versions ("openbsd-7.4", exotic builds) conservatively report false.
func tmuxSupportsNewSessionEnv() bool {
	newSessionEnvOnce.Do(func() {
		out, err := exec.Command("tmux", "-V").Output()
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
