package daemon

import (
	"strings"
	"testing"
)

// Stable-tab-id addressing for CloseTab (#1971) — the third instance of the
// contract #1929 put on RenameTab/ReorderTab, and the one that matters most.
//
// Close is the DESTRUCTIVE tab verb: it kills the tab's tmux session. A wrong-tab
// rename or reorder is an annoyance the user can undo; a wrong-tab close takes
// whatever was running in that pane and is not undoable. So shipping rename and
// reorder id-keyed while close stayed name-keyed left the sharpest edge of the
// dual-identity model #1906 deleted exactly where it cut deepest.
//
// The reuse these pin is a property of the code, not a hypothetical: uniqueTabName
// (session/tab_names.go) hands a freed name straight back to the next tab that asks
// for it. So a client that resolves "the tab named preview" and sends that NAME is
// asking for "whatever is called preview when the daemon gets around to it" — and
// after a concurrent close+create that is a different, live tab.
//
// These drive controlServer.CloseTab — the entry point BOTH transports dispatch to
// (the gob control socket via net/rpc, the HTTP route via rpcHandler) — rather than
// resolveTabTarget underneath, so the guard sequence that gates the close in
// production is inside the path under test. A test that called the resolver directly
// would skip tabMutationTarget entirely and pass against a daemon that ignored the
// id.

// TestCloseTab_RefusesUnresolvableTabID is the headline #1971 case: the addressed
// tab is CLOSED, and uniqueTabName has already reissued its freed name to a NEW,
// live tab. The close must report the miss and leave the new tab alone.
//
// This is the case that makes name-keying unsafe rather than merely imprecise. A
// name-keyed resolve does not fail here — it SUCCEEDS, on a tab the client never
// looked at, and kills its tmux session. Refusing is both the safe answer and the
// honest one: the tab the client addressed really is gone.
func TestCloseTab_RefusesUnresolvableTabID(t *testing.T) {
	const title = "closegone"
	manager, repo := tabEventSession(t, title)
	arrangeWebTabs(t, manager, repo.ID, title, "preview", "build")
	resolved := arrangeTabIDByName(t, manager, repo.ID, title, "preview")

	// The concurrent close, wound forward deterministically rather than raced: it
	// frees the name "preview"…
	if _, err := manager.CloseTab(CloseTabRequest{Title: title, RepoID: repo.ID, TabName: "preview"}); err != nil {
		t.Fatalf("staging concurrent close: %v", err)
	}
	// …and uniqueTabName hands that exact name to the next tab that asks. If this
	// tab came back "preview-2" the hazard would not exist; assert the reissue, since
	// the whole case rests on it.
	arrangeWebTabs(t, manager, repo.ID, title, "preview")
	successor := arrangeTabIDByName(t, manager, repo.ID, title, "preview")
	if successor == resolved {
		t.Fatal("the successor tab reused the closed tab's stable id; ids must never be reused")
	}

	// The client sends the id it resolved and the name it resolved it from. The tab
	// is gone, so the id does not resolve — and falling back to the name would kill
	// the successor (#1779).
	cs := &controlServer{manager: manager}
	var resp CloseTabResponse
	err := cs.CloseTab(CloseTabRequest{
		Title: title, RepoID: repo.ID, TabName: "preview", TabID: resolved,
	}, &resp)
	if err == nil {
		t.Fatalf("CloseTab with a dead tab id succeeded (closed %q) — it fell back to the reused name and KILLED a live tab the client never addressed (#1971)", resp.Name)
	}
	if !strings.Contains(err.Error(), resolved) {
		t.Errorf("error %q does not name the id that failed to resolve", err)
	}

	// The survival assertion: the name's new owner must still be there, tmux session
	// and all.
	snap := snapshotTabs(t, manager, repo.ID, title)
	if got := tabNameByID(snap, successor); got != "preview" {
		t.Errorf("the successor tab is %q, want preview — the refused close destroyed the WRONG tab (#1971)", got)
	}
	want := []string{"agent", "build", "preview"}
	if got := arrangeTabNames(snap); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("roster is %v, want %v — the refused close still removed a tab", got, want)
	}
}

