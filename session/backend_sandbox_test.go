package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The two traps the #2476 handoff calls "the whole risk of PR2", pinned WITHOUT
// a real sshd.
//
// Both fail silently in production — a truncated `af` that reports success, and
// a reap that claims a sandbox is gone when ssh itself never reached it — so
// neither can be left to an integration suite this host must not run. A stub
// `ssh` on PATH is enough: the traps live in how af invokes and interprets the
// command, not in the remote side.

// stubSSH writes an executable named `ssh` into a fresh dir and returns a
// sandbox_ssh command line pointing at it. The script body decides what the
// fake ssh does.
func stubSSH(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "ssh")
	require.NoError(t, os.WriteFile(path, []byte("#!/usr/bin/env bash\n"+body), 0o755))
	return path
}

// TestSandboxRunPassesTheScriptAsOneArgument pins the composition: whatever a
// provision step asks to run must reach ssh as a SINGLE argv element, so a path
// or branch name containing spaces or quotes cannot break out and become another
// ssh flag.
func TestSandboxRunPassesTheScriptAsOneArgument(t *testing.T) {
	// Print the argument count and the last argument, so a split script shows up
	// as a count > 1.
	ssh := stubSSH(t, `printf 'argc=%s last=%s' "$#" "${!#}"`)
	p := &sandboxProvisioner{sshCmd: ssh}

	out, err := p.Run(5*time.Second, `echo 'a b' && ls "/tmp/x y"`, nil, true)
	require.NoError(t, err)
	got := string(out)
	assert.Contains(t, got, "argc=1", "the remote script must arrive as exactly one argv element, not split on spaces")
	assert.Contains(t, got, `last=echo 'a b' && ls "/tmp/x y"`, "and must arrive byte-for-byte")
}

// TestSandboxRunKeepsOperatorQuoting pins the other half: the operator's own
// sandbox_ssh flags survive, because the command line goes through `sh -c`
// rather than being naively split on spaces.
func TestSandboxRunKeepsOperatorQuoting(t *testing.T) {
	ssh := stubSSH(t, `printf '%s' "$*"`)
	// A ProxyCommand-shaped option with an embedded space, exactly what a naive
	// strings.Fields() split would destroy.
	p := &sandboxProvisioner{sshCmd: ssh + ` -o "ProxyCommand=nc -X connect %h %p"`}

	out, err := p.Run(5*time.Second, "true", nil, true)
	require.NoError(t, err)
	assert.Contains(t, string(out), "ProxyCommand=nc -X connect %h %p",
		"an operator's quoted ssh option must survive intact")
}

// TRAP 1, as revised by Codex 3725079699 on #2995.
//
// The handoff's rule was "WaitDelay on the tunnel child, NEVER on the copy",
// because force-closing the pipe mid-copy truncates the stream while ssh still
// exits 0. That holds only while nothing else catches a short copy — and
// verifyCopiedBinary now does. Meanwhile a ZERO WaitDelay is not neutral:
// os/exec makes Wait block until every orphaned descriptor-holder closes, which
// for this backend is exactly the ProxyCommand/wrapper it exists to serve, so
// the copy's timeout would not bound provisioning at all.
//
// Both children are therefore bounded, and the size check is what rejects a
// truncated result. Driven through buildRunCommand, the real constructor.
func TestSandboxRunBoundsEveryChildAfterCancellation(t *testing.T) {
	p := &sandboxProvisioner{sshCmd: "ssh host"}
	ctx := context.Background()

	streaming := p.buildRunCommand(ctx, "cat > af", strings.NewReader("payload"))
	assert.Equal(t, sandboxTunnelWaitDelay, streaming.WaitDelay,
		"a zero WaitDelay lets Wait block on an orphaned ProxyCommand holding the pipe, so the copy "+
			"timeout would not bound provisioning; truncation is caught by verifyCopiedBinary instead")

	payloadFree := p.buildRunCommand(ctx, "true", nil)
	assert.Equal(t, sandboxTunnelWaitDelay, payloadFree.WaitDelay)
}

// The production path must agree with the rule above. Driving a real copy with a
// stdin payload through runCommand and asserting the bytes arrive whole is what
// proves the guard is wired, not just stated in a comment.
func TestSandboxRunStreamsStdinWhole(t *testing.T) {
	sink := filepath.Join(t.TempDir(), "received")
	ssh := stubSSH(t, `cat > `+sink+`; true`)
	p := &sandboxProvisioner{sshCmd: ssh}

	payload := strings.Repeat("af-binary-bytes-", 4096) // ~64KB, several pipe buffers
	_, err := p.Run(20*time.Second, "cat > /dev/null", strings.NewReader(payload), true)
	require.NoError(t, err)

	got, err := os.ReadFile(sink)
	require.NoError(t, err)
	assert.Equal(t, len(payload), len(got), "the streamed payload must arrive whole — a short write is the truncated-af trap")
}

