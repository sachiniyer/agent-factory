package api

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// makeRecordsUnreadable turns a repo's instances.json into a genuine READ error
// — not a missing file, which the loader deliberately maps to "this repo has no
// sessions", and which would make every assertion below vacuous.
//
// chmod 0000 is the realistic shape, but modes inherit the ambient umask and
// root ignores them, so the mode is applied and then PROVEN, with a
// directory-in-place fallback (os.ReadFile refuses a directory for everyone).
func makeRecordsUnreadable(t *testing.T, repoID string) string {
	t.Helper()
	path, err := config.RepoInstancesPath(repoID)
	if err != nil {
		t.Fatalf("RepoInstancesPath(%s): %v", repoID, err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod %s: %v", path, err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if _, err := os.ReadFile(path); err != nil {
		if os.IsNotExist(err) {
			t.Fatalf("fixture must make %s UNREADABLE, not missing: %v", path, err)
		}
		return path
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove %s: %v", path, err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", path, err)
	}
	if _, err := os.ReadFile(path); err == nil {
		t.Fatalf("fixture did not take: %s is still readable", path)
	}
	return path
}

func seedRows(t *testing.T, repoID string, rows ...session.InstanceData) {
	t.Helper()
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := config.SaveRepoInstances(repoID, raw); err != nil {
		t.Fatalf("save %s: %v", repoID, err)
	}
}

// TestFindInstanceByTitle_NamesUnreadableRepoOnNotFound is the #3479 twin of
// TestFindInstanceByTitle_NamesCorruptedRepoOnNotFound. "Not found" is an
// absence claim, and a repo whose instances.json could not be READ is exactly
// the file that would have refuted it — LoadAllRepoInstances drops it silently,
// so the miss came back looking definitive.
func TestFindInstanceByTitle_NamesUnreadableRepoOnNotFound(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_ = captureWarnings(t)

	seedRows(t, "blocked-repo", session.InstanceData{Title: "hidden", Path: "/repos/blocked"})
	path := makeRecordsUnreadable(t, "blocked-repo")

	_, _, err := findInstanceByTitle("hidden")
	if err == nil {
		t.Fatalf("a title that may live behind an unreadable record must not resolve to a clean not-found")
	}
	if errors.Is(err, errTitleNotFound) {
		t.Fatalf("an under-read miss must not carry the definitive not-found sentinel: %v", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error must name the unreadable file %s so it can be repaired, got: %v", path, err)
	}
	if !strings.Contains(err.Error(), "is a directory") && !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error must carry the underlying I/O error, got: %v", err)
	}
}

// TestFindInstanceByTitle_PositiveLookupNotBlockedByUnreadableRepo keeps the
// existing corrupted-repo contract (a healthy repo's session stays findable)
// extended verbatim to the unreadable case. A PRESENCE is not weakened by an
// unread file; only an ABSENCE is.
func TestFindInstanceByTitle_PositiveLookupNotBlockedByUnreadableRepo(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_ = captureWarnings(t)

	seedRows(t, "valid-repo", session.InstanceData{Title: "findme", Path: "/repos/valid"})
	seedRows(t, "blocked-repo", session.InstanceData{Title: "other", Path: "/repos/blocked"})
	makeRecordsUnreadable(t, "blocked-repo")

	data, repoID, err := findInstanceByTitle("findme")
	if err != nil {
		t.Fatalf("a session in a healthy repo must stay findable while another record is unreadable: %v", err)
	}
	if data.Title != "findme" || repoID != "valid-repo" {
		t.Fatalf("unexpected lookup result: data=%+v repoID=%q", data, repoID)
	}
}

// TestAllScopedInstances_RefusesUnreadableRepo covers `send-prompt --all
// --all-repos`. Its caller already refuses to broadcast to a truncated set when
// a repo's JSON is corrupted, on the stated grounds that a hidden session which
// never receives the prompt is worse than an error. An unreadable file truncates
// the set exactly the same way, and was not covered.
func TestAllScopedInstances_RefusesUnreadableRepo(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_ = captureWarnings(t)

	seedRows(t, "valid-repo", session.InstanceData{Title: "a", Path: "/repos/valid"})
	seedRows(t, "blocked-repo", session.InstanceData{Title: "b", Path: "/repos/blocked"})
	path := makeRecordsUnreadable(t, "blocked-repo")

	got, _, err := allScopedInstances()
	if err == nil {
		t.Fatalf("broadcast targets must not be enumerated from a partial read (got %d targets)", len(got))
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("refusal must name the unreadable file %s, got: %v", path, err)
	}
	if !strings.Contains(err.Error(), "is a directory") && !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("refusal must carry the underlying I/O error, got: %v", err)
	}
}
