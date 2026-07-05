package api

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// captureWarnings redirects WarningLog output into a buffer for the duration
// of a test so assertions can confirm corrupted/missing storage is surfaced
// loudly rather than dropped silently (#730). It points the logger straight at
// the buffer (rather than teeing through the prior writer) because sibling
// tests can leave WarningLog attached to a since-closed fd — io.MultiWriter
// aborts on the first writer's error, which would swallow the captured output.
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := log.WarningLog.Writer()
	log.WarningLog.SetOutput(&buf)
	t.Cleanup(func() { log.WarningLog.SetOutput(prev) })
	return &buf
}

// TestFindInstanceByTitle_NamesCorruptedRepoOnNotFound covers #730 for the
// title-lookup path: when the title is absent and a repo is corrupted, the
// returned error must name the corrupted repo (so the user knows the session
// may be hidden behind a bad file) rather than a bare "not found."
func TestFindInstanceByTitle_NamesCorruptedRepoOnNotFound(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	warnBuf := captureWarnings(t)

	corruptedRepoID := "corrupted-repo"
	if err := config.SaveRepoInstances(corruptedRepoID, json.RawMessage("{not valid json")); err != nil {
		t.Fatalf("save corrupted repo: %v", err)
	}

	_, _, err := findInstanceByTitle("ghost-title")
	if err == nil {
		t.Fatalf("expected error when title missing and a repo is corrupted")
	}
	if !strings.Contains(err.Error(), corruptedRepoID) {
		t.Fatalf("expected error to name corrupted repo %q; got: %v", corruptedRepoID, err)
	}
	if !strings.Contains(warnBuf.String(), corruptedRepoID) {
		t.Fatalf("expected warning naming corrupted repo %q; got: %q", corruptedRepoID, warnBuf.String())
	}
}

// TestInstanceTitleExistsInScope_AllRepoSurfacesCorruption is the regression
// test for #861: in all-repo mode the send-prompt pre-check must propagate the
// corruption-aware error from findInstanceByTitle (naming the bad repo) instead
// of collapsing every miss to (false, nil) and letting the caller emit a bare
// "not found." Otherwise users never learn a session may be hidden behind a
// corrupted instances.json.
func TestInstanceTitleExistsInScope_AllRepoSurfacesCorruption(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_ = captureWarnings(t)

	corruptedRepoID := "corrupted-repo"
	if err := config.SaveRepoInstances(corruptedRepoID, json.RawMessage("{not valid json")); err != nil {
		t.Fatalf("save corrupted repo: %v", err)
	}

	exists, err := instanceTitleExistsInScope("", "ghost-title")
	if err == nil {
		t.Fatalf("expected corruption error in all-repo mode, got nil")
	}
	if exists {
		t.Fatalf("expected exists=false when the title is missing")
	}
	if !strings.Contains(err.Error(), corruptedRepoID) {
		t.Fatalf("expected error to name corrupted repo %q; got: %v", corruptedRepoID, err)
	}
}

// TestInstanceTitleExistsInScope_AllRepoCleanNotFound verifies that a clean
// miss (no corruption anywhere) still reports (false, nil) so the send-prompt
// caller keeps driving the --create / friendly "not found" branch (#861).
func TestInstanceTitleExistsInScope_AllRepoCleanNotFound(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_ = captureWarnings(t)

	validJSON, err := json.Marshal([]session.InstanceData{{Title: "other-session"}})
	if err != nil {
		t.Fatalf("marshal valid: %v", err)
	}
	if err := config.SaveRepoInstances("valid-repo", validJSON); err != nil {
		t.Fatalf("save valid repo: %v", err)
	}

	exists, err := instanceTitleExistsInScope("", "ghost-title")
	if err != nil {
		t.Fatalf("clean not-found must not error in all-repo mode; got: %v", err)
	}
	if exists {
		t.Fatalf("expected exists=false for a missing title")
	}
}

// TestInstanceTitleExistsInScope_ScopedUnaffectedByCorruption verifies that
// --repo scoped mode (non-empty repoID) keeps checking only that repo: a clean
// repo reports presence/absence without being tainted by corruption elsewhere,
// preserving the #776 scoping behavior (#861).
func TestInstanceTitleExistsInScope_ScopedUnaffectedByCorruption(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_ = captureWarnings(t)

	if err := config.SaveRepoInstances("corrupted-repo", json.RawMessage("{not valid json")); err != nil {
		t.Fatalf("save corrupted repo: %v", err)
	}
	validJSON, err := json.Marshal([]session.InstanceData{{Title: "scoped-session"}})
	if err != nil {
		t.Fatalf("marshal valid: %v", err)
	}
	if err := config.SaveRepoInstances("clean-repo", validJSON); err != nil {
		t.Fatalf("save clean repo: %v", err)
	}

	exists, err := instanceTitleExistsInScope("clean-repo", "scoped-session")
	if err != nil {
		t.Fatalf("scoped lookup of a clean repo must not error: %v", err)
	}
	if !exists {
		t.Fatalf("expected the scoped title to be found")
	}

	exists, err = instanceTitleExistsInScope("clean-repo", "ghost-title")
	if err != nil {
		t.Fatalf("scoped miss in a clean repo must not error: %v", err)
	}
	if exists {
		t.Fatalf("expected a missing scoped title to report absent")
	}
}

// TestFindInstanceByTitle_PositiveLookupNotBlockedByCorruption verifies that a
// corrupted repo does not prevent a successful lookup of a title that lives in
// a healthy repo — corruption is warned about, not fatal to findable sessions.
func TestFindInstanceByTitle_PositiveLookupNotBlockedByCorruption(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_ = captureWarnings(t)

	if err := config.SaveRepoInstances("corrupted-repo", json.RawMessage("{not valid json")); err != nil {
		t.Fatalf("save corrupted repo: %v", err)
	}
	validJSON, err := json.Marshal([]session.InstanceData{{Title: "findme"}})
	if err != nil {
		t.Fatalf("marshal valid: %v", err)
	}
	if err := config.SaveRepoInstances("valid-repo", validJSON); err != nil {
		t.Fatalf("save valid repo: %v", err)
	}

	data, repoID, err := findInstanceByTitle("findme")
	if err != nil {
		t.Fatalf("expected to find session in healthy repo despite corruption elsewhere: %v", err)
	}
	if data.Title != "findme" || repoID != "valid-repo" {
		t.Fatalf("unexpected lookup result: data=%+v repoID=%q", data, repoID)
	}
}
