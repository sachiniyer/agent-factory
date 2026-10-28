package session

import (
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

// RebuildTeardownMarkerName is the marker's file name inside the session dir.
const RebuildTeardownMarkerName = "rebuild-teardown.pending"

// InstallDurableRebuildTeardown installs the checkpoint and runtime-stop
// callbacks a rebuilding respawn requires, backed by a marker file in dir, and
// restores a pending obligation a previous process left behind.
//
// The runtime stop is a no-op here by design: it exists to stop what no tmux
// session carries — the daemon-supervised VS Code editor — and the in-sandbox
// agent-server supervises none (the editor, when any, is the driving daemon's).
func (i *Instance) InstallDurableRebuildTeardown(dir string) error {
	if dir == "" {
		return fmt.Errorf("rebuild teardown marker needs a session directory")
	}
	marker := filepath.Join(dir, RebuildTeardownMarkerName)
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
