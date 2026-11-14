package preflight

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
)

// #7287 review R2: the launch directory a command is preflighted in is a
// session's worktree on the agent-swap and manual account-swap paths, which
// may sit on a mount that no longer answers (NFS, FUSE, a dead sshfs). Its
// stat is bounded, and one that does not answer fails the preflight with the
// ErrProbeTimedOut + context.DeadlineExceeded pair instead of holding the
// caller, which holds the session's operation lock, forever.
func TestCheckCommandAtDoesNotStallOnAWedgedLaunchDir(t *testing.T) {
	dir := t.TempDir()
	exe := filepath.Join(t.TempDir(), "agent")
	require.NoError(t, os.WriteFile(exe, []byte("#!/bin/sh\n"), 0o755))
	_, err := CheckCommandAt(exe, dir)
	require.NoError(t, err, "precondition: the command preflights in an answering directory")

	release := make(chan struct{})
	restoreStat := fsprobe.SetStatPathForTest(func(path string) (os.FileInfo, error) {
		if path == dir {
			<-release
		}
		return os.Stat(path)
	})
	restoreTimeout := fsprobe.SetProbeTimeoutForTest(50 * time.Millisecond)
	t.Cleanup(func() {
		close(release)
		time.Sleep(50 * time.Millisecond)
		restoreStat()
		restoreTimeout()
	})

	done := make(chan error, 1)
	go func() {
		_, err := CheckCommandAt(exe, dir)
		done <- err
	}()
	select {
	case err := <-done:
		require.True(t, errors.Is(err, fsprobe.ErrProbeTimedOut), "the preflight names the bounded probe: %v", err)
		require.True(t, errors.Is(err, context.DeadlineExceeded), "and its deadline: %v", err)
		// #7292: off the swap paths (session create, doctor, the config
		// agent) the message a user reads is ProgramError's. A launch
		// directory that did not answer is no fault of the program: the
		// lead names the directory and the mount.
		msg := ProgramError("claude", exe, err).Error()
		require.Contains(t, msg, "the launch directory "+dir+" did not answer", "the lead names the directory")
		require.Contains(t, msg, "mount", "and the mount")
		require.NotContains(t, msg, "could not be started", "never that the program failed")
		require.NotContains(t, msg, "not installed", "or is missing")
	case <-time.After(3 * time.Second):
		t.Fatal("a preflight in a launch directory on a wedged mount stalled the caller")
	}
}
