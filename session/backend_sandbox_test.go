package session

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	// Wrapped as an explicit `sh -c '<script>'` so the remote login shell (fish,
	// csh) never parses the script itself (#5571 W2.9).
	assert.Contains(t, got, "last="+remoteShScript(`echo 'a b' && ls "/tmp/x y"`), "and must arrive byte-for-byte inside the explicit sh -c")
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
// exits 0. That holds only while nothing else catches a short copy — and the
// installRemoteBinary sha256 check now does (#5571 W2.2). Meanwhile a ZERO
// WaitDelay is not neutral:
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
			"timeout would not bound provisioning; truncation is caught by installRemoteBinary's sha256 check instead")

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

// The copy-verification half of TRAP 1 moved with the W2.2 staged-binary work:
// installRemoteBinary proves the published cache file by sha256 against the
// daemon's known hash — remote_binary_test.go pins the corrupted-copy,
// whole-copy, and cache-hit cases end to end.

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
	err := waitForSandboxTunnelWithin(string(BackendSandbox), free, 300*time.Millisecond, 50*time.Millisecond)
	require.Error(t, err)
	assert.Less(t, time.Since(start), 5*time.Second)
	assert.Contains(t, err.Error(), "never started listening")
	assert.Contains(t, err.Error(), "sandbox.ssh", "the error must name the thing the operator can test by hand")
}

func TestSandboxTunnelProbeSucceedsWhenSomethingListens(t *testing.T) {
	ln := listenLocalForTest(t)
	defer func() { _ = ln.Close() }()
	require.NoError(t, waitForSandboxTunnelWithin(string(BackendSandbox), ln.Addr().String(), 2*time.Second, 20*time.Millisecond))
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
	assert.Contains(t, script, ") && ( rm -rf",
		"a failed kill must stop the removal — and the kill runs in a SUBSHELL, so its own `exit 0` "+
			"when the process is already gone cannot terminate the script before the removal")
	assert.NotContains(t, script, "rm -rf '/remote/af-session' && printf",
		"the PROOF must not be chained to the removal: an answered rm failure has to emit the sentinel "+
			"too, or reap can never latch a definite failure and retries it forever")
	assert.Contains(t, script, "| tr 'a-z' 'A-Z'",
		"the response must be COMPUTED remotely, so a wrapper echoing argv cannot forge it")
	assert.NotContains(t, script, expect,
		"the script must never contain the expected answer — that is what makes it unforgeable")

	// With no recorded PID there is no literal PID to signal: the only kill left is
	// the identity-gated one over the HOST's launch record, read at run time.
	noPID, _ := (&sandboxProvisioner{sessionDir: "/remote/af-session"}).reapScript("a1b2c3d4")
	assert.Contains(t, noPID, `pid="$hp"`)
	assert.NotContains(t, noPID, "pid='")
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
	// And the production path must actually carry it. Since #5564 the tunnel
	// child is built by spawnTunnel in sandbox_tunnel.go (one command text for
	// the lifeline and direct spawn shapes alike), so that is the file pinned.
	src, err := os.ReadFile("sandbox_tunnel.go")
	require.NoError(t, err)
	assert.Contains(t, string(src), `-o ExitOnForwardFailure=yes -N -L`,
		"spawnTunnel must pass ExitOnForwardFailure so a lost bind kills the child instead of "+
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

// The reap script must survive the identity kill's own `exit 0`.
//
// That kill exits 0 when the process is ALREADY GONE — the common case after a
// clean shutdown — and the script used a `{ … }` group, which runs in the
// current shell. The exit therefore terminated the whole remote script before
// `rm -rf` or the confirmation ran, so the reap looked like "exited 0, never
// confirmed": the session directory survived and the record was retained
// forever. Caught by the ssh round-trip integration tests once backend=ssh
// started routing through this reap (#3052).
func TestSandboxReapCompletesWhenTheProcessIsAlreadyGone(t *testing.T) {
	// A stub that RUNS the reap script in a real shell, so the group-vs-subshell
	// semantics are exercised rather than asserted. `kill -0` on a pid that does
	// not exist makes the identity script take its `exit 0` path.
	ssh := stubSSH(t, `sh -c "${!#}"`)
	dir := t.TempDir()
	marker := filepath.Join(dir, "session-dir")
	require.NoError(t, os.MkdirAll(marker, 0o755))

	p := &sandboxProvisioner{sshCmd: ssh, sessionDir: marker, remotePID: "999999"}
	require.NoError(t, p.reap(),
		"a reap whose process is already gone must still remove the directory and confirm it ran")
	assert.True(t, p.reaped, "and it must latch, not be retried forever")

	_, statErr := os.Stat(marker)
	assert.True(t, os.IsNotExist(statErr),
		"the session directory must actually be removed — the whole point of the reap")
}

// stubSandboxReapNonce swaps the reap's nonce source for the duration of a test.
func stubSandboxReapNonce(t *testing.T, fn func() (string, error)) {
	t.Helper()
	previous := sandboxReapNonce
	sandboxReapNonce = fn
	t.Cleanup(func() { sandboxReapNonce = previous })
}

// #3449. A nonce that cannot be generated is a PRE-FLIGHT failure: no script is
// built, ssh is never invoked, and the sandbox is untouched — still running, and
// still billing. Reporting that as a plain error tells TeardownStateUnknown the
// teardown was determinate, and deleteSessionRecord then deletes the record that
// is the user's only handle on it.
//
// Same shape as ssh's ambiguous 255 above, so it must be classified the same way:
// unknown-state, and it must NOT latch, or the next poll reads a cached verdict
// instead of retrying once the entropy source recovers.
func TestSandboxReapNonceFailureIsUndetermined(t *testing.T) {
	ranSSH := filepath.Join(t.TempDir(), "ssh-ran")
	ssh := stubSSH(t, "touch "+ranSSH+"\nexit 0")
	stubSandboxReapNonce(t, func() (string, error) { return "", errors.New("entropy exhausted") })

	p := &sandboxProvisioner{sshCmd: ssh, sessionDir: "/remote/dir", remotePID: "42"}

	err := p.reap()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrWorkspaceStateUnknown),
		"a nonce failure cannot prove the sandbox was cleaned up, so the record must be retained")
	assert.False(t, p.reaped, "and it must not latch, or the next poll will never retry")
	assert.NoFileExists(t, ranSSH, "the reap failed before the script existed, so ssh cannot have run")

	// And it really re-attempts rather than returning a cached verdict.
	second := p.reap()
	assert.True(t, errors.Is(second, ErrWorkspaceStateUnknown),
		"a second reap while entropy is still exhausted must stay unknown-state")
}

