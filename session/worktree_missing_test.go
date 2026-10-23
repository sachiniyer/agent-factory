package session

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// worktreeMissingInstance returns an instance whose recorded worktree path is
// path. Nothing is created on disk; tests decide whether path exists.
func worktreeMissingInstance(t *testing.T, path string) *Instance {
	t.Helper()
	inst, err := NewInstance(InstanceOptions{Title: "wt-missing", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	gw, err := git.NewGitWorktreeFromStorage(
		filepath.Join(filepath.Dir(path), "repo"), path, "wt-missing", "af/wt-missing", "", false, true,
	)
	require.NoError(t, err)
	inst.gitWorktree = gw
	return inst
}

func TestRefreshWorktreeMissing_SetClearKeep(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wt")
	inst := worktreeMissingInstance(t, path)

	missing, changed := inst.RefreshWorktreeMissing()
	assert.True(t, missing)
	assert.True(t, changed)
	gotMissing, reason := inst.WorktreeMissing()
	assert.True(t, gotMissing)
	assert.Contains(t, reason, path)

	missing, changed = inst.RefreshWorktreeMissing()
	assert.True(t, missing)
	assert.False(t, changed, "a repeated Absent answer is not a transition")

	// An af-owned relocation makes the probe Unknown: a flag already set stays
	// set rather than being cleared on a non-answer.
	require.NoError(t, inst.gitWorktree.RestoreRelocationRecovery(git.RelocationRecovery{
		State: git.RelocationRecoveryStalled,
	}))
	missing, changed = inst.RefreshWorktreeMissing()
	assert.True(t, missing, "Unknown must keep a previously established flag")
	assert.False(t, changed)

	// A fresh instance whose path is unanswerable is never flagged on a guess.
	unknown := worktreeMissingInstance(t, filepath.Join(t.TempDir(), "wt"))
	require.NoError(t, unknown.gitWorktree.RestoreRelocationRecovery(git.RelocationRecovery{
		State: git.RelocationRecoveryStalled,
	}))
	missing, changed = unknown.RefreshWorktreeMissing()
	assert.False(t, missing)
	assert.False(t, changed)

	// A recreated pathname does NOT clear the flag: the agent's cwd is still the
	// unlinked inode, so only af rebuilding the worktree and respawning the agent
	// into it (ClearWorktreeMissing) makes the row deliverable again.
	recreated := worktreeMissingInstance(t, filepath.Join(t.TempDir(), "wt"))
	_, changed = recreated.RefreshWorktreeMissing()
	require.True(t, changed)
	require.NoError(t, os.Mkdir(recreated.gitWorktree.GetWorktreePath(), 0o755))
	missing, changed = recreated.RefreshWorktreeMissing()
	assert.True(t, missing, "Present must not clear a set flag")
	assert.False(t, changed)
	_, reason = recreated.WorktreeMissing()
	assert.Contains(t, reason, "deleted outside af")

	recreated.ClearWorktreeMissing()
	missing, changed = recreated.RefreshWorktreeMissing()
	assert.False(t, missing, "once af cleared it, a present path keeps it clear")
	assert.False(t, changed)
}

func TestRefreshWorktreeMissing_NoWorktreeLeavesFlag(t *testing.T) {
	inst, err := NewInstance(InstanceOptions{Title: "no-wt", Path: t.TempDir(), Program: "claude"})
	require.NoError(t, err)
	inst.SetWorktreeMissing("recorded earlier")
	missing, changed := inst.RefreshWorktreeMissing()
	assert.True(t, missing)
	assert.False(t, changed)
}

// The flag and reason survive the durable record and the client projection.
// The row is archived before it is serialized so FromInstanceData loads it
// inert: a live local record makes the loader reattach or respawn a real tmux
// session running the agent, which ties the test to that program existing on
// the runner (it did not on macOS CI). The round-trip under test does not
// depend on liveness — the archived row is the shape the archive-gone route
// persists anyway.
func TestWorktreeMissing_SerializationRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wt")
	inst := worktreeMissingInstance(t, path)
	_, changed := inst.RefreshWorktreeMissing()
	require.True(t, changed)
	_, wantReason := inst.WorktreeMissing()
	inst.liveness = LiveArchived

	data := inst.ToInstanceData()
	assert.True(t, data.Worktree.Missing)
	assert.Equal(t, wantReason, data.Worktree.MissingReason)

	client := data.ForClientRead()
	assert.True(t, client.Worktree.Missing, "the flag is record state, not a scrubbed projection")
	assert.Equal(t, wantReason, client.Worktree.MissingReason)

	restored, err := FromInstanceData(data.ForStorage())
	require.NoError(t, err)
	require.Equal(t, LiveArchived, restored.GetLiveness(), "premise: the row loads inert, with no runtime launched")
	require.False(t, restored.Started())
	missing, reason := restored.WorktreeMissing()
	assert.True(t, missing, "a restart must come back still knowing the worktree is gone")
	assert.Equal(t, wantReason, reason)
}

func runRenameGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.email=t@t", "-c", "user.name=t"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return strings.TrimSpace(string(out))
}

