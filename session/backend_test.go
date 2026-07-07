package session

import (
	"bytes"
	"errors"
	"fmt"
	stdlog "log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMain(m *testing.M) {
	// #837: fail the package loudly if any test touches the real config.json.
	verifyRealConfig := testguard.ConfigTripwire()
	// #1056: fail loudly if a test leaks an af_ session onto the ambient tmux
	// server, and default the whole package into a sandboxed
	// AGENT_FACTORY_HOME so stray config/state/log writes land in a temp dir
	// instead of the developer's real one. Sandbox AFTER the tripwires
	// snapshot the real environment, BEFORE logging resolves its file path.
	verifyTmux := testguard.TmuxTripwire()
	restoreHome := testguard.SandboxHome()
	// #1122: default the whole package onto a private tmux server so a test
	// that forgets IsolateTmux can never create or sweep sessions on the
	// developer's real server.
	restoreTmux := testguard.SandboxTmux()
	log.Initialize(false)
	code := m.Run()
	log.Close()
	restoreTmux()
	restoreHome()
	if err := verifyRealConfig(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	if err := verifyTmux(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		code = 1
	}
	os.Exit(code)
}

// --- Backend interface compliance ---

func TestLocalBackendType(t *testing.T) {
	b := &LocalBackend{}
	assert.Equal(t, "local", b.Type())
}

func TestHookBackendType(t *testing.T) {
	b := &HookBackend{Hooks: config.RemoteHooks{}}
	assert.Equal(t, "remote", b.Type())
}

// TestLocalBackendKillBestEffort_TmuxFails is a regression test for issue
// #478. When the tmux teardown fails, Kill must still clear in-memory state
// and return nil so the caller can finish removing the session from the
// persisted instances.json. The failure is surfaced as a WarningLog entry
// (including the instance title) for diagnosis.
func TestLocalBackendKillBestEffort_TmuxFails(t *testing.T) {
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			// has-session reports the session still present, so the failed
			// kill-session is a GENUINE teardown failure rather than the
			// idempotent already-gone no-op (#967). Everything else fails.
			if strings.Contains(c.String(), "has-session") {
				return nil
			}
			return errors.New("kill failed")
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) {
			return nil, nil
		},
	}
	ts := tmux.NewTmuxSessionWithDeps("best-effort-tmux", "bash", nil, cmdExec)
	inst := &Instance{
		Title:   "best-effort-tmux",
		backend: &LocalBackend{},
		started: true,
		Tabs:    []*Tab{newAgentTab(ts)},
	}

	buf := captureWarningLog(t)

	require.NoError(t, inst.Kill(), "tmux cleanup failure must not block deletion")
	assert.False(t, inst.Started(), "started flag should be cleared")
	assert.Nil(t, inst.tmuxLocked(), "tmux pointer should be cleared so a retry is a clean no-op")

	logged := buf.String()
	assert.Contains(t, logged, "best-effort-tmux", "warning must include instance title for correlation in agent-factory.log")
	assert.Contains(t, logged, "tmux cleanup for tab")

	require.NoError(t, inst.Kill(), "second kill on a cleared instance must be a no-op")
}

// TestLocalBackendKillBestEffort_WorktreeFails covers the issue #478
// guarantee: when worktree cleanup genuinely fails, Kill logs a warning and
// returns nil so the caller can remove the row from the sidebar and the
// persisted record. The original #478 scenario (path exists but is no longer
// a working tree) now self-heals via the #802 ownership check — see
// TestLocalBackendKill_RecoversStaleWorktreeDir — so this test provokes a
// failure that still surfaces: the stored repo path is not a git repo, every
// git command fails, and `git worktree list` being unreadable means Cleanup
// cannot establish ownership and must NOT delete the directory.
func TestLocalBackendKillBestEffort_WorktreeFails(t *testing.T) {
	notARepo := filepath.Join(t.TempDir(), "not-a-repo")
	require.NoError(t, os.MkdirAll(notARepo, 0755))

	stalePath := filepath.Join(t.TempDir(), "stale-worktree")
	require.NoError(t, os.MkdirAll(stalePath, 0755))

	gw, err := git.NewGitWorktreeFromStorage(notARepo, stalePath, "issue-478", "issue-478-branch", "", false, false)
	require.NoError(t, err)

	inst := &Instance{
		Title:       "issue-478",
		backend:     &LocalBackend{},
		started:     true,
		gitWorktree: gw,
	}

	buf := captureWarningLog(t)

	require.NoError(t, inst.Kill(), "worktree cleanup failure must not block deletion")
	assert.False(t, inst.Started())
	assert.Nil(t, inst.gitWorktree, "git worktree pointer should be cleared")

	logged := buf.String()
	assert.Contains(t, logged, "issue-478", "warning must include instance title")
	assert.Contains(t, logged, "git worktree cleanup failed")
	assert.Contains(t, logged, "not a git repository", "warning should preserve the underlying git error so users can diagnose")

	// Safety: with ownership unknown (worktree list unreadable), the
	// directory must be left alone.
	_, statErr := os.Stat(stalePath)
	assert.NoError(t, statErr, "Cleanup must not delete a directory whose git ownership it cannot establish")
}