// The other direction, so the fix above cannot degenerate into "a sandbox reap is
// always unknown": once the nonce source recovers, the SAME provisioner reaps for
// real and latches, and the record becomes deletable.
func TestSandboxReapRecoversAfterANonceFailure(t *testing.T) {
	ssh := stubSSH(t, `printf '%s' "${!#}" | grep -o 'af-sandbox-reaped-[0-9a-f]*' | tr 'a-z' 'A-Z'; exit 0`)
	failNonce := true
	stubSandboxReapNonce(t, func() (string, error) {
		if failNonce {
			return "", errors.New("entropy exhausted")
		}
		return "0123456789abcdef", nil
	})

	p := &sandboxProvisioner{sshCmd: ssh, sessionDir: "/remote/dir", remotePID: "42"}
	require.Error(t, p.reap())
	require.False(t, p.reaped)

	failNonce = false
	require.NoError(t, p.reap(), "the retry the non-latch preserves must be able to succeed")
	assert.True(t, p.reaped, "a confirmed reap latches, so the record may finally be retired")
}

// ErrWaitDelay fusion. exec.CommandContext can return exec.ErrWaitDelay when the
// child exited CLEANLY (exit code 0 — a non-zero exit surfaces as an
// *exec.ExitError, never as ErrWaitDelay) and only a descendant held the capture
// pipe open past cmd.WaitDelay, so the captured output is already complete.
//
// The rest of the tree treats a bare ErrWaitDelay as success (12 normalize-to-nil
// sites: backend_docker.go:161, doctor/remote.go:132, tmux/bounded.go, the git
// bounded helpers, etc.). runCommand was the sole exception: its check was
// `if ctx.Err() != nil` AFTER Output/CombinedOutput, with no ErrWaitDelay
// carve-out, so when an orphan pushed the return past the deadline it rewrote
// the bare ErrWaitDelay as context.DeadlineExceeded — flattening "the command
// completed; only pipe cleanup overran" into "the deadline killed the command."
// In reap that routed a sentinel-present reap (positive proof the far side ran)
// into the ErrWorkspaceStateUnknown branch instead of latching.
//
// The fix normalizes a bare ErrWaitDelay to nil BEFORE the deadline check,
// matching backend_docker.go and doctor/remote.go, and gates the deadline wrap
// on `err != nil` so it never fires for a completed command. These tests pin
// both the fix and the genuine-timeout path it must not touch.

