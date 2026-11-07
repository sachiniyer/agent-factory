package proctree

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestOpenFilePaths_FindsFilesTheProcessHoldsOpen exercises the descriptor
// walk on whatever platform runs it — Linux /proc, Darwin proc_pidfdinfo —
// against real open descriptors, so a backend that answers nothing (or
// errors) fails its own CI rather than shipping silent (#6847 review).
func TestOpenFilePaths_FindsFilesTheProcessHoldsOpen(t *testing.T) {
	if _, err := OpenFilePaths(os.Getpid()); errors.Is(err, ErrUnsupportedPlatform) {
		t.Skip("no open-file backend on this platform")
	}

	// The test process itself holds the file: no child-management timing.
	held := filepath.Join(t.TempDir(), "held-self.jsonl")
	f, err := os.Create(held)
	require.NoError(t, err)
	defer f.Close()

	assertHeld(t, os.Getpid(), held)

	// A child holding the same file: the walk must see other pids, not just
	// the caller's own.
	assertHeld(t, spawnFdHolder(t, held), held)
}

// assertHeld requires paths from OpenFilePaths(pid) to name file, waiting out
// any first-call descriptor lag. The comparison resolves the expected path
// because kernel descriptor targets are canonical while test paths may sit
// under a symlinked temp root (macOS /var → /private/var).
func assertHeld(t *testing.T, pid int, file string) {
	t.Helper()
	want, err := filepath.EvalSymlinks(file)
	require.NoError(t, err)
	var paths map[string]struct{}
	require.Eventually(t, func() bool {
		paths, err = OpenFilePaths(pid)
		if err != nil {
			return false
		}
		_, ok := paths[file]
		if !ok {
			_, ok = paths[want]
		}
		return ok
	}, 5*time.Second, 20*time.Millisecond,
		"pid %d must report %s among its open files", pid, file)
}

// spawnFdHolder starts a real child that holds file open until killed, and
// returns its pid once the descriptor exists.
func spawnFdHolder(t *testing.T, file string) int {
	t.Helper()
	holder := exec.Command("tail", "-f", file)
	require.NoError(t, holder.Start())
	t.Cleanup(func() { _ = holder.Process.Kill() })
	return holder.Process.Pid
}

// TestOpenFilePaths_HelperProcessPattern is belt-and-braces coverage for the
// runtime-attribution shape: the held file is opened BY a child process, and
// the walk must find it in the child's descriptor set (not the parent's).
func TestOpenFilePaths_HelperProcessPattern(t *testing.T) {
	if _, err := OpenFilePaths(os.Getpid()); errors.Is(err, ErrUnsupportedPlatform) {
		t.Skip("no open-file backend on this platform")
	}
	if os.Getenv("PROCTREE_FD_HELPER") == "1" {
		f, err := os.Open(os.Getenv("PROCTREE_FD_PATH"))
		if err != nil {
			os.Exit(2)
		}
		fmt.Println("ready")
		time.Sleep(30 * time.Second)
		_ = f.Close()
		os.Exit(0)
	}

	held := filepath.Join(t.TempDir(), "held-child.jsonl")
	require.NoError(t, os.WriteFile(held, []byte("x\n"), 0o644))

	child := exec.Command(os.Args[0], "-test.run=TestOpenFilePaths_HelperProcessPattern$")
	child.Env = append(os.Environ(), "PROCTREE_FD_HELPER=1", "PROCTREE_FD_PATH="+held)
	stdout, err := child.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, child.Start())
	t.Cleanup(func() { _ = child.Process.Kill() })
	// Wait for the helper's readiness line — the fd is open from that point.
	buf := make([]byte, 16)
	_, err = stdout.Read(buf)
	require.NoError(t, err, "helper must announce the descriptor is open")

	assertHeld(t, child.Process.Pid, held)
}
