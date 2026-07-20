package daemon

import (
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// TestCreateTab_ReusesTheNameARenamedTabGaveUp is the #1957 repro at the RPC
// layer — the CLI sequence from the issue, verbatim:
//
//	af sessions tab-rename beta --name fresh --new-name fresh-old
//	af sessions tab-create beta --command … --name fresh   -> {"name":"fresh-2"}
//
// The roster held nothing named "fresh", and the user got "fresh-2" anyway,
// because the renamed tab's still-live tmux session kept the old NAME reserved.
// The reservation was real but charged to the wrong namespace: it is the SPAWN
// that must dodge the live session ("…__fresh-2"), not the name the user typed.
//
// This exercises the whole daemon path — resolve, spawn, persist, respond — and
// asserts the response reports BOTH names, since they now differ and the TUI's
// instant-display attach binds to the second (Instance.AttachShellTab).
func TestCreateTab_ReusesTheNameARenamedTabGaveUp(t *testing.T) {
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

	const title = "beta"
	agentName := "af_" + title + "_agent"
	startedLocalTabInstance(t, manager, repo.ID, repoPath, title, agentName)

	name, tmuxName, err := manager.CreateTab(CreateTabRequest{
		Title: title, RepoID: repo.ID, Command: "btop", Name: "fresh",
	})
	if err != nil {
		t.Fatalf("CreateTab(fresh): %v", err)
	}
	if name != "fresh" || tmuxName != agentName+"__fresh" {
		t.Fatalf("CreateTab(fresh) = %q/%q, want fresh/%s__fresh", name, tmuxName, agentName)
	}

	renamed, err := manager.RenameTab(RenameTabRequest{
		Title: title, RepoID: repo.ID, TabName: "fresh", NewName: "fresh-old",
	})
	if err != nil {
		t.Fatalf("RenameTab: %v", err)
	}
	if renamed != "fresh-old" {
		t.Fatalf("RenameTab = %q, want fresh-old", renamed)
	}

	// Nothing on the roster is called "fresh" now, so asking for it must get it.
	name, tmuxName, err = manager.CreateTab(CreateTabRequest{
		Title: title, RepoID: repo.ID, Command: "btop", Name: "fresh",
	})
	if err != nil {
		t.Fatalf("CreateTab(fresh, second): %v", err)
	}
	if name != "fresh" {
		t.Fatalf("CreateTab returned %q for a name no tab holds; want fresh (#1957)", name)
	}
	if tmuxName != agentName+"__fresh-2" {
		t.Fatalf("spawned tmux session = %q, want %s__fresh-2 — it must miss the renamed tab's live session",
			tmuxName, agentName)
	}

	// And the persisted roster agrees, so a restart rebinds each tab to its own
	// session rather than two tabs racing for one.
	snap := snapshotTabs(t, manager, repo.ID, title)
	names := arrangeTabNames(snap)
	want := []string{"agent", "fresh-old", "fresh"}
	if len(names) != len(want) {
		t.Fatalf("persisted roster %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("persisted roster %v, want %v", names, want)
		}
	}
	seen := map[string]string{}
	for _, td := range snap.Tabs {
		if td.TmuxName == "" {
			continue
		}
		if prev, dup := seen[td.TmuxName]; dup {
			t.Fatalf("tabs %q and %q share the tmux session %q", prev, td.Name, td.TmuxName)
		}
		seen[td.TmuxName] = td.Name
	}
}

// TestCreateTab_ReportsNoTmuxNameForATmuxlessKind: a web tab owns no PTY, so the
// response's tmux name is empty rather than a fabricated "<agent>__<name>" that
// names nothing. The TUI's attach path never runs for these kinds — they are
// materialized by the roster alone (see ReconcileTabsFromData) — and an invented
// session name would be a lie a later reader could act on.
func TestCreateTab_ReportsNoTmuxNameForATmuxlessKind(t *testing.T) {
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

	const title = "webby"
	startedLocalTabInstance(t, manager, repo.ID, repoPath, title, "af_"+title+"_agent")

	name, tmuxName, err := manager.CreateTab(CreateTabRequest{
		Title: title, RepoID: repo.ID, Kind: "web", URL: "http://localhost:5173", Name: "preview",
	})
	if err != nil {
		t.Fatalf("CreateTab(web): %v", err)
	}
	if name != "preview" {
		t.Fatalf("CreateTab(web) name = %q, want preview", name)
	}
	if tmuxName != "" {
		t.Fatalf("CreateTab(web) tmux name = %q, want empty — a web tab owns no session", tmuxName)
	}
	if !tabNamed(snapshotTabs(t, manager, repo.ID, title), "preview") {
		t.Fatal("the web tab was not persisted")
	}
}
