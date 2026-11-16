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
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

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
	sock := filepath.Join(dir, "chain.sock")
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
	require.Error(t, err, "the pane outside the caller's chain must have been reaped")
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
// server, discover the chain above it from the process table, then run the
// real reap over all of them and report what survived. "pane" is cli's
// parent — the pane-root shell — so the daemon's ancestors include the pane
// exactly as a pane-spawned daemon's would.
func runReapChainDaemon(t *testing.T) {
	sock := os.Getenv(reapChainSockEnv)
	child := exec.Command("sleep", "300")
	require.NoError(t, child.Start())
	self := os.Getpid()
	fmt.Fprintf(os.Stderr, "chain-daemon %d: spawned child %d\n", self, child.Process.Pid)

	out, err := exec.Command("tmux", "-S", sock,
		"new-session", "-d", "-s", "other", "sleep", "300").CombinedOutput()
	require.NoError(t, err, "spawn the unrelated pane: %s", out)

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
		why = append(why, "the pane outside the chain survived")
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
	sock := filepath.Join(dir, "srv.sock")
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
	reapSessionProcesses(reapOnRequest, "af_test_srvreap", []proctree.Process{paneP}, 100*time.Millisecond, 100*time.Millisecond)
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
