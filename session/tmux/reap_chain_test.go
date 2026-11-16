package tmux

import (
	"flag"
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
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// The -daemon flag is registered so a re-exec'd helper can carry a FORGED
// agent-factory daemon argv — `af --daemon -test.run=...` — without tripping
// the binary's own flag parser. proctree.ArgvIsAFDaemon reads only argv, so
// the helper is positively identified as a daemon by the kill path exactly the
// way a real one is (#7435).
var _ = flag.Bool("daemon", false, "test-only: lets a re-exec'd helper carry daemon-shaped argv")

// #7433 R2 — reapSessionProcesses must never signal the calling process's own
// chain — itself, its ancestors, or a descendant that meets the reaper below a
// tmux process — for any caller. Descent is not membership: a daemon
// EnsureDaemon forked out of an orphaned pane lands in that pane's process
// tree, so a captured tree can contain the caller, the CLI above it, and
// children the caller already spawned. proctree.Signal spares only os.Getpid;
// these tests run the topologies that fail without the narrowed exemption.
//
// The topology is REAL: a private tmux server on an isolated -S socket owns
// the pane, so the reaper's chain is pane -> cli -> setsid daemon -> child
// exactly as it is in production, with the server above all of them — that is
// what keeps the daemon's own child exempt. Each role is the test binary
// re-executing itself; the verdict travels through a file because the daemon
// is a descendant the driver cannot Wait on.
const (
	reapChainRoleEnv   = "AF_TEST_REAP_CHAIN_ROLE"
	reapChainResultEnv = "AF_TEST_REAP_CHAIN_RESULT"
	reapChainSockEnv   = "AF_TEST_REAP_CHAIN_SOCK"
	reapChainTestName  = "TestReapSessionProcessesNeverSignalsTheCallersOwnChain"
)

func TestReapSessionProcessesNeverSignalsTheCallersOwnChain(t *testing.T) {
	if role := os.Getenv(reapChainRoleEnv); role != "" {
		// A re-exec'd binary runs every test its -test.run regex matches; only
		// the roles this test owns do work here — the rest just return.
		runReapChainHelper(t, role)
		return
	}
	testguard.IsolateTmux(t)

	dir := t.TempDir()
	// A socket under t.TempDir overruns darwin's 104-byte sun_path ceiling —
	// testguard.SocketPath hands back a path proven short up front, which is
	// what the comment in codex_safety_real_test.go exists for.
	sock := testguard.SocketPath(t, "chain.sock")
	resultFile := filepath.Join(dir, "verdict")
	// The pane command runs WITHOUT exec, so the pane's own shell stays a
	// distinct node between the server and the cli role: the chain below the
	// server is pane -> cli -> daemon -> child.
	paneScript := filepath.Join(dir, "pane.sh")
	writeReapChainScript(t, paneScript, "cli", resultFile, sock)

	out, err := exec.Command("tmux", "-S", sock,
		"new-session", "-d", "-s", "chain", "sh", paneScript).CombinedOutput()
	require.NoError(t, err, "start the isolated server and chain session: %s", out)
	t.Cleanup(func() { killReapChain(sock, resultFile) })

	verdict := waitForReapChainVerdict(t, resultFile)
	require.Equal(t, "ok", verdict["verdict"],
		"the kill path left the caller's chain inconsistent with the captured evidence")

	// The session kill ends the pane and the cli — their deaths are the
	// ordering this test covers: the daemon reparents mid-flight, and only a
	// protection set captured BEFORE the kill still spares its child.
	for _, role := range []string{"pane", "cli"} {
		pid, err := strconv.Atoi(verdict[role])
		require.NoError(t, err, "the verdict did not report the %s pid", role)
		_, err = proctree.Lookup(pid)
		require.Error(t, err, "the %s (pid %d) dies with the session kill", role, pid)
	}
	for _, role := range []string{"daemon", "child", "other"} {
		pid, err := strconv.Atoi(verdict[role])
		require.NoError(t, err, "the verdict did not report the %s pid", role)
		_, err = proctree.Lookup(pid)
		require.NoError(t, err, "the %s (pid %d) must still be alive — it is the caller's chain or an unrelated session", role, pid)
	}
}

// runReapChainHelper plays one node of pane -> cli -> setsid daemon -> child.
// Every helper stays alive as a chain member until the driver kills the whole
// chain; the bound keeps a leaked helper from living forever.
func runReapChainHelper(t *testing.T, role string) {
	switch role {
	case "cli":
		daemon := reexecReapChain(t, "daemon")
		daemon.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		require.NoError(t, daemon.Start())
	case "daemon":
		runReapChainDaemon(t)
	default:
		// A role owned by the sibling test in the shared -test.run regex.
		return
	}
	time.Sleep(5 * time.Minute)
}

// The daemon role: spawn its own child and an unrelated pane on the same
// server, stamp the bare-pane markers on its own session, then run the REAL
// kill path — ReapProvenBareSession, which captures the pane tree and the
// reaper's own-chain evidence, kills the session (the pane shell and the cli
// die with it, reparenting the daemon), and reaps the capture. The ordering
// this pins is the one Codex flagged: the protection evidence must come from
// before the kill, or the reparented daemon's child loses it.
func runReapChainDaemon(t *testing.T) {
	sock := os.Getenv(reapChainSockEnv)
	home := filepath.Dir(sock)
	child := exec.Command("sleep", "300")
	require.NoError(t, child.Start())
	self := os.Getpid()
	fmt.Fprintf(os.Stderr, "chain-daemon %d: spawned child %d\n", self, child.Process.Pid)

	out, err := exec.Command("tmux", "-S", sock,
		"new-session", "-d", "-s", "other", "sleep", "300").CombinedOutput()
	require.NoError(t, err, "spawn the unrelated pane: %s", out)

	// The session proves itself a bare pane this home owns, exactly as a
	// config-agent/login session does: name, absolute home, generation — and
	// no AF_INSTANCE_ID.
	for _, kv := range [][2]string{
		{EnvMarkerSession, "chain"},
		{EnvMarkerHome, home},
		{EnvMarkerGeneration, "1"},
	} {
		out, err := exec.Command("tmux", "-S", sock, "set-environment", "-t", "chain",
			kv[0], kv[1]).CombinedOutput()
		require.NoError(t, err, "stamp %s: %s", kv[0], out)
	}

	var paneP, cliP, selfP, childP, otherP proctree.Process
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
		// The child's identity is captured pre-kill: a mutation that lets the
		// reap signal it leaves it gone afterwards, and a Lookup then would
		// fail the helper before the verdict file is written.
		var cerr error
		if childP, cerr = proctree.Lookup(child.Process.Pid); cerr != nil {
			return false
		}
		lout, err := exec.Command("tmux", "-S", sock, "list-panes", "-t", "other", "-F", "#{pane_pid}").Output()
		if err != nil {
			return false
		}
		opid, err := strconv.Atoi(strings.TrimSpace(string(lout)))
		if err != nil {
			return false
		}
		otherP, err = proctree.Lookup(opid)
		return err == nil
	}, 10*time.Second, 50*time.Millisecond, "the pane -> cli -> daemon chain and the other pane never became visible")
	fmt.Fprintf(os.Stderr, "chain-daemon %d: chain pane=%d cli=%d other=%d\n", self, paneP.PID, cliP.PID, otherP.PID)

	reap := ReapProvenBareSession(socketExecutor{socket: sock}, "", BareSessionProof{
		SessionName: "chain",
		Home:        home,
	})
	fmt.Fprintf(os.Stderr, "chain-daemon %d: kill path returned stopped=%v why=%q\n", self, reap.Stopped, reap.Why)

	// pane and cli die with the session kill — that is the ordering this test
	// exists to cover — but SIGHUP may still be in flight when the kill path
	// returns, so their deaths are waited on rather than read once.
	paneDead, cliDead := false, false
	for range 40 {
		paneDead, cliDead = !proctree.AliveSame(paneP), !proctree.AliveSame(cliP)
		if paneDead && cliDead {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	var why []string
	if !reap.Stopped {
		why = append(why, "the session kill did not fire: "+reap.Why)
	}
	if !proctree.AliveSame(selfP) {
		why = append(why, "daemon was signalled")
	}
	if !proctree.AliveSame(childP) {
		why = append(why, "child was signalled")
	}
	if !proctree.AliveSame(otherP) {
		why = append(why, "the pane on another session was signalled")
	}
	if !paneDead {
		why = append(why, "pane survived the session kill")
	}
	if !cliDead {
		why = append(why, "cli survived the session kill")
	}
	status := "ok"
	if len(why) > 0 {
		status = "fail: " + strings.Join(why, "; ")
	}
	writeReapChainVerdict(t, os.Getenv(reapChainResultEnv), map[string]string{
		"pane": strconv.Itoa(paneP.PID), "cli": strconv.Itoa(cliP.PID),
		"daemon": strconv.Itoa(selfP.PID), "child": strconv.Itoa(childP.PID),
		"other": strconv.Itoa(otherP.PID), "verdict": status,
	})
}

// The complementary topology the narrowed guard exists for: the session's
// tmux server is a DESCENDANT of the reaper — reparented beneath it, or
// spawned by it outright — and a pane process of that server is still reaped,
// because a pane's own walk meets the server before the reaper. The reaper
// here spawns a foreground -D server directly, so the server is literally its
// child, the strongest form of the production case.
func TestReapSessionProcessesReapsAPaneWhoseServerIsADescendantOfTheReaper(t *testing.T) {
	if role := os.Getenv(reapChainRoleEnv); role != "" {
		if role == "srvreaper" {
			runReapChainServerDescendant(t)
		}
		return
	}
	testguard.IsolateTmux(t)

	dir := t.TempDir()
	sock := testguard.SocketPath(t, "srv.sock")
	resultFile := filepath.Join(dir, "verdict")

	reaper := reexecReapChain(t, "srvreaper")
	reaper.Env = append(reaper.Env, reapChainResultEnv+"="+resultFile, reapChainSockEnv+"="+sock)
	require.NoError(t, reaper.Start())
	t.Cleanup(func() { killReapChain(sock, resultFile) })

	verdict := waitForReapChainVerdict(t, resultFile)
	require.Equal(t, "ok", verdict["verdict"],
		"a pane whose server sits under the reaper must still be reaped")

	panePID, err := strconv.Atoi(verdict["pane"])
	require.NoError(t, err)
	_, err = proctree.Lookup(panePID)
	require.Error(t, err, "the pane process must have been reaped")
	serverPID, err := strconv.Atoi(verdict["server"])
	require.NoError(t, err)
	_, err = proctree.Lookup(serverPID)
	require.NoError(t, err, "the server itself is not a captured process: nothing signals it")
}

// #7435 item 1 — descent is not ownership, and membership is not
// signalability. Inside one captured pane tree sit three shapes the reap must
// treat differently:
//
//   - a setsid'd agent-factory DAEMON and the child it already spawned:
//     captured by descent, but positively identified (af binary + --daemon
//     argv, the shared classifier) and excluded as a daemon subtree — not
//     because the reaper is in their chain (it is not; the test binary itself
//     reaps, from outside the pane entirely);
//   - a detached, setsid'd non-daemon child: captured by descent and REAPED —
//     this is what TreeOf exists for, and what SID-only membership would leak
//     (#1093/#1104);
//   - a pane member reparented to init while the pane is still up: captured by
//     its kernel SID and reaped.
const (
	reapDaemonSubtreeTestName = "TestReapSessionProcessesSparesAFDaemonSubtreeButReapsItsDetachedPeers"
	subtreePidFileEnv         = "AF_TEST_SUBTREE_PIDFILE"
	subtreeAfPidFileEnv       = "AF_TEST_SUBTREE_AFPID"
	subtreeRepIDEnv           = "AF_TEST_SUBTREE_REPID"
)

func TestReapSessionProcessesSparesAFDaemonSubtreeButReapsItsDetachedPeers(t *testing.T) {
	if role := os.Getenv(reapChainRoleEnv); role != "" {
		runDaemonSubtreeHelper(t, role)
		return
	}
	testguard.IsolateTmux(t)

	dir := t.TempDir()
	sock := testguard.SocketPath(t, "subtree.sock")
	pidFile := filepath.Join(dir, "pids")
	afpidFile := filepath.Join(dir, "afpids")
	repFile := filepath.Join(dir, "repid")

	// The pane runs WITHOUT exec, so its own shell stays a distinct chain node
	// between the server and the cli role: pane -> cli -> the spawned subtrees.
	script := "#!/bin/sh\n" +
		reapChainRoleEnv + "=dchaincli " +
		reapChainSockEnv + "=" + sock + " " +
		subtreePidFileEnv + "=" + pidFile + " " +
		subtreeAfPidFileEnv + "=" + afpidFile + " " +
		subtreeRepIDEnv + "=" + repFile + " " +
		"'" + os.Args[0] + "' '-test.run=^" + reapDaemonSubtreeTestName + "$'\n"
	paneScript := filepath.Join(dir, "pane.sh")
	require.NoError(t, os.WriteFile(paneScript, []byte(script), 0o755))

	out, err := exec.Command("tmux", "-S", sock,
		"new-session", "-d", "-s", "dchain", "sh", paneScript).CombinedOutput()
	require.NoError(t, err, "start the isolated server and daemonchain session: %s", out)
	t.Cleanup(func() {
		killReapChain(sock, pidFile)
		killReapChain(sock, afpidFile)
		killReapChain(sock, repFile)
	})

	out, err = exec.Command("tmux", "-S", sock,
		"new-session", "-d", "-s", "subother", "sleep", "300").CombinedOutput()
	require.NoError(t, err, "spawn the unrelated session's pane: %s", out)

	// The session proves itself a bare pane this home owns: name, absolute
	// home, generation — and no AF_INSTANCE_ID.
	for _, kv := range [][2]string{
		{EnvMarkerSession, "dchain"},
		{EnvMarkerHome, dir},
		{EnvMarkerGeneration, "1"},
	} {
		out, err := exec.Command("tmux", "-S", sock, "set-environment", "-t", "dchain",
			kv[0], kv[1]).CombinedOutput()
		require.NoError(t, err, "stamp %s: %s", kv[0], out)
	}

	// Wait for every spawned role to write its pid, then resolve the chain
	// identities. The afdaemon's argv is asserted daemon-shaped up front, or
	// the fixture does not test the exclusion it claims to.
	var pids map[string]string
	require.Eventually(t, func() bool {
		for _, f := range []string{pidFile, afpidFile, repFile} {
			if _, err := os.Stat(f); err != nil {
				return false
			}
		}
		pids = readReapChainPids(pidFile)
		for _, f := range []string{afpidFile, repFile} {
			for line, v := range readReapChainPids(f) {
				pids[line] = v
			}
		}
		_, hasAFD := pids["afdaemon"]
		_, hasDet := pids["detached"]
		_, hasChild := pids["afchild"]
		_, hasRep := pids["reparented"]
		return hasAFD && hasDet && hasChild && hasRep
	}, 15*time.Second, 50*time.Millisecond, "the cli role never reported its spawned pids")

	afPID := atoiReapChainPid(t, pids["afdaemon"])
	require.True(t, proctree.ArgvIsAFDaemon(proctree.Argv(afPID)),
		"the forged helper must positively classify as an agent-factory daemon or the fixture tests nothing")

	var paneP, cliP, repP, otherP proctree.Process
	var afP, childP, detP proctree.Process
	require.Eventually(t, func() bool {
		var err error
		if afP, err = proctree.Lookup(afPID); err != nil {
			return false
		}
		snap, err := proctree.Snapshot()
		if err != nil {
			return false
		}
		cliP, ok := snap[afP.PPID]
		if !ok {
			return false
		}
		paneP, ok = snap[cliP.PPID]
		if !ok {
			return false
		}
		if childP, err = proctree.Lookup(atoiReapChainPid(t, pids["afchild"])); err != nil {
			return false
		}
		if detP, err = proctree.Lookup(atoiReapChainPid(t, pids["detached"])); err != nil {
			return false
		}
		repP, err = proctree.Lookup(atoiReapChainPid(t, pids["reparented"]))
		if err != nil || repP.PPID != 1 {
			// Still the dying sh's child: the reparented shape is only real
			// once the kernel hands it to init (no reaper in this chain marks
			// itself a subreaper).
			return false
		}
		lout, err := exec.Command("tmux", "-S", sock, "list-panes", "-t", "subother", "-F", "#{pane_pid}").Output()
		if err != nil {
			return false
		}
		opid, err := strconv.Atoi(strings.TrimSpace(string(lout)))
		if err != nil {
			return false
		}
		otherP, err = proctree.Lookup(opid)
		return err == nil
	}, 10*time.Second, 50*time.Millisecond, "the pane -> cli -> subtree chain never became visible")

	reap := ReapProvenBareSession(socketExecutor{socket: sock}, "", BareSessionProof{
		SessionName: "dchain",
		Home:        dir,
	})
	require.True(t, reap.Stopped, "the guarded kill path refused: %s", reap.Why)

	// Everything that is NOT a daemon subtree dies: the pane and cli with the
	// session kill, the detached child to the reap's own signals (it left the
	// pane session, so only descent plus signalling could reach it), the
	// reparented survivor to the session kill's SIGHUP or the reap.
	for role, p := range map[string]proctree.Process{
		"pane": paneP, "cli": cliP, "detached": detP, "reparented": repP,
	} {
		p := p
		require.Eventually(t, func() bool {
			return !proctree.AliveSame(p)
		}, 10*time.Second, 50*time.Millisecond,
			"the %s (pid %d) must be reaped — descent and SID membership capture it, and nothing protects it", role, p.PID)
	}
	// The af daemon and its child survive as a positively-identified daemon
	// subtree; the unrelated session's pane was never captured.
	for role, p := range map[string]proctree.Process{
		"afdaemon": afP, "afchild": childP, "other": otherP,
	} {
		require.True(t, proctree.AliveSame(p),
			"the %s (pid %d) must still be alive — an af-daemon subtree, or another session entirely", role, p.PID)
	}
}

// runDaemonSubtreeHelper plays one node of the daemonchain fixture:
// dchaincli spawns the three subtree shapes, afdaemon spawns its child. Every
// role stays alive until the driver's cleanup kills the chain.
func runDaemonSubtreeHelper(t *testing.T, role string) {
	switch role {
	case "dchaincli":
		af := reexecDaemonSubtree(t, "afdaemon")
		af.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		require.NoError(t, af.Start())
		det := exec.Command("sleep", "300")
		det.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		require.NoError(t, det.Start())
		rep := exec.Command("sh", "-c",
			`sleep 300 & echo "reparented=$!" > "$AF_TEST_SUBTREE_REPID"; exec sleep 0.05`)
		require.NoError(t, rep.Start())
		writeReapChainPids(t, os.Getenv(subtreePidFileEnv), map[string]int{
			"afdaemon": af.Process.Pid, "detached": det.Process.Pid,
		})
	case "afdaemon":
		child := exec.Command("sleep", "300")
		require.NoError(t, child.Start())
		writeReapChainPids(t, os.Getenv(subtreeAfPidFileEnv), map[string]int{
			"afchild": child.Process.Pid,
		})
	default:
		return
	}
	time.Sleep(5 * time.Minute)
}

// reexecDaemonSubtree re-executes the test binary under the daemonchain
// fixture's own -test.run regex. The afdaemon role additionally forges its
// argv to `af --daemon -test.run=...`: argv is all the classifier reads, so
// the helper is a positively-identified agent-factory daemon to the kill path
// while remaining this test's dispatch.
func reexecDaemonSubtree(t *testing.T, role string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+reapDaemonSubtreeTestName+"$")
	cmd.Env = append(os.Environ(), reapChainRoleEnv+"="+role)
	if role == "afdaemon" {
		cmd.Args = []string{"af", "--daemon", "-test.run=^" + reapDaemonSubtreeTestName + "$"}
	}
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	return cmd
}

