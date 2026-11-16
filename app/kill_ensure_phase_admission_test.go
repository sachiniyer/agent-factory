package app

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// productionWithDaemonHTTPMutation / productionDaemonHTTPClient capture the
// production ensure-chain seams at package load, before any test stubs them.
// The kill-path regression below runs the REAL ensure chain —
// withDaemonHTTPMutation → callDaemonHTTP → daemonHTTPClient →
// daemon.EnsureDaemonUntil — exactly the path the bug lives in. Tests that
// stub withDaemonHTTPMutation bypass daemonHTTPClient/EnsureDaemon, so they
// never exercise the ensure-phase error the rewrap misclassifies.
var (
	productionWithDaemonHTTPMutation = withDaemonHTTPMutation
	productionDaemonHTTPClient       = daemonHTTPClient
)

// admissionDeadlineErr is the ensure-phase admission deadline error shape that
// daemonHTTPClient's EnsureDaemonUntil call returns (daemonAdmissionDeadlineError
// is unexported in the daemon package, so this rebuilds the same chain — see
// daemon.ErrAdmissionDeadline): it wraps BOTH the exported sentinel and
// context.DeadlineExceeded, which is the crux of the bug — the DeadlineExceeded
// root is what the kill rewrap and apiclient.IsMutationOutcomeUncertain match.
func admissionDeadlineErr() error {
	return fmt.Errorf("%w: %w", daemon.ErrAdmissionDeadline, context.DeadlineExceeded)
}

// admissionDeadlineKillResult is the error the fixed killSessionThroughDaemon
// returns when the ensure phase times out, built the same way so a change to
// the wrapping is caught here too. It mirrors wedgedKillResult (the genuine
// post-send timeout shape) for the ensure-phase arm.
func admissionDeadlineKillResult() error {
	return fmt.Errorf("the daemon was not reachable for the kill request — no teardown was started: %w", admissionDeadlineErr())
}

