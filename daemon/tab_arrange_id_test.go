package daemon

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

// Stable-tab-id addressing for the in-place tab mutations (#1929). RenameTab and
// ReorderTab shipped keyed on TabName/TabIndex only — both REUSABLE handles — so
// a client that resolved a tab and then sent its name addressed whatever tab
// answered to that name by the time the daemon handled the request. These pin the
// three properties the id has to buy:
//
//   - a request carrying a TabID lands on THAT tab, even when the name it also
//     carries now belongs to a different tab;
//   - a TabID that no longer resolves is REFUSED, not quietly fallen back to the
//     name (#1779: falling back re-opens the exact race the id closes);
//   - a request with no TabID resolves by name exactly as it always did.
//
// They drive controlServer — the entry point BOTH transports dispatch to (the gob
// control socket via net/rpc, the HTTP route via rpcHandler) — rather than
// Manager.RenameTab directly, so the guard sequence that gates the mutation in
// production is in the path under test.

// arrangeTabIDByName returns the stable id of title's tab called name, read out of
// the SNAPSHOT — the same projection a real client resolves an id from before it
// sends a mutation.
func arrangeTabIDByName(t *testing.T, m *Manager, repoID, title, name string) string {
	t.Helper()
	data := snapshotTabs(t, m, repoID, title)
	for _, tab := range data.Tabs {
		if tab.Name == name {
			if tab.ID == "" {
				t.Fatalf("tab %q of session %q has no stable id", name, title)
			}
			return tab.ID
		}
	}
	t.Fatalf("session %q has no tab named %q (roster: %v)", title, name, arrangeTabNames(data))
	return ""
}

// tabNameByID returns the current name of the tab with stable id, or "" when the
// id no longer resolves.
func tabNameByID(data session.InstanceData, id string) string {
	for _, tab := range data.Tabs {
		if tab.ID == id {
			return tab.Name
		}
	}
	return ""
}

// arrangeWebTabs creates one web tab per name, in order, under title.
func arrangeWebTabs(t *testing.T, m *Manager, repoID, title string, names ...string) {
	t.Helper()
	for _, n := range names {
		if _, err := m.CreateTab(CreateTabRequest{
			Title: title, RepoID: repoID, Kind: "web", URL: "http://localhost:5173", Name: n,
		}); err != nil {
			t.Fatalf("CreateTab(%s): %v", n, err)
		}
	}
}

// reuseName is the race both headline tests stage, wound forward deterministically
// rather than raced: a client resolves the tab named "preview" (capturing its
// stable id), and before its mutation is handled the roster moves that NAME onto a
// DIFFERENT tab — "preview" is renamed away to "storefront", and "build" takes the
// freed name. Both tabs are still live, so a name-keyed resolve does not fail; it
// SUCCEEDS on the wrong tab, which is what makes the bug silent.
//
// Returns the id the client resolved (the tab now called "storefront") and the id
// of the name's new owner (the tab now called "preview") — the tab that must NOT
// be touched.
func reuseName(t *testing.T, m *Manager, repoID, title string) (resolved, newOwner string) {
	t.Helper()
	resolved = arrangeTabIDByName(t, m, repoID, title, "preview")
	newOwner = arrangeTabIDByName(t, m, repoID, title, "build")

	if _, err := m.RenameTab(RenameTabRequest{
		Title: title, RepoID: repoID, TabName: "preview", NewName: "storefront",
	}); err != nil {
		t.Fatalf("staging rename preview->storefront: %v", err)
	}
	if _, err := m.RenameTab(RenameTabRequest{
		Title: title, RepoID: repoID, TabName: "build", NewName: "preview",
	}); err != nil {
		t.Fatalf("staging rename build->preview: %v", err)
	}
	return resolved, newOwner
}

// TestRenameTab_ByStableIDIgnoresReusedName is the headline #1929 case: the name
// the client resolved now belongs to a different tab, and the rename must land on
// the tab whose id was sent — never on the name's new owner.
func TestRenameTab_ByStableIDIgnoresReusedName(t *testing.T) {
	const title = "renameid"
	manager, repo := tabEventSession(t, title)
	arrangeWebTabs(t, manager, repo.ID, title, "preview", "build")
	resolved, newOwner := reuseName(t, manager, repo.ID, title)

	// The client sends the id it resolved AND the stale name it resolved it from.
	// The id wins: the name is not cross-checked, because a name that changed
	// underneath the client is the normal case this field exists to survive.
	cs := &controlServer{manager: manager}
	var resp RenameTabResponse
	if err := cs.RenameTab(RenameTabRequest{
		Title: title, RepoID: repo.ID, TabName: "preview", TabID: resolved, NewName: "shop",
	}, &resp); err != nil {
		t.Fatalf("RenameTab by id: %v", err)
	}
	if resp.Name != "shop" {
		t.Fatalf("RenameTab returned %q, want shop", resp.Name)
	}

	snap := snapshotTabs(t, manager, repo.ID, title)
	if got := tabNameByID(snap, resolved); got != "shop" {
		t.Errorf("the addressed tab is named %q, want shop — the rename did not land on the tab whose id was sent", got)
	}
	if got := tabNameByID(snap, newOwner); got != "preview" {
		t.Errorf("the tab that inherited the name is now %q, want preview — the rename hit the WRONG tab (#1929)", got)
	}
}

