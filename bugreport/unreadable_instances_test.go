package bugreport

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// TestCollectInstancesRecordsUnreadableRepo pins the one thing a diagnostic
// bundle must never do: look like a complete picture of the machine while
// silently omitting a project.
//
// config.LoadAllRepoInstances drops repos it cannot read, so a bundle built on
// it listed the readable ones and said nothing about the rest — and the person
// reading the bug report has no way to tell a machine with two projects from one
// with three (#3479). Recorded in Errors rather than made fatal: a partial
// bundle still beats no bundle when someone is filing a bug.
func TestCollectInstancesRecordsUnreadableRepo(t *testing.T) {
	home := t.TempDir()
	afHome := filepath.Join(home, ".agent-factory")
	t.Setenv("HOME", home)
	t.Setenv("AGENT_FACTORY_HOME", afHome)

	rows, err := json.Marshal([]session.InstanceData{{Title: "visible", Path: "/repos/visible"}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := config.SaveRepoInstances("visible-repo", rows); err != nil {
		t.Fatalf("save visible: %v", err)
	}
	if err := config.SaveRepoInstances("blocked-repo", rows); err != nil {
		t.Fatalf("save blocked: %v", err)
	}

	// A genuine READ error, not a missing file — the loader maps a missing file
	// to "[]", which would make this test vacuous. chmod is applied and then
	// PROVEN, with a directory-in-place fallback for root and umask-ignoring
	// filesystems.
	path, err := config.RepoInstancesPath("blocked-repo")
	if err != nil {
		t.Fatalf("RepoInstancesPath: %v", err)
	}
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	if _, err := os.ReadFile(path); err == nil {
		if err := os.Remove(path); err != nil {
			t.Fatalf("remove: %v", err)
		}
		if err := os.Mkdir(path, 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
	}
	readErr := func() error { _, e := os.ReadFile(path); return e }()
	if readErr == nil {
		t.Fatalf("fixture did not take: %s is still readable", path)
	}
	if os.IsNotExist(readErr) {
		t.Fatalf("fixture must produce a READ error, not a missing file: %v", readErr)
	}

	got, errs := collectInstances(newRedactor(), nil)

	var haveVisible bool
	for _, ri := range got {
		if ri.RepoID == "visible-repo" {
			haveVisible = true
		}
		if ri.RepoID == "blocked-repo" {
			t.Errorf("an unreadable repo must not appear as collected data")
		}
	}
	if !haveVisible {
		t.Errorf("the readable repo must still be collected; a gap is reported alongside the data, not instead of it")
	}
	joined := strings.Join(errs, "\n")
	if !strings.Contains(joined, path) {
		t.Errorf("the bundle must record the file it could not read; errors were: %q", joined)
	}
	if !strings.Contains(joined, "omits") {
		t.Errorf("the record must say the bundle is incomplete, not just that a read failed; errors were: %q", joined)
	}
}
