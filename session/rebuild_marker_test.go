package session

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

var testKey = RebuildTeardownKey("/ws/a", "t")

func markerPath(dir string) string { return markerPathFor(dir, testKey) }

func markerPathFor(dir, key string) string { return filepath.Join(dir, RebuildTeardownMarkerName(key)) }

// The sandbox-side instance can make the arm durable: before this, the very first
// SetRebuildTeardownPending(true) of a relaunch refused (#5824).
func TestDurableRebuildTeardownMarkerArmsAndDischarges(t *testing.T) {
	dir := t.TempDir()
	inst := &Instance{Title: "t"}

	require.Error(t, inst.SetRebuildTeardownPending(true), "premise: with no callback installed the arm is refused")
	inst = &Instance{Title: "t"}

	require.NoError(t, inst.InstallDurableRebuildTeardown(dir, testKey))
	require.NoError(t, inst.SetRebuildTeardownPending(true))
	info, err := os.Stat(markerPath(dir))
	require.NoError(t, err, "the arm must be on disk before the rebuild it guards")
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	require.NoError(t, inst.SetRebuildTeardownPending(false))
	assert.NoFileExists(t, markerPath(dir), "discharging the obligation removes the marker")
}

// A crash mid-rebuild leaves the marker; the restarted agent-server must re-arm
// the obligation before anything can respawn.
func TestDurableRebuildTeardownSurvivesARestart(t *testing.T) {
	dir := t.TempDir()
	first := &Instance{Title: "t"}
	require.NoError(t, first.InstallDurableRebuildTeardown(dir, testKey))
	require.NoError(t, first.SetRebuildTeardownPending(true))
	// ...the agent-server dies here, mid-rebuild.

	second := &Instance{Title: "t"}
	require.False(t, second.RebuildTeardownPending(), "premise: a fresh process knows nothing")
	require.NoError(t, second.InstallDurableRebuildTeardown(dir, testKey))
	assert.True(t, second.RebuildTeardownPending(), "the owed teardown must be restored from the marker")
	assert.FileExists(t, markerPath(dir))

	// A marker that is not a regular file is not guessed at.
	bad := t.TempDir()
	require.NoError(t, os.Mkdir(markerPath(bad), 0o700))
	assert.Error(t, (&Instance{Title: "t"}).InstallDurableRebuildTeardown(bad, testKey))
	assert.Error(t, (&Instance{Title: "t"}).InstallDurableRebuildTeardown("", testKey))
}

// The property of #5824: a relaunch on a sandbox whose worktree was deleted
// rebuilds it on the recorded branch, with the same durable-checkpoint guarantee
// the daemon-side respawn has — the marker is armed before the rebuild and
// discharged after it.
func TestRelaunchRebuildsADeletedWorktreeOnTheRecordedBranch(t *testing.T) {
	for _, install := range []bool{false, true} {
		name := "without durable callbacks"
		if install {
			name = "with the sandbox's durable callbacks"
		}
		t.Run(name, func(t *testing.T) {
			log.Initialize(false)
			defer log.Close()
			t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

			repoRoot := initTempGitRepo(t)
			gitOut(t, repoRoot, "config", "user.email", "test@test.com")
			gitOut(t, repoRoot, "config", "user.name", "test")
			gitOut(t, repoRoot, "commit", "--allow-empty", "-m", "initial")
			const branch = "af/relaunch-rebuild"
			gitOut(t, repoRoot, "branch", branch)

			const agentName = "af_relaunch_rebuild"
			worktreePath := filepath.Join(t.TempDir(), "repo-relaunch-rebuild") // never created: deleted
			branchCreatedByUs := true
			data := InstanceData{
				Title: "relaunch-rebuild", Path: repoRoot, Branch: branch,
				Program: tmux.ProgramCodex, Status: Lost, TmuxName: agentName,
				Tabs: []TabData{{
					Name: agentTabName, Kind: TabKindAgent, TmuxName: agentName,
					Conversation: &AgentConversationData{
						Agent: tmux.ProgramCodex, ID: "019f386f-7206-7fc2-803b-f7045e07a242",
						CaptureKind: ConversationCaptureCodexRollout,
					},
				}},
				Worktree: GitWorktreeData{
					RepoPath: repoRoot, WorktreePath: worktreePath, SessionName: "relaunch-rebuild",
					BranchName: branch, BranchCreatedByUs: &branchCreatedByUs,
				},
			}
			var newSessions int
			var spawns []string
			exec := deadPaneAwareExec(map[string]bool{}, recordingExec(map[string]bool{}, &newSessions, &spawns))
			pty := persistPtyFactory{t: t, cmdExec: exec}
			prev := restoreTmuxSession
			restoreTmuxSession = func(name, program string) *tmux.TmuxSession {
				return tmux.NewTmuxSessionFromSanitizedNameWithDeps(name, program, pty, exec)
			}
			t.Cleanup(func() { restoreTmuxSession = prev })

			inst, err := FromInstanceData(data)
			require.NoError(t, err)
			require.NoDirExists(t, worktreePath, "premise: the worktree is gone")

			markerDir := t.TempDir()
			if install {
				require.NoError(t, inst.InstallDurableRebuildTeardown(markerDir, testKey))
			}
			// The exact call relaunchLocalAgent makes once the stop is confirmed.
			err = inst.currentBackend().(*LocalBackend).Respawn(inst)

			if !install {
				require.ErrorContains(t, err, "no durable checkpoint",
					"the regression: with no callbacks every relaunch of a deleted worktree refused")
				return
			}
			require.NoError(t, err)
			assert.DirExists(t, worktreePath, "the workspace is rebuilt")
			assert.Equal(t, branch, gitOut(t, worktreePath, "rev-parse", "--abbrev-ref", "HEAD"),
				"on the recorded branch")
			assert.NoFileExists(t, markerPath(markerDir), "the teardown obligation is discharged once the rebuild settles")
			assert.False(t, inst.RebuildTeardownPending())
		})
	}
}

