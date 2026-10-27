package api

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// These tests pin the detach-key wiring in `af sessions attach`'s RunE. The
// command recognizes the detach key CLIENT-side in its own process (apiclient's
// stdin pump matches tmux.DetachKeyByte live), so the global must be armed from
// the user's config before the attach dials — exactly what the TUI does at
// startup. Before the fix the CLI never loaded detach_keys, so a fresh `af
// sessions attach` always ran with the hardcoded default ctrl+w regardless of
// config, even though the command's Long help promises "the configured detach
// key".
//
// The attach is driven over a REMOTE target pointing at a closed port, so the
// dial fails immediately AFTER the detach key is armed and BEFORE any real
// network or tmux involvement. Only the remote path is exercised: the local
// path calls daemon.EnsureDaemon, which spawns a real daemon and has no place
// in a unit test (mirrors TestSessionsAttachCmd_RemoteReachesTheDaemon).

// snapshotDetachKey records the process-global detach key and restores it on
// cleanup, so a test mutating the global (via the command's RunE) cannot leak
// into a sibling test. apiclient's matcher reads the global live, so every test
// starts from the built-in default rather than whatever a prior test left.
func snapshotDetachKey(t *testing.T) {
	t.Helper()
	prevByte, prevDisplay := tmux.DetachKeyByte, tmux.DetachKeyDisplay
	t.Cleanup(func() { tmux.SetDetachKey(prevByte, prevDisplay) })
	tmux.SetDetachKey(23, "ctrl+w")
}

// prepareRemoteAttach points the CLI at a closed remote port for the duration
// of a test and isolates the config home + cwd the way the attach path sees
// them. It MUST run before writeGlobalConfig so the config is written into the
// ISOLATED home (under AGENT_FACTORY_HOME), not the operator's real one.
func prepareRemoteAttach(t *testing.T) {
	t.Helper()
	clientWithoutTheSession(t)
	prev := apiclient.FlagDaemonURL
	apiclient.FlagDaemonURL = "http://127.0.0.1:1"
	t.Cleanup(func() { apiclient.FlagDaemonURL = prev })
}

// writeGlobalConfig writes a config.toml carrying detachKeys into the home the
// command will load from. The TUI/CLI global config layer is the only source for
// detach_keys (it is a sourceGlobalOnly key), so this is the whole configuration
// surface the wiring reads. Call after prepareRemoteAttach so GetConfigDir
// resolves to the isolated AGENT_FACTORY_HOME.
func writeGlobalConfig(t *testing.T, detachKeys string) {
	t.Helper()
	configDir, err := config.GetConfigDir()
	if err != nil {
		t.Fatalf("GetConfigDir: %v", err)
	}
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	content := "detach_keys = \"" + detachKeys + "\"\n"
	if err := os.WriteFile(filepath.Join(configDir, config.TomlConfigFileName), []byte(content), 0o644); err != nil {
		t.Fatalf("write config.toml: %v", err)
	}
}

// runRemoteAttachFailAtDial runs sessionsAttachCmd's RunE and asserts it reaches
// the daemon dial (failing there) rather than failing earlier at resolution. The
// dial fails fast (connection refused on a privileged port with no listener),
// and only AFTER the detach key is armed, so a caller can assert on the global
// detach key state on return.
func runRemoteAttachFailAtDial(t *testing.T) {
	t.Helper()
	cmd := *sessionsAttachCmd
	cmd.SetContext(context.Background())
	err := cmd.RunE(&cmd, []string{"remote-only"})
	if err == nil {
		t.Fatal("dialing a closed port must fail")
	}
	if strings.Contains(err.Error(), "not found") {
		t.Fatalf("attach resolved the session on the client's disk instead of reaching the dial: %v", err)
	}
	if !strings.Contains(err.Error(), "failed to attach") {
		t.Fatalf("expected the failure to come from the daemon dial (after the detach key is armed), got: %v", err)
	}
}

// TestSessionsAttachCmd_AppliesConfiguredDetachKey is the core regression: a
// user who customized detach_keys to a non-default key must detach with THAT key
// from `af sessions attach`, not the hardcoded ctrl+w. Before the fix the
// command never loaded detach_keys, so the matcher ran with the default byte 23
// regardless of config and the configured key was silently forwarded to the
// agent as ordinary input.
func TestSessionsAttachCmd_AppliesConfiguredDetachKey(t *testing.T) {
	snapshotDetachKey(t)
	prepareRemoteAttach(t)
	writeGlobalConfig(t, "ctrl-q")

	runRemoteAttachFailAtDial(t)

	if tmux.DetachKeyByte != 17 {
		t.Fatalf("configured ctrl-q (byte 17) must arm the detach matcher, got byte %d — the CLI attach did not apply detach_keys", tmux.DetachKeyByte)
	}
	if tmux.DetachKeyDisplay != "ctrl+q" {
		t.Fatalf("detach key display = %q, want the normalized ctrl+q help-overlay spelling", tmux.DetachKeyDisplay)
	}
}

// TestSessionsAttachCmd_DetachKeyDefaultsToCtrlW pins the common (default)
// path: with no custom detach_keys the command arms the documented default
// ctrl+w. It pre-arms a non-default key FIRST, so the assertion proves the
// command APPLIED the default config (resetting the global to ctrl+w) rather
// than merely leaving the prior value untouched — the difference between
// "wiring ran" and "wiring was skipped", which is exactly the bug.
func TestSessionsAttachCmd_DetachKeyDefaultsToCtrlW(t *testing.T) {
	snapshotDetachKey(t)
	prepareRemoteAttach(t)
	// Pre-arm a non-default key so a skipped wiring (the bug) is detectable:
	// the bug leaves this value in place, the fix resets it to the default.
	tmux.SetDetachKey(1, "ctrl+a")
	// No custom detach_keys: the first-run default config materializes
	// detach_keys = "ctrl-w".
	runRemoteAttachFailAtDial(t)

	if tmux.DetachKeyByte != 23 {
		t.Fatalf("default detach key must be ctrl+w (byte 23), got byte %d — a default-configuration user would be detached by the wrong key", tmux.DetachKeyByte)
	}
	if tmux.DetachKeyDisplay != "ctrl+w" {
		t.Fatalf("detach key display = %q, want the default ctrl+w", tmux.DetachKeyDisplay)
	}
}
