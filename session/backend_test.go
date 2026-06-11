package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	stdlog "log"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd"
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
	log.Initialize(false)
	defer log.Close()
	// #837: fail the package loudly if any test touches the real config.json.
	verifyRealConfig := testguard.ConfigTripwire()
	code := m.Run()
	if err := verifyRealConfig(); err != nil {
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
		RunFunc: func(*exec.Cmd) error {
			return errors.New("kill failed")
		},
		OutputFunc: func(*exec.Cmd) ([]byte, error) {
			return nil, nil
		},
	}
	ts := tmux.NewTmuxSessionWithDeps("best-effort-tmux", "bash", nil, cmdExec)
	inst := &Instance{
		Title:       "best-effort-tmux",
		backend:     &LocalBackend{},
		started:     true,
		tmuxSession: ts,
	}

	buf := captureWarningLog(t)

	require.NoError(t, inst.Kill(), "tmux cleanup failure must not block deletion")
	assert.False(t, inst.Started(), "started flag should be cleared")
	assert.Nil(t, inst.tmuxSession, "tmux pointer should be cleared so a retry is a clean no-op")

	logged := buf.String()
	assert.Contains(t, logged, "best-effort-tmux", "warning must include instance title for correlation in agent-factory.log")
	assert.Contains(t, logged, "tmux cleanup failed")

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
		RunFunc: func(*exec.Cmd) error {
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
		tmuxSession: ts,
		gitWorktree: gw,
	}

	buf := captureWarningLog(t)

	require.NoError(t, inst.Kill())
	assert.Nil(t, inst.tmuxSession)
	assert.Nil(t, inst.gitWorktree)

	logged := buf.String()
	assert.Contains(t, logged, "tmux cleanup failed")
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
	return makeHooksWithListName(t, slugify("test-session"))
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

func TestHookBackendStartFirstTime(t *testing.T) {
	b := makeHooks(t)
	i := &Instance{
		Title:   "test-session",
		Path:    t.TempDir(),
		backend: b,
	}

	err := b.Start(i, true)
	require.NoError(t, err)
	assert.True(t, i.Started())
	assert.Equal(t, slugify("test-session"), i.Branch)
	assert.NotNil(t, i.remoteMeta)
	assert.Equal(t, "running", i.remoteMeta["status"])

	// Cleanup
	b.closePTY(i.Title)
}

func TestHookBackendStartRestore(t *testing.T) {
	b := makeHooks(t)
	i := &Instance{
		Title:   "test-session",
		Path:    t.TempDir(),
		backend: b,
	}

	err := b.Start(i, false)
	require.NoError(t, err)
	assert.True(t, i.Started())

	b.closePTY(i.Title)
}

// TestHookBackendStartRestoreDeadSession is a regression test for #645:
// when list_cmd no longer reports the persisted remote session, Start in
// the restore branch must return an error rather than silently marking the
// instance as Ready. Without this guard, deleted/expired remote sessions
// were restored with a green Ready dot in the sidebar even though attaching
// was a silent no-op.
//
// Since #841 the error must also name what list_cmd DID return, so a
// hook-script rename (list_cmd reporting the session under a new name) is
// self-diagnosing instead of requiring a manual list_cmd run.
func TestHookBackendStartRestoreDeadSession(t *testing.T) {
	// list_cmd reports a different session, so our instance looks "dead".
	b := makeHooksWithListName(t, "some-other-session")
	i := &Instance{
		Title:   "test-session",
		Path:    t.TempDir(),
		backend: b,
	}

	err := b.Start(i, false)
	require.Error(t, err, "restore must fail when list_cmd does not report the session")
	assert.Contains(t, err.Error(), "no longer exists")
	assert.Contains(t, err.Error(), "listed: some-other-session",
		"error must include the names list_cmd did report so a rename mismatch is self-diagnosing (#841)")
	assert.False(t, i.Started(), "instance must not be marked Started when remote session is gone")
}

// TestHookBackendStartRestoreEmptyList covers the genuinely-empty-list leg of
// #841: when list_cmd runs fine but reports zero sessions, restore keeps the
// plain "no longer exists" message without a bogus "(listed: ...)" suffix.
func TestHookBackendStartRestoreEmptyList(t *testing.T) {
	dir := t.TempDir()
	listCmd := writeScript(t, dir, "list.sh", `echo '[]'`)
	attachCmd := writeScript(t, dir, "attach.sh", `echo "attached"; sleep 0.1`)
	b := &HookBackend{
		Hooks: config.RemoteHooks{
			ListCmd:   listCmd,
			AttachCmd: attachCmd,
		},
	}
	i := &Instance{
		Title:   "test-session",
		Path:    t.TempDir(),
		backend: b,
	}

	err := b.Start(i, false)
	require.Error(t, err)
	assert.False(t, i.Started())
	assert.Contains(t, err.Error(), "no longer exists")
	assert.NotContains(t, err.Error(), "listed:",
		"an empty list must not grow a listed-names suffix")
}

// TestHookBackendStartRestoreListCmdFails covers the second leg of #645:
// when list_cmd itself fails (e.g. network/auth error), we treat the
// session as not-alive and refuse to mark it Started rather than
// optimistically restoring a possibly-dead session.
//
// Since #841 the error must say that list_cmd failed — NOT the misleading
// "no longer exists in list_cmd output", which implies list_cmd ran and the
// session was deleted remotely when in fact nothing was verified.
func TestHookBackendStartRestoreListCmdFails(t *testing.T) {
	dir := t.TempDir()
	listCmd := writeScript(t, dir, "list.sh", `echo "ssh: connect refused" >&2; exit 1`)
	attachCmd := writeScript(t, dir, "attach.sh", `echo "attached"; sleep 0.1`)
	b := &HookBackend{
		Hooks: config.RemoteHooks{
			ListCmd:   listCmd,
			AttachCmd: attachCmd,
		},
	}
	i := &Instance{
		Title:   "test-session",
		Path:    t.TempDir(),
		backend: b,
	}

	err := b.Start(i, false)
	require.Error(t, err)
	assert.False(t, i.Started())
	assert.Contains(t, err.Error(), "cannot verify remote session")
	assert.Contains(t, err.Error(), "list_cmd failed",
		"an exec failure must be reported as a list_cmd failure (#841)")
	assert.Contains(t, err.Error(), "ssh: connect refused",
		"the failure tail must carry list_cmd's own output")
	assert.NotContains(t, err.Error(), "no longer exists",
		"an exec failure must not be reported as a remotely-deleted session (#841)")
}

// TestHookBackendStartRestoreListCmdNoJSON covers the unparseable-output leg
// of #841: list_cmd exits 0 but emits no JSON. Nothing was verified, so the
// error must blame list_cmd's output, not claim the session was deleted.
func TestHookBackendStartRestoreListCmdNoJSON(t *testing.T) {
	dir := t.TempDir()
	listCmd := writeScript(t, dir, "list.sh", `echo "usage: mytool list [--json]"`)
	attachCmd := writeScript(t, dir, "attach.sh", `echo "attached"; sleep 0.1`)
	b := &HookBackend{
		Hooks: config.RemoteHooks{
			ListCmd:   listCmd,
			AttachCmd: attachCmd,
		},
	}
	i := &Instance{
		Title:   "test-session",
		Path:    t.TempDir(),
		backend: b,
	}

	err := b.Start(i, false)
	require.Error(t, err)
	assert.False(t, i.Started())
	assert.Contains(t, err.Error(), "cannot verify remote session")
	assert.Contains(t, err.Error(), "no JSON")
	assert.NotContains(t, err.Error(), "no longer exists",
		"unparseable list_cmd output must not be reported as a remotely-deleted session (#841)")
}