// TestKillSessionThroughDaemon_EnsurePhaseTimeout_NoFalseTeardown is the
// end-to-end regression for the TUI kill misreporting a 5s ensure-phase admission
// timeout as a 60s in-flight kill. It drives the REAL killSessionThroughDaemon
// against a wedged control socket with the production ensure chain
// (withDaemonHTTPMutation + daemonHTTPClient + EnsureDaemonUntil) fully in
// play — no stubs on the ensure path.
//
// Pre-fix, EnsureDaemonUntil times out against the wedged socket and returns
// daemonAdmissionDeadlineError (wrapping context.DeadlineExceeded) BEFORE
// KillSession is ever sent; killSessionThroughDaemon's
// errors.Is(err, context.DeadlineExceeded) matches it and rewraps as
// errDaemonUnresponsive within 60s — fabricating a teardown that "may still
// finish in the background" for a request that was never dispatched. The rewrap
// also drops the only proof the request was never sent (the admission-deadline
// text) from the error chain.
//
// Post-fix, the ensure-phase error is tagged with daemon.ErrAdmissionDeadline
// and recognized BEFORE the DeadlineExceeded check, so the kill returns an
// honest "no teardown was started" message, does not wrap errDaemonUnresponsive,
// does not report the 60s bound, and is not classified as an uncertain mutation
// outcome. A detector listener on daemon-http.sock (the socket KillSession
// dials after the ensure succeeds) records zero connections, proving the
// request was never dispatched.
func TestKillSessionThroughDaemon_EnsurePhaseTimeout_NoFalseTeardown(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	// Wedge the control socket the ensure phase pings: accepts the dial, never
	// services the RPC, so EnsureDaemonUntil's ping parks on an unanswered read
	// until its admission deadline fires.
	startWedgedControlSocketInHome(t)

	// Reinstall the production ensure chain. Tests that stub
	// withDaemonHTTPMutation bypass daemonHTTPClient/EnsureDaemon — exactly
	// the ensure step the bug lives in.
	origWith := withDaemonHTTPMutation
	origClient := daemonHTTPClient
	t.Cleanup(func() { withDaemonHTTPMutation = origWith; daemonHTTPClient = origClient })
	withDaemonHTTPMutation = productionWithDaemonHTTPMutation
	daemonHTTPClient = productionDaemonHTTPClient

	// Bind a counting listener at the HTTP socket KillSession dials (via
	// apiclient.NewTargeted) AFTER the ensure succeeds. The ensure fails first,
	// so NewTargeted is never called and this detector records zero — proving
	// no KillSession request was ever dispatched, so no background teardown can
	// exist. A wedged listener (accept + close) is enough: a connection is the
	// signal the request was sent.
	httpSocket, err := daemon.DaemonHTTPSocketPath()
	require.NoError(t, err)
	var httpConns atomic.Int32
	httpLn, err := net.Listen("unix", httpSocket)
	require.NoError(t, err)
	t.Cleanup(func() { _ = httpLn.Close() })
	go func() {
		for {
			c, acceptErr := httpLn.Accept()
			if acceptErr != nil {
				return
			}
			httpConns.Add(1)
			_ = c.Close()
		}
	}()

	done := make(chan error, 1)
	go func() {
		done <- killSessionThroughDaemon(daemon.KillSessionRequest{ID: "alpha-id", Title: "alpha", RepoID: "repo-1"})
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("want an error when the ensure phase times out, got nil — the TUI would report a successful kill that never happened")
		}
		// The kill must NOT claim the daemon took a request and went quiet: that
		// is the post-send-timeout premise the errDaemonUnresponsive rewrap and
		// the in-TUI restart offer are built on, and it is false here.
		if errors.Is(err, errDaemonUnresponsive) {
			t.Fatalf("ensure-phase timeout must not wrap errDaemonUnresponsive (KillSession was never sent): %q", err.Error())
		}
		// No false in-flight teardown and no fabricated 60s bound.
		if strings.Contains(err.Error(), "may still finish") {
			t.Fatalf("ensure-phase timeout must not claim a teardown may still finish (no request was sent): %q", err.Error())
		}
		if strings.Contains(err.Error(), "1m0s") {
			t.Fatalf("ensure-phase timeout must not report the 60s kill bound that was never in force: %q", err.Error())
		}
		// Must be recognized as the admission deadline and say so honestly.
		if !errors.Is(err, daemon.ErrAdmissionDeadline) {
			t.Fatalf("ensure-phase timeout must wrap daemon.ErrAdmissionDeadline so callers can tell a never-sent request from a post-send timeout: %q", err.Error())
		}
		if !strings.Contains(err.Error(), "no teardown was started") {
			t.Fatalf("error must say no teardown was started for a never-sent kill: %q", err.Error())
		}
		// A never-sent request is definitive, not an uncertain outcome — the
		// daemon provably did not act, so the handler must not tell the user the
		// kill "could not be confirmed" or that the daemon "may have done it".
		if mutationOutcomeUnknown(err) {
			t.Fatalf("ensure-phase timeout must not read as an uncertain mutation outcome (no request was sent, the daemon did not act): %q", err.Error())
		}
		// The admission-deadline text — the only proof the request was never
		// sent — must survive in the chain, not be laundered out by the rewrap.
		if !strings.Contains(err.Error(), "daemon admission retry deadline elapsed") {
			t.Fatalf("the admission-deadline text must survive in the error chain: %q", err.Error())
		}
		// Zero HTTP-socket connections: KillSession was provably never dispatched.
		assert.Equal(t, int32(0), httpConns.Load(),
			"KillSession must never be dispatched when the ensure phase times out — no background teardown can exist")
	case <-time.After(20 * time.Second):
		t.Fatal("HUNG: killSessionThroughDaemon never returned against a wedged control socket — the ensure phase's bound did not fire")
	}
}

// TestKillEnsurePhase_LocalTarget_NoRestartOffer is the LOCAL handler arm of the
// fix. A kill whose ensure phase timed out must NOT offer the in-TUI daemon
// restart: the offer is premised on a post-send timeout ("the daemon took the
// request and went quiet"), which is false when no request was ever sent. It
// routes to the honest "Cannot kill session / the session is retained" recovery
// instead — the session IS retained because the kill was never dispatched.
func TestKillEnsurePhase_LocalTarget_NoRestartOffer(t *testing.T) {
	setRemoteTargetForTest(t, false)
	setRestartActionForTest(t, func() error {
		t.Fatal("an ensure-phase timeout must never run the local daemon restart — the request was never sent")
		return nil
	})

	h := newTestHome(t)
	inst := newKillableInstance(t, "ensure-timeout")
	require.NoError(t, inst.Transition(session.BeginKill()))
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	target := captureSessionActionTarget(inst, h.repoID)

	model, _ := h.Update(instanceKilledMsg{target: target, err: admissionDeadlineKillResult()})
	hm := model.(*home)

	assert.NotEqual(t, stateConfirm, hm.state, "an ensure-phase timeout must not open the restart confirm")
	assert.Nil(t, hm.confirmationOverlay, "no restart confirm for a never-sent kill")
	require.NotNil(t, hm.recovery, "a never-sent kill must route to the 'session is retained' recovery, not the restart offer")
	assert.Contains(t, hm.recovery.condition, "Cannot kill session", "the recovery names a kill failure, not an unconfirmed outcome")
	assert.Contains(t, hm.recovery.detail, "retained", "the session is retained because the kill was never sent")
	assert.Contains(t, hm.recovery.detail, "no teardown was started", "the recovery must say no teardown was started")
	assert.NotContains(t, hm.errBox.FullError(), "could not be confirmed", "an ensure-phase timeout is definitive, not an uncertain outcome")
	assert.NotContains(t, hm.errBox.FullError(), "may still finish", "no false in-flight teardown for a never-sent kill")
	assert.NotContains(t, hm.errBox.FullError(), "Restart it?", "no restart offer text")
	assert.NotEqual(t, session.OpKilling, inst.GetInFlightOp(), "the optimistic kill fence reverts so the row cannot strand and can be retried")
}

