package daemon

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/apiproto"
)

// TestKillSession_RealControlSocketCommittedOutcomeReachesCLIClassifier is the
// end-to-end half of #3252 the daemon-side tests left open: they stop at the
// in-process handler. This drives the FULL control-socket transport — a real
// net/rpc gob round trip via the public daemon.KillSession (= callDaemon →
// dial → handler → envelope → gob back → CommittedOutcome() reconstitution
// into *rpcMutationCommittedError) — against a manager whose teardown can
// never complete safely, and asserts the error the CLI RunE would receive is
// classified committed by the rule apiclient.IsMutationCommitted delegates to.
//
// The CLI's sessionsKillCmd.RunE calls killSessionViaDaemon (= daemon.KillSession)
// and then apiclient.IsMutationCommitted(err) — which is apiproto.IsMutationCommitted
// under apiclient's name. This test asserts that same classification on the real
// committed error that survives the socket, plus the retained-tombstone contract,
// so G9 holds from killSessionRequestedBy through the transport to the predicate
// the CLI gates its exit code on. (The CLI RunE branch itself is pinned by
// TestSessionsKillReportsCommittedTeardownAsSuccess in package api, which
// substitutes the seam with the same committed-marker type this transport
// reconstitutes.)
func TestKillSession_RealControlSocketCommittedOutcomeReachesCLIClassifier(t *testing.T) {
	manager, repoID, created := newUnsafeTeardownKillFixture(t, "committed-socket-kill")

	// callDaemon runs EnsureDaemon, which would spawn a daemon process if the
	// socket were not already dialed. The in-process control server IS bound, so
	// stub the launcher to a no-op — Ping reaches the bound socket and
	// EnsureDaemon returns nil — the way TestCallDaemon_RetriesThroughWarmup does.
	prevLaunch := launchDaemonProcessFn
	launchDaemonProcessFn = func() error { return nil }
	t.Cleanup(func() { launchDaemonProcessFn = prevLaunch })

	closeServer, err := startControlServer(manager, nil, nil, nil)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	t.Cleanup(func() { _ = closeServer() })

	// The public KillSession goes through callDaemon → the real gob control
	// socket → the handler → manager.KillSession, which tombstones durably and
	// fails the unsafe teardown, wrapping the failure as a committed mutation.
	killErr := KillSession(KillSessionRequest{Title: "committed-socket-kill", RepoID: repoID})
	if killErr == nil {
		t.Fatal("expected KillSession over the control socket to surface the committed teardown failure")
	}

	// THIS is the guarantee the bug broke: the committed marker must survive the
	// real transport so the CLI's apiclient.IsMutationCommitted(err) branch fires
	// (ok:true + warning, zero exit) instead of jsonError + non-zero exit. The rule
	// the CLI's apiclient.IsMutationCommitted delegates to is apiproto.IsMutationCommitted.
	if !apiproto.IsMutationCommitted(killErr) {
		t.Fatalf("control-socket KillSession error = %T %v, want a committed-mutation marker "+
			"the CLI's apiclient.IsMutationCommitted would classify (the tombstone is durable, "+
			"so the CLI must report success-with-warning, not a hard failure)", killErr, killErr)
	}

	// The committed warning must carry the teardown failure's text so the CLI can
	// show it. The committed wrapper's canonical warning mentions the unfinished
	// teardown and the automatic retry — the same surface
	// TestControlKillSession_CommittedTeardownFailure_FillsEnvelopeWithoutKilledEvent
	// asserts at the handler envelope ("retried automatically").
	if !strings.Contains(killErr.Error(), "retried automatically") {
		t.Fatalf("committed warning must carry the teardown-failure/automatic-retry text, got: %v", killErr)
	}

	// G8 contract, viewed through the real socket: the tombstoned row is retained
	// for the asynchronous finishUserKill/ghost-worker reap, so a CLI retry can
	// neither recreate it nor read it as cleanly gone.
	rec := recordFor(t, repoID, "committed-socket-kill")
	if rec == nil || !rec.UserKilled {
		t.Fatalf("the retained tombstoned record must survive the committed kill, got %+v", rec)
	}
	_ = created
}