// TestFormatListedNames pins the listed-names suffix: empty (and nil) lists
// keep the original bare message, short lists are joined verbatim, and lists
// past the cap are truncated with a count so a busy remote host cannot bloat
// the error (#841).
func TestFormatListedNames(t *testing.T) {
	assert.Equal(t, "", formatListedNames(nil))
	assert.Equal(t, "", formatListedNames([]string{}))
	assert.Equal(t, " (listed: a)", formatListedNames([]string{"a"}))
	assert.Equal(t, " (listed: a, b, c, d, e)",
		formatListedNames([]string{"a", "b", "c", "d", "e"}))
	assert.Equal(t, " (listed: a, b, c, d, e, and 2 more)",
		formatListedNames([]string{"a", "b", "c", "d", "e", "f", "g"}))
}

// TestHookBackendStartRestoreEmptyListCmd is the regression test for #753:
// list_cmd is optional at config-validation time (import/sync treat empty as
// "nothing to enumerate", #738), but restore has no other way to verify
// liveness. With an empty list_cmd, restore must fail fast with an actionable
// error that names the missing field — not the misleading "no longer exists in
// list_cmd output" message, which falsely implies the remote session was
// deleted (it was the local config that was incomplete).
func TestHookBackendStartRestoreEmptyListCmd(t *testing.T) {
	dir := t.TempDir()
	attachCmd := writeScript(t, dir, "attach.sh", `echo "attached"; sleep 0.1`)
	b := &HookBackend{
		Hooks: config.RemoteHooks{
			// ListCmd intentionally empty.
			AttachCmd: attachCmd,
		},
	}
	i := &Instance{
		Title:   "my-session",
		Path:    t.TempDir(),
		backend: b,
	}

	err := b.Start(i, false)
	require.Error(t, err)
	assert.False(t, i.Started())
	assert.Contains(t, err.Error(), "list_cmd is required for restore",
		"error must explain that restore needs list_cmd")
	assert.Contains(t, err.Error(), "list_cmd",
		"error must name the missing field")
	// Must NOT use the misleading "no longer exists" wording reserved for the
	// case where list_cmd is present but does not list the session.
	assert.NotContains(t, err.Error(), "no longer exists",
		"empty list_cmd must not be reported as a remotely-deleted session")
}

// TestHookBackendStartRestoreListCmdHangs covers the timeout path: when
// list_cmd takes longer than restoreAliveTimeout, restore must return an
// error rather than blocking the TUI startup indefinitely for every
// persisted instance (#645).
func TestHookBackendStartRestoreListCmdHangs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timeout-bound test in short mode")
	}

	dir := t.TempDir()
	// Sleep long enough that the 2s restoreAliveTimeout fires first.
	listCmd := writeScript(t, dir, "list.sh", `sleep 30; echo '[]'`)
	attachCmd := writeScript(t, dir, "attach.sh", `echo "attached"; sleep 0.1`)
	b := &HookBackend{
		Hooks: config.RemoteHooks{
			ListCmd:   listCmd,
			AttachCmd: attachCmd,
		},
	}
	i := &Instance{
		Title:   "test-session",
		Path:    t.TempDir(),
		backend: b,
	}

	start := time.Now()
	err := b.Start(i, false)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.False(t, i.Started())
	// The bound is the TUI startup latency promise: even with a hung
	// list_cmd, restore must return within a small multiple of
	// restoreAliveTimeout.
	assert.Less(t, elapsed, 5*time.Second,
		"restore must abort within timeout when list_cmd hangs (got %v)", elapsed)
	// A timeout means nothing was verified, so it must be reported as a
	// list_cmd failure, not as a remotely-deleted session (#841).
	assert.NotContains(t, err.Error(), "no longer exists",
		"a timed-out list_cmd must not be reported as a remotely-deleted session")
}

// TestHookBackendIsAliveListCmdHangs is the runtime analogue of
// TestHookBackendStartRestoreListCmdHangs and the regression test for #666:
// background reconciliation ticks call IsAlive every 3-5s on the TUI event
// loop, so a list_cmd that SSHs to a wedged host must not be allowed to
// freeze the UI indefinitely.
func TestHookBackendIsAliveListCmdHangs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timeout-bound test in short mode")
	}

	dir := t.TempDir()
	// Sleep well past runtimeAliveTimeout so the timeout, not the script,
	// is what ends the call.
	listCmd := writeScript(t, dir, "list.sh", `sleep 30; echo '[]'`)
	b := &HookBackend{
		Hooks: config.RemoteHooks{ListCmd: listCmd},
	}
	i := &Instance{
		Title:   "test-session",
		Path:    t.TempDir(),
		backend: b,
	}

	start := time.Now()
	alive := b.IsAlive(i)
	elapsed := time.Since(start)

	assert.False(t, alive, "IsAlive must report false when list_cmd hangs past timeout")
	// runtimeAliveTimeout is 5s; allow a small buffer for WaitDelay (500ms)
	// plus scheduling slack. The key bound is that IsAlive must NOT block
	// anywhere near the script's 30s sleep — that was the #666 freeze.
	assert.Less(t, elapsed, runtimeAliveTimeout+2*time.Second,
		"IsAlive must return within runtimeAliveTimeout+tolerance when list_cmd hangs (got %v)", elapsed)
}

func TestHookBackendStartEmptyTitle(t *testing.T) {
	b := makeHooks(t)
	i := &Instance{
		Title:   "",
		backend: b,
	}
	err := b.Start(i, true)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "empty")
}

func TestHookBackendKill(t *testing.T) {
	b := makeHooks(t)
	i := &Instance{
		Title:   "test-session",
		Path:    t.TempDir(),
		backend: b,
	}

	// Start first so there's something to kill
	err := b.Start(i, true)
	require.NoError(t, err)
	assert.True(t, i.Started())

	err = b.Kill(i)
	require.NoError(t, err)
	assert.False(t, i.Started())
	assert.Nil(t, i.remoteMeta)
}

