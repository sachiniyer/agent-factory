package daemon

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// RenameTab/ReorderTab daemon tests (#1813). They cover the same three
// properties the #1812 regression tests pin for CreateTab/CloseTab — the
// mutation lands, it is persisted (a fresh Snapshot agrees), and it is ANNOUNCED
// on the events plane — plus every guard the two new verbs add. The announce is
// the easiest to omit and the least visible when omitted: without it a second
// browser window renders the old name/order indefinitely, because a client only
// re-Snapshots after its OWN mutation.

// arrangeTabNames returns data's roster names in order — the projection a client
// rebuilds its tab bar from.
func arrangeTabNames(data session.InstanceData) []string {
	names := make([]string, 0, len(data.Tabs))
	for _, tab := range data.Tabs {
		names = append(names, tab.Name)
	}
	return names
}

// TestRenameTab_PublishesSessionUpdated: the headline rename case. The rename
// round-trips, the event carries the RELABELLED roster, and a fresh Snapshot
// agrees.
func TestRenameTab_PublishesSessionUpdated(t *testing.T) {
	const title = "renameevt"
	manager, repo := tabEventSession(t, title)

	if _, err := manager.CreateTab(CreateTabRequest{
		Title: title, RepoID: repo.ID, Kind: "web", URL: "http://localhost:5173", Name: "preview",
	}); err != nil {
		t.Fatalf("CreateTab(web): %v", err)
	}

	// Subscribe after the create so the rename's event is unambiguous.
	_, ch := manager.events.subscribe()
	name, err := manager.RenameTab(RenameTabRequest{Title: title, RepoID: repo.ID, TabName: "preview", NewName: "storefront"})
	if err != nil {
		t.Fatalf("RenameTab: %v", err)
	}
	if name != "storefront" {
		t.Fatalf("RenameTab returned %q, want storefront", name)
	}

	got := drainNextSessionEvent(t, ch, agentproto.EventSessionUpdated)
	if !tabNamed(got, "storefront") || tabNamed(got, "preview") {
		t.Fatalf("session.updated roster %v did not pick up the rename", arrangeTabNames(got))
	}
	snap := snapshotTabs(t, manager, repo.ID, title)
	if !tabNamed(snap, "storefront") || tabNamed(snap, "preview") {
		t.Fatalf("a fresh Snapshot %v did not pick up the rename", arrangeTabNames(snap))
	}
}

// TestRenameTab_ReturnsResolvedName: the requested name is sanitized and made
// unique, so the daemon must report what the tab is ACTUALLY called — clients
// render this, and every other tab verb addresses the tab by it.
func TestRenameTab_ReturnsResolvedName(t *testing.T) {
	const title = "resolvename"
	manager, repo := tabEventSession(t, title)

	for _, n := range []string{"dup", "victim"} {
		if _, err := manager.CreateTab(CreateTabRequest{
			Title: title, RepoID: repo.ID, Kind: "web", URL: "http://localhost:5173", Name: n,
		}); err != nil {
			t.Fatalf("CreateTab(%s): %v", n, err)
		}
	}

	// Renaming onto a taken name suffixes rather than producing two tabs with the
	// same name (which every verb addresses by name).
	name, err := manager.RenameTab(RenameTabRequest{Title: title, RepoID: repo.ID, TabName: "victim", NewName: "dup"})
	if err != nil {
		t.Fatalf("RenameTab: %v", err)
	}
	if name != "dup-2" {
		t.Fatalf("RenameTab returned %q, want dup-2", name)
	}

	// A name is also sanitized to the tmux-safe token set, exactly as create does.
	name, err = manager.RenameTab(RenameTabRequest{Title: title, RepoID: repo.ID, TabName: "dup-2", NewName: "my tab:9"})
	if err != nil {
		t.Fatalf("RenameTab(sanitize): %v", err)
	}
	if name != "my-tab-9" {
		t.Fatalf("RenameTab returned %q, want my-tab-9", name)
	}
}

// TestRenameTab_RejectsSanitizeToEmpty: #1813 calls silent mangling out as a
// wart, so a name with nothing usable in it is an error rather than a quiet fall
// back to the "web" default.
func TestRenameTab_RejectsSanitizeToEmpty(t *testing.T) {
	const title = "emptyname"
	manager, repo := tabEventSession(t, title)

	if _, err := manager.CreateTab(CreateTabRequest{
		Title: title, RepoID: repo.ID, Kind: "web", URL: "http://localhost:5173", Name: "preview",
	}); err != nil {
		t.Fatalf("CreateTab(web): %v", err)
	}

	_, err := manager.RenameTab(RenameTabRequest{Title: title, RepoID: repo.ID, TabName: "preview", NewName: "...."})
	if err == nil {
		t.Fatal("expected an error for a name that sanitizes to nothing, got nil")
	}
	if !strings.Contains(err.Error(), "no usable characters") {
		t.Fatalf("expected an actionable sanitize error, got: %v", err)
	}
	if !tabNamed(snapshotTabs(t, manager, repo.ID, title), "preview") {
		t.Fatal("a rejected rename must leave the tab's name intact")
	}
}

