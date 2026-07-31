package daemon

import (
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// The daemon-side disk contract for #2669's review follow-up. The session
// package proves the retention mechanism; these prove the two things only the
// daemon can: that an unconfirmed teardown's cleanup handle actually reaches
// instances.json (the point of it being durable), and that the wedge the finding
// describes — a same-named tab re-deriving the survivor's tmux name — is gone.

// unkillableTabExec models a tmux server that refuses kill-session for the named
// sessions and leaves them running, the shape close.go reports as a real
// teardown error rather than a timeout.
func unkillableTabExec(alive map[string]bool, unkillable map[string]bool) (cmd_test.MockCmdExec, func(string) bool) {
	var mu sync.Mutex
	existing := map[string]bool{}
	for name, ok := range alive {
		existing[name] = ok
	}
	nameOf := func(cmd *exec.Cmd) string {
		for i, a := range cmd.Args {
			switch {
			case (a == "-t" || a == "-s") && i+1 < len(cmd.Args):
				return strings.TrimSuffix(strings.TrimPrefix(cmd.Args[i+1], "="), ":")
			case strings.HasPrefix(a, "-t="):
				return strings.TrimPrefix(a, "-t=")
			case strings.HasPrefix(a, "-s="):
				return strings.TrimPrefix(a, "-s=")
			}
		}
		return ""
	}
	mockExec := cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			s := cmd.String()
			name := nameOf(cmd)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case strings.Contains(s, "has-session"):
				if existing[name] {
					return nil
				}
				return &tabNoSessionErr{}
			case strings.Contains(s, "new-session"):
				existing[name] = true
			case strings.Contains(s, "kill-session"):
				if unkillable[name] {
					return errors.New("kill-session refused")
				}
				delete(existing, name)
			}
			return nil
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) { return []byte("content"), nil },
	}
	return mockExec, func(name string) bool {
		mu.Lock()
		defer mu.Unlock()
		return existing[name]
	}
}

// persistedInstance reads back the one persisted record for repoID.
func persistedInstance(t *testing.T, repoID string) session.InstanceData {
	t.Helper()
	raw, err := config.LoadRepoInstances(repoID)
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
	return data[0]
}

// TestCloseTab_UnconfirmedTeardownPersistsCleanupHandle is the #2669 review
// regression at the storage boundary. CloseTab commits the shrunken roster
// before it kills tmux, so a kill that answers with the session still alive used
// to leave the persisted record naming that tmux session NOWHERE: the process
// was untracked across every future daemon restart. The close must still
// succeed — it is durably committed — while the record retains a cleanup handle.
func TestCloseTab_UnconfirmedTeardownPersistsCleanupHandle(t *testing.T) {
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
	agentName := "af_" + title + "_agent"
	// The tab's tmux name is derived at spawn, so refuse every kill except the
	// agent's: the process tab's session is whichever one CreateTab lands on.
	mockExec, aliveFn := unkillableTabExec(
		map[string]bool{agentName: true},
		map[string]bool{agentName + "__btop": true})
	inst := startedLocalTabInstanceWithExec(t, manager, repo.ID, repoPath, title, agentName, mockExec)
	created, err := manager.CreateTab(CreateTabRequest{Title: title, RepoID: repo.ID, Command: "btop -t"})
	if err != nil {
		t.Fatalf("CreateTab: %v", err)
	}
	if created.TmuxName != agentName+"__btop" {
		t.Fatalf("precondition: process tab tmux = %q, want %q", created.TmuxName, agentName+"__btop")
	}

	name, err := manager.CloseTab(CloseTabRequest{Title: title, RepoID: repo.ID, TabID: created.ID})
	if err != nil {
		t.Fatalf("CloseTab must succeed on an unconfirmed teardown: the roster decision is durable: %v", err)
	}
	if name != "btop" {
		t.Fatalf("closed tab name = %q, want btop", name)
	}
	if !aliveFn(created.TmuxName) {
		t.Fatal("precondition: the fixture's kill must leave the session alive; the test's premise is wrong")
	}
	if got := inst.TabCount(); got != 1 {
		t.Fatalf("tab count = %d, want 1: the close is committed regardless of teardown", got)
	}

	data := persistedInstance(t, repo.ID)
	for _, tab := range data.Tabs {
		if tab.TmuxName == created.TmuxName {
			t.Fatal("the persisted record still carries the closed tab; a restart would respawn it")
		}
	}
	if len(data.PendingTabCleanup) != 1 {
		t.Fatalf("persisted cleanup handles = %d, want 1: the leaked tmux session %q is now untracked "+
			"across daemon restarts", len(data.PendingTabCleanup), created.TmuxName)
	}
	if got := data.PendingTabCleanup[0].TmuxName; got != created.TmuxName {
		t.Fatalf("persisted cleanup handle names %q, want the closed tab's session %q", got, created.TmuxName)
	}
	if data.PendingTabCleanup[0].TabID != created.ID {
		t.Fatalf("cleanup handle tab id = %q, want %q", data.PendingTabCleanup[0].TabID, created.ID)
	}
}