// TestHookBackendKillUnallocatedSkipsDeleteCmd verifies that Kill on an
// instance that was never successfully Start'd (no remoteMeta) does not
// invoke delete_cmd. Otherwise we'd ask the user-provided cleanup script
// to delete a slug it never saw — surfacing as a spurious failure on a
// kill that had nothing to do. See issue #518.
func TestHookBackendKillUnallocatedSkipsDeleteCmd(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "delete-ran")
	// delete_cmd touches a sentinel file iff it runs; the test fails the
	// kill guard by detecting the sentinel afterward.
	deleteCmd := writeScript(t, dir, "delete.sh", `touch `+sentinel+`; echo '{"deleted": true}'`)

	b := &HookBackend{
		Hooks: config.RemoteHooks{
			DeleteCmd: deleteCmd,
		},
	}
	i := &Instance{
		Title:   "never-started",
		Path:    t.TempDir(),
		backend: b,
	}

	// Sanity: no Start call, so remoteMeta is nil.
	require.Nil(t, i.remoteMeta)

	err := b.Kill(i)
	require.NoError(t, err)

	_, statErr := os.Stat(sentinel)
	assert.True(t, os.IsNotExist(statErr),
		"delete_cmd should not run when no remote session was allocated (sentinel exists: %v)", statErr)
	assert.False(t, i.Started())
	assert.Nil(t, i.remoteMeta)
}

func TestHookBackendPreview(t *testing.T) {
	b := makeHooks(t)
	i := &Instance{
		Title:   "test-session",
		Path:    t.TempDir(),
		backend: b,
	}

	err := b.Start(i, true)
	require.NoError(t, err)

	// Give the background PTY reader a moment to capture output
	time.Sleep(500 * time.Millisecond)

	content, err := b.Preview(i)
	require.NoError(t, err)
	// The attach.sh script echoes "attached to test-session"
	assert.Contains(t, content, "attached to test-session")

	b.closePTY(i.Title)
}

func TestHookBackendPreviewFullHistory(t *testing.T) {
	b := makeHooks(t)
	i := &Instance{
		Title:   "test-session",
		Path:    t.TempDir(),
		backend: b,
	}

	err := b.Start(i, true)
	require.NoError(t, err)
	time.Sleep(500 * time.Millisecond)

	content, err := b.PreviewFullHistory(i)
	require.NoError(t, err)
	assert.Contains(t, content, "attached to test-session")

	b.closePTY(i.Title)
}

func TestHookBackendPreviewNoPTY(t *testing.T) {
	b := &HookBackend{Hooks: config.RemoteHooks{}}
	i := &Instance{Title: "no-pty", backend: b}

	content, err := b.Preview(i)
	require.NoError(t, err)
	assert.Equal(t, "", content)
}

func TestHookBackendIsAlive(t *testing.T) {
	b := makeHooks(t)
	i := &Instance{
		Title:   "test-session",
		backend: b,
	}

	alive := b.IsAlive(i)
	assert.True(t, alive)
}

func TestHookBackendIsAliveNotFound(t *testing.T) {
	b := makeHooks(t)
	i := &Instance{
		Title:   "nonexistent-session",
		backend: b,
	}

	alive := b.IsAlive(i)
	assert.False(t, alive)
}

func TestHookBackendIsAliveFailedCmd(t *testing.T) {
	dir := t.TempDir()
	listCmd := writeScript(t, dir, "list.sh", `exit 1`)
	b := &HookBackend{
		Hooks: config.RemoteHooks{ListCmd: listCmd},
	}
	i := &Instance{Title: "test", backend: b}
	assert.False(t, b.IsAlive(i))
}

func TestHookBackendHasUpdated(t *testing.T) {
	b := &HookBackend{Hooks: config.RemoteHooks{}}
	i := &Instance{backend: b}
	updated, hasPrompt := b.HasUpdated(i)
	assert.False(t, updated)
	assert.False(t, hasPrompt)
}

func TestHookBackendSendPromptReturnsError(t *testing.T) {
	b := &HookBackend{Hooks: config.RemoteHooks{}}
	i := &Instance{backend: b}
	err := b.SendPrompt(i, "test")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not supported")
}

func TestHookBackendSendPromptCommandReturnsError(t *testing.T) {
	b := &HookBackend{Hooks: config.RemoteHooks{}}
	i := &Instance{backend: b}
	err := b.SendPromptCommand(i, "test")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not supported")
}

func TestHookBackendSendKeysReturnsError(t *testing.T) {
	b := &HookBackend{Hooks: config.RemoteHooks{}}
	i := &Instance{backend: b}
	err := b.SendKeys(i, "test")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not supported")
}

// Regression test for #267: SendKeys on a remote instance must return an
// error rather than panic with a nil tmuxSession dereference.
func TestInstanceSendKeysRemoteNoPanic(t *testing.T) {
	b := &HookBackend{Hooks: config.RemoteHooks{}}
	i := &Instance{
		Title:   "remote-inst",
		backend: b,
		started: true, // simulate remote instance started without tmux session
	}
	err := i.SendKeys("hello")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not supported")
}

// Regression test for #267: LocalBackend.HasUpdated must not panic when
// started=true but tmuxSession=nil (it should report no updates instead).
func TestLocalBackendHasUpdatedNilTmuxSession(t *testing.T) {
	b := &LocalBackend{}
	i := &Instance{
		Title:   "local-inst",
		backend: b,
		started: true, // tmuxSession intentionally left nil
	}
	updated, hasPrompt := b.HasUpdated(i)
	assert.False(t, updated)
	assert.False(t, hasPrompt)
}

// Regression test for #329: LocalBackend.TapEnter must not panic when
// started=true and AutoYes=true but tmuxSession=nil (it should return
// early instead).
func TestLocalBackendTapEnterNilTmuxSession(t *testing.T) {
	b := &LocalBackend{}
	i := &Instance{
		Title:   "local-inst",
		backend: b,
		started: true,
		AutoYes: true, // tmuxSession intentionally left nil
	}
	// Should not panic.
	b.TapEnter(i)
}

// LocalBackend.SendKeys should return an error (not panic) when the tmux
// session has not been initialized yet.
func TestLocalBackendSendKeysNilTmuxSession(t *testing.T) {
	b := &LocalBackend{}
	i := &Instance{
		Title:   "local-inst",
		backend: b,
		started: true, // tmuxSession intentionally left nil
	}
	err := b.SendKeys(i, "hello")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "tmux session not initialized")
}

// LocalBackend.SendKeys should return an error when the instance has not
// been started yet.
func TestLocalBackendSendKeysNotStarted(t *testing.T) {
	b := &LocalBackend{}
	i := &Instance{
		Title:   "local-inst",
		backend: b,
	}
	err := b.SendKeys(i, "hello")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "has not been started")
}

func TestHookBackendSetPreviewSizeIsNoop(t *testing.T) {
	b := &HookBackend{Hooks: config.RemoteHooks{}}
	i := &Instance{backend: b}
	err := b.SetPreviewSize(i, 80, 24)
	assert.NoError(t, err)
}

func TestHookBackendCheckAndHandleTrustPrompt(t *testing.T) {
	b := &HookBackend{Hooks: config.RemoteHooks{}}
	i := &Instance{backend: b}
	assert.False(t, b.CheckAndHandleTrustPrompt(i))
}