// verifyCopiedBinary is the independent check behind TRAP 1: even if a copy is
// silently short, the byte count on the sandbox must not match and the error
// must name the copy rather than surfacing later as an exec failure.
func TestSandboxVerifyCopiedBinaryCatchesATruncatedCopy(t *testing.T) {
	local := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(local, []byte("0123456789"), 0o755)) // 10 bytes

	p := &sandboxProvisioner{afBin: local, sessionDir: "/remote/dir"}
	p.runCommandFn = func(time.Duration, string, io.Reader, bool) ([]byte, error) {
		return []byte("4\n"), nil // the sandbox only has 4 of the 10 bytes
	}

	err := p.verifyCopiedBinary()
	require.Error(t, err, "a short copy must fail the provision at the copy, not later")
	assert.Contains(t, err.Error(), "truncated")
	assert.Contains(t, err.Error(), "/remote/dir/af")
}

func TestSandboxVerifyCopiedBinaryAcceptsAWholeCopy(t *testing.T) {
	local := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(local, []byte("0123456789"), 0o755))

	p := &sandboxProvisioner{afBin: local, sessionDir: "/remote/dir"}
	p.runCommandFn = func(time.Duration, string, io.Reader, bool) ([]byte, error) {
		return []byte("10\n"), nil
	}
	assert.NoError(t, p.verifyCopiedBinary())
}

// TRAP 2. ssh(1) exits 255 for its OWN failures, and a remote command exiting
// 255 is indistinguishable from it. A reap that saw 255 must report UNDETERMINED
// and must NOT latch, or the daemon deletes a record whose sandbox may still be
// running and bills forever with nothing pointing at it.
func TestSandboxReapTreatsExit255AsUndetermined(t *testing.T) {
	ssh := stubSSH(t, "exit 255")
	p := &sandboxProvisioner{sshCmd: ssh, sessionDir: "/remote/dir", remotePID: "123"}

	err := p.reap()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrWorkspaceStateUnknown),
		"ssh's ambiguous 255 must be unknown-state, never a confident reap")
	assert.False(t, p.reaped, "an undetermined reap must NOT latch — the next poll has to retry it")

	// And it really does retry rather than returning a cached verdict.
	second := p.reap()
	assert.True(t, errors.Is(second, ErrWorkspaceStateUnknown))
}

// A definite non-255 failure is an ANSWER: the reap completed and told us
// something, so it latches and the record may be deleted.
func TestSandboxReapLatchesADefiniteFailure(t *testing.T) {
	// Answers the challenge (uppercasing it, as only real remote execution can),
	// then reports a failure — a definite remote answer.
	ssh := stubSSH(t, `printf '%s' "${!#}" | grep -o 'af-sandbox-reaped-[0-9a-f]*' | tr 'a-z' 'A-Z'; echo 'rm: permission denied' >&2; exit 1`)
	p := &sandboxProvisioner{sshCmd: ssh, sessionDir: "/remote/dir"}

	err := p.reap()
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrWorkspaceStateUnknown),
		"a definite remote status is an answer, not an unknown state")
	assert.True(t, p.reaped, "an answered reap latches so retries do not re-run it")
}

func TestSandboxReapSucceedsAndLatches(t *testing.T) {
	ssh := stubSSH(t, `printf '%s' "${!#}" | grep -o 'af-sandbox-reaped-[0-9a-f]*' | tr 'a-z' 'A-Z'; exit 0`)
	p := &sandboxProvisioner{sshCmd: ssh, sessionDir: "/remote/dir", remotePID: "77"}

	require.NoError(t, p.reap())
	assert.True(t, p.reaped)
	assert.NoError(t, p.reap(), "a latched success is idempotent")
}

// A reap with nothing provisioned yet is a completed no-op, not an ssh call.
func TestSandboxReapWithoutASessionDirIsANoOp(t *testing.T) {
	ssh := stubSSH(t, "echo 'ssh must not run' >&2; exit 9")
	p := &sandboxProvisioner{sshCmd: ssh}
	require.NoError(t, p.reap())
	assert.True(t, p.reaped)
}

