// Package testguard fences test binaries off from the developer's real
// agent-factory environment. Test packages that can spawn af processes or
// write config files call ConfigTripwire from TestMain; if any test (or a
// child process it spawned) escapes its AGENT_FACTORY_HOME sandbox and
// touches the real config.json, the package run fails loudly instead of the
// user discovering days later that their settings were silently replaced
// (#837). TmuxTripwire does the same for real tmux sessions leaked onto the
// developer's tmux server, SandboxHome defaults a whole package into a
// throwaway AGENT_FACTORY_HOME, and IsolateTmux gives a test a private tmux
// server so nothing it creates can outlive it (#1056).
//
// This package deliberately has no dependency on the config package — config
// imports session/tmux, and the tripwire must be usable from that package's
// tests without an import cycle — so the config-dir resolution below mirrors
// config.GetConfigDir.
package testguard

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// ambientConfigPath resolves the config.json the test process would touch
// with its ambient (pre-test) environment: $AGENT_FACTORY_HOME when set,
// otherwise ~/.agent-factory. Returns "" when the path cannot be resolved
// (no HOME — e.g. some CI sandboxes), in which case the tripwire is a no-op.
func ambientConfigPath() string {
	dir := os.Getenv("AGENT_FACTORY_HOME")
	if strings.HasPrefix(dir, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		switch {
		case dir == "~":
			dir = home
		case strings.HasPrefix(dir, "~/"):
			dir = filepath.Join(home, dir[2:])
		default:
			// Malformed tilde form; GetConfigDir would error out, so there
			// is no real config at risk.
			return ""
		}
	}
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		dir = filepath.Join(home, ".agent-factory")
	}
	return filepath.Join(dir, "config.json")
}

func hashFile(path string) ([]byte, bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	sum := sha256.Sum256(data)
	return sum[:], true, nil
}

// ConfigTripwire snapshots the real config.json and returns a verify func
// for TestMain to call after m.Run(). Verify returns a non-nil error when:
//   - the file existed at snapshot time and was modified or deleted, or
//   - the file did not exist and a test materialized one.
//
// On boxes without a resolvable home (or with AF_DISABLE_CONFIG_TRIPWIRE=1)
// both snapshot and verify are no-ops, so CI runs are unaffected.
func ConfigTripwire() func() error {
	if os.Getenv("AF_DISABLE_CONFIG_TRIPWIRE") == "1" {
		return func() error { return nil }
	}
	path := ambientConfigPath()
	if path == "" {
		return func() error { return nil }
	}
	before, existed, err := hashFile(path)
	if err != nil {
		// Unreadable real config (permissions?) — nothing we can guard.
		return func() error { return nil }
	}
	return func() error {
		after, exists, err := hashFile(path)
		if err != nil {
			return fmt.Errorf("config tripwire: cannot re-read %s after the test run: %w", path, err)
		}
		switch {
		case existed && !exists:
			return fmt.Errorf("config tripwire: %s was DELETED during this package's test run — a test escaped its AGENT_FACTORY_HOME sandbox (#837)", path)
		case existed && !bytes.Equal(before, after):
			return fmt.Errorf("config tripwire: %s was MODIFIED during this package's test run — a test escaped its AGENT_FACTORY_HOME sandbox (#837)", path)
		case !existed && exists:
			return fmt.Errorf("config tripwire: %s was CREATED during this package's test run — a test materialized config into the real config dir (#837)", path)
		}
		return nil
	}
}

// ambientAFSessions lists the af_-prefixed session names on the tmux server
// the current environment resolves to. A nil map means no reachable server —
// nothing to leak against.
func ambientAFSessions() map[string]bool {
	out, err := exec.Command("tmux", "list-sessions", "-F", "#{session_name}").Output()
	if err != nil {
		return nil
	}
	sessions := make(map[string]bool)
	for _, name := range strings.Split(string(out), "\n") {
		// Anchor the prefix so a non-agent session like "my_af_project"
		// never trips the wire (same anchoring as tmux.CleanupSessions).
		if strings.HasPrefix(name, "af_") {
			sessions[name] = true
		}
	}
	return sessions
}