// TestReorderTab_ByStableIDIgnoresReusedName: the reorder half of the headline
// case. Reorder is the verb that invalidates every other client's index, so it is
// the likeliest to be sent against a roster that already moved.
func TestReorderTab_ByStableIDIgnoresReusedName(t *testing.T) {
	const title = "reorderid"
	manager, repo := tabEventSession(t, title)
	arrangeWebTabs(t, manager, repo.ID, title, "preview", "build", "docs")
	resolved, newOwner := reuseName(t, manager, repo.ID, title)

	// Roster is now [agent, storefront(resolved), preview(newOwner), docs].
	// Moving the resolved tab to the last slot must move storefront, not preview.
	cs := &controlServer{manager: manager}
	var resp ReorderTabResponse
	if err := cs.ReorderTab(ReorderTabRequest{
		Title: title, RepoID: repo.ID, TabName: "preview", TabID: resolved, NewIndex: 3,
	}, &resp); err != nil {
		t.Fatalf("ReorderTab by id: %v", err)
	}
	if resp.Name != "storefront" {
		t.Fatalf("ReorderTab moved %q, want storefront — it resolved the reused NAME, not the id (#1929)", resp.Name)
	}

	snap := snapshotTabs(t, manager, repo.ID, title)
	want := []string{"agent", "preview", "docs", "storefront"}
	if got := arrangeTabNames(snap); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("roster is %v, want %v — the move landed on the wrong tab", got, want)
	}
	if got := tabNameByID(snap, newOwner); got != "preview" {
		t.Errorf("the name's new owner is %q, want preview (it must not have been moved)", got)
	}
}

// TestRenameTab_ByStableIDIgnoresShiftedIndex stages the OTHER reusable handle. The
// tests above move a NAME onto a different tab; this moves the INDEX, which is the
// reuse a reorder causes — and reorder invalidates every other client's ordinals at
// once, so this is the broader of the two exposures.
//
// The client resolves the tab at index 1, a concurrent reorder moves that tab to the
// end, and the client's request arrives carrying the stale index. TabName is left
// empty deliberately: resolveTabTarget's precedence is id, then name, then index, so
// a request that also carried a name would be resolved by the NAME and would never
// exercise the index at all.
func TestRenameTab_ByStableIDIgnoresShiftedIndex(t *testing.T) {
	const title = "renameshift"
	manager, repo := tabEventSession(t, title)
	arrangeWebTabs(t, manager, repo.ID, title, "preview", "build", "docs")
	resolved := arrangeTabIDByName(t, manager, repo.ID, title, "preview")

	// The concurrent reorder: [agent, preview, build, docs] -> [agent, build, docs,
	// preview]. The client's index 1 now names "build" — a live tab, so an
	// index-keyed resolve succeeds on it rather than failing.
	if _, _, err := manager.ReorderTab(ReorderTabRequest{
		Title: title, RepoID: repo.ID, TabName: "preview", NewIndex: 3,
	}); err != nil {
		t.Fatalf("staging reorder preview->3: %v", err)
	}

	cs := &controlServer{manager: manager}
	var resp RenameTabResponse
	if err := cs.RenameTab(RenameTabRequest{
		Title: title, RepoID: repo.ID, TabIndex: 1, TabID: resolved, NewName: "shop",
	}, &resp); err != nil {
		t.Fatalf("RenameTab by id: %v", err)
	}
	if resp.Name != "shop" {
		t.Fatalf("RenameTab returned %q, want shop", resp.Name)
	}

	snap := snapshotTabs(t, manager, repo.ID, title)
	if got := tabNameByID(snap, resolved); got != "shop" {
		t.Errorf("the addressed tab is named %q, want shop — the rename did not land on the tab whose id was sent", got)
	}
	want := []string{"agent", "build", "docs", "shop"}
	if got := arrangeTabNames(snap); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("roster is %v, want %v — the rename landed on whatever had shifted into the stale index (#1929)", got, want)
	}
}