// TestCloseTab_ByStableIDIgnoresReusedName: the other half of the reuse hazard —
// the addressed tab is still LIVE, but its name has moved to a different tab. Here
// the close must succeed, on the tab whose id was sent.
//
// Refusing is not enough on its own: an id that resolves must still beat the stale
// name carried beside it, or the client's id would only ever turn silent misroutes
// into errors instead of actually addressing the right tab.
func TestCloseTab_ByStableIDIgnoresReusedName(t *testing.T) {
	const title = "closeid"
	manager, repo := tabEventSession(t, title)
	arrangeWebTabs(t, manager, repo.ID, title, "preview", "build")
	// reuseName renames preview->storefront and build->preview, so the name the client
	// resolved now belongs to a different, live tab. Both are live, so a name-keyed
	// resolve succeeds — on the wrong one.
	resolved, newOwner := reuseName(t, manager, repo.ID, title)

	cs := &controlServer{manager: manager}
	var resp CloseTabResponse
	if err := cs.CloseTab(CloseTabRequest{
		Title: title, RepoID: repo.ID, TabName: "preview", TabID: resolved,
	}, &resp); err != nil {
		t.Fatalf("CloseTab by id: %v", err)
	}
	if resp.Name != "storefront" {
		t.Fatalf("CloseTab closed %q, want storefront — it resolved the reused NAME, not the id (#1971)", resp.Name)
	}

	snap := snapshotTabs(t, manager, repo.ID, title)
	if tabNameByID(snap, resolved) != "" {
		t.Error("the addressed tab survived the close")
	}
	if got := tabNameByID(snap, newOwner); got != "preview" {
		t.Errorf("the tab that inherited the name is %q, want preview — the close KILLED the wrong tab (#1971)", got)
	}
}

// TestCloseTab_WithoutTabIDResolvesByName is a LOCK, not a repro: the CLI
// (api/sessions_tabs.go) and the TUI (app/session_control.go) send no tab id, so
// the name must remain the resolving handle when none is supplied. The id is
// additive — this change must not migrate the name-keyed path out from under them.
func TestCloseTab_WithoutTabIDResolvesByName(t *testing.T) {
	const title = "closenoid"
	manager, repo := tabEventSession(t, title)
	arrangeWebTabs(t, manager, repo.ID, title, "preview", "build")

	cs := &controlServer{manager: manager}
	var resp CloseTabResponse
	if err := cs.CloseTab(CloseTabRequest{
		Title: title, RepoID: repo.ID, TabName: "preview",
	}, &resp); err != nil {
		t.Fatalf("CloseTab by name: %v", err)
	}
	if resp.Name != "preview" {
		t.Fatalf("CloseTab closed %q, want preview", resp.Name)
	}
	want := []string{"agent", "build"}
	if got := arrangeTabNames(snapshotTabs(t, manager, repo.ID, title)); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("roster is %v, want %v", got, want)
	}
}

// TestCloseTab_WithoutTabIDResolvesByIndex is the second half of that lock: with no
// id and no name, TabIndex still selects the tab.
func TestCloseTab_WithoutTabIDResolvesByIndex(t *testing.T) {
	const title = "closenoidx"
	manager, repo := tabEventSession(t, title)
	arrangeWebTabs(t, manager, repo.ID, title, "preview", "build")

	cs := &controlServer{manager: manager}
	var resp CloseTabResponse
	if err := cs.CloseTab(CloseTabRequest{
		Title: title, RepoID: repo.ID, TabIndex: 2,
	}, &resp); err != nil {
		t.Fatalf("CloseTab by index: %v", err)
	}
	if resp.Name != "build" {
		t.Fatalf("CloseTab closed %q, want build", resp.Name)
	}
	want := []string{"agent", "preview"}
	if got := arrangeTabNames(snapshotTabs(t, manager, repo.ID, title)); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("roster is %v, want %v", got, want)
	}
}