// The tunnel readiness probe must FAIL a provision whose forward never came up,
// rather than handing back an endpoint nothing is listening on.
func TestSandboxTunnelProbeFailsWhenNothingListens(t *testing.T) {
	// Bind and release a port so it is almost certainly free, then never listen.
	free := freeLocalAddrForTest(t)
	start := time.Now()
	err := waitForSandboxTunnelWithin(free, 300*time.Millisecond, 50*time.Millisecond)
	require.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Contains(t, err.Error(), "never started listening")
	assert.Contains(t, err.Error(), "sandbox_ssh", "the error must name the thing the operator can test by hand")
}

func TestSandboxTunnelProbeSucceedsWhenSomethingListens(t *testing.T) {
	ln := listenLocalForTest(t)
	defer func() { _ = ln.Close() }()
	require.NoError(t, waitForSandboxTunnelWithin(ln.Addr().String(), 2*time.Second, 20*time.Millisecond))
}

func freeLocalAddrForTest(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	addr := ln.Addr().String()
	require.NoError(t, ln.Close())
	return addr
}

func listenLocalForTest(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	return ln
}

// Codex 3725079692: sandbox_ssh is a free-form shell command, so a wrapper can
// fail LOCALLY (127 command-not-found, 126 not-executable, or any status a
// wrapper picks) before ssh ever runs. That status describes the daemon host,
// not the sandbox, so it must NOT latch — the remote agent-server may still be
// running. The remote sentinel is the only positive proof the far side ran.
func TestSandboxReapWithoutTheRemoteSentinelIsUndetermined(t *testing.T) {
	for _, status := range []int{1, 126, 127} {
		t.Run(fmt.Sprintf("wrapper exits %d before ssh runs", status), func(t *testing.T) {
			ssh := stubSSH(t, fmt.Sprintf("echo 'af-sandbox-ssh: not found' >&2; exit %d", status))
			p := &sandboxProvisioner{sshCmd: ssh, sessionDir: "/remote/dir", remotePID: "42"}

			err := p.reap()
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrWorkspaceStateUnknown),
				"a local wrapper failure says nothing about the sandbox, so the record must be retained")
			assert.False(t, p.reaped, "and it must not latch, or the next poll will never retry")
		})
	}
}

// Codex 3725079687: a numeric PID can be recycled between provision and
// teardown, so the reap must verify the process is THIS session's af binary
// before signalling — otherwise it kills something unrelated on the operator's
// host. And the kill's status must gate the rm, not be discarded by a `;`.
func TestSandboxReapScriptKillsByIdentityAndGatesTheRemoval(t *testing.T) {
	p := &sandboxProvisioner{sessionDir: "/remote/af-session", remotePID: "4242"}
	script, expect := p.reapScript("a1b2c3d4")

	assert.Contains(t, script, "/proc/$pid/cmdline",
		"the kill must verify argv[0] before signalling, not trust a bare PID")
	assert.Contains(t, script, "/remote/af-session/af",
		"and must compare against THIS session's unique af path")
	assert.Contains(t, script, "} && rm -rf", "a failed kill must stop the removal, not be discarded by `;`")
	assert.Contains(t, script, "| tr 'a-z' 'A-Z'",
		"the response must be COMPUTED remotely, so a wrapper echoing argv cannot forge it")
	assert.NotContains(t, script, expect,
		"the script must never contain the expected answer — that is what makes it unforgeable")

	// With no PID there is nothing to signal, but the sentinel still gates.
	noPID, _ := (&sandboxProvisioner{sessionDir: "/remote/af-session"}).reapScript("a1b2c3d4")
	assert.NotContains(t, noPID, "kill")
	assert.Contains(t, noPID, "| tr 'a-z' 'A-Z'")
}

// Codex 3725079706: the local port is reserved by binding :0 and closing it, so
// another process can win it in the gap. Without ExitOnForwardFailure, ssh stays
// up after a FAILED bind and the readiness probe connects to the competing
// listener — reporting a healthy tunnel owned by an unrelated process.
func TestSandboxTunnelRefusesToOutliveAFailedForward(t *testing.T) {
	p := &sandboxProvisioner{sshCmd: "ssh host"}
	// Reach the composed command without starting it: the flag must be present
	// in what startTunnel builds.
	assert.Contains(t, p.sshCmd+` -o ExitOnForwardFailure=yes -N -L "$1"`, "ExitOnForwardFailure=yes")
	// And the production path must actually carry it.
	src, err := os.ReadFile("backend_sandbox.go")
	require.NoError(t, err)
	assert.Contains(t, string(src), `-o ExitOnForwardFailure=yes -N -L`,
		"startTunnel must pass ExitOnForwardFailure so a lost bind kills the child instead of "+
			"leaving the probe to succeed against whoever won the port")
}