// TestLocalBackendCheckAndHandleTrustPromptDispatch verifies which agents are
// routed through the tmux trust handler. Codex was excluded until #729, so a
// codex trust/confirmation dialog was never dismissed even though
// isReadyContent could surface it — letting the next user prompt get typed
// into the dialog. Dispatch is observed by whether the handler reaches the
// pane capture (tmux capture-pane); agents not in the set short-circuit to
// false without capturing. Mirrors the NewTmuxSessionWithDeps + MockCmdExec
// pattern used by the Kill best-effort tests above.
func TestLocalBackendCheckAndHandleTrustPromptDispatch(t *testing.T) {
	cases := []struct {
		name         string
		program      string
		wantDispatch bool
	}{
		{"claude dispatches", tmux.ProgramClaude, true},
		{"codex dispatches (#729)", tmux.ProgramCodex, true},
		{"aider dispatches", tmux.ProgramAider, true},
		{"gemini dispatches", tmux.ProgramGemini, true},
		{"legacy codex path dispatches (#729)", "/usr/local/bin/codex", true},
		{"unknown program does not dispatch", "some-other-tool", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var captured bool
			cmdExec := cmd_test.MockCmdExec{
				OutputFunc: func(*exec.Cmd) ([]byte, error) {
					captured = true
					// An idle pane with no trust prompt: the handler returns
					// false regardless, but the capture call itself is the
					// observable proof that the agent was dispatched.
					return []byte("idle pane, no trust prompt"), nil
				},
				RunFunc: func(*exec.Cmd) error { return nil },
			}
			ts := tmux.NewTmuxSessionWithDeps("trust-dispatch", tc.program, nil, cmdExec)
			inst := &Instance{
				Title:       "trust-dispatch",
				Program:     tc.program,
				backend:     &LocalBackend{},
				started:     true,
				tmuxSession: ts,
			}

			inst.CheckAndHandleTrustPrompt()

			assert.Equal(t, tc.wantDispatch, captured,
				"capture-pane should run iff the agent is in the trust-handling set")
		})
	}
}

func TestHookBackendTapEnterIsNoop(t *testing.T) {
	b := &HookBackend{Hooks: config.RemoteHooks{}}
	i := &Instance{backend: b}
	// Should not panic
	b.TapEnter(i)
}

func TestHookBackendAttachNotStarted(t *testing.T) {
	b := &HookBackend{Hooks: config.RemoteHooks{}}
	i := &Instance{backend: b, started: false}
	_, err := b.Attach(i)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not been started")
}

// --- Serialization round-trip ---

func TestToInstanceDataIncludesBackendType(t *testing.T) {
	t.Run("local", func(t *testing.T) {
		i := &Instance{
			Title:   "local-inst",
			backend: &LocalBackend{},
		}
		data := i.ToInstanceData()
		assert.Equal(t, "local", data.BackendType)
		assert.Nil(t, data.RemoteMeta)
	})

	t.Run("remote", func(t *testing.T) {
		meta := map[string]interface{}{"name": "test", "status": "running"}
		i := &Instance{
			Title:      "remote-inst",
			backend:    &HookBackend{Hooks: config.RemoteHooks{}},
			remoteMeta: meta,
		}
		data := i.ToInstanceData()
		assert.Equal(t, "remote", data.BackendType)
		assert.Equal(t, "test", data.RemoteMeta["name"])
		assert.Equal(t, "running", data.RemoteMeta["status"])
	})
}

func TestInstanceDataJSONRoundTrip(t *testing.T) {
	t.Run("local backend serializes correctly", func(t *testing.T) {
		data := InstanceData{
			Title:       "test-local",
			Path:        "/tmp/test",
			Branch:      "main",
			Status:      Running,
			BackendType: "local",
			Program:     "claude",
		}

		jsonBytes, err := json.Marshal(data)
		require.NoError(t, err)

		var restored InstanceData
		err = json.Unmarshal(jsonBytes, &restored)
		require.NoError(t, err)

		assert.Equal(t, "local", restored.BackendType)
		assert.Equal(t, "test-local", restored.Title)
		assert.Nil(t, restored.RemoteMeta)
	})

	t.Run("remote backend serializes correctly", func(t *testing.T) {
		meta := map[string]interface{}{
			"name":   "fix-bug",
			"status": "running",
			"host":   "remote-1.example.com",
		}
		data := InstanceData{
			Title:       "test-remote",
			Path:        "/tmp/test",
			Branch:      "fix-bug",
			Status:      Running,
			BackendType: "remote",
			RemoteMeta:  meta,
		}

		jsonBytes, err := json.Marshal(data)
		require.NoError(t, err)

		var restored InstanceData
		err = json.Unmarshal(jsonBytes, &restored)
		require.NoError(t, err)

		assert.Equal(t, "remote", restored.BackendType)
		assert.Equal(t, "fix-bug", restored.RemoteMeta["name"])
		assert.Equal(t, "running", restored.RemoteMeta["status"])
		assert.Equal(t, "remote-1.example.com", restored.RemoteMeta["host"])
	})

	t.Run("empty backend_type defaults to empty string", func(t *testing.T) {
		// Simulate old data without backend_type
		jsonStr := `{"title":"old-inst","path":"/tmp","branch":"main","status":0}`
		var restored InstanceData
		err := json.Unmarshal([]byte(jsonStr), &restored)
		require.NoError(t, err)
		assert.Equal(t, "", restored.BackendType)
		assert.Nil(t, restored.RemoteMeta)
	})
}

// --- HookBackend launch (no prompt) ---

// --- HookBackend launch failure ---

func TestHookBackendStartLaunchCmdFails(t *testing.T) {
	dir := t.TempDir()
	launchCmd := writeScript(t, dir, "launch.sh", `exit 1`)

	b := &HookBackend{
		Hooks: config.RemoteHooks{LaunchCmd: launchCmd},
	}

	i := &Instance{
		Title:   "fail-test",
		Path:    t.TempDir(),
		backend: b,
	}

	err := b.Start(i, true)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "launch_cmd failed")
	assert.False(t, i.Started())
}

func TestHookBackendStartLaunchCmdBadJSON(t *testing.T) {
	dir := t.TempDir()
	launchCmd := writeScript(t, dir, "launch.sh", `echo "not json"`)

	b := &HookBackend{
		Hooks: config.RemoteHooks{LaunchCmd: launchCmd},
	}

	i := &Instance{
		Title:   "badjson-test",
		Path:    t.TempDir(),
		backend: b,
	}

	err := b.Start(i, true)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "no JSON")
}

// --- HookBackend kill failure ---

func TestHookBackendKillDeleteCmdFails(t *testing.T) {
	dir := t.TempDir()
	deleteCmd := writeScript(t, dir, "delete.sh", `echo "error" >&2; exit 1`)
	launchCmd := writeScript(t, dir, "launch.sh",
		`echo '{"name": "test", "status": "running"}'`)
	attachCmd := writeScript(t, dir, "attach.sh", `sleep 0.1`)
	listCmd := writeScript(t, dir, "list.sh", `echo '[]'`)

	b := &HookBackend{
		Hooks: config.RemoteHooks{
			LaunchCmd: launchCmd,
			ListCmd:   listCmd,
			AttachCmd: attachCmd,
			DeleteCmd: deleteCmd,
		},
	}

	i := &Instance{
		Title:   "test",
		Path:    t.TempDir(),
		backend: b,
	}

	err := b.Start(i, true)
	require.NoError(t, err)
	assert.True(t, i.Started())

	err = b.Kill(i)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "delete_cmd failed")

	// Even when delete_cmd fails, the instance must be marked stopped and
	// its remote metadata cleared so that the UI doesn't show it as running
	// while its PTY is already closed (see issue #266).
	assert.False(t, i.Started(), "instance should be stopped after failed Kill")
	i.mu.RLock()
	meta := i.remoteMeta
	i.mu.RUnlock()
	assert.Nil(t, meta, "remoteMeta should be cleared after failed Kill")
	// The PTY should have been cleaned up too.
	assert.Nil(t, b.getPTY(i.Title), "PTY should be closed after failed Kill")
}