// TestCloseTab_ConfirmedTeardownPersistsNoCleanupHandle keeps the retention from
// becoming a leak of its own: an ordinary close proves the session is gone, so
// the record must carry no handle for a future daemon to retry.
func TestCloseTab_ConfirmedTeardownPersistsNoCleanupHandle(t *testing.T) {
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
	agentName := "af_" + title + "_agent"
	mockExec, aliveFn := unkillableTabExec(map[string]bool{agentName: true}, nil)
	startedLocalTabInstanceWithExec(t, manager, repo.ID, repoPath, title, agentName, mockExec)
	created, err := manager.CreateTab(CreateTabRequest{Title: title, RepoID: repo.ID, Command: "btop -t"})
	if err != nil {
		t.Fatalf("CreateTab: %v", err)
	}

	if _, err := manager.CloseTab(CloseTabRequest{Title: title, RepoID: repo.ID, TabID: created.ID}); err != nil {
		t.Fatalf("CloseTab: %v", err)
	}
	if aliveFn(created.TmuxName) {
		t.Fatal("precondition: a confirmed close must actually kill the session")
	}

	if got := persistedInstance(t, repo.ID).PendingTabCleanup; len(got) != 0 {
		t.Fatalf("a confirmed teardown left %d cleanup handle(s) on disk; every future daemon start "+
			"would retry a kill that already succeeded", len(got))
	}
}

// TestCreateTab_AvoidsTmuxNameRetainedByUnconfirmedClose is the user-visible half
// of the finding. After a close whose kill failed, nothing on the roster holds
// the "btop" token, so the next `tab-create` re-derived the survivor's exact tmux
// name and TmuxSession.Start rejected it as already existing — a wedge no retry
// clears, because every retry derives the same name. The retained handle
// reserves the token, so the spawn walks to "btop-2" and succeeds.
func TestCreateTab_AvoidsTmuxNameRetainedByUnconfirmedClose(t *testing.T) {
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
	agentName := "af_" + title + "_agent"
	mockExec, aliveFn := unkillableTabExec(
		map[string]bool{agentName: true},
		map[string]bool{agentName + "__btop": true})
	startedLocalTabInstanceWithExec(t, manager, repo.ID, repoPath, title, agentName, mockExec)
	first, err := manager.CreateTab(CreateTabRequest{Title: title, RepoID: repo.ID, Command: "btop -t"})
	if err != nil {
		t.Fatalf("CreateTab: %v", err)
	}
	if _, err := manager.CloseTab(CloseTabRequest{Title: title, RepoID: repo.ID, TabID: first.ID}); err != nil {
		t.Fatalf("CloseTab: %v", err)
	}
	if !aliveFn(first.TmuxName) {
		t.Fatal("precondition: the first tab's session must survive the failed kill")
	}

	second, err := manager.CreateTab(CreateTabRequest{Title: title, RepoID: repo.ID, Command: "btop -t"})
	if err != nil {
		t.Fatalf("recreating the tab must not collide with the un-reaped session: %v", err)
	}
	if second.TmuxName == first.TmuxName {
		t.Fatalf("the new tab re-derived the surviving session's tmux name %q; Start would reject it "+
			"as already existing and no retry could ever clear it", second.TmuxName)
	}
	// The user still gets the name they asked for — only the tmux namespace walks
	// (#1957): the reservation must not charge the display namespace for a
	// collision in tmux's.
	if second.Name != "btop" {
		t.Fatalf("recreated tab name = %q, want btop: the cleanup handle must not take the user's name", second.Name)
	}
}

// TestSweepStartupTabCleanup_RetiresConfirmedHandlesOnDisk covers the wiring the
// session-package retry test cannot: that the daemon's startup pass records the
// retirement through the targeted per-repo writer, so the next start does not
// retry a finished teardown forever.
//
// The seeded handle names a tmux session that does not exist, which Close treats
// as a confirmed absence (#967 idempotent teardown). That keeps the test's only
// real tmux contact to a kill and a probe against a name nothing owns.
func TestSweepStartupTabCleanup_RetiresConfirmedHandlesOnDisk(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed; the startup sweep drives a real teardown")
	}
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
	agentName := "af_" + title + "_agent"
	mockExec, _ := unkillableTabExec(map[string]bool{agentName: true}, nil)
	inst := startedLocalTabInstanceWithExec(t, manager, repo.ID, repoPath, title, agentName, mockExec)
	// A handle left by a previous daemon whose close committed but whose kill was
	// never confirmed. The session it names is long gone.
	inst.SetPendingTabCleanupForTest([]session.TabCleanupData{
		{TabID: "closed-tab", TmuxName: agentName + "__reaped-by-a-crash"},
	})
	if err := persistInstanceData(repo.ID, inst.ToInstanceData()); err != nil {
		t.Fatalf("seeding the pending handle: %v", err)
	}
	if len(persistedInstance(t, repo.ID).PendingTabCleanup) != 1 {
		t.Fatal("precondition: the seeded handle must be on disk")
	}

	sweepStartupTabCleanup(manager)

	if got := inst.PendingTabCleanup(); len(got) != 0 {
		t.Fatalf("the sweep left %d handle(s) in memory for a session that is positively gone", len(got))
	}
	if got := persistedInstance(t, repo.ID).PendingTabCleanup; len(got) != 0 {
		t.Fatalf("the sweep reaped the session but did not record it; every future start would "+
			"retry the finished teardown (%d handle(s) still on disk)", len(got))
	}
}
