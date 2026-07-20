package daemon

import (
	"os"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// newVSCodeCreateFixture builds a manager with one started local instance and
// returns it with the repo id and title, for the create/close validation tests
// (which never spawn an editor).
func newVSCodeCreateFixture(t *testing.T) (m *Manager, repoID, title string) {
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
	const name = "vscodecreate"
	startedLocalTabInstance(t, manager, repo.ID, repoPath, name, "af_"+name+"_agent")
	return manager, repo.ID, name
}

// TestCreateTab_VSCodeKind: `--kind vscode` creates a VS Code tab with no target
// and no command, and does NOT require an editor to be installed — the pane
// renders an install hint later instead.
func TestCreateTab_VSCodeKind(t *testing.T) {
	manager, repoID, title := newVSCodeCreateFixture(t)

	name, _, err := manager.CreateTab(CreateTabRequest{Title: title, RepoID: repoID, Kind: "vscode"})
	if err != nil {
		t.Fatalf("CreateTab(vscode): %v", err)
	}
	if name != "vscode" {
		t.Fatalf("tab name = %q, want %q", name, "vscode")
	}

	inst := manager.instances[daemonInstanceKey(repoID, title)]
	tabs := inst.GetTabs()
	if len(tabs) != 2 {
		t.Fatalf("tab count = %d, want 2", len(tabs))
	}
	if tabs[1].Kind != session.TabKindVSCode {
		t.Fatalf("tab kind = %v, want TabKindVSCode", tabs[1].Kind)
	}
	if tabs[1].URL != "" {
		t.Fatalf("tab URL = %q, want empty: a vscode tab resolves its editor at proxy time", tabs[1].URL)
	}
}

// TestCreateTab_VSCodeRejectsTargets: a vscode tab always opens the session's own
// worktree, so a --url/--port/--command is meaningless. Reject it rather than
// silently ignoring it and leaving the caller believing it took effect.
func TestCreateTab_VSCodeRejectsTargets(t *testing.T) {
	manager, repoID, title := newVSCodeCreateFixture(t)

	for _, tc := range []struct {
		name string
		req  CreateTabRequest
		want string
	}{
		{"url", CreateTabRequest{Kind: "vscode", URL: "http://localhost:3000"}, "always opens the session's worktree"},
		{"port", CreateTabRequest{Kind: "vscode", Port: 3000}, "always opens the session's worktree"},
		{"command", CreateTabRequest{Kind: "vscode", Command: "vim"}, "not valid for a vscode tab"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			req.Title, req.RepoID = title, repoID
			_, _, err := manager.CreateTab(req)
			if err == nil {
				t.Fatalf("CreateTab(%+v) succeeded; want a rejection", tc.req)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// TestCreateTab_UnknownKindNamesTheVocabulary: the kind vocabulary is shared with
// the CLI (session.ParseTabKindName), so an unknown kind must report the real
// list rather than a hand-maintained string that can drift from it.
func TestCreateTab_UnknownKindNamesTheVocabulary(t *testing.T) {
	manager, repoID, title := newVSCodeCreateFixture(t)

	_, _, err := manager.CreateTab(CreateTabRequest{Title: title, RepoID: repoID, Kind: "emacs"})
	if err == nil {
		t.Fatal("CreateTab with an unknown kind succeeded; want a rejection")
	}
	for _, want := range []string{"emacs", "vscode", "web"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want one naming %q", err, want)
		}
	}
}

// TestCloseTab_StopsEditorOnlyWithTheLastVSCodeTab: the editor is per SESSION, so
// closing one of two vscode tabs must NOT tear down the editor the other is still
// showing; closing the last one must.
func TestCloseTab_StopsEditorOnlyWithTheLastVSCodeTab(t *testing.T) {
	manager, repoID, title := newVSCodeCreateFixture(t)
	key := daemonInstanceKey(repoID, title)
	inst := manager.instances[key]

	for _, n := range []string{"one", "two"} {
		if _, _, err := manager.CreateTab(CreateTabRequest{Title: title, RepoID: repoID, Kind: "vscode", Name: n}); err != nil {
			t.Fatalf("CreateTab(%s): %v", n, err)
		}
	}

	// Stand a marker in the supervisor's map for this session. stopFor deletes the
	// entry, so its presence/absence is a faithful probe of whether the close path
	// decided the editor was still needed — without spawning a real one.
	manager.vscode.servers[key] = &vscodeServer{worktree: "/nowhere", exited: make(chan struct{})}

	if _, err := manager.CloseTab(CloseTabRequest{Title: title, RepoID: repoID, TabName: "one"}); err != nil {
		t.Fatalf("CloseTab(one): %v", err)
	}
	if _, ok := manager.vscode.servers[key]; !ok {
		t.Fatal("closing one of two vscode tabs stopped the editor the other tab still needs")
	}

	if _, err := manager.CloseTab(CloseTabRequest{Title: title, RepoID: repoID, TabName: "two"}); err != nil {
		t.Fatalf("CloseTab(two): %v", err)
	}
	if _, ok := manager.vscode.servers[key]; ok {
		t.Fatal("closing the last vscode tab left its editor running")
	}
	if instanceHasVSCodeTab(inst) {
		t.Fatal("the instance still reports a vscode tab after both were closed")
	}
}

// TestCloseTab_ShellTabLeavesEditorAlone: closing an unrelated tab must not touch
// the editor.
func TestCloseTab_ShellTabLeavesEditorAlone(t *testing.T) {
	manager, repoID, title := newVSCodeCreateFixture(t)
	key := daemonInstanceKey(repoID, title)

	if _, _, err := manager.CreateTab(CreateTabRequest{Title: title, RepoID: repoID, Kind: "vscode"}); err != nil {
		t.Fatalf("CreateTab(vscode): %v", err)
	}
	if _, _, err := manager.CreateTab(CreateTabRequest{Title: title, RepoID: repoID, Shell: true}); err != nil {
		t.Fatalf("CreateTab(shell): %v", err)
	}
	manager.vscode.servers[key] = &vscodeServer{worktree: "/nowhere", exited: make(chan struct{})}

	if _, err := manager.CloseTab(CloseTabRequest{Title: title, RepoID: repoID, TabName: "shell"}); err != nil {
		t.Fatalf("CloseTab(shell): %v", err)
	}
	if _, ok := manager.vscode.servers[key]; !ok {
		t.Fatal("closing a shell tab stopped the session's VS Code editor")
	}
}

// TestCloseTab_StopsEditorEvenWhenPersistFails is the codex P2 fix. CloseTab
// removes the tab from the live instance BEFORE persisting, so a persist failure
// (disk full, permissions) leaves a session with no reachable vscode tab — and,
// without this, an editor running until daemon shutdown that nothing can ever
// reach again or clean up.
func TestCloseTab_StopsEditorEvenWhenPersistFails(t *testing.T) {
	manager, repoID, title := newVSCodeCreateFixture(t)
	key := daemonInstanceKey(repoID, title)

	if _, _, err := manager.CreateTab(CreateTabRequest{Title: title, RepoID: repoID, Kind: "vscode"}); err != nil {
		t.Fatalf("CreateTab(vscode): %v", err)
	}
	manager.vscode.servers[key] = &vscodeServer{worktree: "/nowhere", exited: make(chan struct{})}

	// Force the persist to fail. Corrupting the on-disk JSON (rather than
	// chmod-ing it read-only) is what makes this deterministic everywhere: the
	// container suite runs as root, which would sail straight through a permission
	// bit, and the test would then pass vacuously.
	path, err := config.RepoInstancesPath(repoID)
	if err != nil {
		t.Fatalf("RepoInstancesPath: %v", err)
	}
	if err := os.WriteFile(path, []byte("{ not json"), 0o600); err != nil {
		t.Fatalf("corrupting the instances file: %v", err)
	}

	_, err = manager.CloseTab(CloseTabRequest{Title: title, RepoID: repoID, TabName: "vscode"})
	if err == nil {
		t.Fatal("CloseTab succeeded despite a persist failure; the test's premise is wrong")
	}
	if _, ok := manager.vscode.servers[key]; ok {
		t.Fatal("the editor is still running after its last vscode tab was closed and the persist failed; nothing can reach or reap it until daemon shutdown")
	}
}