// A gone-worktree archived row must not block its own title reuse: the rename
// repoints the record instead of attempting a move of bytes that do not exist
// (#5102).
func TestRenameArchived_GoneWorktreeRepointsWithoutMove(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoRoot := initTempGitRepo(t)
	runRenameGit(t, repoRoot, "commit", "--allow-empty", "-m", "init")
	wtPath := filepath.Join(filepath.Dir(repoRoot), "old-title")
	runRenameGit(t, repoRoot, "worktree", "add", "-b", "af/old-title", wtPath)
	require.NoError(t, os.RemoveAll(wtPath))

	inst, err := NewInstance(InstanceOptions{Title: "old-title", Path: repoRoot, Program: "claude"})
	require.NoError(t, err)
	gw, err := git.NewGitWorktreeFromStorage(repoRoot, wtPath, "old-title", "af/old-title", "", false, true)
	require.NoError(t, err)
	inst.gitWorktree = gw
	inst.liveness = LiveArchived

	dest := filepath.Join(filepath.Dir(repoRoot), "old-title-renamed")
	require.NoError(t, inst.RenameArchived("old-title-renamed", dest, "af/old-title-renamed"))

	assert.Equal(t, "old-title-renamed", inst.Title)
	assert.Equal(t, "af/old-title-renamed", gw.GetBranchName())
	assert.Equal(t, dest, gw.GetWorktreePath())
	_, statErr := os.Lstat(dest)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "no move was attempted, so nothing exists at the new path")
	runRenameGit(t, repoRoot, "show-ref", "--verify", "refs/heads/af/old-title-renamed")

	// The row was never flagged — the directory vanished between polls — so the
	// repoint itself must record the deletion, naming the path the record now
	// points at. Otherwise a directory appearing at dest later reads Present, the
	// sticky flag never sets, and restore adopts it as the session's worktree.
	missing, reason := inst.WorktreeMissing()
	assert.True(t, missing, "an absent-path repoint must stamp the missing flag")
	assert.Equal(t, WorktreeMissingRepointedReason(dest), reason)
	data := inst.ToInstanceData()
	assert.True(t, data.Worktree.Missing, "the persisted row must carry the flag")
	assert.Equal(t, dest, data.Worktree.WorktreePath)
	assert.Contains(t, data.Worktree.MissingReason, dest, "the reason must name the repointed path, not the deleted one")
	assert.NotContains(t, data.Worktree.MissingReason, wtPath+" ")

	// And the stamped flag is what fences a directory that appears at the new
	// destination: a further rename refuses it rather than moving it.
	require.NoError(t, os.MkdirAll(dest, 0o755))
	err = inst.RenameArchived("old-title-renamed-2", dest+"-2", "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "recorded as deleted outside af")
}

// The respawn path is where af re-materializes a deleted worktree, so it is where
// the flag clears — but only when it rebuilt the path or git proves the
// directory there is the session's own checkout, and a fresh pane started in
// it. A directory git does not prove (someone mkdir'd it, a checkout of another
// branch moved in) is unverified: respawn refuses to start the agent in it and
// leaves the flag set. A registered checkout of the session's branch is what an
// earlier attempt's rebuild leaves when it failed before its pane started, and
// refusing it would strand the row forever (#5102).
func TestRecover_ClearsWorktreeMissingOnlyAfterRebuild(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	for _, tc := range []struct {
		name        string
		occupant    string
		wantMissing bool
	}{
		{name: "rebuilt and respawned clears", occupant: "", wantMissing: false},
		{name: "an existing path on a flagged row is refused and keeps it", occupant: "mkdir", wantMissing: true},
		{name: "a checkout of another branch is refused and keeps it", occupant: "other-branch", wantMissing: true},
		{name: "an earlier rebuild of the session's branch is proven and clears", occupant: "own-checkout", wantMissing: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repoRoot := initTempGitRepo(t)
			gitOut(t, repoRoot, "config", "user.email", "test@test.com")
			gitOut(t, repoRoot, "config", "user.name", "test")
			gitOut(t, repoRoot, "commit", "--allow-empty", "-m", "initial")
			const branch = "af/wt-missing-recover"
			gitOut(t, repoRoot, "branch", branch)

			const agentName = "af_wt_missing_recover"
			shellName := agentName + tmuxTabSeparator + shellTabName
			worktreePath := filepath.Join(t.TempDir(), "worktree")
			switch tc.occupant {
			case "mkdir":
				require.NoError(t, os.Mkdir(worktreePath, 0o755))
			case "other-branch":
				gitOut(t, repoRoot, "worktree", "add", "-b", "someone-else", worktreePath)
			case "own-checkout":
				gitOut(t, repoRoot, "worktree", "add", worktreePath, branch)
			}
			instance := accountLostInstanceForRecover(
				t, repoRoot, worktreePath, branch, agentName, shellName, nameKeyedExec(map[string]bool{}),
			)
			instance.SetWorktreeMissing("tracked worktree path " + worktreePath + " does not exist (deleted outside af)")

			err := instance.Recover()
			if tc.wantMissing {
				var unavailable *WorktreeUnavailableError
				require.ErrorAs(t, err, &unavailable, "a flagged row must not respawn into an unverified directory")
				assert.Contains(t, err.Error(), "unverified")
			} else {
				require.NoError(t, err)
			}
			require.DirExists(t, worktreePath)
			missing, _ := instance.WorktreeMissing()
			assert.Equal(t, tc.wantMissing, missing)
		})
	}
}