// sandboxOrphanGuard bounds the real-exec tests below. A completed command with
// an orphaned pipe-holder returns at ~sandboxTunnelWaitDelay (2s); a missing
// WaitDelay regression would block on the straggler's full sleep (30s) and trip
// the guard. Ample slack over 2s keeps a loaded box from flaking the fixed path.
const sandboxOrphanGuard = 8 * time.Second

// stubSSHStraggler writes an executable `ssh` stub that runs printCmd (writing
// the wanted output to stdout), then backgrounds a `sleep` that inherits the
// capture pipe (the orphaned descriptor-holder of #1967), records its pid, and
// exits 0 — so CombinedOutput() returns exec.ErrWaitDelay after
// sandboxTunnelWaitDelay while the buffer already holds printCmd's output. The
// straggler is killed on cleanup so a regression cannot leak it past the test.
func stubSSHStraggler(t *testing.T, printCmd string) string {
	t.Helper()
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "straggler.pid")
	body := "#!/usr/bin/env bash\n" +
		printCmd + "\n" +
		"sleep 30 &\n" +
		"echo $! > " + shellQuote(pidFile) + "\n" +
		"exit 0\n"
	path := filepath.Join(dir, "ssh")
	require.NoError(t, os.WriteFile(path, []byte(body), 0o755))
	t.Cleanup(func() {
		data, rerr := os.ReadFile(pidFile)
		if rerr != nil {
			return
		}
		pid, perr := strconv.Atoi(strings.TrimSpace(string(data)))
		if perr != nil || pid <= 0 {
			return
		}
		if proc, ferr := os.FindProcess(pid); ferr == nil {
			_ = proc.Kill()
		}
	})
	return path
}

// TestSandboxRunNormalizesErrWaitDelayToNilWhenOrphanHoldsPipe drives the REAL
// runCommand (real buildRunCommand + CombinedOutput) against a stub ssh that
// prints a marker, orphans a sleep holding the capture pipe, and exits 0. With a
// 1s timeout and a 2s WaitDelay the orphaned pipe-holder pushes the return past
// the deadline, so os/exec returns exec.ErrWaitDelay with ctx.Err() = Deadline-
// Exceeded simultaneously. A bare ErrWaitDelay means the child exited 0 and the
// output is complete, so runCommand must normalize it to nil — NOT rewrite it as
// a timeout. Before the fix this returned "...timed out...: context deadline
// exceeded"; after, it returns the complete output and no error.
func TestSandboxRunNormalizesErrWaitDelayToNilWhenOrphanHoldsPipe(t *testing.T) {
	ssh := stubSSHStraggler(t, "printf 'AF-SANDBOX-ANSWER\\n'")
	p := &sandboxProvisioner{sshCmd: ssh}

	var (
		out   []byte
		err   error
		start = time.Now()
	)
	require.True(t, returnsWithinExec(sandboxOrphanGuard, func() {
		out, err = p.Run(1*time.Second, "true", nil, true)
	}), "runCommand must return within the guard — a missing WaitDelay would block on the straggler (#1967)")

	elapsed := time.Since(start)
	assert.NoError(t, err,
		"a bare exec.ErrWaitDelay (child exited 0, output complete) must NOT be wrapped as a timeout")
	assert.False(t, errors.Is(err, context.DeadlineExceeded),
		"a completed command must not be reported as a deadline kill")
	assert.Contains(t, string(out), "AF-SANDBOX-ANSWER",
		"the orphaned pipe-holder must not lose already-written output")
	// ~sandboxTunnelWaitDelay, not the command's runtime: the stub exits ~instantly,
	// so the elapsed time is the WaitDelay overrun, proving the err came from the
	// orphaned-pipe path rather than a slow command (and self-checking that the
	// straggler did hold the pipe, else elapsed would be ~0).
	assert.GreaterOrEqual(t, elapsed, sandboxTunnelWaitDelay-500*time.Millisecond,
		"the return must wait at least sandboxTunnelWaitDelay for the orphan's pipe")
	assert.Less(t, elapsed, sandboxOrphanGuard, "and must not block on the straggler's full sleep")
}