// --- PTY management ---

func TestHookBackendPTYEnsureIdempotent(t *testing.T) {
	b := makeHooks(t)
	i := &Instance{
		Title:   "pty-test",
		Path:    t.TempDir(),
		backend: b,
	}

	// ensurePTY should be safe to call multiple times
	b.ensurePTY(i)
	b.ensurePTY(i) // Should not create a second PTY

	b.mu.Lock()
	count := len(b.ptys)
	b.mu.Unlock()
	assert.Equal(t, 1, count)

	b.closePTY(i.Title)

	b.mu.Lock()
	count = len(b.ptys)
	b.mu.Unlock()
	assert.Equal(t, 0, count)
}

func TestHookBackendClosePTYNonexistent(t *testing.T) {
	b := &HookBackend{Hooks: config.RemoteHooks{}}
	// Should not panic
	b.closePTY("nonexistent")
}

// TestHookBackendEnsurePTYRecreatesAfterAttachCmdExits verifies that when
// attach_cmd exits on its own (e.g. SSH disconnect, remote-side restart),
// a subsequent ensurePTY call replaces the dead entry instead of leaving
// it cached forever. Regression test for issue #328.
func TestHookBackendEnsurePTYRecreatesAfterAttachCmdExits(t *testing.T) {
	dir := t.TempDir()
	// attach_cmd exits immediately so the read goroutine sees EOF and
	// must mark the hookPTY closed.
	attachCmd := writeScript(t, dir, "attach.sh",
		`echo "first run for $1"; exit 0`)
	b := &HookBackend{
		Hooks: config.RemoteHooks{AttachCmd: attachCmd},
	}
	i := &Instance{
		Title:   "recreate-test",
		Path:    t.TempDir(),
		backend: b,
	}

	b.ensurePTY(i)

	// Wait for the reader goroutine to observe EOF and mark the entry closed.
	deadline := time.Now().Add(2 * time.Second)
	var hp *hookPTY
	for time.Now().Before(deadline) {
		hp = b.getPTY(i.Title)
		if hp == nil {
			t.Fatalf("ensurePTY did not register a hookPTY entry")
		}
		hp.mu.Lock()
		closed := hp.closed
		hp.mu.Unlock()
		if closed {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	hp.mu.Lock()
	closed := hp.closed
	hp.mu.Unlock()
	require.True(t, closed,
		"reader goroutine should have marked the dead entry as closed")
	firstCmd := hp.cmd

	// A second ensurePTY call must drop the stale entry and start a new
	// process rather than returning early.
	b.ensurePTY(i)
	hp2 := b.getPTY(i.Title)
	require.NotNil(t, hp2)
	assert.NotSame(t, firstCmd, hp2.cmd,
		"ensurePTY should have created a fresh process, not reused the dead one")

	// Cleanup: the second process also exits quickly, but closePTY is idempotent.
	b.closePTY(i.Title)
}

// TestHookBackendEnsurePTYReturnsEarlyWhenAlive ensures we don't replace a
// healthy preview process — only stale ones get recreated.
func TestHookBackendEnsurePTYReturnsEarlyWhenAlive(t *testing.T) {
	b := makeHooks(t) // attach script sleeps 0.1s, alive when we re-check
	i := &Instance{
		Title:   "alive-test",
		Path:    t.TempDir(),
		backend: b,
	}

	b.ensurePTY(i)
	hp := b.getPTY(i.Title)
	require.NotNil(t, hp)
	firstCmd := hp.cmd

	// Immediately call ensurePTY again — the existing entry is still alive.
	b.ensurePTY(i)
	hp2 := b.getPTY(i.Title)
	require.NotNil(t, hp2)
	assert.Same(t, firstCmd, hp2.cmd,
		"ensurePTY must reuse a live entry rather than spawning a duplicate")

	b.closePTY(i.Title)
}

// TestHookBackendAttachReturnsImmediatelyWhenPreviewIsSlowToDie is a
// regression test for #817: Attach runs on the bubbletea event loop, and it
// used to call closePTY synchronously, blocking the TUI for the full 2s
// grace period whenever the preview process did not exit promptly after its
// stdout pipe was closed. Attach must return well within the grace period
// and must drop the preview entry immediately so ensurePTY after detach
// starts a fresh process.
func TestHookBackendAttachReturnsImmediatelyWhenPreviewIsSlowToDie(t *testing.T) {
	dir := t.TempDir()
	// The interactive attach (under a real PTY, stdout is a tty) exits
	// immediately. The preview invocation (stdout is a pipe) prints once and
	// then sleeps without writing again, so closing the pipe's read end
	// delivers no EPIPE and the process outlives the 2s grace period unless
	// the reaper kills it.
	attachCmd := writeScript(t, dir, "attach.sh",
		`if [ -t 1 ]; then exit 0; fi
echo "preview for $1"
sleep 3`)
	b := &HookBackend{Hooks: config.RemoteHooks{AttachCmd: attachCmd}}
	i := &Instance{
		Title:   "slow-preview-test",
		Path:    t.TempDir(),
		backend: b,
	}
	i.started = true

	require.NoError(t, b.ensurePTY(i))
	require.NotNil(t, b.getPTY(i.Title))
	// Wait until the banner has been written. Before that point the preview
	// is NOT slow-dying: its first echo would hit the closed pipe and kill it
	// with SIGPIPE immediately, so attaching too early would pass even
	// against the synchronous pre-#817 code.
	require.Eventually(t, func() bool {
		out, _ := b.Preview(i)
		return strings.Contains(out, "preview for")
	}, 2*time.Second, 10*time.Millisecond, "preview process never produced its banner")

	start := time.Now()
	done, err := b.Attach(i)
	elapsed := time.Since(start)
	require.NoError(t, err)
	assert.Less(t, elapsed, 500*time.Millisecond,
		"Attach must not wait out the preview grace period on the event loop (#817), took %v", elapsed)
	assert.Nil(t, b.getPTY(i.Title),
		"Attach must drop the preview entry immediately so a fresh one can start after detach")

	// Cleanup: wait for the interactive attach goroutine to finish (it also
	// restarts the preview process on detach), then reap that preview.
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatalf("interactive attach did not exit")
	}
	b.closePTY(i.Title)
}

// TestHookBackendReapKillsStubbornPreviewProcess verifies that the
// background reap Attach hands the detached preview to (#817) actually
// terminates a process that ignores the closed pipe, so detached previews
// cannot accumulate as leaked processes.
func TestHookBackendReapKillsStubbornPreviewProcess(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "preview.pid")
	// exec replaces the shell, so the PID written here is the PID reap kills.
	// The process never writes after the banner, so it ignores the pipe close
	// and only dies when reap's grace period expires and it gets killed.
	attachCmd := writeScript(t, dir, "attach.sh",
		`echo $$ > `+pidFile+`
exec sleep 30`)
	b := &HookBackend{Hooks: config.RemoteHooks{AttachCmd: attachCmd}}
	i := &Instance{
		Title:   "stubborn-preview-test",
		Path:    t.TempDir(),
		backend: b,
	}

	require.NoError(t, b.ensurePTY(i))
	pid := waitForPidFile(t, pidFile)

	hp := b.stopPreview(i.Title)
	require.NotNil(t, hp)
	assert.Nil(t, b.getPTY(i.Title))
	hp.reap()

	// reap returns right after sending the kill; give the kernel and the
	// reaping Wait goroutine a moment to finish the death.
	require.Eventually(t, func() bool {
		return syscall.Kill(pid, 0) != nil
	}, 2*time.Second, 20*time.Millisecond,
		"stubborn preview process %d should be dead after reap", pid)
}

