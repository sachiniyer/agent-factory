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
	// And it must not tell them to clear the OLDEST first. The unsuffixed base
	// is the first backup ever written, so on a repeated convert/downgrade it
	// holds the original real settings — the one file this helper never
	// overwrites. "Oldest first" points straight at it (Codex on #2941).
	if strings.Contains(strings.ToLower(err.Error()), "oldest first") {
		t.Errorf("error = %q tells the user to clear the oldest backup first, which is the one most "+
			"likely to hold their original settings and the one availableBackupPath never overwrites", err)
	}

	// The backups are the remedy's target, so the message must say they are
	// backups; "config.toml.bak.1..999 all exist" does not.
	if !strings.Contains(strings.ToLower(err.Error()), "backup") {
		t.Errorf("error = %q, want it to identify these files as backups", err)
	}

	// But it must NOT promise they are safe to delete (Codex on #2941). This
	// helper is SHARED: writeSchemaMigrationBackup passes `<store>.bak.schema-v<N>`,
	// whose files are the pre-migration rollback copies for that store, and the
	// helper cannot tell its callers apart. A blanket reassurance here would talk
	// a user into deleting their only way back from a schema migration.
	for _, unsafe := range []string{"loses nothing", "safe to remove", "safe to delete"} {
		if strings.Contains(strings.ToLower(err.Error()), unsafe) {
			t.Errorf("error = %q claims removal is harmless (%q), but this helper also serves "+
				"writeSchemaMigrationBackup, whose backups are the only pre-migration rollback data", err, unsafe)
		}
	}
}

// TestSchemaMigrationBackupsAreNotCalledDisposable pins the caller that makes the
// reassurance dangerous, so the shared helper's wording stays true for BOTH: this
// base is the shape writeSchemaMigrationBackup passes.
func TestSchemaMigrationBackupsAreNotCalledDisposable(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "instances.json.bak.schema-v0")
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
	if strings.Contains(strings.ToLower(err.Error()), "loses nothing") {
		t.Errorf("error = %q tells the user that deleting pre-migration rollback copies loses nothing", err)
	}
	// It must still be actionable — the point is accurate advice, not no advice.
	if !strings.Contains(err.Error(), dir) || !strings.Contains(err.Error(), "delete") {
		t.Errorf("error = %q, want it to name the directory and an action", err)
	}
}
