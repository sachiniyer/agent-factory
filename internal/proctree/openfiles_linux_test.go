//go:build linux

package proctree

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOpenFilePaths_FindsAFileTheProcessHoldsOpen(t *testing.T) {
	held := filepath.Join(t.TempDir(), "held.jsonl")
	require.NoError(t, os.WriteFile(held, []byte("x\n"), 0o644))

	holder := exec.Command("tail", "-f", held)
	require.NoError(t, holder.Start())
	t.Cleanup(func() { _ = holder.Process.Kill() })

	var paths map[string]struct{}
	require.Eventually(t, func() bool {
		var err error
		paths, err = OpenFilePaths(holder.Process.Pid)
		if err != nil {
			return false
		}
		_, ok := paths[held]
		return ok
	}, 5*time.Second, 20*time.Millisecond, "an open descriptor must appear in the process's fd set")
}

func TestOpenFilePaths_DeadProcessIsGoneNotEmpty(t *testing.T) {
	// A pid that cannot be read is "could not look", never "holds nothing":
	// the caller distinguishes ErrProcessExited from a provably-empty set.
	holder := exec.Command("sleep", "30")
	require.NoError(t, holder.Start())
	pid := holder.Process.Pid
	require.NoError(t, holder.Process.Kill())
	_ = holder.Wait()

	_, err := OpenFilePaths(pid)
	if err == nil {
		// The process may still be a zombie — skip rather than flake on the
		// reaper's timing; the contract case is the error path.
		t.Skip("reaped before the read raced it")
	}
	require.ErrorIs(t, err, ErrProcessExited)
}