// TestHookBackendKillReapsPreviewBeforeDeleteCmd pins the synchronous half of
// the #817 split: Kill must still wait for the preview process to die before
// invoking delete_cmd, so the user's cleanup script never races a preview
// attach_cmd that is still connected to the remote session being deleted.
func TestHookBackendKillReapsPreviewBeforeDeleteCmd(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "preview.pid")
	sentinel := filepath.Join(dir, "preview-was-alive")
	// The preview writes continuously, so the pipe close from stopPreview
	// kills it with SIGPIPE within one loop iteration and Kill's reap sees a
	// clean (fully reaped) exit before delete_cmd starts.
	attachCmd := writeScript(t, dir, "attach.sh",
		`echo $$ > `+pidFile+`
while true; do echo tick; sleep 0.05; done`)
	// delete_cmd records whether the preview process was still alive when it ran.
	deleteCmd := writeScript(t, dir, "delete.sh",
		`if kill -0 "$(cat `+pidFile+`)" 2>/dev/null; then touch `+sentinel+`; fi
echo '{"deleted": true}'`)
	b := &HookBackend{Hooks: config.RemoteHooks{AttachCmd: attachCmd, DeleteCmd: deleteCmd}}
	i := &Instance{
		Title:   "kill-sync-test",
		Path:    t.TempDir(),
		backend: b,
	}
	i.started = true
	i.remoteMeta = map[string]interface{}{"name": "kill-sync-test"}

	require.NoError(t, b.ensurePTY(i))
	waitForPidFile(t, pidFile)

	require.NoError(t, b.Kill(i))

	_, statErr := os.Stat(sentinel)
	assert.True(t, os.IsNotExist(statErr),
		"delete_cmd must not run while the preview process is still alive (statErr: %v)", statErr)
}

// waitForPidFile polls until the hook script has written its PID and returns it.
func waitForPidFile(t *testing.T, path string) int {
	t.Helper()
	var pid int
	require.Eventually(t, func() bool {
		data, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err != nil || n <= 0 {
			return false
		}
		pid = n
		return true
	}, 2*time.Second, 20*time.Millisecond, "hook script never wrote its pid to %s", path)
	return pid
}

// --- Instance delegation ---

func TestInstanceDelegatesStartToBackend(t *testing.T) {
	b := makeHooks(t)
	i := &Instance{
		Title:   "delegate-test",
		Path:    t.TempDir(),
		backend: b,
	}

	err := i.Start(true)
	require.NoError(t, err)
	assert.True(t, i.Started())

	b.closePTY(i.Title)
}

func TestInstanceDelegatesKillToBackend(t *testing.T) {
	b := makeHooks(t)
	i := &Instance{
		Title:   "delegate-kill",
		Path:    t.TempDir(),
		backend: b,
	}

	err := i.Start(true)
	require.NoError(t, err)

	err = i.Kill()
	require.NoError(t, err)
	assert.False(t, i.Started())
}

func TestInstanceDelegatesPreviewToBackend(t *testing.T) {
	b := makeHooks(t)
	i := &Instance{
		Title:   "delegate-preview",
		Path:    t.TempDir(),
		backend: b,
	}

	err := i.Start(true)
	require.NoError(t, err)
	time.Sleep(500 * time.Millisecond)

	content, err := i.Preview()
	require.NoError(t, err)
	assert.Contains(t, content, "attached to delegate-preview")

	b.closePTY(i.Title)
}

func TestInstanceRepoNameErrorsForRemote(t *testing.T) {
	b := &HookBackend{Hooks: config.RemoteHooks{}}
	i := &Instance{
		Title:   "remote-inst",
		backend: b,
		started: true,
	}
	_, err := i.RepoName()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "remote")
}

func TestInstanceGetWorktreePathEmptyForRemote(t *testing.T) {
	b := &HookBackend{Hooks: config.RemoteHooks{}}
	i := &Instance{
		Title:   "remote-inst",
		backend: b,
		started: true,
	}
	assert.Equal(t, "", i.GetWorktreePath())
}

// --- list_cmd variations ---

func TestHookBackendIsAliveWithBadJSON(t *testing.T) {
	dir := t.TempDir()
	listCmd := writeScript(t, dir, "list.sh", `echo "not json"`)
	b := &HookBackend{
		Hooks: config.RemoteHooks{ListCmd: listCmd},
	}
	i := &Instance{Title: "test", backend: b}
	assert.False(t, b.IsAlive(i))
}

func TestHookBackendIsAliveWithStoppedSession(t *testing.T) {
	dir := t.TempDir()
	slug := slugify("test-session")
	listCmd := writeScript(t, dir, "list.sh",
		`echo '[{"name": "`+slug+`", "status": "stopped"}]'`)
	b := &HookBackend{
		Hooks: config.RemoteHooks{ListCmd: listCmd},
	}
	i := &Instance{Title: "test-session", backend: b}
	// status=stopped means not alive
	assert.False(t, b.IsAlive(i))
}

func TestHookBackendIsAliveWithMultipleSessions(t *testing.T) {
	dir := t.TempDir()
	slugA := slugify("session-a")
	slugB := slugify("session-b")
	listCmd := writeScript(t, dir, "list.sh",
		`echo '[{"name": "`+slugA+`", "status": "stopped"}, {"name": "`+slugB+`", "status": "running"}]'`)
	b := &HookBackend{
		Hooks: config.RemoteHooks{ListCmd: listCmd},
	}

	iA := &Instance{Title: "session-a", backend: b}
	iB := &Instance{Title: "session-b", backend: b}

	assert.False(t, b.IsAlive(iA))
	assert.True(t, b.IsAlive(iB))
}

// --- slugify shape ---

func TestSlugifyNoHashSuffix(t *testing.T) {
	cases := map[string]string{
		"hello":             "hello",
		"Hello World":       "hello-world",
		"My App!":           "my-app",
		"  spaced  ":        "spaced",
		"CAPS":              "caps",
		"already-a-slug":    "already-a-slug",
		"af-test":           "af-test",
		"some/name:thing@1": "somenamething1",
	}
	for title, want := range cases {
		assert.Equal(t, want, slugify(title), "slugify(%q)", title)
	}
}

