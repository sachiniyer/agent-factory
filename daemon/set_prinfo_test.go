package daemon

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// TestSetPRInfo_SetsAndPersists verifies SetPRInfo records the PR info on the
// live instance and persists it so it round-trips through a reload.
func TestSetPRInfo_SetsAndPersists(t *testing.T) {
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

	want := session.PRInfoData{Number: 42, Title: "feat: thing", URL: "https://example/pr/42", State: "OPEN"}
	if err := manager.SetPRInfo(SetPRInfoRequest{Title: title, RepoID: repo.ID, PRInfo: want}); err != nil {
		t.Fatalf("SetPRInfo: %v", err)
	}

	if got := inst.GetPRInfo(); got == nil || got.Number != want.Number || got.URL != want.URL || got.State != want.State {
		t.Fatalf("in-memory PR info = %+v, want %+v", got, want)
	}

	// Round-trip through disk: the persisted record carries the PR info so it
	// survives a reload. We assert on the persisted JSON rather than calling
	// FromInstanceData, which would do a full restore + tmux reattach of the
	// mock-backed agent session — that reattach has no live session to find in
	// a headless CI environment and would time out, testing tmux reconnection
	// rather than PR-info persistence.
	raw, err := config.LoadRepoInstances(repo.ID)
	if err != nil {
		t.Fatalf("LoadRepoInstances: %v", err)
	}
	var data []session.InstanceData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("unmarshal instances: %v", err)
	}
	if len(data) != 1 {
		t.Fatalf("expected 1 persisted instance, got %d", len(data))
	}
	if data[0].PRInfo != want {
		t.Fatalf("persisted PR info = %+v, want %+v", data[0].PRInfo, want)
	}
}

// TestSetPRInfo_ByIDTargetsCanonicalSession proves the daemon half of the
// retained-fetch invariant: with duplicate titles across repos, the stable ID
// chooses one session and supplies its canonical title/repo. A stale display
// title must never redirect the persistence onto the sibling.
func TestSetPRInfo_ByIDTargetsCanonicalSession(t *testing.T) {
	manager, repoA, dataA, repoB, dataB := createDuplicateTitleSessions(t, "feature")
	want := session.PRInfoData{Number: 42, Title: "repo B PR", State: "OPEN"}

	if err := manager.SetPRInfo(SetPRInfoRequest{
		ID: dataB.ID, Title: "stale-display-title", PRInfo: want,
	}); err != nil {
		t.Fatalf("SetPRInfo by id B: %v", err)
	}

	assertTracked(t, manager, repoA.ID, "feature", dataA.ID)
	assertTracked(t, manager, repoB.ID, "feature", dataB.ID)
	instA, _, _, err := manager.findSession("feature", repoA.ID)
	if err != nil {
		t.Fatalf("findSession A: %v", err)
	}
	instB, _, _, err := manager.findSession("feature", repoB.ID)
	if err != nil {
		t.Fatalf("findSession B: %v", err)
	}
	if got := instA.GetPRInfo(); got != nil {
		t.Fatalf("repo A PR info = %+v, want untouched", got)
	}
	if got := instB.GetPRInfo(); got == nil || got.Number != want.Number || got.Title != want.Title {
		t.Fatalf("repo B PR info = %+v, want %+v", got, want)
	}
}

// TestSetPRInfo_ClearsWithZeroValue verifies a zero-value PRInfo (Number 0)
// clears previously-recorded info, both in memory and on disk.
func TestSetPRInfo_ClearsWithZeroValue(t *testing.T) {
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

	if err := manager.SetPRInfo(SetPRInfoRequest{Title: title, RepoID: repo.ID, PRInfo: session.PRInfoData{Number: 5, State: "OPEN"}}); err != nil {
		t.Fatalf("SetPRInfo set: %v", err)
	}
	if err := manager.SetPRInfo(SetPRInfoRequest{Title: title, RepoID: repo.ID, PRInfo: session.PRInfoData{}}); err != nil {
		t.Fatalf("SetPRInfo clear: %v", err)
	}

	if got := inst.GetPRInfo(); got != nil {
		t.Fatalf("in-memory PR info after clear = %+v, want nil", got)
	}
	raw, err := config.LoadRepoInstances(repo.ID)
	if err != nil {
		t.Fatalf("LoadRepoInstances: %v", err)
	}
	var data []session.InstanceData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("unmarshal instances: %v", err)
	}
	if data[0].PRInfo != (session.PRInfoData{}) {
		t.Fatalf("persisted PR info after clear = %+v, want zero value", data[0].PRInfo)
	}
}

// TestControlServer_SetPRInfo_GatedAndValidated covers the RPC-handler gate: a
// warming (not-ready) manager fails fast with the typed starting error, and a
// traversal RepoID is rejected at the network boundary.
func TestControlServer_SetPRInfo_GatedAndValidated(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))

	shell, err := newManagerShell(config.DefaultConfig())
	if err != nil {
		t.Fatalf("newManagerShell: %v", err)
	}
	if shell.Ready() {
		t.Fatal("manager shell must not report ready")
	}
	notReady := &controlServer{manager: shell}
	var resp SetPRInfoResponse
	if err := notReady.SetPRInfo(SetPRInfoRequest{Title: "x"}, &resp); !IsDaemonStartingErr(err) {
		t.Fatalf("SetPRInfo on warming manager: want daemon-starting error, got: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	ready := &controlServer{manager: manager}
	err = ready.SetPRInfo(SetPRInfoRequest{Title: "x", RepoID: "foo/../bar"}, &resp)
	if err == nil || !strings.Contains(err.Error(), "rejected RPC request") {
		t.Fatalf("SetPRInfo traversal RepoID: want rejection, got: %v", err)
	}
}
