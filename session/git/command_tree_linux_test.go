//go:build linux

package git

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

const gitAdoptedPipeHolderChild = "AF_TEST_GIT_ADOPTED_PIPE_HOLDER"

func TestIntegrityProbeReapsAdoptedPipeHolder(t *testing.T) {
	if os.Getenv(gitAdoptedPipeHolderChild) == "1" {
		runAdoptedPipeHolderChild(t)
		return
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestIntegrityProbeReapsAdoptedPipeHolder$", "-test.v")
	cmd.Env = append(os.Environ(), gitAdoptedPipeHolderChild+"=1")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
}

func runAdoptedPipeHolderChild(t *testing.T) {
	require.NoError(t, unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0))
	binDir := t.TempDir()
	pidFile := filepath.Join(t.TempDir(), "helper-pid")
	fakeGit := filepath.Join(binDir, "git")
	script := fmt.Sprintf(`#!/bin/sh
(trap '' HUP; sleep 30) &
child=$!
printf '%%s' "$child" > %q
printf 'complete output\n'
exit 0
`, pidFile)
	require.NoError(t, os.WriteFile(fakeGit, []byte(script), 0o700))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	_, err := runIntegrityGit(context.Background(), t.TempDir(), "status")
	require.NoError(t, err)
	rawPID, err := os.ReadFile(pidFile)
	require.NoError(t, err)
	helperPID, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = syscall.Kill(helperPID, syscall.SIGKILL)
		var status syscall.WaitStatus
		_, _ = syscall.Wait4(helperPID, &status, syscall.WNOHANG, nil)
	})
	require.ErrorIs(t, syscall.Kill(helperPID, 0), syscall.ESRCH,
		"a pipe-holding helper adopted by the AF process must be collected, not left as a zombie")
}