// TestSandboxRunGenuineTimeoutWrapsAsDeadlineExceeded pins the path the fix must
// NOT change: a command still running at the deadline is killed (group SIGKILL),
// producing an *exec.ExitError — never a bare exec.ErrWaitDelay — so the
// normalize-to-nil does not fire and runCommand still wraps it as a timeout.
// Non-reap callers (installRemoteBinary, etc.) that do not branch on error identity
// continue to see a correct timeout for a genuine deadline kill.
func TestSandboxRunGenuineTimeoutWrapsAsDeadlineExceeded(t *testing.T) {
	ssh := stubSSH(t, "sleep 10")
	p := &sandboxProvisioner{sshCmd: ssh}

	var err error
	start := time.Now()
	require.True(t, returnsWithinExec(sandboxOrphanGuard, func() {
		_, err = p.Run(1*time.Second, "true", nil, true)
	}), "a genuinely timed-out command must be killed at the deadline, not block forever")
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded),
		"a genuine deadline kill must still surface as context.DeadlineExceeded")
	assert.Contains(t, err.Error(), "timed out")
	assert.GreaterOrEqual(t, elapsed, 1*time.Second-300*time.Millisecond,
		"the process must be killed near the 1s deadline")
	assert.Less(t, elapsed, sandboxOrphanGuard, "and must not wait for the 10s sleep to end naturally")
}

// TestSandboxReapSentinelRoutingDependsOnRunCommandErrorIdentity pins that reap's
// switch is the site that decides a sentinel-present reap, so a regression that
// re-introduces the wrap is caught at the decision boundary rather than only
// end-to-end. Through the runCommandFn seam three error identities meet the same
// answered=true reap:
//
//   - context.DeadlineExceeded → the first case → ErrWorkspaceStateUnknown, NOT
//     latched (the bug shape, retained so a genuine timeout reap stays unknown).
//   - nil (the fix: runCommand normalized a bare ErrWaitDelay) → the
//     `err == nil && answered` early return → latched.
//   - a raw exec.ErrWaitDelay (the narrower carve-out) → falls to `default` →
//     latched; the sentinel is positive proof the far side ran.
func TestSandboxReapSentinelRoutingDependsOnRunCommandErrorIdentity(t *testing.T) {
	_, expect := sandboxReapChallenge("0123456789abcdef")
	stubSandboxReapNonce(t, func() (string, error) { return "0123456789abcdef", nil })

	cases := []struct {
		name        string
		err         error
		wantLatched bool
		wantUnknown bool
	}{
		{
			name:        "DeadlineExceeded misroutes a sentinel-present reap to unknown",
			err:         context.DeadlineExceeded,
			wantLatched: false,
			wantUnknown: true,
		},
		{
			name:        "nil latches via the answered early return",
			err:         nil,
			wantLatched: true,
			wantUnknown: false,
		},
		{
			name:        "raw ErrWaitDelay latches via the default branch",
			err:         exec.ErrWaitDelay,
			wantLatched: true,
			wantUnknown: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &sandboxProvisioner{sshCmd: "ssh host", sessionDir: "/remote/dir", remotePID: "1"}
			p.runCommandFn = func(time.Duration, string, io.Reader, bool) ([]byte, error) {
				return []byte(expect), tc.err
			}
			err := p.reap()
			if tc.wantUnknown {
				require.Error(t, err)
				assert.True(t, errors.Is(err, ErrWorkspaceStateUnknown),
					"a deadline-expired reap must retain the record as unknown-state")
			} else {
				assert.False(t, errors.Is(err, ErrWorkspaceStateUnknown),
					"a sentinel-present reap that completed must not be reported unknown")
			}
			assert.Equal(t, tc.wantLatched, p.reaped)
		})
	}
}