func TestSlugifyDeterministic(t *testing.T) {
	title := "some-session-title"
	assert.Equal(t, slugify(title), slugify(title))
}

func TestSlugifyNonEmpty(t *testing.T) {
	// Even pathological inputs should produce a non-empty slug.
	for _, title := range []string{"!!!", "   ", ""} {
		s := slugify(title)
		assert.NotEmpty(t, s, "slugify(%q) should not be empty", title)
	}
}

func TestSlugifyCollisionsReduce(t *testing.T) {
	collisions := [][2]string{
		{"my_app", "myapp"},
		{"My App!", "my-app"},
		{"HELLO", "hello"},
	}
	for _, p := range collisions {
		assert.Equal(t, slugify(p[0]), slugify(p[1]))
	}
}

func TestFindSlugCollision(t *testing.T) {
	mk := func(title string) *Instance { return &Instance{Title: title} }
	existing := []*Instance{mk("myapp"), mk("other"), nil}

	assert.Equal(t, "myapp", FindSlugCollision("my_app", existing))
	assert.Equal(t, "myapp", FindSlugCollision("MyApp", existing))
	assert.Equal(t, "", FindSlugCollision("fresh-title", existing))
	assert.Equal(t, "", FindSlugCollision("myapp", existing))
	assert.Equal(t, "", FindSlugCollision("", existing))
}

func TestRemoteHookNamePrefersRemoteMetaName(t *testing.T) {
	assert.Equal(t, "box-af-test", RemoteHookName("af-test", map[string]interface{}{"name": "box-af-test"}))
	assert.Equal(t, "af-test", RemoteHookName("af-test", nil))
}

func TestListRemoteHookInstanceDataImportsRunningSessions(t *testing.T) {
	dir := t.TempDir()
	listCmd := writeScript(t, dir, "list.sh",
		`echo '[{"name": "remote-one", "status": "running", "host": "h1"}, {"name": "remote-two", "status": "stopped"}]'`)

	now := time.Now()
	data, err := ListRemoteHookInstanceData("/repo/root", config.RemoteHooks{ListCmd: listCmd}, now)
	require.NoError(t, err)
	require.Len(t, data, 1)
	assert.Equal(t, "remote-one", data[0].Title)
	assert.Equal(t, "/repo/root", data[0].Path)
	assert.Equal(t, "remote-one", data[0].Branch)
	assert.Equal(t, Running, data[0].Status)
	assert.Equal(t, "remote", data[0].BackendType)
	assert.Equal(t, "remote-one", data[0].RemoteMeta["name"])
	assert.Equal(t, "h1", data[0].RemoteMeta["host"])
}

// TestListRemoteHookInstanceDataIgnoresStderrDiagnostics covers the common
// pattern of a list_cmd script that writes progress to stderr while emitting
// JSON on stdout (e.g. an ssh-backed lister that logs "connecting…"). The
// captured stderr must not corrupt the JSON we parse. See #561.
func TestListRemoteHookInstanceDataIgnoresStderrDiagnostics(t *testing.T) {
	dir := t.TempDir()
	listCmd := writeScript(t, dir, "list.sh", `
echo "connecting to remote host..." >&2
echo "fetched 1 session" >&2
echo '[{"name": "remote-one", "status": "running", "host": "h1"}]'
`)

	now := time.Now()
	data, err := ListRemoteHookInstanceData("/repo/root", config.RemoteHooks{ListCmd: listCmd}, now)
	require.NoError(t, err)
	require.Len(t, data, 1)
	assert.Equal(t, "remote-one", data[0].Title)
	assert.Equal(t, "remote-one", data[0].RemoteMeta["name"])
	assert.Equal(t, "h1", data[0].RemoteMeta["host"])
}

// TestListRemoteHookInstanceDataSurfacesStderrOnFailure verifies that when
// list_cmd exits non-zero, the returned error includes the captured stderr
// so the warning surfaced at app/sync.go and daemon/control.go is actually
// diagnostic. Before #561 the error was just "list_cmd failed: exit status 1".
func TestListRemoteHookInstanceDataSurfacesStderrOnFailure(t *testing.T) {
	dir := t.TempDir()
	listCmd := writeScript(t, dir, "list.sh", `
echo "ssh: could not resolve hostname remote.example.com" >&2
exit 1
`)

	_, err := ListRemoteHookInstanceData("/repo/root", config.RemoteHooks{ListCmd: listCmd}, time.Now())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list_cmd failed")
	assert.Contains(t, err.Error(), "ssh: could not resolve hostname remote.example.com",
		"error must surface captured stderr so users can debug list_cmd failures (#561)")
}

// TestListRemoteHookInstanceDataListCmdHangs is the regression test for #692:
// ListRemoteHookInstanceData runs the user-supplied list_cmd at TUI startup
// inside the daemon handler that the TUI blocks on over RPC (with no
// client-side call deadline). A hanging list_cmd (e.g. SSH to a wedged host)
// previously had no timeout here, so startup blocked for the full duration of
// the script. The startup import path must abort within restoreAliveTimeout
// plus a small tolerance for WaitDelay and scheduling slack.
func TestListRemoteHookInstanceDataListCmdHangs(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping timeout-bound test in short mode")
	}

	dir := t.TempDir()
	// Sleep well past restoreAliveTimeout so the timeout, not the script,
	// is what ends the call.
	listCmd := writeScript(t, dir, "list.sh", `sleep 30; echo '[]'`)

	start := time.Now()
	_, err := ListRemoteHookInstanceData("/repo/root", config.RemoteHooks{ListCmd: listCmd}, time.Now())
	elapsed := time.Since(start)

	require.Error(t, err, "startup import must error when list_cmd hangs past timeout")
	// restoreAliveTimeout is 2s; allow a buffer for WaitDelay (500ms) plus
	// scheduling slack. The key bound is that startup must NOT block anywhere
	// near the script's 30s sleep — that was the #692 hang.
	assert.Less(t, elapsed, restoreAliveTimeout+2*time.Second,
		"startup import must return within restoreAliveTimeout+tolerance when list_cmd hangs (got %v)", elapsed)
}

func TestRunHookAttachWithDetachKey(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()

	done := make(chan error, 1)
	go func() {
		cmd := exec.Command("sh", "-c", "sleep 10")
		done <- runHookAttachWithDetach(cmd, stdinR, io.Discard, io.Discard)
	}()

	_, err := stdinW.Write([]byte{tmux.DetachKeyByte})
	require.NoError(t, err)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatalf("remote attach did not exit after detach key")
	}
}

// remoteJunk is what a remote tmux client typically sets on the local
// terminal through the raw attach stream: alt screen, a scroll region, a
// hidden cursor, and mouse reporting. Used by the #845 restore tests below.
const remoteJunk = "\x1b[?1049h\x1b[5;20r\x1b[?25l\x1b[?1002h"

