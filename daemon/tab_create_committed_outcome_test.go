package daemon

import (
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// tabKillRefusedExec mirrors tabNameKeyedExec, except kill-session ANSWERS with
// a failure and the session provably survives (has-session keeps reporting it):
// the exact shape session/tmux/close.go classifies as "error killing tmux
// session" — a rollback that could not prove the spawned tab absent (#3237).
func tabKillRefusedExec(alive map[string]bool) cmd_test.MockCmdExec {
	existing := map[string]bool{}
	for k, v := range alive {
		existing[k] = v
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
	return cmd_test.MockCmdExec{
		RunFunc: func(cmd *exec.Cmd) error {
			s := cmd.String()
			n := nameOf(cmd)
			switch {
			case strings.Contains(s, "has-session"):
				if existing[n] {
					return nil
				}
				return &tabNoSessionErr{}
			case strings.Contains(s, "new-session"):
				existing[n] = true
				return nil
			case strings.Contains(s, "kill-session"):
				// Refused, and the session survives: existing is NOT cleared, so
				// the post-kill has-session probe confirms it is still alive.
				return errors.New("kill-session refused")
			}
			return nil
		},
		OutputFunc: func(cmd *exec.Cmd) ([]byte, error) {
			if strings.Contains(cmd.String(), "list-panes") {
				return nil, nil
			}
			return []byte("content"), nil
		},
	}
}

// startedTabInstanceWithExec is startedLocalTabInstance with a caller-chosen
// executor, so a test can model tab-teardown outcomes beyond the happy path.
func startedTabInstanceWithExec(t *testing.T, m *Manager, repoID, repoPath, title, agentName string, exec cmd_test.MockCmdExec) *session.Instance {
	t.Helper()
	pty := tabPtyFactory{t: t, cmdExec: exec}

	gw, err := git.NewGitWorktreeFromStorage(
		repoPath, filepath.Join(t.TempDir(), "wt"), title,
		title+"-branch", "", false, true)
	if err != nil {
		t.Fatalf("NewGitWorktreeFromStorage: %v", err)
	}

	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: repoPath, Program: "claude"})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	inst.SetGitWorktreeForTest(gw)
	inst.SetTmuxSession(tmux.NewTmuxSessionFromSanitizedNameWithDeps(agentName, "claude", pty, exec))
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)

	seedDiskInstance(t, repoID, title, repoPath)
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, title)] = inst
	m.mu.Unlock()
	return inst
}

// failPersistFor makes every targeted record write for title fail.
func failPersistFor(t *testing.T, title string, failure error) {
	t.Helper()
	prev := testHookPersistInstanceData
	t.Cleanup(func() { testHookPersistInstanceData = prev })
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		if data.Title != title {
			return nil
		}
		return failure
	}
}

// TestCreateTab_PersistAndRollbackFailure_ReportsCommittedTab is #3237: the tab
// was spawned before its roster write failed, and the rollback could not prove
// the tmux session absent — it is confirmed still alive. A plain error tells
// CLI/API clients failed-nothing-committed about a live tmux session, and loses
// the daemon-minted tab identity they would need to explain or target it.
func TestCreateTab_PersistAndRollbackFailure_ReportsCommittedTab(t *testing.T) {
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
	startedTabInstanceWithExec(t, manager, repo.ID, repoPath, title, agentName, tabKillRefusedExec(map[string]bool{agentName: true}))
	failPersistFor(t, title, errors.New("no space left on device"))

	created, err := manager.CreateTab(CreateTabRequest{Title: title, RepoID: repo.ID, Command: "btop"})
	if err == nil {
		t.Fatal("CreateTab must surface the failed persist")
	}
	if !isMutationCommitted(err) {
		t.Fatalf("CreateTab error = %T %v, want a committed-mutation marker: the spawned tmux tab provably survived the rollback", err, err)
	}
	if created.ID == "" || created.Name == "" {
		t.Fatalf("the minted tab identity must be preserved on the committed path, got %+v", created)
	}
	if created.TmuxName != agentName+"__btop" {
		t.Fatalf("the surviving tmux session's name = %q, want %q so the operator can target it", created.TmuxName, agentName+"__btop")
	}
}

// TestCreateTab_PersistFailureWithConfirmedRollback_StaysPlainFailure pins the
// other half of #3237's contract: when the rollback proves the spawned tab
// absent, nothing survives, and the clean failed-nothing-committed shape stays.
func TestCreateTab_PersistFailureWithConfirmedRollback_StaysPlainFailure(t *testing.T) {
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
	startedTabInstanceWithExec(t, manager, repo.ID, repoPath, title, agentName, tabNameKeyedExec(map[string]bool{agentName: true}))
	failPersistFor(t, title, errors.New("no space left on device"))

	created, err := manager.CreateTab(CreateTabRequest{Title: title, RepoID: repo.ID, Command: "btop"})
	if err == nil {
		t.Fatal("CreateTab must surface the failed persist")
	}
	if isMutationCommitted(err) {
		t.Fatalf("a confirmed rollback left nothing behind and must stay a plain failure, got committed: %v", err)
	}
	if created.ID != "" || created.TmuxName != "" {
		t.Fatalf("no identity may be claimed when the rollback confirmed the tab absent, got %+v", created)
	}
}

// TestControlCreateTab_CommittedSurvivingTab_FillsEnvelope pins the handler
// half of #3237: the committed outcome and the minted identity must ride the
// response envelope, since net/rpc flattens a returned error to a string.
func TestControlCreateTab_CommittedSurvivingTab_FillsEnvelope(t *testing.T) {
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
	cs := &controlServer{manager: manager}

	const title = "worker"
	agentName := "af_" + title + "_agent"
	startedTabInstanceWithExec(t, manager, repo.ID, repoPath, title, agentName, tabKillRefusedExec(map[string]bool{agentName: true}))
	failPersistFor(t, title, errors.New("no space left on device"))

	var resp CreateTabResponse
	if err := cs.CreateTab(CreateTabRequest{Title: title, RepoID: repo.ID, Command: "btop"}, &resp); err != nil {
		t.Fatalf("a committed tab-create must land in the envelope, not be returned as an rpc error: %v", err)
	}
	if resp.MutationOutcome.Code != apiproto.ErrorCodeMutationCommitted {
		t.Fatalf("resp code = %q, want %q", resp.MutationOutcome.Code, apiproto.ErrorCodeMutationCommitted)
	}
	if resp.ID == "" || resp.TmuxName == "" {
		t.Fatalf("the minted identity must ride the envelope so clients can target the surviving tab, got %+v", resp)
	}
}