// TestSandboxReapLatchesRealErrWaitDelayOrphanEndToEnd closes the composition gap:
// runCommandFn is wired to the REAL runCommand (real buildRunCommand +
// CombinedOutput) with a 1s timeout, so sandboxTunnelWaitDelay (2s) overruns the
// deadline within test time. The stub ssh prints the uppercase sentinel (as only
// real remote execution can — it transforms the lowercase challenge via tr),
// orphans a sleep holding the capture pipe, and exits 0. After the fix runCommand
// normalizes the resulting exec.ErrWaitDelay to nil, so reap's
// `err == nil && answered` early return latches and retires the record — instead
// of misrouting a completed reap into ErrWorkspaceStateUnknown (the bug, end to
// end through the real transport).
func TestSandboxReapLatchesRealErrWaitDelayOrphanEndToEnd(t *testing.T) {
	ssh := stubSSHStraggler(t, `printf '%s' "${!#}" | grep -o 'af-sandbox-reaped-[0-9a-f]*' | tr 'a-z' 'A-Z'`)
	p := &sandboxProvisioner{sshCmd: ssh, sessionDir: "/remote/dir", remotePID: "77"}
	p.runCommandFn = func(_ time.Duration, script string, stdin io.Reader, combined bool) ([]byte, error) {
		return p.runCommand(1*time.Second, script, stdin, combined)
	}

	var err error
	require.True(t, returnsWithinExec(sandboxOrphanGuard, func() {
		err = p.reap()
	}), "reap must return within the guard — a missing WaitDelay would block on the straggler")

	assert.False(t, errors.Is(err, ErrWorkspaceStateUnknown),
		"a sentinel-present reap that completed with an orphaned pipe-holder must latch, not misroute to unknown-state")
	assert.True(t, p.reaped, "the completed reap must latch so the record may be retired")
}

// A captured-but-non-numeric remote PID must NOT be conflated with "nothing to
// signal". The free-form `sandbox.ssh` transport exists to run operator
// wrappers, and a wrapper that emits stdout noise while the nohup'd `af
// agent-server` did start makes the launch script's `echo $!` return a polluted
// string; provisioning fails on the validity check, but the server may still be
// running detached on the remote. The reap must refuse with
// ErrWorkspaceStateUnknown and NOT latch, NOT remove the session dir, NOT
// invoke the transport: the record is the operator's only handle on the
// orphaned server and the next poll has to retry rather than read a cached
// verdict. This is the guard 42aa81f3 added in backend_ssh_reap.go, the
// convergence deleted at 3820431a, and the refactor's success-only PID
// mirroring at 6d83aaf3 made unreachable on exactly this path. Both ends are
// restored: provision mirrors w.RemotePID before checking the validity error
// (see TestSandboxProvisionFailureMirrorsTheRawPIDForTheReap), and reap
// refuses here.
func TestSandboxReapRefusesAnUnverifiableRemotePID(t *testing.T) {
	// A stub ssh that RUNS the reap script in a real shell, so a missing guard
	// would actually `rm -rf` the session dir and answer the challenge (latching
	// a clean reap of a possibly-running server).
	ssh := stubSSH(t, `sh -c "${!#}"`)
	dir := t.TempDir()
	marker := filepath.Join(dir, "session-dir")
	require.NoError(t, os.MkdirAll(marker, 0o755))
	sentinel := filepath.Join(marker, "still-here")
	require.NoError(t, os.WriteFile(sentinel, []byte("present"), 0o644))

	p := &sandboxProvisioner{sshCmd: ssh, sessionDir: marker, remotePID: "af-wrapper noise 4242"}

	err := p.reap()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrWorkspaceStateUnknown),
		"an unverifiable PID means the agent-server may still be running; the record must be retained as unknown-state")
	assert.False(t, p.reaped,
		"the reap must NOT latch — the next poll must retry, not return a cached verdict")
	assert.Contains(t, err.Error(), "refusing to reap",
		"the error must say it refused, not that the reap completed")
	assert.Contains(t, err.Error(), "af-wrapper noise 4242",
		"the error must name the invalid PID so the operator can diagnose it")
	assert.Contains(t, err.Error(), marker,
		"the error must name the session dir so the operator can inspect it by hand")

	_, statErr := os.Stat(sentinel)
	assert.NoError(t, statErr,
		"the session dir must NOT be removed while the agent-server may still be running")
}

