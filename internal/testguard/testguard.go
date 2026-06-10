// Package testguard fences test binaries off from the developer's real
// agent-factory config. Test packages that can spawn af processes or write
// config files call ConfigTripwire from TestMain; if any test (or a child
// process it spawned) escapes its AGENT_FACTORY_HOME sandbox and touches the
// real config.json, the package run fails loudly instead of the user
// discovering days later that their settings were silently replaced (#837).
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
	"path/filepath"
	"strings"
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