// TestRenameTab_RejectsUndisplayedKinds: agent and shell tabs render fixed
// labels on every surface (ui/tree/labels.go textForTab and its web mirror
// ignore Name for both), so renaming them would write a field nothing reads —
// a success the user cannot see. Refuse with an actionable message instead.
func TestRenameTab_RejectsUndisplayedKinds(t *testing.T) {
	const title = "kindguard"
	manager, repo := tabEventSession(t, title)

	if _, err := manager.CreateTab(CreateTabRequest{Title: title, RepoID: repo.ID, Shell: true}); err != nil {
		t.Fatalf("CreateTab(shell): %v", err)
	}

	// The agent tab, by index (it is unnamed on the wire for most clients).
	_, err := manager.RenameTab(RenameTabRequest{Title: title, RepoID: repo.ID, TabIndex: 0, NewName: "boss"})
	if err == nil || !strings.Contains(err.Error(), "agent tab") {
		t.Fatalf("expected an agent-tab rejection, got: %v", err)
	}

	// The shell tab, by name.
	_, err = manager.RenameTab(RenameTabRequest{Title: title, RepoID: repo.ID, TabName: "shell", NewName: "editor"})
	if err == nil || !strings.Contains(err.Error(), "shell tabs") {
		t.Fatalf("expected a shell-tab rejection, got: %v", err)
	}
	if !tabNamed(snapshotTabs(t, manager, repo.ID, title), "shell") {
		t.Fatal("a rejected rename must leave the tab's name intact")
	}
}

// TestReorderTab_PersistsAndPublishes: the headline reorder case. The move lands
// in the roster order, is persisted, and is announced.
func TestReorderTab_PersistsAndPublishes(t *testing.T) {
	const title = "reorderevt"
	manager, repo := tabEventSession(t, title)

	for _, n := range []string{"a", "b", "c"} {
		if _, err := manager.CreateTab(CreateTabRequest{
			Title: title, RepoID: repo.ID, Kind: "web", URL: "http://localhost:5173", Name: n,
		}); err != nil {
			t.Fatalf("CreateTab(%s): %v", n, err)
		}
	}

	_, ch := manager.events.subscribe()
	name, index, err := manager.ReorderTab(ReorderTabRequest{Title: title, RepoID: repo.ID, TabName: "a", NewIndex: 3})
	if err != nil {
		t.Fatalf("ReorderTab: %v", err)
	}
	if name != "a" || index != 3 {
		t.Fatalf("ReorderTab returned (%q, %d), want (a, 3)", name, index)
	}

	want := []string{"agent", "b", "c", "a"}
	got := drainNextSessionEvent(t, ch, agentproto.EventSessionUpdated)
	if diff := strings.Join(arrangeTabNames(got), ","); diff != strings.Join(want, ",") {
		t.Fatalf("session.updated roster = %v, want %v", arrangeTabNames(got), want)
	}
	snap := snapshotTabs(t, manager, repo.ID, title)
	if diff := strings.Join(arrangeTabNames(snap), ","); diff != strings.Join(want, ",") {
		t.Fatalf("a fresh Snapshot roster = %v, want %v", arrangeTabNames(snap), want)
	}
}

// TestReorderTab_PinsAgentSlot is the invariant gate. Tabs[0] IS the agent tab
// positionally to the session package (archive teardown keeps Tabs[0], the agent
// conversation and agent tmux session are read off it), so slot 0 is refused in
// BOTH directions: the agent tab can't move, and nothing may move in front of
// it. A roster that survives this is what the rest of the package assumes.
func TestReorderTab_PinsAgentSlot(t *testing.T) {
	const title = "pinagent"
	manager, repo := tabEventSession(t, title)

	for _, n := range []string{"a", "b"} {
		if _, err := manager.CreateTab(CreateTabRequest{
			Title: title, RepoID: repo.ID, Kind: "web", URL: "http://localhost:5173", Name: n,
		}); err != nil {
			t.Fatalf("CreateTab(%s): %v", n, err)
		}
	}

	// Moving the agent tab.
	if _, _, err := manager.ReorderTab(ReorderTabRequest{Title: title, RepoID: repo.ID, TabIndex: 0, NewIndex: 2}); err == nil {
		t.Fatal("expected the agent tab to be unmovable, got nil")
	} else if !strings.Contains(err.Error(), "agent tab") {
		t.Fatalf("expected an agent-tab rejection, got: %v", err)
	}

	// Moving another tab INTO slot 0 — the direction that would displace the agent
	// tab rather than move it.
	if _, _, err := manager.ReorderTab(ReorderTabRequest{Title: title, RepoID: repo.ID, TabName: "b", NewIndex: 0}); err == nil {
		t.Fatal("expected index 0 to be reserved, got nil")
	} else if !strings.Contains(err.Error(), "reserved for the agent tab") {
		t.Fatalf("expected a reserved-slot rejection, got: %v", err)
	}

	// Out of range.
	if _, _, err := manager.ReorderTab(ReorderTabRequest{Title: title, RepoID: repo.ID, TabName: "b", NewIndex: 7}); err == nil {
		t.Fatal("expected an out-of-range destination to be refused, got nil")
	}

	snap := snapshotTabs(t, manager, repo.ID, title)
	want := []string{"agent", "a", "b"}
	if strings.Join(arrangeTabNames(snap), ",") != strings.Join(want, ",") {
		t.Fatalf("roster after rejected moves = %v, want %v unchanged", arrangeTabNames(snap), want)
	}
	if snap.Tabs[0].Kind != session.TabKindAgent {
		t.Fatalf("slot 0 holds kind %v, want the agent tab", snap.Tabs[0].Kind)
	}
}