// TestKillEnsurePhase_RemoteTarget_RoutesToRetainedNotUncertain is the REMOTE
// handler arm of the fix. A remote kill whose ensure phase timed out must NOT
// report "could not be confirmed — the daemon may have done it": the daemon did
// nothing, because no request was ever sent (EnsureDaemonUntil failed before
// NewTargeted was ever called). It routes to the same honest "session is
// retained" recovery as the local arm, minus the (local-only) restart offer.
func TestKillEnsurePhase_RemoteTarget_RoutesToRetainedNotUncertain(t *testing.T) {
	setRemoteTargetForTest(t, true)
	setRestartActionForTest(t, func() error {
		t.Fatal("a remote target must never run the local daemon restart")
		return nil
	})

	h := newTestHome(t)
	inst := newKillableInstance(t, "ensure-remote")
	require.NoError(t, inst.Transition(session.BeginKill()))
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	target := captureSessionActionTarget(inst, h.repoID)

	model, _ := h.Update(instanceKilledMsg{target: target, err: admissionDeadlineKillResult()})
	hm := model.(*home)

	assert.NotEqual(t, stateConfirm, hm.state, "no restart confirm for a remote ensure-phase timeout")
	assert.Nil(t, hm.confirmationOverlay, "no restart confirm for a remote never-sent kill")
	require.NotNil(t, hm.recovery, "a never-sent kill routes to the 'session is retained' recovery, not the uncertain-outcome notice")
	assert.Contains(t, hm.recovery.detail, "retained", "the session is retained because the kill was never sent")
	assert.NotContains(t, hm.errBox.FullError(), "could not be confirmed", "an ensure-phase timeout is definitive — the daemon did not 'may have done it'")
	assert.NotContains(t, hm.errBox.FullError(), "may still finish", "no false in-flight teardown")
	assert.NotEqual(t, session.OpKilling, inst.GetInFlightOp(), "the optimistic kill fence reverts so the row cannot strand")
}

// TestMutationMayHaveLanded_ExcludesEnsurePhaseAdmissionDeadline pins the
// classifier fix that backs the archive/restore (and remote-kill) arms. An
// ensure-phase admission deadline is NOT an uncertain mutation outcome: the
// request was never sent, so the daemon provably did not act. A bare
// context.DeadlineExceeded (the caller's own post-send timeout) STAYS uncertain,
// preserving the existing #4820 behavior for a request that may be on the wire —
// the classifier distinguishes the two by the sentinel, not by the deadline.
func TestMutationMayHaveLanded_ExcludesEnsurePhaseAdmissionDeadline(t *testing.T) {
	admission := admissionDeadlineErr()
	require.False(t, mutationMayHaveLanded(admission),
		"an ensure-phase admission deadline is not uncertain: the request was never sent")
	require.False(t, mutationOutcomeUnknown(admission),
		"an ensure-phase admission deadline is not an unknown outcome: the daemon did not act")
	// A bare post-send deadline stays uncertain — the request may be in flight.
	require.True(t, mutationMayHaveLanded(context.DeadlineExceeded),
		"a bare context.DeadlineExceeded stays uncertain (the caller gave up on a request that may be on the wire)")
	// The reference shapes from mutation_outcome_test.go are unchanged.
	require.False(t, mutationMayHaveLanded(neverSent()),
		"a never-sent transport failure stays definitive")
	require.True(t, mutationMayHaveLanded(replyLost()),
		"a reply-lost transport failure stays uncertain")
}

