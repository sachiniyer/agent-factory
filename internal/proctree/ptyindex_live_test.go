//go:build linux || darwin

package proctree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// #7469: the slave→master→server attribution a detached carrier needs. A tmux
// server holds each pane's pty MASTER; a process detached out of that pane
// keeps the pane's slave as its stdio. PtySlaveIndices(carrier) must name the
// index PtyMasterIndices(server) reports — the kernel link a reparented
// process keeps to the server it left — and it must do so with the same
// answer on linux's fdinfo tty-index and darwin's ptmx vnode rdev.
func TestPtyIndices_ADetachedCarrierStillNamesTheServerItLeft(t *testing.T) {
	dir := privateSocketDir(t)
	sock := filepath.Join(dir, "s")
	server := socketTmux(t, dir, sock)

	// The pane's tty, then a carrier detached from the pane's own process
	// tree — setsid'd and reparented, holding the pane's slave as stdio, the
	// #3699 re-host shape.
	out, err := exec.Command("tmux", "-S", sock, "-f", "/dev/null",
		"list-panes", "-a", "-F", "#{pane_tty}").CombinedOutput()
	require.NoError(t, err, string(out))
	paneTTY := strings.TrimSpace(string(out))
	tty, err := os.OpenFile(paneTTY, os.O_RDWR, 0)
	require.NoError(t, err)
	defer tty.Close()
	carrier := exec.Command("/bin/sleep", "30")
	carrier.Stdin, carrier.Stdout, carrier.Stderr = tty, tty, tty
	carrier.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, carrier.Start())
	defer func() { _ = carrier.Process.Kill(); _, _ = carrier.Process.Wait() }()
	time.Sleep(200 * time.Millisecond)

	slaves, err := PtySlaveIndices(carrier.Process.Pid)
	require.NoError(t, err)
	require.NotEmpty(t, slaves, "a carrier holding the pane's slave must report it")

	masters, err := PtyMasterIndices(server)
	require.NoError(t, err)
	require.NotEmpty(t, masters, "a tmux server with one live pane must report its master")
	for _, idx := range slaves {
		require.Contains(t, masters, idx,
			"the carrier's slave %+v is mastered by server pid %d", idx, server)
	}

	// The pane's own shell reports the same slave index — the carrier and the
	// pane are the two ends of one attribution.
	paneOut, err := exec.Command("tmux", "-S", sock, "-f", "/dev/null",
		"list-panes", "-a", "-F", "#{pane_pid}").CombinedOutput()
	require.NoError(t, err)
	panePID, err := strconv.Atoi(strings.TrimSpace(string(paneOut)))
	require.NoError(t, err)
	paneSlaves, err := PtySlaveIndices(panePID)
	require.NoError(t, err)
	for _, idx := range slaves {
		require.Contains(t, paneSlaves, idx)
	}
}

// A process holding no pty at all reports an empty slice — "holds none", not
// an error and never a fabricated index.
func TestPtyIndices_AProcessWithNoPtyReportsNone(t *testing.T) {
	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	require.NoError(t, err)
	defer devNull.Close()
	noPty := exec.Command("/bin/sleep", "30")
	noPty.Stdin, noPty.Stdout, noPty.Stderr = devNull, devNull, devNull
	noPty.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, noPty.Start())
	defer func() { _ = noPty.Process.Kill(); _, _ = noPty.Process.Wait() }()
	time.Sleep(100 * time.Millisecond)

	slaves, err := PtySlaveIndices(noPty.Process.Pid)
	require.NoError(t, err)
	require.Empty(t, slaves)
	masters, err := PtyMasterIndices(noPty.Process.Pid)
	require.NoError(t, err)
	require.Empty(t, masters)
}
