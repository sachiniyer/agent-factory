package session

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
