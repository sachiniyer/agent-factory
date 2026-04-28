package api

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// TestAppendInstanceFn_ValidJSON verifies that appendInstanceFn appends a new
// session to a valid instances array.
func TestAppendInstanceFn_ValidJSON(t *testing.T) {
	existing := []session.InstanceData{{Title: "existing-session"}}
	raw, err := json.Marshal(existing)
	if err != nil {
		t.Fatalf("marshal existing: %v", err)
	}

	newData := session.InstanceData{Title: "new-session"}
	fn := appendInstanceFn(newData)

	out, err := fn(raw)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var got []session.InstanceData
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(got))
	}
	if got[0].Title != "existing-session" || got[1].Title != "new-session" {
		t.Fatalf("unexpected result: %+v", got)
	}
}

// TestAppendInstanceFn_DuplicateTitle is the regression test for issue #371.
// Previously, appendInstanceFn blindly appended without checking whether an
// entry with the same Title already existed, producing duplicate stored
// entries that confused title-based lookup (findInstanceByTitle returns only
// the first match). It must now reject duplicates so the caller can clean up
// the just-created instance (tmux session + worktree) rather than orphaning
// the prior session's state.
func TestAppendInstanceFn_DuplicateTitle(t *testing.T) {
	existing := []session.InstanceData{{Title: "dupe"}}
	raw, err := json.Marshal(existing)
	if err != nil {
		t.Fatalf("marshal existing: %v", err)
	}

	out, err := appendInstanceFn(session.InstanceData{Title: "dupe"})(raw)
	if err == nil {
		t.Fatalf("expected error on duplicate title, got nil (output=%s)", string(out))
	}
	if out != nil {
		t.Fatalf("expected nil output on error, got: %s", string(out))
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("expected duplicate-title error, got: %v", err)
	}
}

// TestAppendInstanceFn_CorruptedJSON is the regression test for issue #257.
// Previously, the callback silently reset the existing data to an empty
// array on unmarshal failure, wiping all saved sessions. It must now return
// an error so the caller aborts the update and preserves the corrupted file.
func TestAppendInstanceFn_CorruptedJSON(t *testing.T) {
	corrupted := json.RawMessage(`{not valid json`)
	newData := session.InstanceData{Title: "new-session"}

	out, err := appendInstanceFn(newData)(corrupted)
	if err == nil {
		t.Fatalf("expected error on corrupted JSON, got nil (output=%s)", string(out))
	}
	if out != nil {
		t.Fatalf("expected nil output on error, got: %s", string(out))
	}
	if !strings.Contains(err.Error(), "failed to parse existing instances") {
		t.Fatalf("expected wrapped parse error, got: %v", err)
	}
}

// TestUpdateRepoInstances_CorruptedFilePreserved exercises the full path
// through config.UpdateRepoInstances: a corrupted instances.json must be
// left untouched when the callback returns an error, so users can recover
// the prior data manually.
func TestUpdateRepoInstances_CorruptedFilePreserved(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmp)

	repoID := "test-repo"
	instancesPath, err := config.RepoInstancesPath(repoID)
	if err != nil {
		t.Fatalf("RepoInstancesPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(instancesPath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	corrupted := []byte(`{this is not a valid instances array`)
	if err := os.WriteFile(instancesPath, corrupted, 0644); err != nil {
		t.Fatalf("write corrupted file: %v", err)
	}

	err = config.UpdateRepoInstances(repoID, appendInstanceFn(session.InstanceData{Title: "new-session"}))
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "failed to parse existing instances") {
		t.Fatalf("expected wrapped parse error, got: %v", err)
	}

	// The corrupted file must still be on disk untouched for recovery.
	got, readErr := os.ReadFile(instancesPath)
	if readErr != nil {
		t.Fatalf("read back instances: %v", readErr)
	}
	if string(got) != string(corrupted) {
		t.Fatalf("corrupted file was overwritten; got %q want %q", string(got), string(corrupted))
	}
}