// TestArchiveEnsurePhaseTimeout_NotUncertain is the archive arm of the fix.
// archiveSessionThroughDaemon does not rewrap, so the raw ensure-phase error
// (wrapping context.DeadlineExceeded) used to flow into handleInstanceArchived's
// mutationOutcomeUnknown check and report "could not be confirmed — the daemon
// may have done it" for a request that was never sent. Post-fix the
// mutationMayHaveLanded exclusion recognizes daemon.ErrAdmissionDeadline
// upstream of the DeadlineExceeded check, so the archive routes to the honest
// "Cannot archive session / the session is retained" recovery.
func TestArchiveEnsurePhaseTimeout_NotUncertain(t *testing.T) {
	h := newTestHome(t)
	inst := newKillableInstance(t, "arch-ensure")
	require.NoError(t, inst.Transition(session.BeginArchive()))
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	target := captureSessionActionTarget(inst, h.repoID)

	model, _ := h.Update(instanceArchivedMsg{target: target, err: admissionDeadlineErr()})
	hm := model.(*home)

	assert.NotContains(t, hm.errBox.FullError(), "could not be confirmed",
		"an ensure-phase archive timeout is definitive — the daemon did not 'may have done it'")
	assert.NotContains(t, hm.errBox.FullError(), "may have done it",
		"a never-sent archive must not claim the daemon may have applied it")
	assert.Contains(t, hm.errBox.FullError(), "failed to archive",
		"a never-sent archive is a definitive failure")
	require.NotNil(t, hm.recovery, "a never-sent archive routes to the 'session is retained' recovery, not the uncertain-outcome notice")
	assert.Contains(t, hm.recovery.detail, "retained",
		"the session is retained because the archive was never sent")
	assert.NotEqual(t, session.OpArchiving, inst.GetInFlightOp(),
		"the optimistic archive fence reverts so the row cannot strand")
}

// TestRestoreEnsurePhaseTimeout_NotUncertain is the restore arm of the fix.
// restoreSessionThroughDaemon does not rewrap; like archive, a pre-fix
// ensure-phase timeout reported "could not be confirmed — the daemon may have
// done it" for a request never sent. Post-fix it reports a definitive failure.
func TestRestoreEnsurePhaseTimeout_NotUncertain(t *testing.T) {
	h := newTestHome(t)
	inst := newKillableInstance(t, "restore-ensure")
	h.store.AddInstance(inst)
	h.sidebar.SetSelectedInstance(0)
	target := captureSessionActionTarget(inst, h.repoID)

	_, _ = h.Update(instanceRestoredMsg{target: target, err: admissionDeadlineErr()})

	assert.NotContains(t, h.errBox.FullError(), "could not be confirmed",
		"an ensure-phase restore timeout is definitive — the daemon did not 'may have done it'")
	assert.NotContains(t, h.errBox.FullError(), "may have done it",
		"a never-sent restore must not claim the daemon may have applied it")
	assert.Contains(t, h.errBox.FullError(), "failed to restore",
		"a never-sent restore is a definitive failure")
}

// TestKillTimeout_PostSendTimeoutStillRewraps is the no-regression guard for the
// existing post-send-timeout path. A genuine context.DeadlineExceeded that does
// NOT wrap daemon.ErrAdmissionDeadline (the kill ctx expired after the request
// was sent) must STILL rewrap as errDaemonUnresponsive within 60s, so the
// handler keeps offering the in-TUI restart for the real post-send
// wedged-daemon case (#2479). The fix only intercepts the ensure-phase admission
// deadline — a bare post-send timeout must not trip that branch.
func TestKillTimeout_PostSendTimeoutStillRewraps(t *testing.T) {
	// Point the client straight at a wedged HTTP socket (accepts the connection
	// and never answers), bypassing the ensure phase so the request IS sent and
	// the kill ctx is what expires — the genuine "took the request and went
	// quiet" case the rewrap was built for.
	sock := wedgedDaemonSocket(t)
	origWith := withDaemonHTTPMutation
	origTimeout := killRPCTimeout
	t.Cleanup(func() { withDaemonHTTPMutation = origWith; killRPCTimeout = origTimeout })
	withDaemonHTTPMutation = func(fn func(*apiclient.Client) error) error {
		return fn(apiclient.NewWithSocket(sock))
	}
	killRPCTimeout = 250 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		done <- killSessionThroughDaemon(daemon.KillSessionRequest{ID: "x", Title: "x", RepoID: "r"})
	}()
	select {
	case err := <-done:
		require.Error(t, err, "the post-send timeout must surface an error")
		assert.False(t, errors.Is(err, daemon.ErrAdmissionDeadline),
			"a genuine post-send timeout must not trip the admission-deadline branch: %q", err.Error())
		assert.True(t, errors.Is(err, errDaemonUnresponsive),
			"a genuine post-send timeout must still wrap errDaemonUnresponsive so the handler offers the in-TUI restart (#2479): %q", err.Error())
		assert.Contains(t, err.Error(), "may still finish",
			"the post-send timeout rewrap must still admit the teardown may be in flight: %q", err.Error())
	case <-time.After(10 * time.Second):
		t.Fatal("HUNG: killSessionThroughDaemon never returned for a post-send timeout")
	}
}