// The refusal must not latch, so the next poll re-runs the reap. A latched
// refusal would silently drop the record on the next attempt — exactly the
// retain-forever-until-the-operator-clears-it state the pre-refactor
// guard deliberately left the row in.
func TestSandboxReapRefusesAnUnverifiableRemotePIDAndRetries(t *testing.T) {
	ssh := stubSSH(t, `sh -c "${!#}"`)
	dir := t.TempDir()
	marker := filepath.Join(dir, "session-dir")
	require.NoError(t, os.MkdirAll(marker, 0o755))

	p := &sandboxProvisioner{sshCmd: ssh, sessionDir: marker, remotePID: "af-wrapper noise 4242"}

	first := p.reap()
	require.Error(t, first)
	require.True(t, errors.Is(first, ErrWorkspaceStateUnknown))
	require.False(t, p.reaped, "the first refusal must not latch")

	second := p.reap()
	require.Error(t, second)
	assert.True(t, errors.Is(second, ErrWorkspaceStateUnknown),
		"a second reap with the same invalid PID must refuse again, not read a cached verdict")
	assert.False(t, p.reaped, "and still must not latch")

	_, statErr := os.Stat(marker)
	assert.False(t, os.IsNotExist(statErr),
		"the session dir must survive a repeated refusal")
}

// The legitimate empty-PID path ("nothing ever started") is unchanged: an
// empty `remotePID` continues to mean there is nothing to signal, the reap
// removes the partial session dir, and a clean challenge-response latches.
// This is the pre-`startAgentServer` failure path (e.g. a clone or
// binary-copy failure), where no nohup'd server exists, and the refuse guard
// must not regress it.
func TestSandboxReapWithEmptyRemotePIDStillReapsPartialSession(t *testing.T) {
	ssh := stubSSH(t, `sh -c "${!#}"`)
	dir := t.TempDir()
	marker := filepath.Join(dir, "session-dir")
	require.NoError(t, os.MkdirAll(marker, 0o755))

	p := &sandboxProvisioner{sshCmd: ssh, sessionDir: marker, remotePID: ""}

	require.NoError(t, p.reap(),
		"a pre-launch failure with no captured PID removes the partial session dir and latches")
	assert.True(t, p.reaped, "an answered clean reap latches so retries do not re-run it")

	_, statErr := os.Stat(marker)
	assert.True(t, os.IsNotExist(statErr),
		"the session dir was only ever a partial workspace and must be removed")
}

