package api

import (
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
