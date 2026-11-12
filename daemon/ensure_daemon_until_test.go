package daemon

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// errWedgedSpawnMustNotRun is the launcher guard for the wedged-socket
// EnsureDaemonUntil tests: the wedged ping's deadline must short-circuit
// admission before launch, so a real daemon is never spawned under the test
// home. Reaching the launcher would mean the deadline did not bind the ensure.
var errWedgedSpawnMustNotRun = errors.New("test: EnsureDaemonUntil reached the spawn path; the wedged ping's deadline did not short-circuit admission")

// stubWedgedLauncherFail replaces launchDaemonProcessFn with the spawn guard
// above and restores it on cleanup. The wedged ping's deadline is expected to
// short-circuit EnsureDaemonUntil on admissionDeadlineExpired before launch, so
// the guard never fires in production of these tests; a slow box that races the
// deadline still cannot spawn a real daemon under the test home.
func stubWedgedLauncherFail(t *testing.T) {
	t.Helper()
	prev := launchDaemonProcessFn
	launchDaemonProcessFn = func() error { return errWedgedSpawnMustNotRun }
	t.Cleanup(func() { launchDaemonProcessFn = prev })
}

// TestEnsureDaemonUntilBoundedOnWedgedSocket is the regression for the unbounded
// ensure phase the public zero-deadline EnsureDaemon left in place. Pre-fix a
// caller that needed a deadline-bounded ensure had no public entry point:
// EnsureDaemon forwards a zero deadline, so pingDaemonUntil →
// callDaemonNoEnsureAttemptBefore skipped conn.SetDeadline (the RPC deadline is
// gated on !deadline.IsZero()), and against a wedged control socket (accepts the
// dial, never services the RPC) client.Call blocked forever — hanging the caller
// and holding ensureDaemonMu against every later daemon operation. Post-fix
// EnsureDaemonUntil forwards the deadline, SetDeadline fires at the deadline,
// and the ensure returns within the bound instead of parking forever.
//
// Pre-fix this hangs and the time.After branch fatals; post-fix <-done wins.
func TestEnsureDaemonUntilBoundedOnWedgedSocket(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	stubWedgedLauncherFail(t)
	startWedgedControlSocket(t)

	const deadline = 500 * time.Millisecond
	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- EnsureDaemonUntil(time.Now().Add(deadline)) }()
	select {
	case err := <-done:
		assert.Error(t, err, "a wedged control socket is not a serving daemon; EnsureDaemonUntil must report the failed ping")
	case <-time.After(deadline + 5*time.Second):
		t.Fatalf("EnsureDaemonUntil hung for >%s on a wedged socket; the ensure ping is unbounded (no SetDeadline on the RPC conn)", deadline+5*time.Second)
	}
	elapsed := time.Since(start)
	assert.GreaterOrEqual(t, elapsed, deadline/2,
		"the wedged ping should wait for its read deadline, not return near-instantly (elapsed=%s)", elapsed)
	assert.Less(t, elapsed, deadline+5*time.Second,
		"the wedged ping must be bounded by the caller's deadline (elapsed=%s)", elapsed)
}

// TestEnsureDaemonUntilReleasesMutexOnWedgedSocket guards the second half of the
// hang's impact: a wedged EnsureDaemonUntil must NOT keep holding ensureDaemonMu
// past its deadline, or every later daemon operation in the process queues
// behind it. Post-fix lockEnsureDaemonUntil's TryLock loop AND the ping's RPC
// read are both bounded by the same deadline, so a second ensure begun after the
// first's deadline fires acquires the mutex and returns within its own bound
// instead of parking forever behind the wedged holder.
func TestEnsureDaemonUntilReleasesMutexOnWedgedSocket(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	stubWedgedLauncherFail(t)
	startWedgedControlSocket(t)

	const deadline = 500 * time.Millisecond

	first := make(chan error, 1)
	go func() { first <- EnsureDaemonUntil(time.Now().Add(deadline)) }()
	select {
	case <-first:
	case <-time.After(deadline + 5*time.Second):
		t.Fatalf("first EnsureDaemonUntil hung for >%s on a wedged socket", deadline+5*time.Second)
	}

	// A second ensure begun after the first returned must acquire ensureDaemonMu
	// and reach its own ping without queueing behind the wedged first holder.
	second := make(chan error, 1)
	go func() { second <- EnsureDaemonUntil(time.Now().Add(deadline)) }()
	select {
	case <-second:
	case <-time.After(deadline + 5*time.Second):
		t.Fatalf("second EnsureDaemonUntil hung for >%s; the wedged first ensure held ensureDaemonMu past its deadline", deadline+5*time.Second)
	}
}