// End-to-end through provision() + reapProvisionFailure: a polluted `echo $!`
// output (wrapper stdout noise while the nohup'd server did start) fails
// provisioning with "invalid background PID"; the mirroring carries the raw
// PID onto the provisioner, so the failure reap refuses rather than
// `rm -rf`-ing the session dir of a possibly-running server. The surfaced
// error joins both diagnostics and wraps ErrWorkspaceStateUnknown so the
// daemon retains the record for retry — instead of latching a clean reap and
// retiring the row over an orphan.
func TestSandboxProvisionFailureMirrorsTheRawPIDForTheReap(t *testing.T) {
	localBin := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(localBin, []byte("0123456789"), 0o755))

	var reapTransportCalls []string
	stageBinaryReply := binaryStageReplies(t, localBin)
	p := &sandboxProvisioner{
		spec:    ProvisionSpec{Title: "test-session"},
		afBin:   localBin,
		program: "",
	}
	p.runCommandFn = func(_ time.Duration, script string, _ io.Reader, _ bool) ([]byte, error) {
		if out, ok := stageBinaryReply(script); ok {
			return out, nil
		}
		switch {
		case strings.Contains(script, "mktemp -d"):
			return []byte("/remote/af-sessions/test.XXXXXX\n"), nil
		case strings.Contains(script, "git config --global"):
			return nil, nil
		case strings.Contains(script, "git clone"):
			return nil, nil
		case strings.Contains(script, "cat >") && strings.Contains(script, "chmod +x"):
			return nil, nil
		case strings.Contains(script, "wc -c"):
			return []byte("10\n"), nil
		case strings.Contains(script, "nohup"):
			return []byte("af-wrapper noise 4242\n"), nil
		case strings.Contains(script, "rm -rf") ||
			strings.Contains(script, "af-sandbox-reaped-") ||
			strings.Contains(script, "kill -0"):
			reapTransportCalls = append(reapTransportCalls, script)
			return nil, nil
		default:
			return nil, nil
		}
	}

	_, err := p.provision()
	require.Error(t, err, "an invalid captured PID must fail provisioning")
	assert.Contains(t, err.Error(), "invalid background PID",
		"the provision error must name what failed")
	assert.Contains(t, err.Error(), "af-wrapper noise 4242")

	// The mirroring fix: even on the failure path, the raw PID is on the
	// provisioner for the reaper to see — "" would mean "nothing started" and
	// the rm would proceed; the garbage PID is what makes the refuse guard
	// reachable. The success-only mirroring at 6d83aaf3 regressed this to "".
	assert.Equal(t, "af-wrapper noise 4242", p.remotePID,
		"the raw captured PID must be mirrored onto the provisioner even on the invalid-PID failure path")
	assert.Equal(t, "/remote/af-sessions/test.XXXXXX", p.sessionDir)

	joinedErr := p.reapProvisionFailure(err)
	require.Error(t, joinedErr)
	assert.True(t, errors.Is(joinedErr, ErrWorkspaceStateUnknown),
		"the joined error must preserve the reap refusal's unknown-state classification, so the daemon retains the record")
	assert.False(t, p.reaped,
		"the reap must not latch — the record must be retried, not retired")
	assert.Contains(t, joinedErr.Error(), "provisioning failed and cleanup of its partial workspace",
		"the surfaced error must say cleanup did not complete, so the operator inspects before retrying")
	assert.Contains(t, joinedErr.Error(), "refusing to reap",
		"the surfaced error must say the reap refused, so the operator knows the server may still be running")
	assert.Contains(t, joinedErr.Error(), "af-wrapper noise 4242",
		"the surfaced error must name the invalid PID for diagnosis")

	assert.Empty(t, reapTransportCalls,
		"the refuse guard must short-circuit before the reap invokes the transport")
}

// A launch that suppressed `echo $!` (a wrapper swallowing stdout while the
// nohup'd `af agent-server` did start) leaves RemotePID empty, but the nohup
// still ran — so this is NOT the pre-launch empty-PID path that may `rm -rf`
// the partial workspace. The refuse guard must fail closed: an empty PID after
// a launch attempts the reap with launchAttempted set, so the reap refuses,
// does not latch, does not remove the session dir, and never invokes the
// transport — the record is the operator's only handle on the orphan.
func TestSandboxReapRefusesAnEmptyRemotePIDAfterLaunch(t *testing.T) {
	ssh := stubSSH(t, `sh -c "${!#}"`)
	dir := t.TempDir()
	marker := filepath.Join(dir, "session-dir")
	require.NoError(t, os.MkdirAll(marker, 0o755))
	sentinel := filepath.Join(marker, "still-here")
	require.NoError(t, os.WriteFile(sentinel, []byte("present"), 0o644))

	p := &sandboxProvisioner{sshCmd: ssh, sessionDir: marker, launchAttempted: true}

	err := p.reap()
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrWorkspaceStateUnknown),
		"an empty PID after a launch means the agent-server may still be running; the record must be retained as unknown-state")
	assert.False(t, p.reaped,
		"the reap must NOT latch — the next poll must retry, not return a cached verdict")
	assert.Contains(t, err.Error(), "refusing to reap",
		"the error must say it refused, not that the reap completed")
	assert.Contains(t, err.Error(), "no remote PID",
		"the error must name the empty PID so the operator can diagnose it")
	assert.Contains(t, err.Error(), marker,
		"the error must name the session dir so the operator can inspect it by hand")

	_, statErr := os.Stat(sentinel)
	assert.NoError(t, statErr,
		"the session dir must NOT be removed while the agent-server may still be running")
}

