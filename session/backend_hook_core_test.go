package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	assert.Equal(t, Slugify("test-session"), i.Branch)
	assert.NotNil(t, i.remoteMeta)
	assert.Equal(t, "running", i.remoteMeta["status"])

	// Cleanup
	b.closePTY(i.Title)
}

// TestHookBackendProvisionLaunchSeam locks in the #1592 Phase 1 PR4 split:
// provision establishes the remote workspace (launch_cmd → remoteMeta/Branch)
// WITHOUT starting the local launch machinery — the instance is not yet marked
// started and carries no tabs — and launch is what flips it live and syncs the
// tab model. Start(true) must be exactly provision(true) then launch(true).
func TestHookBackendProvisionLaunchSeam(t *testing.T) {
	b := makeHooks(t)
	i := &Instance{
		Title:   "test-session",
		Path:    t.TempDir(),
		backend: b,
	}

	// PROVISION: remote workspace allocated, but nothing launched locally yet.
	require.NoError(t, b.Provision(i, true))
	assert.NotNil(t, i.remoteMeta, "provision records the remote workspace metadata")
	assert.Equal(t, Slugify("test-session"), i.Branch)
	assert.False(t, i.Started(), "provision must not mark the instance started")
	assert.Empty(t, i.Tabs, "provision must not build the tab model")

	// LAUNCH: local machinery comes up and the instance goes live.
	require.NoError(t, b.Launch(i, true))
	assert.True(t, i.Started(), "launch marks the instance started")
	assert.NotEmpty(t, i.Tabs, "launch syncs the remote tab model")

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

// TestHookBackendKillRetryAfterFailure is the regression test for #922: when
// delete_cmd fails, Kill must NOT clear remoteMeta, so a retried Kill on the
// same *Instance (the daemon reuses the pointer across RPC calls) still sees a
// remote allocation and re-runs delete_cmd. Before the fix, the early
// remoteMeta = nil meant the retry computed hadRemote == false, skipped
// delete_cmd, returned nil, and leaked the remote session.
//
// delete_cmd appends a line to a counter file on every invocation and exits
// non-zero or zero depending on a "succeed" flag file, so the test can flip it
// from failing to succeeding between the two Kill calls and count invocations.
func TestHookBackendKillRetryAfterFailure(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "delete-calls")
	succeedFlag := filepath.Join(dir, "delete-should-succeed")

	launchCmd := writeScript(t, dir, "launch.sh",
		`echo '{"name": "'"$2"'", "status": "running"}'`)
	attachCmd := writeScript(t, dir, "attach.sh",
		`echo "attached to $1"; sleep 0.1`)
	// Record the call, then succeed only once the flag file exists; otherwise
	// fail so the first Kill returns an error.
	deleteCmd := writeScript(t, dir, "delete.sh",
		`echo x >> `+counter+`
if [ -f `+succeedFlag+` ]; then
  echo '{"deleted": true}'
  exit 0
fi
echo "delete failed" >&2
exit 1`)

	b := &HookBackend{
		Hooks: config.RemoteHooks{
			LaunchCmd: launchCmd,
			AttachCmd: attachCmd,
			DeleteCmd: deleteCmd,
		},
	}
	i := &Instance{
		Title:   "test-session",
		Path:    t.TempDir(),
		backend: b,
	}

	// Start a remote session so remoteMeta is populated.
	require.NoError(t, b.Start(i, true))
	require.NotNil(t, i.remoteMeta)

	deleteCalls := func() int {
		data, err := os.ReadFile(counter)
		if os.IsNotExist(err) {
			return 0
		}
		require.NoError(t, err)
		return strings.Count(string(data), "x")
	}

	// First Kill: delete_cmd fails. Kill must return an error, must have
	// invoked delete_cmd exactly once, and must NOT have cleared remoteMeta.
	err := b.Kill(i)
	require.Error(t, err, "Kill should surface the delete_cmd failure")
	assert.Equal(t, 1, deleteCalls(), "delete_cmd should have run once on the first Kill")
	assert.NotNil(t, i.remoteMeta,
		"remoteMeta must survive a failed delete_cmd so a retry can re-run it (#922)")

	// Flip delete_cmd to succeed, then retry. The retry must re-run delete_cmd
	// (total 2), return no error, and clear remoteMeta now that the remote
	// session is actually gone.
	require.NoError(t, os.WriteFile(succeedFlag, []byte("y"), 0644))

	err = b.Kill(i)
	require.NoError(t, err, "retried Kill should succeed once delete_cmd succeeds")
	assert.Equal(t, 2, deleteCalls(),
		"delete_cmd must run again on retry — skipping it would leak the remote session (#922)")
	assert.Nil(t, i.remoteMeta, "remoteMeta should be cleared after delete_cmd succeeds")
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
	updated, hasPrompt, _ := b.HasUpdated(i)
	assert.False(t, updated)
	assert.False(t, hasPrompt)
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
	updated, hasPrompt, _ := b.HasUpdated(i)
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
		{"amp dispatches", tmux.ProgramAmp, true},
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
				Title:   "trust-dispatch",
				Program: tc.program,
				backend: &LocalBackend{},
				started: true,
				Tabs:    []*Tab{newAgentTab(ts)},
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
