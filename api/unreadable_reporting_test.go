package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// TestListSessions_DiskFallbackUnreadableLoud extends #730's loud corrupt-file
// contract to the class it never covered. `sessions list` deliberately refuses
// rather than render a short list, so a caller can tell "no sessions" apart from
// "sessions hidden behind a file I could not open" — and an unreadable file
// hides them exactly as a corrupted one does.
func TestListSessions_DiskFallbackUnreadableLoud(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_ = captureWarnings(t)
	stubSnapshot(t, daemonUnavailable)

	seedRows(t, "visible-repo", session.InstanceData{Title: "a", Path: "/repos/visible"})
	seedRows(t, "blocked-repo", session.InstanceData{Title: "b", Path: "/repos/blocked"})
	path := makeRecordsUnreadable(t, "blocked-repo")

	got, err := listSessions("")
	if err == nil {
		t.Fatalf("a partial list must not be rendered as complete (got %d sessions)", len(got))
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error must name the unreadable file %s, got: %v", path, err)
	}
	if !strings.Contains(err.Error(), "is a directory") && !strings.Contains(err.Error(), "permission denied") {
		t.Errorf("error must carry the underlying I/O error, got: %v", err)
	}
}

// TestDiskWhoami_NamesUnreadableRepoOnNotFound mirrors the corrupted-repo
// caveat this path already appends. "No session for this tmux session" is an
// absence claim, and the file that could not be read is what would refute it.
func TestDiskWhoami_NamesUnreadableRepoOnNotFound(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_ = captureWarnings(t)

	seedRows(t, "blocked-repo", session.InstanceData{Title: "hidden", TmuxName: "af_x_hidden"})
	path := makeRecordsUnreadable(t, "blocked-repo")

	_, err := diskWhoami("af_x_hidden")
	if err == nil {
		t.Fatalf("whoami must not report a clean not-found while a record is unreadable")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("the not-found must name the unreadable file %s, got: %v", path, err)
	}
}

// TestGetSessionByTitle_ReportsIncompleteWideningButStillResolves is the
// report-don't-refuse case, and the boundary against PR 1.
//
// The daemon-side twin of this widening (findSession) REFUSES, because the
// destructive paths resolve through it. This one backstops a read — `sessions
// get` — where breaking a working lookup costs more than the wrong project
// name it prevents, so it resolves and says the check was incomplete. Same rule,
// different consequence.
func TestGetSessionByTitle_ReportsIncompleteWideningButStillResolves(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_ = captureWarnings(t)
	warn := captureWarnWriter(t)

	seedRows(t, "blocked-repo", session.InstanceData{Title: "foo", Path: "/repos/blocked"})
	path := makeRecordsUnreadable(t, "blocked-repo")

	live := []session.InstanceData{{Title: "foo", Path: "/repos/live"}}
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) { return live, nil })

	got, err := getSessionByTitle("foo")
	if err != nil {
		t.Fatalf("a read must not break because its ambiguity widening was incomplete: %v", err)
	}
	if got == nil || got.Path != "/repos/live" {
		t.Fatalf("expected the live match to resolve, got: %+v", got)
	}
	if !strings.Contains(warn.String(), path) {
		t.Errorf("an incomplete widening must be reported, naming the file; stderr was: %q", warn.String())
	}
	if !strings.Contains(warn.String(), "--repo") {
		t.Errorf("the notice should point at the scope that would settle it; stderr was: %q", warn.String())
	}
}

// resetIncompleteNotices clears the once-per-invocation dedupe so each test
// starts from a clean slate; the production set is process-lifetime by design.
func resetIncompleteNotices(t *testing.T) {
	t.Helper()
	notedIncompleteAnswers.Range(func(k, _ any) bool {
		notedIncompleteAnswers.Delete(k)
		return true
	})
	t.Cleanup(func() {
		notedIncompleteAnswers.Range(func(k, _ any) bool {
			notedIncompleteAnswers.Delete(k)
			return true
		})
	})
}

// TestIncompleteWideningNoticeIsSaidOnce guards a long-running `sessions watch`.
// An unscoped watch re-resolves the title every poll — roughly 900 times at the
// default 30-minute timeout — and a caveat printed each pass buries whatever
// else the command has to say.
func TestIncompleteWideningNoticeIsSaidOnce(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_ = captureWarnings(t)
	warn := captureWarnWriter(t)
	resetIncompleteNotices(t)

	seedRows(t, "blocked-repo", session.InstanceData{Title: "foo", Path: "/repos/blocked"})
	path := makeRecordsUnreadable(t, "blocked-repo")

	live := []session.InstanceData{{Title: "foo", Path: "/repos/live"}}
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) { return live, nil })

	for i := 0; i < 25; i++ {
		if _, err := getSessionByTitle("foo"); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}
	// Count the NOTICE, not the path: the path appears twice within one message
	// (once from the skip's own rendering, once inside the wrapped I/O error).
	if got := strings.Count(warn.String(), "warning: could not check every project"); got != 1 {
		t.Fatalf("the caveat must be said once per invocation, not once per poll; said it %d times", got)
	}
	if !strings.Contains(warn.String(), path) {
		t.Errorf("the one notice must still name the unreadable file; stderr was: %q", warn.String())
	}
}

// TestGetSessionByTitle_ReportsEnumerationFailure closes the wider of the two
// holes. When the instances directory itself cannot be read the loader returns
// an error rather than per-repo gaps, and nothing was checked at all — so
// dropping that while reporting the narrower per-repo case would be backwards.
func TestGetSessionByTitle_ReportsEnumerationFailure(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)
	_ = captureWarnings(t)
	warn := captureWarnWriter(t)
	resetIncompleteNotices(t)

	// Make the instances DIRECTORY itself unreadable, so enumeration fails
	// rather than any individual repo. Proven, not assumed.
	seedRows(t, "some-repo", session.InstanceData{Title: "foo", Path: "/repos/x"})
	dir := filepath.Join(home, "instances")
	if err := os.Chmod(dir, 0o000); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	if _, err := os.ReadDir(dir); err == nil {
		t.Skip("cannot make the instances directory unreadable in this environment (running as root?)")
	}

	live := []session.InstanceData{{Title: "foo", Path: "/repos/live"}}
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) { return live, nil })

	got, err := getSessionByTitle("foo")
	if err != nil {
		t.Fatalf("a read must not break because its widening could not run: %v", err)
	}
	if got == nil || got.Path != "/repos/live" {
		t.Fatalf("expected the live match to resolve, got: %+v", got)
	}
	if !strings.Contains(warn.String(), "could not check any project") {
		t.Errorf("a total enumeration failure must be reported, not silently dropped; stderr was: %q", warn.String())
	}
}
