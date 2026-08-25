package daemon

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// setupBareCloneWorktree3358 builds the issue's exact repository shape: the
// linked worktree has a bare common directory and no main working tree.
func setupBareCloneWorktree3358(t *testing.T) (parent, bare, worktree string) {
	t.Helper()
	parent = testguard.CanonicalTempDir(t)
	source := filepath.Join(parent, "source")
	bare = filepath.Join(parent, "bare.git")
	worktree = filepath.Join(parent, "worktree")

	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v failed: %v: %s", args, err, out)
		}
	}
	run(parent, "init", source)
	run(source, "config", "user.email", "test@test.com")
	run(source, "config", "user.name", "Test")
	run(source, "commit", "--allow-empty", "-m", "init")
	run(parent, "clone", "--bare", source, bare)
	run(bare, "worktree", "add", worktree)
	return parent, bare, worktree
}

// TestCreateSessionPreservesAndNamesLegacyBareCloneRows pins the compatibility
// rule for rows written before #3358. They stay under the old parent identity —
// old records discarded the requesting worktree, so moving them could claim an
// unrelated repository — and the create names that retained state explicitly.
func TestCreateSessionPreservesAndNamesLegacyBareCloneRows(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	parent, _, worktree := setupBareCloneWorktree3358(t)
	legacyID := config.RepoIDFromRoot(parent)
	branchOwned := false
	legacy := session.InstanceData{
		ID:     "legacy-bare-row",
		Title:  "legacy-parent",
		Path:   parent,
		Status: session.Archived,
		Worktree: session.GitWorktreeData{
			RepoPath:          parent,
			WorktreePath:      parent,
			SessionName:       "legacy-parent",
			ExternalWorktree:  true,
			BranchCreatedByUs: &branchOwned,
		},
	}
	if err := appendInstanceData(legacyID, legacy); err != nil {
		t.Fatalf("seed legacy row: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	var warnings bytes.Buffer
	previous := log.WarningLog.Writer()
	log.WarningLog.SetOutput(&warnings)
	t.Cleanup(func() { log.WarningLog.SetOutput(previous) })
	if _, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: "new-identity", RepoPath: worktree, Program: "claude", InPlace: true,
	}); err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	rows, err := loadRepoInstanceData(legacyID)
	if err != nil {
		t.Fatalf("reload legacy rows: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != legacy.ID {
		t.Fatalf("legacy rows were moved or overwritten: %+v", rows)
	}
	if warning := warnings.String(); !strings.Contains(warning, "pre-#3358") || !strings.Contains(warning, legacyID) {
		t.Fatalf("compatibility warning did not name retained identity %s: %q", legacyID, warning)
	}
}

// TestCreateSessionAtBareCloneWorktreeUsesBareIdentityAndWorkspace drives the
// real daemon create path. Identity belongs to the bare common directory, but
// --here must provision at the linked worktree the caller requested.
func TestCreateSessionAtBareCloneWorktreeUsesBareIdentityAndWorkspace(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	parent, bare, worktree := setupBareCloneWorktree3358(t)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	data, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title:    "bare-here",
		RepoPath: worktree,
		Program:  "claude",
		InPlace:  true,
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if len(*seen) != 1 {
		t.Fatalf("expected 1 NewInstance call, got %d", len(*seen))
	}
	if got := (*seen)[0].Path; got != worktree {
		t.Fatalf("create workspace = %q, want linked worktree %q", got, worktree)
	}
	if data.Path != worktree {
		t.Fatalf("persisted workspace = %q, want linked worktree %q", data.Path, worktree)
	}

	identityID := config.RepoIDFromRoot(bare)
	rows, err := loadRepoInstanceData(identityID)
	if err != nil {
		t.Fatalf("load identity-keyed rows: %v", err)
	}
	if len(rows) != 1 || rows[0].Title != "bare-here" {
		t.Fatalf("bare identity %s rows = %+v, want bare-here", identityID, rows)
	}
	legacyID := config.RepoIDFromRoot(parent)
	legacyRows, err := loadRepoInstanceData(legacyID)
	if err != nil {
		t.Fatalf("load legacy parent-keyed rows: %v", err)
	}
	if len(legacyRows) != 0 {
		t.Fatalf("new create leaked under legacy parent identity %s: %+v", legacyID, legacyRows)
	}
}

func TestCreateSessionCheckpointKeepsFirstWriteBareIdentity(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installOptionsRecordingBackend(t)
	_, bare, worktree := setupBareCloneWorktree3358(t)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	data, err := manager.CreateSession(context.Background(), CreateSessionRequest{
		Title: "bare-worktreeless", RepoPath: worktree, Program: "claude",
	})
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}
	if data.Worktree.RepoPath != "" {
		t.Fatalf("fixture must create a worktree-less row, got repo path %q", data.Worktree.RepoPath)
	}

	identityID := config.RepoIDFromRoot(bare)
	cmd := exec.Command("git", "-C", bare, "worktree", "remove", "--force", worktree)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("remove linked worktree: %v: %s", err, out)
	}
	if err := os.MkdirAll(worktree, 0o755); err != nil {
		t.Fatalf("recreate former workspace path: %v", err)
	}
	wrongID := config.RepoIDForPath(worktree)
	if wrongID == identityID {
		t.Fatalf("fixture did not change identity after removing linked worktree: %s", identityID)
	}

	if err := manager.SaveInstances(); err != nil {
		t.Fatalf("SaveInstances: %v", err)
	}
	identityRows, err := loadRepoInstanceData(identityID)
	if err != nil {
		t.Fatalf("load identity-keyed rows: %v", err)
	}
	if len(identityRows) != 1 || identityRows[0].Title != "bare-worktreeless" {
		t.Fatalf("first-write identity %s rows = %+v", identityID, identityRows)
	}
	wrongRows, err := loadRepoInstanceData(wrongID)
	if err != nil {
		t.Fatalf("load path-derived rows: %v", err)
	}
	if len(wrongRows) != 0 {
		t.Fatalf("checkpoint duplicated fresh row under re-resolved identity %s: %+v", wrongID, wrongRows)
	}
}

// TestEnsureRootAgentsCreatesRootAtBareCloneWorktree flips PR #3334's parity
// anchor: the registered checkout remains the root agent's in-place workspace,
// while its row and daemon key use the bare repository's identity.
func TestEnsureRootAgentsCreatesRootAtBareCloneWorktree(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	seen := installOptionsRecordingBackend(t)
	_, bare, worktree := setupBareCloneWorktree3358(t)

	project := registerTestProject(t, worktree)
	writePersonalRootAgent(t, project.ID, "enabled = true\nprogram = \"/opt/bare-root\"")
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	manager.EnsureRootAgents()

	if len(*seen) != 1 {
		t.Fatalf("enabled bare-clone worktree project got %d creates, want 1", len(*seen))
	}
	if got := (*seen)[0].Path; got != worktree {
		t.Fatalf("root workspace = %q, want registered worktree %q", got, worktree)
	}
	if got := (*seen)[0].Program; got != "/opt/bare-root" {
		t.Fatalf("root program = %q, want personal program", got)
	}
	manager.mu.Lock()
	root := manager.instances[daemonInstanceKey(config.RepoIDFromRoot(bare), session.RootSessionTitle)]
	manager.mu.Unlock()
	if root == nil {
		t.Fatalf("root not keyed under bare repository identity %s", config.RepoIDFromRoot(bare))
	}
}
