package daemon

import (
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// Tab-level misses on the Preview RPC (#1948 review).
//
// `af sessions preview` gained --tab/--tab-id, and on the REMOTE path both
// answered dishonestly:
//
//   - an unresolvable --tab-id came back as a bare Gone, which the CLI printed as
//     "session %q is no longer running" — a lie about a perfectly healthy session
//   - an out-of-range --tab skipped resolution entirely and produced a SILENT
//     EMPTY capture with no error at all
//
// Both are the shape this cluster exists to remove: the daemon knew the answer
// and the client could not see it. Gone now carries TabGone, which says the
// SESSION is fine and only the addressed tab is missing — so the CLI reports the
// flag that was wrong instead of guessing from "I sent a selector", a guess that
// is wrong in exactly the case where the session really did die mid-capture.

// previewTestSession stands up a started local session with one extra process
// tab, so the roster is [agent, a] — ordinals 0 and 1 are real, 2+ are not.
func previewTestSession(t *testing.T) (*controlServer, string, string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	const title = "worker"
	inst := startedLocalTabInstance(t, manager, repo.ID, repoPath, title, "af_"+title+"_agent")
	if _, err := inst.AddProcessTab("a", ""); err != nil {
		t.Fatalf("AddProcessTab: %v", err)
	}
	if got := inst.TabCount(); got != 2 {
		t.Fatalf("premise: want a 2-tab roster, got %d", got)
	}
	return &controlServer{manager: manager}, title, repo.ID
}

// TestPreview_OutOfRangeOrdinalIsATabMiss is the silent-empty bug. Before this,
// a bare out-of-range --tab bypassed resolution (the guard was `TabID != "" ||
// TabName != ""`), reached the capture with a bad index, and returned "" with
// Gone=false and no error — so `af sessions preview x --tab 99` printed an empty
// capture and exited 0, which reads as "that tab is empty" rather than "there is
// no such tab".
func TestPreview_OutOfRangeOrdinalIsATabMiss(t *testing.T) {
	cs, title, repoID := previewTestSession(t)

	for _, ordinal := range []int{2, 99, -1} {
		var resp PreviewResponse
		if err := cs.Preview(PreviewRequest{Title: title, RepoID: repoID, Tab: ordinal}, &resp); err != nil {
			t.Fatalf("--tab %d must be a structured miss, not a transport error: %v", ordinal, err)
		}
		if !resp.Gone {
			t.Errorf("--tab %d on a 2-tab session returned Gone=false with content %q — a capture "+
				"verb answering \"\" to a slot that does not exist is indistinguishable from an "+
				"empty tab", ordinal, resp.Content)
		}
		if !resp.TabGone {
			t.Errorf("--tab %d reported Gone without TabGone, so the CLI cannot tell it from a dead "+
				"session and prints \"session is no longer running\" about a healthy one", ordinal)
		}
		if resp.Content != "" {
			t.Errorf("--tab %d captured %q; a miss must capture nothing", ordinal, resp.Content)
		}
	}
}

// TestPreview_UnresolvableTabIDIsATabMiss pins the other half: the id case
// already answered Gone (#1779), but said nothing about WHICH thing was gone.
func TestPreview_UnresolvableTabIDIsATabMiss(t *testing.T) {
	cs, title, repoID := previewTestSession(t)

	var resp PreviewResponse
	if err := cs.Preview(PreviewRequest{Title: title, RepoID: repoID, TabID: "never-minted"}, &resp); err != nil {
		t.Fatalf("an unknown tab id must be a structured miss: %v", err)
	}
	if !resp.Gone {
		t.Fatal("an unknown tab id must report gone rather than capture tab 0")
	}
	if !resp.TabGone {
		t.Error("an unknown tab id reported Gone without TabGone — the session is alive and the CLI " +
			"would tell the user it had ended because they mistyped --tab-id")
	}
}

// TestPreview_InRangeOrdinalIsUnchanged is the no-regression guard on the path
// that was already correct. Routing the bare ordinal through ResolveTabIndex must
// bounds-check it WITHOUT altering a legacy positional capture: same content,
// and emphatically not a miss.
func TestPreview_InRangeOrdinalIsUnchanged(t *testing.T) {
	cs, title, repoID := previewTestSession(t)

	for _, ordinal := range []int{0, 1} {
		var resp PreviewResponse
		if err := cs.Preview(PreviewRequest{Title: title, RepoID: repoID, Tab: ordinal}, &resp); err != nil {
			t.Fatalf("in-range --tab %d must still capture: %v", ordinal, err)
		}
		if resp.Gone || resp.TabGone {
			t.Errorf("in-range --tab %d reported gone=%v tabGone=%v; the pre-existing positional "+
				"path must be untouched", ordinal, resp.Gone, resp.TabGone)
		}
	}
}

// TestPreview_TabGoneNarrowsRatherThanReplacesGone pins the compatibility rule
// the TUI depends on: TabGone never appears without Gone. The TUI reads only
// Gone and degrades to its session-gone fallback; if a tab miss set TabGone
// alone, the TUI would render a stale pane instead.
func TestPreview_TabGoneNarrowsRatherThanReplacesGone(t *testing.T) {
	cs, title, repoID := previewTestSession(t)

	for _, req := range []PreviewRequest{
		{Title: title, RepoID: repoID, Tab: 99},
		{Title: title, RepoID: repoID, TabID: "never-minted"},
	} {
		var resp PreviewResponse
		if err := cs.Preview(req, &resp); err != nil {
			t.Fatalf("Preview(%+v): %v", req, err)
		}
		if resp.TabGone && !resp.Gone {
			t.Errorf("Preview(%+v) set TabGone without Gone; a client that only knows Gone (the TUI) "+
				"would keep rendering a tab that is not there", req)
		}
	}
}