// af's own ordinary archive or restore takes a record-free relocation claim, so
// the relocation snapshot stays clean mid-move and the probe reads ENOENT for
// af's move. The operation fence is what sees it: a probe answered while an op
// is in flight must not stamp the flag, or a successful archive leaves a row
// that stays flagged as missing forever (#5102).
func TestRefreshWorktreeMissing_InFlightOpNeverStamps(t *testing.T) {
	for _, op := range []InFlightOp{OpArchiving, OpRestoring, OpKilling, OpCreating} {
		inst := worktreeMissingInstance(t, filepath.Join(t.TempDir(), "wt"))
		inst.inFlightOp = op
		missing, changed := inst.RefreshWorktreeMissing()
		assert.False(t, missing, "op %v", op)
		assert.False(t, changed, "op %v", op)
		flag, _ := inst.WorktreeMissing()
		assert.False(t, flag, "op %v", op)
	}
}

// A row loaded flagged worktree-missing whose tmux session is gone must load
// inert and stay listed: re-spawning into the missing directory either fails —
// and a failed load drops the row, taking its archive/kill remedies with it — or
// rebuilds the worktree behind the user's back (#5102).
func TestFromInstanceData_FlaggedRowWithoutTmuxLoadsInert(t *testing.T) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")

	var commands []string
	inner := nameKeyedExec(map[string]bool{}) // no tmux session exists
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			commands = append(commands, c.String())
			return inner.Run(c)
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			commands = append(commands, c.String())
			return inner.Output(c)
		},
	}
	previous := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, persistPtyFactory{t: t, cmdExec: cmdExec}, cmdExec)
	}
	t.Cleanup(func() { restoreTmuxSession = previous })

	repoRoot := initTempGitRepo(t)
	gitOut(t, repoRoot, "config", "user.email", "test@test.com")
	gitOut(t, repoRoot, "config", "user.name", "test")
	gitOut(t, repoRoot, "commit", "--allow-empty", "-m", "initial")
	const branch = "af/flagged-load"
	gitOut(t, repoRoot, "branch", branch)
	worktreePath := filepath.Join(t.TempDir(), "deleted-worktree")
	branchCreatedByUs := true
	const agentName = "af_flagged_load"

	restored, err := FromInstanceData(InstanceData{
		Title:    "flagged-load",
		Path:     repoRoot,
		Branch:   branch,
		Program:  "claude",
		Status:   Ready,
		TmuxName: agentName,
		Tabs:     []TabData{{Name: agentTabName, Kind: TabKindAgent, TmuxName: agentName}},
		Worktree: GitWorktreeData{
			RepoPath:          repoRoot,
			WorktreePath:      worktreePath,
			SessionName:       "flagged-load",
			BranchName:        branch,
			BranchCreatedByUs: &branchCreatedByUs,
			Missing:           true,
			MissingReason:     "tracked worktree path " + worktreePath + " does not exist (deleted outside af)",
		},
	})
	require.NoError(t, err, "a flagged row must load, not be dropped")
	require.NotNil(t, restored)
	assert.True(t, restored.Started(), "loaded like a recorded Lost row: bound, listed, killable")
	missing, _ := restored.WorktreeMissing()
	assert.True(t, missing)
	assert.Equal(t, worktreePath, restored.GetWorktreePath())
	_, statErr := os.Lstat(worktreePath)
	assert.ErrorIs(t, statErr, os.ErrNotExist, "load must not rebuild the deleted worktree")
	for _, c := range commands {
		assert.NotContains(t, c, "new-session", "load must not re-spawn the agent")
	}
}

