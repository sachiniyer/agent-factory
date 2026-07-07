package session

import (
	"bytes"
	"io"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