// TmuxTripwire snapshots the af_-prefixed sessions on the ambient tmux
// server and returns a verify func for TestMain to call after m.Run().
// Verify returns a non-nil error when the run left behind af_ sessions that
// were not there before — i.e. a test created a real agent tmux session on
// the developer's server and never killed it (#1056). Tests that need real
// tmux should call IsolateTmux, whose private server this tripwire cannot
// even see.
//
// Call it BEFORE any test changes TMUX_TMPDIR/TMUX so both snapshots target
// the ambient server. No-ops when tmux is not installed or with
// AF_DISABLE_TMUX_TRIPWIRE=1. Caveat for dev boxes running a real daemon:
// an af_ session legitimately created by the real daemon DURING the package
// run is indistinguishable from a leak; the error lists the session names so
// that case is recognizable, and the escape hatch covers it.
func TmuxTripwire() func() error {
	if os.Getenv("AF_DISABLE_TMUX_TRIPWIRE") == "1" {
		return func() error { return nil }
	}
	if _, err := exec.LookPath("tmux"); err != nil {
		return func() error { return nil }
	}
	before := ambientAFSessions()
	return func() error {
		var leaked []string
		for name := range ambientAFSessions() {
			if !before[name] {
				leaked = append(leaked, name)
			}
		}
		if len(leaked) == 0 {
			return nil
		}
		sort.Strings(leaked)
		return fmt.Errorf("tmux tripwire: this package's test run left tmux session(s) %v on the ambient tmux server — a test created a real af_ session without testguard.IsolateTmux or cleanup (#1056)", leaked)
	}
}

// SandboxHome points AGENT_FACTORY_HOME at a fresh temp dir for the whole
// package run and returns a restore func. Call it from TestMain AFTER
// ConfigTripwire (which must snapshot the real, pre-sandbox config) and
// BEFORE log.Initialize (so the package's log file lands in the sandbox
// instead of the production daemon log — #1056). Individual tests that
// t.Setenv their own home still win for their duration; this is the default
// for tests that never set one.
func SandboxHome() func() {
	dir, err := os.MkdirTemp("", "af-test-home-")
	if err != nil {
		panic("testguard: cannot create sandbox AGENT_FACTORY_HOME: " + err.Error())
	}
	prev, had := os.LookupEnv("AGENT_FACTORY_HOME")
	if err := os.Setenv("AGENT_FACTORY_HOME", dir); err != nil {
		panic("testguard: cannot set sandbox AGENT_FACTORY_HOME: " + err.Error())
	}
	return func() {
		if had {
			_ = os.Setenv("AGENT_FACTORY_HOME", prev)
		} else {
			_ = os.Unsetenv("AGENT_FACTORY_HOME")
		}
		_ = os.RemoveAll(dir)
	}
}

// IsolateTmux points the test at a private tmux server: a fresh TMUX_TMPDIR
// socket dir with $TMUX cleared, so a test run from inside a tmux pane does
// not fall back to the surrounding server ($TMUX wins over TMUX_TMPDIR in
// tmux's socket resolution). A cleanup kills the private server, so every
// session the test created dies with it and nothing can leak onto the
// developer's real server (#1056). Child processes — exec'd af binaries and
// the daemons they spawn — inherit both variables through os.Environ().
//
// The socket dir lives under os.TempDir() rather than t.TempDir() because
// unix socket paths are length-limited (~104 bytes) and t.TempDir() embeds
// the full test name. Skips the test when tmux is not installed; must not be
// used with t.Parallel (t.Setenv forbids it).
func IsolateTmux(t testing.TB) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skipf("tmux not available: %v", err)
	}
	dir, err := os.MkdirTemp("", "af-tmux-")
	if err != nil {
		t.Fatalf("testguard: cannot create private tmux socket dir: %v", err)
	}
	t.Setenv("TMUX_TMPDIR", dir)
	// Empty counts as unset for tmux's "am I inside tmux" check; t.Setenv
	// cannot fully unset a variable.
	t.Setenv("TMUX", "")
	t.Cleanup(func() {
		// Runs before t.Setenv's env restore (cleanups are LIFO), so this
		// kill targets the private server, then removes its socket dir.
		_ = exec.Command("tmux", "kill-server").Run()
		_ = os.RemoveAll(dir)
	})
}
