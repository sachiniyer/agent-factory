package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestExhaustedBackupSlotsSayWhatToDo is the #2917 regression for the config
// half of the class: a guard that REFUSES a user-initiated write has to name the
// action that clears the refusal.
//
// The message this replaces named a thousand files and stopped there. It fires
// on every subsequent `af config set` on a long-lived home, so the user is
// blocked from writing config at all and holds only the observation that some
// files exist — not that they are backups, not where they live, and not that
// removing them is safe.
func TestExhaustedBackupSlotsSayWhatToDo(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "config.toml.bak")

	// Fill every slot the helper will try.
	if err := os.WriteFile(base, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 1; i < 1000; i++ {
		if err := os.WriteFile(fmt.Sprintf("%s.%d", base, i), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	_, err := availableBackupPath(base)
	if err == nil {
		t.Fatal("availableBackupPath returned a path with every slot taken")
	}

	// It must name the directory the user has to go clean, not just the file
	// pattern — the base path is inside AF_HOME, which they may never have opened.
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("error = %q, want it to name the directory %q holding the backups", err, dir)
	}
	// And it must say what to DO. The remedy is real and safe (these are
	// backups, not state), so refusing without stating it strands the user.
	if !strings.Contains(err.Error(), "remove") && !strings.Contains(err.Error(), "delete") {
		t.Errorf("error = %q, want it to tell the user to remove or delete the old backups — "+
			"a guard that blocks a write and names no remedy leaves them stuck (#2917)", err)
	}
	// The backups are the remedy's target, so the message must say they are
	// backups and disposable; "config.toml.bak.1..999 all exist" does not.
	if !strings.Contains(strings.ToLower(err.Error()), "backup") {
		t.Errorf("error = %q, want it to identify these files as backups", err)
	}
}
