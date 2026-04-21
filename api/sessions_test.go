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
