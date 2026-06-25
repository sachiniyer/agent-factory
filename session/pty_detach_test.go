package session

import (
	"io"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/require"
)

// The remote attach_cmd PTY proxy (runHookAttachWithDetach) reads stdin in
// chunks and intercepts the detach key (Ctrl-W / tmux.DetachKeyByte) before
// forwarding bytes to the PTY. stdin.Read can batch the detach key together
// with preceding bytes in a single read (buffered terminal input), so the
// interception must look at the LAST byte of the read rather than requiring the
// detach key to be the sole byte — the #975 regression, where the old
// `n == 1 && buf[0] == DetachKeyByte` guard silently forwarded a batched Ctrl-W
// to the PTY and never detached.
//
// io.Pipe delivers each Write in a single Read on the other side (the proxy's
// 32-byte buffer is larger than every payload here), so a one-shot
// stdinW.Write reproduces the "batched into one read" condition exactly.

// TestRunHookAttach_DetachKeyBatchedAfterPrecedingBytes is the #975 regression:
// a multi-byte read ENDING in the detach key must detach. Before the fix this
// read was forwarded to the PTY and the proxy never exited, so the test timed
// out.
func TestRunHookAttach_DetachKeyBatchedAfterPrecedingBytes(t *testing.T) {
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

	// Printable bytes ahead of the detach key, all in one read.
	if _, err := stdinW.Write(append([]byte("abc"), tmux.DetachKeyByte)); err != nil {
		t.Fatalf("write stdin: %v", err)
	}

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatalf("attach did not detach when the detach key was batched after preceding bytes (#975)")
	}
}

// TestRunHookAttach_DetachKeyAsSoleByte locks in that the single-byte detach
// case the old guard handled still detaches after the fix.
func TestRunHookAttach_DetachKeyAsSoleByte(t *testing.T) {
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

	if _, err := stdinW.Write([]byte{tmux.DetachKeyByte}); err != nil {
		t.Fatalf("write stdin: %v", err)
	}

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatalf("attach did not detach on a single-byte detach-key read")
	}
}

// TestRunHookAttach_ReadNotEndingInDetachKeyForwards covers the inverse: a
// multi-byte read that does NOT end in the detach key is forwarded to the PTY
// untouched and does not trigger a detach. The child is `cat`, so the PTY's
// line-discipline echo reflects the forwarded bytes back onto stdout, which is
// how the test observes that they reached the PTY. (Forwarding of the bytes
// PRECEDING a batched detach key uses this same ptmx.Write call but is not
// asserted directly here — observing it would race the detach SIGKILL that
// tears the PTY down.)
func TestRunHookAttach_ReadNotEndingInDetachKeyForwards(t *testing.T) {
	if _, err := exec.LookPath("cat"); err != nil {
		t.Skip("cat not available")
	}

	stdinR, stdinW := io.Pipe()
	defer stdinW.Close()

	var out syncBuffer
	done := make(chan error, 1)
	go func() {
		cmd := exec.Command("cat") // copies its PTY stdin back to stdout
		done <- runHookAttachWithDetach(cmd, stdinR, &out, io.Discard)
	}()

	if _, err := stdinW.Write([]byte("hello world")); err != nil {
		t.Fatalf("write stdin: %v", err)
	}

	// The forwarded bytes reach the PTY and are echoed back. No detach key was
	// sent, so the proxy must still be attached while we observe them.
	require.Eventually(t, func() bool {
		return strings.Contains(out.String(), "hello world")
	}, 2*time.Second, 5*time.Millisecond, "preceding bytes were not forwarded to the PTY")

	select {
	case err := <-done:
		t.Fatalf("attach exited without a detach key: %v", err)
	default:
	}

	// A subsequent read carrying the detach key tears it down cleanly.
	if _, err := stdinW.Write([]byte{tmux.DetachKeyByte}); err != nil {
		t.Fatalf("write detach key: %v", err)
	}
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatalf("attach did not detach after the detach key")
	}
}