// TestLocalBackendKill_RecoversStaleWorktreeDir pins the #802 behavior change
// to the original #478 scenario: the stored path exists on disk but git does
// not register it as a worktree (`worktree remove` fails, `worktree list`
// omits it). Instead of surfacing "is not a working tree" and leaking the
// directory, Kill now removes it and completes cleanly.
func TestLocalBackendKill_RecoversStaleWorktreeDir(t *testing.T) {
	repoRoot := initTempGitRepo(t)

	stalePath := filepath.Join(t.TempDir(), "stale-worktree")
	require.NoError(t, os.MkdirAll(stalePath, 0755))

	gw, err := git.NewGitWorktreeFromStorage(repoRoot, stalePath, "stale-dir", "stale-dir-branch", "", false, false)
	require.NoError(t, err)

	inst := &Instance{
		Title:       "stale-dir",
		backend:     &LocalBackend{},
		started:     true,
		gitWorktree: gw,
	}

	buf := captureWarningLog(t)

	require.NoError(t, inst.Kill())
	assert.Nil(t, inst.gitWorktree)

	_, statErr := os.Stat(stalePath)
	assert.True(t, os.IsNotExist(statErr),
		"Kill must remove a leftover directory git no longer registers as a worktree (#802)")
	assert.NotContains(t, buf.String(), "git worktree cleanup failed",
		"recovered cleanup should not warn")
}

// TestLocalBackendKillBestEffort_BothFail covers the multi-component failure
// case: both tmux and worktree cleanup blow up, and Kill should still return
// nil with a warning per component.
func TestLocalBackendKillBestEffort_BothFail(t *testing.T) {
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			// has-session reports present so the failed kill-session is a
			// genuine teardown failure, not the #967 idempotent no-op.
			if strings.Contains(c.String(), "has-session") {
				return nil
			}
			return errors.New("kill failed")
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) {
			return nil, nil
		},
	}
	ts := tmux.NewTmuxSessionWithDeps("both-fail", "bash", nil, cmdExec)

	// Non-repo path: every git command fails, so the worktree cleanup error
	// surfaces (ownership unknown — see TestLocalBackendKillBestEffort_WorktreeFails).
	notARepo := filepath.Join(t.TempDir(), "not-a-repo")
	require.NoError(t, os.MkdirAll(notARepo, 0755))
	stalePath := filepath.Join(t.TempDir(), "stale")
	require.NoError(t, os.MkdirAll(stalePath, 0755))
	gw, err := git.NewGitWorktreeFromStorage(notARepo, stalePath, "both-fail", "both-fail-branch", "", false, false)
	require.NoError(t, err)

	inst := &Instance{
		Title:       "both-fail",
		backend:     &LocalBackend{},
		started:     true,
		Tabs:        []*Tab{newAgentTab(ts)},
		gitWorktree: gw,
	}

	buf := captureWarningLog(t)

	require.NoError(t, inst.Kill())
	assert.Nil(t, inst.tmuxLocked())
	assert.Nil(t, inst.gitWorktree)

	logged := buf.String()
	assert.Contains(t, logged, "tmux cleanup for tab")
	assert.Contains(t, logged, "git worktree cleanup failed")
	assert.Equal(t, 2, strings.Count(logged, `kill "both-fail":`), "title should appear in both component warnings")
}

// captureWarningLog redirects log.WarningLog to a buffer for the duration of
// the test and returns the buffer. Restoration happens via t.Cleanup.
func captureWarningLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	orig := log.WarningLog
	log.WarningLog = stdlog.New(&buf, "WARNING: ", 0)
	t.Cleanup(func() { log.WarningLog = orig })
	return &buf
}

// initTempGitRepo initializes an empty git repo in a temp directory and
// returns its absolute path. Used by best-effort Kill tests that need a
// real repo path for git worktree commands to dispatch against.
func initTempGitRepo(t *testing.T) string {
	t.Helper()
	repoRoot := filepath.Join(t.TempDir(), "repo")
	require.NoError(t, os.MkdirAll(repoRoot, 0755))
	cmd := exec.Command("git", "init")
	cmd.Dir = repoRoot
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return repoRoot
}

// --- IsRemote helper ---

func TestIsRemote(t *testing.T) {
	t.Run("local backend", func(t *testing.T) {
		i := &Instance{backend: &LocalBackend{}}
		assert.False(t, i.IsRemote())
	})
	t.Run("hook backend", func(t *testing.T) {
		i := &Instance{backend: &HookBackend{Hooks: config.RemoteHooks{}}}
		assert.True(t, i.IsRemote())
	})
	t.Run("nil backend", func(t *testing.T) {
		i := &Instance{}
		assert.False(t, i.IsRemote())
	})
}

// --- HookBackend with real scripts ---

// writeScript writes an executable shell script to the given path.
func writeScript(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	err := os.WriteFile(path, []byte("#!/bin/sh\n"+content), 0755)
	require.NoError(t, err)
	return path
}

// makeHooks creates a set of fake hook scripts in a temp dir and returns
// a HookBackend configured to use them.
func makeHooks(t *testing.T) *HookBackend {
	t.Helper()
	return makeHooksWithListName(t, Slugify("test-session"))
}

// makeHooksWithListName is like makeHooks but lets the caller control
// which session name the list_cmd will report.
func makeHooksWithListName(t *testing.T, listName string) *HookBackend {
	t.Helper()
	dir := t.TempDir()

	launchCmd := writeScript(t, dir, "launch.sh",
		`echo '{"name": "'"$2"'", "status": "running"}'`)
	listCmd := writeScript(t, dir, "list.sh",
		`echo '[{"name": "`+listName+`", "status": "running"}]'`)
	attachCmd := writeScript(t, dir, "attach.sh",
		`echo "attached to $1"; sleep 0.1`)
	deleteCmd := writeScript(t, dir, "delete.sh",
		`echo '{"name": "'"$2"'", "deleted": true}'`)

	return &HookBackend{
		Hooks: config.RemoteHooks{
			LaunchCmd: launchCmd,
			ListCmd:   listCmd,
			AttachCmd: attachCmd,
			DeleteCmd: deleteCmd,
		},
	}
}