// End-to-end through provision() + reapProvisionFailure: when the launch
// transport exits cleanly but returns no PID (a wrapper that swallowed `echo
// $!`), startAgentServer fails with "invalid background PID" and an empty
// RemotePID. The launchAttempted flag is set before the call, so the failure
// reap refuses rather than `rm -rf`-ing the session dir of a possibly-running
// server. The surfaced error joins the provision error with the reap refusal
// and wraps ErrWorkspaceStateUnknown so the daemon retains the record.
func TestSandboxProvisionFailureRefusesAnEmptyRemotePIDAfterLaunch(t *testing.T) {
	localBin := filepath.Join(t.TempDir(), "af")
	require.NoError(t, os.WriteFile(localBin, []byte("0123456789"), 0o755))

	var reapTransportCalls []string
	stageBinaryReply := binaryStageReplies(t, localBin)
	p := &sandboxProvisioner{
		spec:    ProvisionSpec{Title: "test-session"},
		afBin:   localBin,
		program: "",
	}
	p.runCommandFn = func(_ time.Duration, script string, _ io.Reader, _ bool) ([]byte, error) {
		if out, ok := stageBinaryReply(script); ok {
			return out, nil
		}
		switch {
		case strings.Contains(script, "mktemp -d"):
			return []byte("/remote/af-sessions/test.XXXXXX\n"), nil
		case strings.Contains(script, "git config --global"):
			return nil, nil
		case strings.Contains(script, "git clone"):
			return nil, nil
		case strings.Contains(script, "cat >") && strings.Contains(script, "chmod +x"):
			return nil, nil
		case strings.Contains(script, "wc -c"):
			return []byte("10\n"), nil
		case strings.Contains(script, "nohup"):
			// The transport exits cleanly but suppresses `echo $!`: the nohup
			// ran, the agent-server may be detached, and the captured PID is
			// empty rather than the non-numeric noise the garbage-PID test uses.
			return []byte("\n"), nil
		case strings.Contains(script, "rm -rf") ||
			strings.Contains(script, "af-sandbox-reaped-") ||
			strings.Contains(script, "kill -0"):
			reapTransportCalls = append(reapTransportCalls, script)
			return nil, nil
		default:
			return nil, nil
		}
	}

	_, err := p.provision()
	require.Error(t, err, "an empty captured PID must fail provisioning")
	assert.Contains(t, err.Error(), "invalid background PID",
		"the provision error must name what failed")
	assert.Contains(t, err.Error(), `""`,
		"the provision error must name the empty PID that failed the validity check")

	assert.True(t, p.launchAttempted,
		"the launch was attempted, so the reap must treat an empty PID as a possibly-running server")
	assert.Equal(t, "", p.remotePID,
		"the empty captured PID is mirrored onto the provisioner unchanged")
	assert.Equal(t, "/remote/af-sessions/test.XXXXXX", p.sessionDir)

	joinedErr := p.reapProvisionFailure(err)
	require.Error(t, joinedErr)
	assert.True(t, errors.Is(joinedErr, ErrWorkspaceStateUnknown),
		"the joined error must preserve the reap refusal's unknown-state classification, so the daemon retains the record")
	assert.False(t, p.reaped,
		"the reap must not latch — the record must be retried, not retired")
	assert.Contains(t, joinedErr.Error(), "provisioning failed and cleanup of its partial workspace",
		"the surfaced error must say cleanup did not complete, so the operator inspects before retrying")
	assert.Contains(t, joinedErr.Error(), "refusing to reap",
		"the surfaced error must say the reap refused, so the operator knows the server may still be running")
	assert.Contains(t, joinedErr.Error(), "no remote PID",
		"the surfaced error must name the empty PID for diagnosis")

	assert.Empty(t, reapTransportCalls,
		"the refuse guard must short-circuit before the reap invokes the transport")
}