// TestArrangeTab_RejectsArchivedSession: archive is inert in BOTH directions
// (#1809). Archive PRESERVES web tabs so a restore can render them again, which
// makes an archived session the one that carries renameable tabs — and a rename
// or reorder against it would rewrite the record the restore is meant to bring
// back intact. Both verbs must refuse, actionably, without touching the record.
func TestArrangeTab_RejectsArchivedSession(t *testing.T) {
	const title = "archarrange"
	manager, repo := tabEventSession(t, title)

	if _, err := manager.CreateTab(CreateTabRequest{
		Title: title, RepoID: repo.ID, Kind: "web", URL: "http://localhost:5173", Name: "preview",
	}); err != nil {
		t.Fatalf("CreateTab(web): %v", err)
	}

	manager.mu.Lock()
	inst := manager.instances[daemonInstanceKey(repo.ID, title)]
	manager.mu.Unlock()
	inst.SetStatusForTest(session.Archived)

	_, err := manager.RenameTab(RenameTabRequest{Title: title, RepoID: repo.ID, TabName: "preview", NewName: "later"})
	if err == nil || !strings.Contains(err.Error(), "archived") {
		t.Fatalf("expected an archived rejection for rename, got: %v", err)
	}
	if !strings.Contains(err.Error(), "af sessions restore") {
		t.Fatalf("the archived rejection must name the way out, got: %v", err)
	}

	_, _, err = manager.ReorderTab(ReorderTabRequest{Title: title, RepoID: repo.ID, TabName: "preview", NewIndex: 1})
	if err == nil || !strings.Contains(err.Error(), "archived") {
		t.Fatalf("expected an archived rejection for reorder, got: %v", err)
	}
	if !tabNamed(snapshotTabs(t, manager, repo.ID, title), "preview") {
		t.Fatal("a rejected arrange must leave the archived record intact")
	}
}

// TestArrangeTab_RejectsRemoteInstance: a remote session's tabs come from its
// remote_hooks config, not from user edits, so neither verb applies — mirroring
// CreateTab/CloseTab and the TUI's rules.
func TestArrangeTab_RejectsRemoteInstance(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	const title = "rem"
	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: repoPath, Program: "claude"})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	inst.SetBackend(remoteTypeBackend{session.NewFakeBackend()})
	inst.SetStartedForTest(true)
	seedDiskInstance(t, repo.ID, title, repoPath)
	manager.mu.Lock()
	manager.instances[daemonInstanceKey(repo.ID, title)] = inst
	manager.mu.Unlock()

	if _, err := manager.RenameTab(RenameTabRequest{Title: title, RepoID: repo.ID, TabName: "shell", NewName: "editor"}); err == nil {
		t.Fatal("expected a remote rejection for rename, got nil")
	} else if !strings.Contains(err.Error(), "remote") {
		t.Fatalf("expected a remote-rejection error, got: %v", err)
	}

	if _, _, err := manager.ReorderTab(ReorderTabRequest{Title: title, RepoID: repo.ID, TabName: "shell", NewIndex: 1}); err == nil {
		t.Fatal("expected a remote rejection for reorder, got nil")
	} else if !strings.Contains(err.Error(), "remote") {
		t.Fatalf("expected a remote-rejection error, got: %v", err)
	}
}

// TestArrangeTab_RejectsUnknownTab: addressing a tab that isn't there is an
// error, not a silent success — the same rule tab-delete follows, and via the
// same shared resolver.
func TestArrangeTab_RejectsUnknownTab(t *testing.T) {
	const title = "unknowntab"
	manager, repo := tabEventSession(t, title)

	if _, err := manager.RenameTab(RenameTabRequest{Title: title, RepoID: repo.ID, TabName: "ghost", NewName: "x"}); err == nil {
		t.Fatal("expected an error for an unknown tab name, got nil")
	} else if !strings.Contains(err.Error(), "no tab named") {
		t.Fatalf("expected a no-such-tab error, got: %v", err)
	}

	if _, _, err := manager.ReorderTab(ReorderTabRequest{Title: title, RepoID: repo.ID, TabIndex: 9, NewIndex: 1}); err == nil {
		t.Fatal("expected an error for an out-of-range tab index, got nil")
	} else if !strings.Contains(err.Error(), "no tab at index") {
		t.Fatalf("expected a no-such-tab error, got: %v", err)
	}
}