// A flagged row whose agent is still alive must keep it across a daemon
// restart even when an account-scoped sibling tab did not survive (#5102).
// Sibling restore would respawn into the deleted path, the account-scoped
// refusal would kill the reattached agent, and the load would drop the row.
//
// Nor may the reattach's account-environment refresh strand it: a missing row's
// reattach only keeps the live pane bound so the row stays archivable, so it
// runs none of those tmux commands, and a transient failure in them can no
// longer load the row startup-unknown — which archive refuses.
func TestFromInstanceData_FlaggedRowKeepsLiveAgentDespiteAccountScopedSibling(t *testing.T) {
	for _, failRefresh := range []bool{false, true} {
		t.Run(map[bool]string{false: "refresh would succeed", true: "refresh would fail"}[failRefresh], func(t *testing.T) {
			testFlaggedRowKeepsLiveAgent(t, failRefresh)
		})
	}
}

func testFlaggedRowKeepsLiveAgent(t *testing.T, failRefresh bool) {
	log.Initialize(false)
	defer log.Close()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("SHELL", "/bin/sh")

	const agentName = "af_flagged_live"
	shellName := agentName + tmuxTabSeparator + shellTabName
	var commands []string
	inner := nameKeyedExec(map[string]bool{agentName: true}) // the sibling is gone
	refreshFailure := func(c *exec.Cmd) error {
		s := c.String()
		if failRefresh && (strings.Contains(s, "set-environment") || strings.Contains(s, "default-command")) {
			return errors.New("transient tmux failure")
		}
		return nil
	}
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			commands = append(commands, c.String())
			if err := refreshFailure(c); err != nil {
				return err
			}
			return inner.Run(c)
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			commands = append(commands, c.String())
			if err := refreshFailure(c); err != nil {
				return nil, err
			}
			return inner.Output(c)
		},
	}
	previous := restoreTmuxSession
	restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
		return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, persistPtyFactory{t: t, cmdExec: cmdExec}, cmdExec)
	}
	t.Cleanup(func() { restoreTmuxSession = previous })

	repoRoot := initTempGitRepo(t)
	gitOut(t, repoRoot, "config", "user.email", "test@test.com")
	gitOut(t, repoRoot, "config", "user.name", "test")
	gitOut(t, repoRoot, "commit", "--allow-empty", "-m", "initial")
	const branch = "af/flagged-live"
	gitOut(t, repoRoot, "branch", branch)
	worktreePath := filepath.Join(t.TempDir(), "deleted-worktree")
	branchCreatedByUs := true

	restored, err := FromInstanceData(InstanceData{
		Title:    "flagged-live",
		Path:     repoRoot,
		Branch:   branch,
		Program:  "codex",
		Account:  "work",
		Status:   Ready,
		TmuxName: agentName,
		Tabs: []TabData{
			{Name: agentTabName, Kind: TabKindAgent, TmuxName: agentName},
			{Name: shellTabName, Kind: TabKindShell, TmuxName: shellName},
		},
		Worktree: GitWorktreeData{
			RepoPath:          repoRoot,
			WorktreePath:      worktreePath,
			SessionName:       "flagged-live",
			BranchName:        branch,
			BranchCreatedByUs: &branchCreatedByUs,
			Missing:           true,
			MissingReason:     WorktreeMissingDeletedReason(worktreePath),
		},
	})
	require.NoError(t, err, "a flagged row with a live agent must load")
	require.NotNil(t, restored)
	assert.True(t, restored.Started())
	assert.False(t, restored.StartupStateUnknown(), "the row must stay archivable, not load startup-unknown")
	restored.mu.RLock()
	agentTmux := restored.tmuxLocked()
	restored.mu.RUnlock()
	require.NotNil(t, agentTmux, "the reattached agent's tmux handle must be kept")
	assert.Equal(t, agentName, agentTmux.SanitizedName())
	for _, c := range commands {
		assert.NotContains(t, c, "set-environment", "a missing row's reattach must not refresh account env")
		assert.NotContains(t, c, "new-session", "no tab may be re-spawned into the deleted worktree")
		assert.False(t, strings.Contains(c, "kill-session") && strings.Contains(c, agentName) && !strings.Contains(c, shellName),
			"the live agent must not be killed: %s", c)
	}
	_, statErr := os.Lstat(worktreePath)
	assert.ErrorIs(t, statErr, os.ErrNotExist)
}
