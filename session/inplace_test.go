package session

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// inPlaceTmuxWorld is a hermetic tmux double for full first-time-start flows:
// the pty factory records every `tmux new-session -s NAME` and marks NAME as
// created, and the executor answers `has-session -t =NAME` from that set. This
// lets LocalBackend.Start(true) — agent session AND the default shell tab —
// run end-to-end with no real tmux server.
type inPlaceTmuxWorld struct {
	t        *testing.T
	mu       sync.Mutex
	commands []*exec.Cmd
	// created holds sanitized session names spawned via new-session.
	created map[string]bool
}

func newInPlaceTmuxWorld(t *testing.T) *inPlaceTmuxWorld {
	return &inPlaceTmuxWorld{t: t, created: make(map[string]bool)}
}

// argAfter returns the argument following flag in args, or "".
func argAfter(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func (w *inPlaceTmuxWorld) Start(c *exec.Cmd) (*os.File, error) {
	w.mu.Lock()
	w.commands = append(w.commands, c)
	w.mu.Unlock()
	if name := argAfter(c.Args, "-s"); name != "" {
		w.mu.Lock()
		w.created[name] = true
		w.mu.Unlock()
	}
	path := filepath.Join(w.t.TempDir(), fmt.Sprintf("pty-%d", rand.Int31()))
	return os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
}

func (w *inPlaceTmuxWorld) Close() {}

func (w *inPlaceTmuxWorld) exec() cmd_test.MockCmdExec {
	return cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			if strings.Contains(c.String(), "has-session") {
				// has-session passes the exact-match target as one arg: -t=NAME
				name := argAfter(c.Args, "-t")
				for _, a := range c.Args {
					if strings.HasPrefix(a, "-t=") {
						name = strings.TrimPrefix(a, "-t=")
					}
				}
				name = strings.TrimPrefix(name, "=")
				name = strings.TrimSuffix(name, ":")
				w.mu.Lock()
				defer w.mu.Unlock()
				if !w.created[name] {
					return fmt.Errorf("can't find session: %s", name)
				}
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			return []byte(""), nil
		},
	}
}

// initInPlaceRepo creates a temp git repo with an initial commit, checked out
// on a user-owned branch, and returns its root.
func initInPlaceRepo(t *testing.T, branch string) string {
	t.Helper()
	repoRoot := initTempGitRepo(t)
	for _, args := range [][]string{
		{"config", "user.email", "test@test.com"},
		{"config", "user.name", "test"},
		{"commit", "--allow-empty", "-m", "initial"},
		{"checkout", "-b", branch},
	} {
		c := exec.Command("git", args...)
		c.Dir = repoRoot
		out, err := c.CombinedOutput()
		require.NoError(t, err, "git %v: %s", args, string(out))
	}
	return repoRoot
}

func gitOut(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	out, err := c.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, string(out))
	return strings.TrimSpace(string(out))
}

// TestInPlaceInstanceLifecycle drives the full `--here` create path through
// NewInstance → LocalBackend.Start(true) → ToInstanceData → Kill against a
// real temp git repo (tmux mocked): the session attaches to the repo's own
// working tree at its current branch with external_worktree=true, creates no
// branch or worktree, and Kill leaves the user's tree and branch intact.
func TestInPlaceInstanceLifecycle(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	const branch = "user/current-work"
	repoRoot := initInPlaceRepo(t, branch)
	branchesBefore := gitOut(t, repoRoot, "branch", "--list")

	inst, err := NewInstance(InstanceOptions{
		Title:   "captain-here",
		Path:    repoRoot,
		Program: "claude",
		InPlace: true,
	})
	require.NoError(t, err)
	require.Equal(t, "local", inst.GetBackend().Type(),
		"--here sessions use the plain local backend")

	world := newInPlaceTmuxWorld(t)
	inst.SetTmuxSession(tmux.NewTmuxSessionWithDeps("captain-here", "claude", world, world.exec()))

	require.NoError(t, inst.Start(true))
	require.True(t, inst.Started())

	// The worktree is the repo root itself, external, on the current branch.
	gw, err := inst.GetGitWorktree()
	require.NoError(t, err)
	assert.True(t, gw.IsExternalWorktree(),
		"--here must persist external_worktree=true so cleanup skips the user's tree")
	assert.False(t, gw.BranchCreatedByUs())
	assert.Equal(t, gw.GetRepoPath(), gw.GetWorktreePath(),
		"--here: worktree_path must equal repo_path")
	wantRoot, _ := filepath.EvalSymlinks(repoRoot)
	gotRoot, _ := filepath.EvalSymlinks(inst.GetWorktreePath())
	assert.Equal(t, wantRoot, gotRoot, "worktree must be the repo root the session was created in")
	assert.Equal(t, branch, inst.GetBranch(), "current branch must be preserved")

	// No branch was created and no linked worktree was registered.
	assert.Equal(t, branchesBefore, gitOut(t, repoRoot, "branch", "--list"),
		"--here must not create a branch")
	assert.Equal(t, branch, gitOut(t, repoRoot, "rev-parse", "--abbrev-ref", "HEAD"),
		"--here must not switch the checked-out branch")
	assert.Equal(t, 1, strings.Count(gitOut(t, repoRoot, "worktree", "list", "--porcelain"), "worktree "),
		"--here must not register a linked worktree")

	// Persisted form carries the external-worktree semantics, and they
	// survive a JSON round-trip (what instances.json stores).
	data := inst.ToInstanceData()
	require.True(t, data.Worktree.ExternalWorktree)
	assert.Equal(t, data.Worktree.RepoPath, data.Worktree.WorktreePath)
	assert.Equal(t, branch, data.Branch)
	require.NotNil(t, data.Worktree.BranchCreatedByUs)
	assert.False(t, *data.Worktree.BranchCreatedByUs)
	raw, err := json.Marshal(data)
	require.NoError(t, err)
	var restored InstanceData
	require.NoError(t, json.Unmarshal(raw, &restored))
	assert.True(t, restored.Worktree.ExternalWorktree,
		"external_worktree must survive the instances.json round-trip")

	// Kill must tear down only the session — the user's working tree and
	// branch survive (mirrors TestCleanup_LegacyExternalWorktreeIsPreserved).
	require.NoError(t, inst.Kill())
	_, statErr := os.Stat(filepath.Join(repoRoot, ".git"))
	require.NoError(t, statErr, "Kill must NOT remove the repo working tree")
	gitOut(t, repoRoot, "show-ref", "--verify", "refs/heads/"+branch)
	assert.Equal(t, branchesBefore, gitOut(t, repoRoot, "branch", "--list"),
		"Kill must not delete any branch")
}

// TestInPlaceRejectsRemote: an in-place session runs in the local working
// tree; combining it with a remote backend is contradictory and must fail
// fast at NewInstance (mirrors the pre-#930 "remote sessions cannot use an
// existing local worktree" guard).
func TestInPlaceRejectsRemote(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	_, err := NewInstance(InstanceOptions{
		Title:       "here-remote",
		Path:        t.TempDir(),
		Program:     "claude",
		InPlace:     true,
		ForceRemote: true,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "in-place")
}
