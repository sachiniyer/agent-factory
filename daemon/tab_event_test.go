package daemon

import (
	"testing"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// The #1812 regression tests: a tab-set mutation is a state change like any
// other, so CreateTab/CloseTab must announce it on the events plane. Before the
// fix both persisted the roster silently, so a tab created by an agent, the CLI,
// the TUI, or a second browser window never reached an already-open web client —
// the session's tab bar stayed stale until an *unrelated* session.updated
// happened to repair it as a side-effect (or the user reloaded). On a quiet
// session no such event ever arrives.
//
// The refreshed InstanceData rides on session.updated rather than a new tab.*
// type: InstanceData already carries the full Tabs roster and every client
// (web's upsertSession, the TUI's ReconcileTabsFromData) already re-projects the
// whole session from it, so the existing event both fixes the bug and needs no
// client change. This mirrors limit.go's choke-point publish.

// tabEventSession builds a manager holding one started, tab-capable local
// session and returns the manager plus its repo context.
func tabEventSession(t *testing.T, title string) (*Manager, *config.RepoContext) {
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
	startedLocalTabInstance(t, manager, repo.ID, repoPath, title, "af_"+title+"_agent")
	return manager, repo
}

// tabNamed reports whether data's roster contains a tab called name.
func tabNamed(data session.InstanceData, name string) bool {
	for _, tab := range data.Tabs {
		if tab.Name == name {
			return true
		}
	}
	return false
}

// snapshotTabs returns the roster the daemon would hand a re-Snapshotting client
// for title — the projection a web client rebuilds from after an event.
func snapshotTabs(t *testing.T, m *Manager, repoID, title string) session.InstanceData {
	t.Helper()
	for _, data := range m.Snapshot(repoID) {
		if data.Title == title {
			return data
		}
	}
	t.Fatalf("session %q missing from snapshot", title)
	return session.InstanceData{}
}

// TestCreateTab_WebPublishesSessionUpdated: the headline #1812 case — an agent
// injecting a live browser view (`tab-create --kind web`) into the user's screen.
// A subscriber must see the tab on the events plane, and a fresh Snapshot must
// agree.
func TestCreateTab_WebPublishesSessionUpdated(t *testing.T) {
	const title = "webevt"
	manager, repo := tabEventSession(t, title)

	_, ch := manager.events.subscribe()
	name, err := manager.CreateTab(CreateTabRequest{
		Title: title, RepoID: repo.ID, Kind: "web", URL: "http://localhost:5173", Name: "livepreview",
	})
	if err != nil {
		t.Fatalf("CreateTab(web): %v", err)
	}

	got := drainNextSessionEvent(t, ch, agentproto.EventSessionUpdated)
	if got.Title != title {
		t.Fatalf("event Title = %q, want %q", got.Title, title)
	}
	if !tabNamed(got, name) {
		t.Fatalf("session.updated roster %v is missing the new web tab %q", got.Tabs, name)
	}
	if !tabNamed(snapshotTabs(t, manager, repo.ID, title), name) {
		t.Fatalf("a fresh Snapshot is missing the new web tab %q", name)
	}
}

// TestCreateTab_ProcessPublishesSessionUpdated: the process-kind counterpart —
// a PTY-backed tab (`tab-create --command`) must announce itself too.
func TestCreateTab_ProcessPublishesSessionUpdated(t *testing.T) {
	const title = "procevt"
	manager, repo := tabEventSession(t, title)

	_, ch := manager.events.subscribe()
	name, err := manager.CreateTab(CreateTabRequest{
		Title: title, RepoID: repo.ID, Command: "sleep 600", Name: "worker",
	})
	if err != nil {
		t.Fatalf("CreateTab(process): %v", err)
	}

	got := drainNextSessionEvent(t, ch, agentproto.EventSessionUpdated)
	if !tabNamed(got, name) {
		t.Fatalf("session.updated roster %v is missing the new process tab %q", got.Tabs, name)
	}
	if !tabNamed(snapshotTabs(t, manager, repo.ID, title), name) {
		t.Fatalf("a fresh Snapshot is missing the new process tab %q", name)
	}
}

// TestCloseTab_PublishesSessionUpdated: the delete side. A tab closed
// out-of-band must vanish from every open client, so the event carries the
// SHRUNK roster (the tab absent), and the agent tab survives.
func TestCloseTab_PublishesSessionUpdated(t *testing.T) {
	const title = "closeevt"
	manager, repo := tabEventSession(t, title)

	name, err := manager.CreateTab(CreateTabRequest{
		Title: title, RepoID: repo.ID, Kind: "web", URL: "http://localhost:5173", Name: "doomed",
	})
	if err != nil {
		t.Fatalf("CreateTab(web): %v", err)
	}

	// Subscribe only after the create so the close's event is unambiguous.
	_, ch := manager.events.subscribe()
	closed, err := manager.CloseTab(CloseTabRequest{Title: title, RepoID: repo.ID, TabName: name})
	if err != nil {
		t.Fatalf("CloseTab: %v", err)
	}
	if closed != name {
		t.Fatalf("CloseTab returned %q, want %q", closed, name)
	}

	got := drainNextSessionEvent(t, ch, agentproto.EventSessionUpdated)
	if tabNamed(got, name) {
		t.Fatalf("session.updated roster %v still contains the closed tab %q", got.Tabs, name)
	}
	if len(got.Tabs) != 1 {
		t.Fatalf("roster after close = %v, want just the agent tab", got.Tabs)
	}
	if tabNamed(snapshotTabs(t, manager, repo.ID, title), name) {
		t.Fatalf("a fresh Snapshot still contains the closed tab %q", name)
	}
}
