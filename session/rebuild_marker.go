package session

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

// Durable rebuild-teardown for the sandbox-side instance (#5824).
//
// A respawn that has to rebuild a deleted worktree first records, durably, that a
// stale-pane teardown is owed (SetRebuildTeardownPending), and refuses to proceed
// when it has no way to make that record durable — so a crash mid-rebuild can
// never leave panes running in the deleted tree with nothing saying they are owed
// a teardown. Every daemon-side recovery installs the callbacks that persist the
// row. The in-sandbox agent-server builds its own instance and persists no row, so
// without this its relaunch refused before any rebuild could run.
//
// The sandbox's store is a marker file in the SESSION's own directory (beside the
// staged binary, where the per-session token already lives) — never the host's
// shared af home, which a policy-file agent-server must not touch (W2.5). Its
// presence means "a teardown is owed"; a restarted agent-server reads it and
// re-arms the obligation before anything else can respawn.

// The marker is keyed by the SESSION's identity, never by the directory it sits
// in (#5842). A provisioned agent-server runs from <sessionDir>/af, so its
// directory is private to it — but one launched from an INSTALLED af (a supported
// remote-hook mode) resolves to the shared install directory, and a fixed file name
// there would be one marker for every agent-server on the host: one session's arm
// would be read as another's obligation, and one's discharge would delete the
// other's. Each session holds its own file, so N sessions on one host hold N
// independent obligations.

// rebuildTeardownMarkerPrefix and Suffix frame the per-session marker file name.
const (
	rebuildTeardownMarkerPrefix = "rebuild-teardown-"
	rebuildTeardownMarkerSuffix = ".pending"
)

// RebuildTeardownKey is the stable per-session key for the marker: a digest of the
// workspace path and the session title, the two facts an agent-server is started
// with that together name one session on one host.
func RebuildTeardownKey(repoPath, title string) string {
	sum := sha256.Sum256([]byte(filepath.Clean(repoPath) + "\x00" + title))
	return hex.EncodeToString(sum[:8])
}

// RebuildTeardownMarkerName is the marker's file name for a session key.
func RebuildTeardownMarkerName(key string) string {
	return rebuildTeardownMarkerPrefix + key + rebuildTeardownMarkerSuffix
}

func validMarkerKey(key string) bool {
	if key == "" || len(key) > 64 {
		return false
	}
	for _, r := range key {
		if !(r >= '0' && r <= '9' || r >= 'a' && r <= 'f') {
			return false
		}
	}
	return true
}

// InstallDurableRebuildTeardown installs the checkpoint and runtime-stop
// callbacks a rebuilding respawn requires, backed by a marker file in dir, and
// restores a pending obligation a previous process left behind.
//
// The runtime stop is a no-op here by design: it exists to stop what no tmux
// session carries — the daemon-supervised VS Code editor — and the in-sandbox
// agent-server supervises none (the editor, when any, is the driving daemon's).
func (i *Instance) InstallDurableRebuildTeardown(dir, key string) error {
	if dir == "" {
		return fmt.Errorf("rebuild teardown marker needs a session directory")
	}
	if !validMarkerKey(key) {
		return fmt.Errorf("rebuild teardown marker needs a session key (RebuildTeardownKey), got %q", key)
	}
	marker := filepath.Join(dir, RebuildTeardownMarkerName(key))
	i.SetRebuildTeardownNotify(func(inst *Instance) error {
		if inst.RebuildTeardownPending() {
			return writeMarkerDurable(marker)
		}
		return removeMarkerDurable(marker)
	})
	i.SetRebuildTeardownRuntimesStop(func(*Instance) error { return nil })

	info, err := os.Lstat(marker)
	switch {
	case os.IsNotExist(err):
		return nil
	case err != nil:
		return fmt.Errorf("inspecting the rebuild teardown marker %s: %w", marker, err)
	case !info.Mode().IsRegular():
		return fmt.Errorf("rebuild teardown marker %s is not a regular file; refusing to guess whether a teardown is owed", marker)
	}
	// A previous process armed the obligation and did not discharge it: re-arm it,
	// so the next respawn runs the stale-pane teardown before launching anything.
	return i.SetRebuildTeardownPending(true)
}

// writeMarkerDurable creates the marker and syncs both the file and its
// directory, so the arm is on disk before the rebuild it guards begins.
func writeMarkerDurable(path string) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".rebuild-teardown-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString("pending\n"); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// removeMarkerDurable discharges the obligation: the marker is gone, and the
// removal is synced, before the caller treats the teardown as settled.
func removeMarkerDurable(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