// TestRenameTab_RefusesUnresolvableTabID: an id whose tab was CLOSED and whose
// name was then handed to a new tab. Refusing is the whole point — falling back to
// the name here would rename the new tab, which is the misroute the id exists to
// prevent (#1779). "Gone" is also the honest answer: the tab the client addressed
// really is not there.
func TestRenameTab_RefusesUnresolvableTabID(t *testing.T) {
	const title = "renamegone"
	manager, repo := tabEventSession(t, title)
	arrangeWebTabs(t, manager, repo.ID, title, "preview")
	resolved := arrangeTabIDByName(t, manager, repo.ID, title, "preview")

	// Close the tab and let a NEW tab take the freed name.
	if _, err := manager.CloseTab(CloseTabRequest{Title: title, RepoID: repo.ID, TabName: "preview"}); err != nil {
		t.Fatalf("CloseTab: %v", err)
	}
	arrangeWebTabs(t, manager, repo.ID, title, "preview")
	successor := arrangeTabIDByName(t, manager, repo.ID, title, "preview")
	if successor == resolved {
		t.Fatal("the successor tab reused the closed tab's stable id; ids must never be reused")
	}

	cs := &controlServer{manager: manager}
	var resp RenameTabResponse
	err := cs.RenameTab(RenameTabRequest{
		Title: title, RepoID: repo.ID, TabName: "preview", TabID: resolved, NewName: "shop",
	}, &resp)
	if err == nil {
		t.Fatalf("RenameTab with a dead tab id succeeded (returned %q) — it fell back to the reused name and renamed the WRONG tab (#1929)", resp.Name)
	}
	if !strings.Contains(err.Error(), resolved) {
		t.Errorf("error %q does not name the id that failed to resolve", err)
	}
	if got := tabNameByID(snapshotTabs(t, manager, repo.ID, title), successor); got != "preview" {
		t.Errorf("the successor tab is named %q, want preview — the refused rename still mutated it", got)
	}
}

// TestReorderTab_RefusesUnresolvableTabID: the reorder half of the refusal.
func TestReorderTab_RefusesUnresolvableTabID(t *testing.T) {
	const title = "reordergone"
	manager, repo := tabEventSession(t, title)
	arrangeWebTabs(t, manager, repo.ID, title, "preview", "build")
	resolved := arrangeTabIDByName(t, manager, repo.ID, title, "preview")

	if _, err := manager.CloseTab(CloseTabRequest{Title: title, RepoID: repo.ID, TabName: "preview"}); err != nil {
		t.Fatalf("CloseTab: %v", err)
	}
	arrangeWebTabs(t, manager, repo.ID, title, "preview")

	cs := &controlServer{manager: manager}
	var resp ReorderTabResponse
	err := cs.ReorderTab(ReorderTabRequest{
		Title: title, RepoID: repo.ID, TabName: "preview", TabID: resolved, NewIndex: 1,
	}, &resp)
	if err == nil {
		t.Fatalf("ReorderTab with a dead tab id succeeded (moved %q) — it fell back to the reused name (#1929)", resp.Name)
	}
	if !strings.Contains(err.Error(), resolved) {
		t.Errorf("error %q does not name the id that failed to resolve", err)
	}
	want := []string{"agent", "build", "preview"}
	if got := arrangeTabNames(snapshotTabs(t, manager, repo.ID, title)); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("roster is %v, want %v — the refused reorder still moved a tab", got, want)
	}
}

// TestRenameTab_WithoutTabIDResolvesByName pins the CLI/TUI path: no id supplied
// means the name is the only handle there is, and it must resolve exactly as it
// did before #1929. The id is additive, not a migration.
func TestRenameTab_WithoutTabIDResolvesByName(t *testing.T) {
	const title = "renamenoid"
	manager, repo := tabEventSession(t, title)
	arrangeWebTabs(t, manager, repo.ID, title, "preview")

	cs := &controlServer{manager: manager}
	var resp RenameTabResponse
	if err := cs.RenameTab(RenameTabRequest{
		Title: title, RepoID: repo.ID, TabName: "preview", NewName: "shop",
	}, &resp); err != nil {
		t.Fatalf("RenameTab by name: %v", err)
	}
	if resp.Name != "shop" {
		t.Fatalf("RenameTab returned %q, want shop", resp.Name)
	}
	if !tabNamed(snapshotTabs(t, manager, repo.ID, title), "shop") {
		t.Error("the name-keyed rename did not land")
	}
}

// TestReorderTab_WithoutTabIDResolvesByIndex pins the other legacy handle: with no
// id and no name, TabIndex still selects the tab.
func TestReorderTab_WithoutTabIDResolvesByIndex(t *testing.T) {
	const title = "reordernoid"
	manager, repo := tabEventSession(t, title)
	arrangeWebTabs(t, manager, repo.ID, title, "preview", "build")

	cs := &controlServer{manager: manager}
	var resp ReorderTabResponse
	if err := cs.ReorderTab(ReorderTabRequest{
		Title: title, RepoID: repo.ID, TabIndex: 1, NewIndex: 2,
	}, &resp); err != nil {
		t.Fatalf("ReorderTab by index: %v", err)
	}
	if resp.Name != "preview" {
		t.Fatalf("ReorderTab moved %q, want preview", resp.Name)
	}
	want := []string{"agent", "build", "preview"}
	if got := arrangeTabNames(snapshotTabs(t, manager, repo.ID, title)); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("roster is %v, want %v", got, want)
	}
}