// syncBuffer is a mutex-guarded bytes.Buffer: the PTY→stdout io.Copy
// goroutine inside runHookAttachWithDetach writes concurrently with the
// test's readiness polling.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestRunHookAttachWithDetach_EmitsTerminalRestoreOnDetachKill is the
// regression test for #845's worst case: the detach key SIGKILLs attach_cmd
// mid-stream, so the remote program never gets to emit its own restore
// sequences and the terminal is stranded with the remote's alt screen,
// scroll region, and mouse modes. runHookAttachWithDetach must append the
// neutral restore to stdout after the last remote byte.
func TestRunHookAttachWithDetach_EmitsTerminalRestoreOnDetachKill(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()

	var out syncBuffer
	done := make(chan error, 1)
	go func() {
		// printf the junk like a remote tmux client would, then hang like a
		// live remote session until the detach key kills us.
		cmd := exec.Command("sh", "-c", `printf '\033[?1049h\033[5;20r\033[?25l\033[?1002h'; sleep 10`)
		done <- runHookAttachWithDetach(cmd, stdinR, &out, io.Discard)
	}()

	// Wait until the junk has been copied to stdout so the detach kill is
	// guaranteed to land mid-stream, after the remote set its modes.
	require.Eventually(t, func() bool { return out.Len() >= len(remoteJunk) },
		2*time.Second, 5*time.Millisecond, "remote junk never arrived on stdout")

	_, err := stdinW.Write([]byte{tmux.DetachKeyByte})
	require.NoError(t, err)

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatalf("remote attach did not exit after detach key")
	}

	got := out.String()
	assert.Contains(t, got, remoteJunk, "test premise: the remote stream was copied raw")
	require.True(t, strings.HasSuffix(got, hookAttachTerminalRestore),
		"stdout must END with the neutral terminal restore so it lands after "+
			"every remote byte (#845); got tail %q", got[max(0, len(got)-120):])
}

// TestRunHookAttachWithDetach_EmitsTerminalRestoreOnNaturalExit covers the
// other half of #845: attach_cmd exits on its own (remote session ended, SSH
// dropped). Even a graceful remote exit can leave the local terminal on the
// main screen buffer while the TUI's renderer believes it owns the alt
// screen, so the restore must be emitted on this path too.
func TestRunHookAttachWithDetach_EmitsTerminalRestoreOnNaturalExit(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh not available")
	}

	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()

	var out bytes.Buffer
	cmd := exec.Command("sh", "-c", `printf '\033[?1049h\033[5;20r\033[?25l\033[?1002h'`)
	err := runHookAttachWithDetach(cmd, stdinR, &out, io.Discard)
	require.NoError(t, err)

	got := out.String()
	assert.Contains(t, got, remoteJunk, "test premise: the remote stream was copied raw")
	require.True(t, strings.HasSuffix(got, hookAttachTerminalRestore),
		"stdout must end with the neutral terminal restore on natural exit too (#845); got tail %q",
		got[max(0, len(got)-120):])
}

// TestRunHookAttachWithDetach_NoRestoreWhenPTYNeverStarted: if pty.Start
// fails, nothing was ever streamed to the terminal, so writing the restore
// would clear modes the TUI legitimately has set. The early-error path must
// leave stdout untouched.
func TestRunHookAttachWithDetach_NoRestoreWhenPTYNeverStarted(t *testing.T) {
	var out bytes.Buffer
	cmd := exec.Command("/nonexistent-binary-for-845-test")
	err := runHookAttachWithDetach(cmd, strings.NewReader(""), &out, io.Discard)
	require.Error(t, err)
	assert.Zero(t, out.Len(),
		"stdout must be untouched when the attach PTY never started — the "+
			"terminal was never written to, so there is nothing to restore")
}

// recordingPtyFactory is a tmux.PtyFactory that records each exec.Cmd passed
// to Start, lets the caller inspect the new-session vs attach-session sequence
// emitted by Restore's lazy-respawn path. It returns a real (writable) temp
// file as the PTY so callers that close it don't crash.
type recordingPtyFactory struct {
	t    *testing.T
	cmds []*exec.Cmd
}

func (p *recordingPtyFactory) Start(c *exec.Cmd) (*os.File, error) {
	path := filepath.Join(p.t.TempDir(), fmt.Sprintf("pty-%s-%d", p.t.Name(), rand.Int31()))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0644)
	if err != nil {
		return nil, err
	}
	p.cmds = append(p.cmds, c)
	return f, nil
}

func (p *recordingPtyFactory) Close() {}

// TestLocalBackendStartRestoreReinjectsSystemPrompt is a regression test for
// issue #511. After a reboot the tmux server is gone, so Restore takes the
// lazy-respawn path added in #386/#444 and spawns a fresh tmux session using
// the program string stored on the TmuxSession. Before the fix that program
// was the raw `i.Program` (e.g. "claude") with no `--plugin-dir` flag, so
// Agent Factory's /af-* slash commands silently disappeared until the user
// killed and recreated the session. The fix re-injects the system prompt in
// LocalBackend.Start before calling Restore, so the respawned tmux session
// receives the same program string as the original first-time launch.
func TestLocalBackendStartRestoreReinjectsSystemPrompt(t *testing.T) {
	// Isolate the plugin dir to a temp config home so ensurePluginDir has
	// somewhere safe to write and tests don't fight over a shared dir.
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	ptyFactory := &recordingPtyFactory{t: t}

	// First two has-session calls report missing (the outer Restore check, then
	// the existence check at the top of Start). After tmux new-session runs,
	// subsequent has-session calls report exists so Start's poll loop and the
	// inner Restore("") attach call succeed.
	hasSessionCalls := 0
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			if strings.Contains(c.String(), "has-session") {
				hasSessionCalls++
				if hasSessionCalls <= 2 {
					return fmt.Errorf("can't find session")
				}
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			return []byte("output"), nil
		},
	}

	repoRoot := initTempGitRepo(t)
	worktreePath := filepath.Join(t.TempDir(), "worktree-511")
	gw, err := git.NewGitWorktreeFromStorage(repoRoot, worktreePath, "respawn-511", "respawn-511-branch", "", false, false)
	require.NoError(t, err)

	// The tmuxSession is pre-attached on the instance (the production path
	// builds it from persisted state). It starts with the raw program string,
	// just like a freshly-deserialized instance.
	ts := tmux.NewTmuxSessionWithDeps("respawn-511", "claude", ptyFactory, cmdExec)

	inst := &Instance{
		Title:       "respawn-511",
		Path:        repoRoot,
		Program:     "claude",
		backend:     &LocalBackend{},
		tmuxSession: ts,
		gitWorktree: gw,
	}

	require.NoError(t, inst.Start(false))
	assert.True(t, inst.Started())

	require.GreaterOrEqual(t, len(ptyFactory.cmds), 1,
		"expected at least one PTY command from the respawn path")
	newSessionCmd := cmd.ToString(ptyFactory.cmds[0])
	require.Contains(t, newSessionCmd, "new-session",
		"first PTY command must be the lazy-respawn new-session (not an attach)")
	require.Contains(t, newSessionCmd, "--plugin-dir",
		"respawned session must include claude --plugin-dir injection so /af-* slash commands keep working (#511)")
}
