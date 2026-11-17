//go:build darwin

package tmux

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/internal/testguard/sleeper"
	"github.com/stretchr/testify/require"
)

// #7494 item 1 — on darwin a REAL af daemon whose executable was replaced
// after exec (upgradetxn's `rename(temp, af)` unlinks the text vnode; there
// is no userspace opener for an unnamed inode, no /proc/<pid>/exe) can never
// verify again, so it blocks every trusting close it sits in until it
// restarts. The property: the refusal names it as an af daemon that could
// not be verified, with the remedy — restart or stop it — on both the
// unmarked candidate itself and a marked cohort member that descends from
// it. Linux needs no analog: /proc/<pid>/exe still opens the running inode
// (encoded by the deleted-executable attestation test's platform split).
func TestReplacedExecutableDaemonBlocksWithANamedRemedy(t *testing.T) {
	shrinkReapWaits(t)
	dir := t.TempDir()
	const name = "af_replaced_daemon"
	home := t.TempDir()
	gen := name + "-gen"
	childPidFile := filepath.Join(dir, "child.pid")

	// A REAL af-module build — this test binary, copied so replacing its
	// path never touches the running suite — runs a pure `af --daemon`
	// claim and attests while its name is live.
	exePath := filepath.Join(dir, "af")
	src, err := os.Open(os.Args[0])
	require.NoError(t, err)
	dst, err := os.OpenFile(exePath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755)
	require.NoError(t, err)
	_, err = io.Copy(dst, src)
	require.NoError(t, err)
	require.NoError(t, dst.Close())
	require.NoError(t, src.Close())

	daemon := exec.Command(exePath)
	daemon.Args = []string{"af", "--daemon"}
	daemon.Env = append(os.Environ(),
		decisionDaemonEnv+"=1",
		decisionDaemonSpecEnv+"="+name+"\t"+home+"\t"+gen+"\t"+childPidFile)
	daemon.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	require.NoError(t, daemon.Start())
	t.Cleanup(func() { _ = daemon.Process.Kill(); _, _ = daemon.Process.Wait() })
	waitForPIDFile(t, childPidFile)
	child := processFromPIDFile(t, childPidFile)

	daemonProc, err := proctree.Lookup(daemon.Process.Pid)
	require.NoError(t, err)
	require.Equal(t, afDaemonIs, afDaemonClass(daemonProc, nil),
		"a real af build attests while its path still names the running inode")

	// The upgrade shape: a foreign build is renamed OVER the running exe,
	// unlinking the daemon's text vnode. On darwin no name can reopen it —
	// attestation can never pass again.
	replacement := sleeper.Named(t, filepath.Join(dir, "replacement"))
	require.NoError(t, os.Rename(replacement, exePath))
	require.Eventually(t, func() bool {
		cur, err := proctree.Lookup(daemon.Process.Pid)
		return err == nil && afDaemonClass(cur, nil) == afDaemonUnverified
	}, 10*time.Second, 50*time.Millisecond,
		"once its path opens a different inode the daemon can never verify on darwin")

	// The daemon rides into candidates with no session marker (it leads its
	// own setsid'd session, not the vanished pane's); the marked child is the
	// cohort member whose ancestry ends at it.
	daemonProc, err = proctree.Lookup(daemon.Process.Pid)
	require.NoError(t, err)
	marked, _, err := markedOrphanProcesses(
		[]proctree.Process{daemonProc, child}, name, home,
		fixtureCohort(gen), cohortMembership{}, map[int]bool{}, nil)
	require.Error(t, err, "an unverified daemon still blocks the close it sits in")
	require.Contains(t, err.Error(), "an agent-factory daemon its kernel executable could not verify",
		"the refusal names it as an af daemon that could not be verified")
	require.Contains(t, err.Error(), "descends from pid "+strconv.Itoa(daemonProc.PID),
		"the marked child's refusal names the daemon pid it descends from")
	require.Contains(t, err.Error(), "restart or stop that daemon",
		"the refusal carries the remedy — restart or stop it")
	for _, p := range marked {
		require.NotEqual(t, daemonProc.PID, p.PID)
		require.NotEqual(t, child.PID, p.PID)
	}
	require.True(t, proctree.AliveSame(daemonProc),
		"the claim keeps the unverified daemon protected from signals")
	require.True(t, proctree.AliveSame(child),
		"and its marked descendant alike — blocking never means signalled")
}