func writeReapChainPids(t *testing.T, path string, pids map[string]int) {
	t.Helper()
	var b strings.Builder
	for k, pid := range pids {
		fmt.Fprintf(&b, "%s=%d\n", k, pid)
	}
	require.NoError(t, os.WriteFile(path, []byte(b.String()), 0o600))
}

func readReapChainPids(path string) map[string]string {
	out := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			out[k] = v
		}
	}
	return out
}

func atoiReapChainPid(t *testing.T, s string) int {
	t.Helper()
	pid, err := strconv.Atoi(s)
	require.NoError(t, err, "a pid file carried a non-numeric pid %q", s)
	return pid
}

// runReapChainServerDescendant is the srvreaper role: spawn a foreground tmux
// server as its own child, put a session on it, and reap that pane.
func runReapChainServerDescendant(t *testing.T) {
	sock := os.Getenv(reapChainSockEnv)
	server := exec.Command("tmux", "-S", sock, "-D")
	require.NoError(t, server.Start())
	fmt.Fprintf(os.Stderr, "srvreaper %d: spawned -D server %d\n", os.Getpid(), server.Process.Pid)

	require.Eventually(t, func() bool {
		return exec.Command("tmux", "-S", sock, "display-message", "-p", "#{pid}").Run() == nil
	}, 10*time.Second, 50*time.Millisecond, "the -D server never answered")
	out, err := exec.Command("tmux", "-S", sock,
		"new-session", "-d", "-s", "x", "sleep", "300").CombinedOutput()
	require.NoError(t, err, "spawn the session pane: %s", out)

	var paneP proctree.Process
	require.Eventually(t, func() bool {
		lout, err := exec.Command("tmux", "-S", sock, "list-panes", "-t", "x", "-F", "#{pane_pid}").Output()
		if err != nil {
			return false
		}
		pid, err := strconv.Atoi(strings.TrimSpace(string(lout)))
		if err != nil {
			return false
		}
		paneP, err = proctree.Lookup(pid)
		return err == nil
	}, 10*time.Second, 50*time.Millisecond, "the pane process never became visible")

	serverP, err := proctree.Lookup(server.Process.Pid)
	require.NoError(t, err)
	reapSessionProcesses(reapOnRequest, "af_test_srvreap", []proctree.Process{paneP}, 100*time.Millisecond, 100*time.Millisecond, selfProtectedSet())
	fmt.Fprintf(os.Stderr, "srvreaper %d: reap returned\n", os.Getpid())

	status := "ok"
	if proctree.AliveSame(paneP) {
		status = "fail: the pane under a descendant server survived"
	}
	if !proctree.AliveSame(serverP) {
		status = "fail: the server itself was signalled"
	}
	writeReapChainVerdict(t, os.Getenv(reapChainResultEnv), map[string]string{
		"pane": strconv.Itoa(paneP.PID), "server": strconv.Itoa(serverP.PID),
		"daemon": strconv.Itoa(os.Getpid()), "verdict": status,
	})
	time.Sleep(5 * time.Minute)
}