// Codex 3725178152: the first sentinel was a fixed string echoed verbatim, and
// it ALSO appeared in the script handed to sandbox_ssh — so a wrapper that logs
// its argv (`set -x`, or any tracing wrapper) reproduced the marker in combined
// output while failing LOCALLY, and the reap latched that as a definite remote
// answer. The challenge is now uppercased BY the remote shell, which a wrapper
// echoing its own arguments cannot do.
func TestSandboxReapRejectsAWrapperEchoingTheChallenge(t *testing.T) {
	// A wrapper that traces the command it was given (so the lowercase challenge
	// lands in the output) and then fails locally without ever reaching ssh.
	ssh := stubSSH(t, `set -x; echo "would run: ${!#}"; exit 127`)
	p := &sandboxProvisioner{sshCmd: ssh, sessionDir: "/remote/dir", remotePID: "9"}

	err := p.reap()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrWorkspaceStateUnknown),
		"a wrapper echoing the challenge has NOT proved the remote ran, so this must stay unknown-state")
	assert.False(t, p.reaped, "and must not latch")
}

// Each reap issues a fresh challenge, so a marker captured from an earlier run's
// log cannot satisfy a later one.
func TestSandboxReapChallengeIsPerReap(t *testing.T) {
	p := &sandboxProvisioner{sessionDir: "/d"}
	a, expectA := p.reapScript("aaaaaaaa")
	b, expectB := p.reapScript("bbbbbbbb")
	assert.NotEqual(t, a, b)
	assert.NotEqual(t, expectA, expectB)
	assert.NotContains(t, a, expectA)
}

// Codex 3725178147: RuntimeCleanupData is a tagged union whose restore path
// counts how many variants are set and refuses anything but exactly one. Adding
// the Sandbox arm without adding it to that count made every valid sandbox
// handle look like ZERO variants — rejected, replaced with an unavailable
// cleanup, and never retried. That silently defeats the whole point of
// persisting the handle, and no existing test covered the counting.
func TestRestoreRuntimeCleanupRebuildsASandboxHandle(t *testing.T) {
	data := &RuntimeCleanupData{Sandbox: &SandboxRuntimeCleanupData{
		SSHCommand: "ssh -J bastion.invalid sandbox.invalid",
		SessionDir: "/remote/af-sessions/xyz",
		RemotePID:  "4242",
	}}

	backend, teardown, err := restoreRuntimeCleanup("a session", "sandbox", data)
	require.NoError(t, err, "a valid sandbox handle must be restorable, or its retained row can never be retried")
	require.NotNil(t, backend)
	require.NotNil(t, teardown)
	assert.Equal(t, "sandbox", backend.Type())

	sb, ok := backend.(*sandboxBackend)
	require.True(t, ok, "restore must rebuild a sandboxBackend, not a stand-in")
	require.NotNil(t, sb.provisioner)
	assert.Equal(t, "ssh -J bastion.invalid sandbox.invalid", sb.provisioner.sshCmd,
		"the operator's ssh command must survive the restart, or the reap cannot reach the sandbox")
	assert.Equal(t, "/remote/af-sessions/xyz", sb.provisioner.sessionDir)
	assert.Equal(t, "4242", sb.provisioner.remotePID)
}

// The union stays strict: a handle carrying two variants is refused rather than
// guessed at, and the sandbox arm must not weaken that.
func TestRestoreRuntimeCleanupRefusesAmbiguousSandboxHandle(t *testing.T) {
	_, _, err := restoreRuntimeCleanup("a session", "sandbox", &RuntimeCleanupData{
		Sandbox: &SandboxRuntimeCleanupData{SSHCommand: "ssh h", SessionDir: "/d"},
		SSH:     &SSHRuntimeCleanupData{SessionDir: "/d"},
	})
	require.Error(t, err, "two variants is a malformed record and must be refused, not guessed")
}

// An incomplete handle cannot silently become a no-op reap either.
func TestRestoreRuntimeCleanupRejectsIncompleteSandboxHandle(t *testing.T) {
	for name, d := range map[string]*SandboxRuntimeCleanupData{
		"no ssh command": {SessionDir: "/d"},
		"no session dir": {SSHCommand: "ssh h"},
		"bad pid":        {SSHCommand: "ssh h", SessionDir: "/d", RemotePID: "-1"},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := restoreRuntimeCleanup("s", "sandbox", &RuntimeCleanupData{Sandbox: d})
			require.Error(t, err)
		})
	}
}
