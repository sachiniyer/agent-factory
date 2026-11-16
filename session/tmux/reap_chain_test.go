package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// #7433 R2 — reapSessionProcesses must never signal the calling process's own
// chain — itself, its ancestors, or its descendants — for any caller. Descent
// is not membership: a daemon EnsureDaemon forked out of an orphaned pane
// lands in that pane's process tree, so a captured tree can contain the
// caller, the CLI above it, and children the caller already spawned.
// proctree.Signal spares only os.Getpid; this test runs the topology that
// fails without the exemption: pane -> cli -> daemon (setsid) -> child, plus
// an unrelated leaf under the pane that IS reaped.
//
// The daemon role itself invokes the reap, so the protected set is computed
// around its pid by the production code path — no seams. Each role is the
// test binary re-executing itself; the verdict travels through a file because
// the daemon is a grandchild the driver cannot Wait on.
const (
	reapChainRoleEnv   = "AF_TEST_REAP_CHAIN_ROLE"
	reapChainResultEnv = "AF_TEST_REAP_CHAIN_RESULT"
)

func TestReapSessionProcessesNeverSignalsTheCallersOwnChain(t *testing.T) {
	if role := os.Getenv(reapChainRoleEnv); role != "" {
		runReapChainHelper(t, role)
		return
	}

	resultFile := filepath.Join(t.TempDir(), "verdict")
	pane := reexecReapChain(t, "pane", resultFile)
	pane.Stdout = os.Stderr
	pane.Stderr = os.Stderr
	require.NoError(t, pane.Start())
	t.Cleanup(func() { killReapChainTree(pane.Process.Pid) })

	verdict := waitForReapChainVerdict(t, resultFile)
	require.Equal(t, "ok", verdict["verdict"],
		"the reap signalled part of its own chain")

	for _, role := range []string{"pane", "cli", "daemon", "child"} {
		pid, err := strconv.Atoi(verdict[role])
		require.NoError(t, err, "the verdict did not report the %s pid", role)
		_, err = proctree.Lookup(pid)
		require.NoError(t, err, "the %s (pid %d) is the caller's chain and must still be alive", role, pid)
	}
	otherPID, err := strconv.Atoi(verdict["other"])
	require.NoError(t, err)
	_, err = proctree.Lookup(otherPID)
	require.Error(t, err, "the leaf outside the caller's chain must have been reaped")
}

// runReapChainHelper plays one node of pane -> cli -> setsid daemon -> child.
// Every helper stays alive as a chain member until the driver kills the whole
// tree; the bound keeps a leaked helper from living forever.
func runReapChainHelper(t *testing.T, role string) {
	switch role {
	case "pane":
		cli := reexecReapChain(t, "cli", os.Getenv(reapChainResultEnv))
		require.NoError(t, cli.Start())
		// The leaf under the pane but outside the daemon's chain: the one
		// process this reap is meant to signal.
		other := exec.Command("sleep", "300")
		require.NoError(t, other.Start())
	case "cli":
		daemon := reexecReapChain(t, "daemon", os.Getenv(reapChainResultEnv))
		daemon.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		require.NoError(t, daemon.Start())
	case "daemon":
		runReapChainDaemon(t)
	}
	time.Sleep(5 * time.Minute)
}

// The daemon role: spawn its own child, discover the chain above it and the
// sibling leaf from the process table, then run the real reap over all of
// them and report what survived.
func runReapChainDaemon(t *testing.T) {
	child := exec.Command("sleep", "300")
	require.NoError(t, child.Start())
	self := os.Getpid()
	fmt.Fprintf(os.Stderr, "chain-daemon %d: spawned child %d\n", self, child.Process.Pid)

	var paneP, cliP, selfP, otherP proctree.Process
	require.Eventually(t, func() bool {
		snap, err := proctree.Snapshot()
		if err != nil {
			return false
		}
		me, ok := snap[self]
		if !ok {
			return false
		}
		cliP, ok = snap[me.PPID]
		if !ok {
			return false
		}
		paneP, ok = snap[cliP.PPID]
		if !ok {
			return false
		}
		selfP = me
		for pid, p := range snap {
			if p.PPID == paneP.PID && pid != cliP.PID {
				otherP = p
				return true
			}
		}
		return false
	}, 10*time.Second, 25*time.Millisecond, "the pane -> cli -> daemon chain never became visible")
	fmt.Fprintf(os.Stderr, "chain-daemon %d: chain pane=%d cli=%d other=%d\n", self, paneP.PID, cliP.PID, otherP.PID)

	childP, err := proctree.Lookup(child.Process.Pid)
	require.NoError(t, err)
	procs := []proctree.Process{paneP, cliP, selfP, childP, otherP}

	reapSessionProcesses(reapOnRequest, "af_test_chain", procs, 100*time.Millisecond, 100*time.Millisecond)
	fmt.Fprintf(os.Stderr, "chain-daemon %d: reap returned\n", self)

	var why []string
	for _, pc := range []struct {
		name string
		p    proctree.Process
	}{
		{name: "pane", p: paneP}, {name: "cli", p: cliP}, {name: "daemon", p: selfP}, {name: "child", p: childP},
	} {
		if !proctree.AliveSame(pc.p) {
			why = append(why, pc.name+" was signalled")
		}
	}
	if proctree.AliveSame(otherP) {
		why = append(why, "the leaf outside the chain survived")
	}
	status := "ok"
	if len(why) > 0 {
		status = "fail: " + strings.Join(why, "; ")
	}
	writeReapChainVerdict(t, map[string]string{
		"pane": strconv.Itoa(paneP.PID), "cli": strconv.Itoa(cliP.PID),
		"daemon": strconv.Itoa(selfP.PID), "child": strconv.Itoa(childP.PID),
		"other": strconv.Itoa(otherP.PID), "verdict": status,
	})
}

func reexecReapChain(t *testing.T, role, resultFile string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestReapSessionProcessesNeverSignalsTheCallersOwnChain$")
	cmd.Env = append(os.Environ(), reapChainRoleEnv+"="+role, reapChainResultEnv+"="+resultFile)
	// Every level forwards its stderr through its parent's, which ultimately
	// lands on the driver's stderr — a helper that dies or fails a require is
	// otherwise invisible to the test's log.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd
}

func writeReapChainVerdict(t *testing.T, fields map[string]string) {
	t.Helper()
	var b strings.Builder
	for _, k := range []string{"pane", "cli", "daemon", "child", "other", "verdict"} {
		fmt.Fprintf(&b, "%s=%s\n", k, fields[k])
	}
	require.NoError(t, os.WriteFile(os.Getenv(reapChainResultEnv), []byte(b.String()), 0o600))
}

func waitForReapChainVerdict(t *testing.T, path string) map[string]string {
	t.Helper()
	out := map[string]string{}
	require.Eventually(t, func() bool {
		b, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			k, v, ok := strings.Cut(line, "=")
			if !ok {
				return false
			}
			out[k] = v
		}
		return out["verdict"] != ""
	}, 15*time.Second, 50*time.Millisecond, "the daemon role never wrote a verdict")
	return out
}

// killReapChainTree kills every descendant of root and root itself. The
// daemon ran setsid, so no process-group kill reaches it; the tree walk does,
// because setsid changes the session id, not the parent link.
func killReapChainTree(root int) {
	if snap, err := proctree.Snapshot(); err == nil {
		for _, p := range proctree.TreeOf(snap, root) {
			if proc, e := os.FindProcess(p.PID); e == nil {
				_ = proc.Kill()
			}
		}
	}
	if proc, e := os.FindProcess(root); e == nil {
		_ = proc.Kill()
	}
}