// Sanity check that errors.Unwrap exposes the underlying json error, so
// callers can inspect it if needed.
func TestAppendInstanceFn_ErrorUnwraps(t *testing.T) {
	out, err := appendInstanceFn(session.InstanceData{})(json.RawMessage(`{bad`))
	if err == nil {
		t.Fatalf("expected error, got nil (output=%s)", string(out))
	}
	if errors.Unwrap(err) == nil {
		t.Fatalf("expected wrapped error, got non-wrapped: %v", err)
	}
}

// TestSessionsKill_GhostSessionDeleted is the regression test for issue #323.
// When a session's metadata exists on disk but the live instance can no
// longer be restored (e.g. tmux session destroyed externally,
// FromInstanceData fails), `af sessions kill <title>` must still remove the
// stale metadata so users can clean up the entry without resorting to the
// destructive `af reset`.
func TestSessionsKill_GhostSessionDeleted(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmp)

	repoID := "ghost-repo"
	instancesPath, err := config.RepoInstancesPath(repoID)
	if err != nil {
		t.Fatalf("RepoInstancesPath: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(instancesPath), 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Write a "ghost" instance whose worktree fields are empty so that
	// session.FromInstanceData -> git.NewGitWorktreeFromStorage fails,
	// reproducing the failure mode of a real ghost session whose tmux
	// session has been destroyed. A second non-ghost entry is included
	// so we can confirm only the targeted entry is removed.
	ghost := session.InstanceData{
		Title:   "ghost-session",
		Path:    tmp,
		Program: "claude",
		// Worktree fields intentionally empty.
	}
	keep := session.InstanceData{
		Title:   "keep-session",
		Path:    tmp,
		Program: "claude",
	}
	raw, err := json.Marshal([]session.InstanceData{ghost, keep})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(instancesPath, raw, 0644); err != nil {
		t.Fatalf("write instances: %v", err)
	}

	// Sanity: findLiveInstanceByTitle must fail for the ghost session,
	// otherwise the regression branch is not exercised.
	if _, _, err := findLiveInstanceByTitle("ghost-session"); err == nil {
		t.Fatalf("expected findLiveInstanceByTitle to fail for ghost session")
	}

	// Run the kill command. Suppress stdout/stderr noise from jsonOut/jsonError.
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer devnull.Close()
	origStdout, origStderr := os.Stdout, os.Stderr
	os.Stdout = devnull
	os.Stderr = devnull
	defer func() {
		os.Stdout = origStdout
		os.Stderr = origStderr
	}()

	if err := sessionsKillCmd.RunE(sessionsKillCmd, []string{"ghost-session"}); err != nil {
		t.Fatalf("sessionsKillCmd returned error for ghost session: %v", err)
	}

	// Verify the ghost entry is gone but the other entry remains.
	out, err := os.ReadFile(instancesPath)
	if err != nil {
		t.Fatalf("read back instances: %v", err)
	}
	var got []session.InstanceData
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("expected 1 remaining entry, got %d: %+v", len(got), got)
	}
	if got[0].Title != "keep-session" {
		t.Fatalf("wrong entry remained: %q", got[0].Title)
	}
}

// TestSessionsKill_UnknownTitle verifies that killing a non-existent session
// (no metadata on disk at all) still surfaces an error rather than silently
// succeeding.
func TestSessionsKill_UnknownTitle(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", tmp)

	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatalf("open devnull: %v", err)
	}
	defer devnull.Close()
	origStdout, origStderr := os.Stdout, os.Stderr
	os.Stdout = devnull
	os.Stderr = devnull
	defer func() {
		os.Stdout = origStdout
		os.Stderr = origStderr
	}()

	if err := sessionsKillCmd.RunE(sessionsKillCmd, []string{"does-not-exist"}); err == nil {
		t.Fatalf("expected error for unknown session, got nil")
	}
}