// #5842: two sessions whose agent-servers share one directory (an installed af, not
// a per-session staged binary) hold two independent obligations. One's arm is not
// the other's, and clearing one leaves the other.
func TestRebuildTeardownMarkersAreIndependentPerSession(t *testing.T) {
	shared := t.TempDir() // the installed binary's directory, common to both
	keyA, keyB := RebuildTeardownKey("/ws/a", "feature"), RebuildTeardownKey("/ws/b", "feature")
	require.NotEqual(t, keyA, keyB, "the same title in two workspaces is two sessions")
	require.NotEqual(t, keyA, RebuildTeardownKey("/ws/a", "other"), "two titles in one workspace are two sessions")
	require.Equal(t, keyA, RebuildTeardownKey("/ws/a/", "feature"), "the key is stable across spellings of one path")

	a, b := &Instance{Title: "feature"}, &Instance{Title: "feature"}
	require.NoError(t, a.InstallDurableRebuildTeardown(shared, keyA))
	require.NoError(t, b.InstallDurableRebuildTeardown(shared, keyB))

	// Each arms its own obligation.
	require.NoError(t, a.SetRebuildTeardownPending(true))
	require.NoError(t, b.SetRebuildTeardownPending(true))
	assert.FileExists(t, markerPathFor(shared, keyA))
	assert.FileExists(t, markerPathFor(shared, keyB))

	// Clearing A leaves B's obligation intact.
	require.NoError(t, a.SetRebuildTeardownPending(false))
	assert.NoFileExists(t, markerPathFor(shared, keyA))
	assert.FileExists(t, markerPathFor(shared, keyB), "A's discharge must not delete B's owed teardown")

	// A restart of A does not inherit B's obligation; a restart of B recovers its own.
	a2, b2 := &Instance{Title: "feature"}, &Instance{Title: "feature"}
	require.NoError(t, a2.InstallDurableRebuildTeardown(shared, keyA))
	assert.False(t, a2.RebuildTeardownPending(), "A must not read B's marker as its own")
	require.NoError(t, b2.InstallDurableRebuildTeardown(shared, keyB))
	assert.True(t, b2.RebuildTeardownPending(), "B's obligation survives A's activity and B's own restart")

	// A key is required: a missing or hostile one is refused rather than guessed.
	for _, bad := range []string{"", "../x", "NOTHEX", "a/b"} {
		assert.Error(t, (&Instance{Title: "t"}).InstallDurableRebuildTeardown(shared, bad), "%q", bad)
	}
}