func reexecReapChain(t *testing.T, role string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^"+reapChainTestName+"$|^TestReapSessionProcessesReapsAPaneWhoseServerIsADescendantOfTheReaper$")
	cmd.Env = append(os.Environ(), reapChainRoleEnv+"="+role)
	// Every level forwards its stderr through its parent's, which ultimately
	// lands on the driver's stderr — a helper that dies or fails a require is
	// otherwise invisible to the test's log.
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	return cmd
}

// writeReapChainScript writes the shell fragment the tmux pane runs. It does
// not exec the helper, so the script's own shell stays a distinct chain node —
// the pane — between the server and the cli role.
func writeReapChainScript(t *testing.T, path, role, resultFile, sock string) {
	t.Helper()
	script := "#!/bin/sh\n" + reapChainRoleEnv + "=" + role + " " +
		reapChainResultEnv + "=" + resultFile + " " +
		reapChainSockEnv + "=" + sock + " " +
		"'" + os.Args[0] + "' '-test.run=^" + reapChainTestName + "$'\n"
	require.NoError(t, os.WriteFile(path, []byte(script), 0o755))
}

func writeReapChainVerdict(t *testing.T, path string, fields map[string]string) {
	t.Helper()
	var b strings.Builder
	for _, k := range []string{"pane", "cli", "daemon", "child", "other", "server", "verdict"} {
		if v, ok := fields[k]; ok {
			fmt.Fprintf(&b, "%s=%s\n", k, v)
		}
	}
	require.NoError(t, os.WriteFile(path, []byte(b.String()), 0o600))
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
	}, 15*time.Second, 50*time.Millisecond, "the reaper role never wrote a verdict")
	return out
}

// killReapChain kills the isolated tmux server — which ends every pane and
// session it owns — and any chain member the verdict file still reports.
// Everything lives on the test's own -S socket: the kill names that socket,
// never the shared one.
func killReapChain(sock, resultFile string) {
	if b, err := os.ReadFile(resultFile); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			k, v, ok := strings.Cut(line, "=")
			if !ok || k == "verdict" {
				continue
			}
			if pid, e := strconv.Atoi(v); e == nil {
				if proc, e2 := os.FindProcess(pid); e2 == nil {
					_ = proc.Kill()
				}
			}
		}
	}
	_ = exec.Command("tmux", "-S", sock, "kill-server").Run()
}
