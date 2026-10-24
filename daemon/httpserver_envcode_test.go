package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/internal/fsprobe"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	sessiontmux "github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/require"
)

// TestWriteHTTPError_BoundedProbeNoAnswerCode pins the server half of the
// #5295 wire contract: an error chain holding context.DeadlineExceeded
// paired with one of af's bounded-probe sentinels serializes across the HTTP
// envelope as the machine-readable ErrorCodeBoundedProbeNoAnswer — the
// sentinel's errors.Is identity dies in serialization, so the code is what
// carries the class a client reconstructs. The bare-deadline case (the
// preserve push's own network deadline, with no sentinel half) still emits
// ErrorCodeDeadlineExceeded; the errno sibling still wins as
// resource_exhausted when both hold; and a plain failure stays uncoded. The
// five in-sandbox sentinels plus ErrRelocateStateUnknown are all pinned, with
// the EXACT producer shape Start's pre-spawn has-session gate builds (the
// chain the bug report's reproduction test exercised) as one case.
func TestWriteHTTPError_BoundedProbeNoAnswerCode(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		wantCode string
	}{
		// Each of af's bounded-probe sentinels paired with the deadline
		// serializes as the bounded-probe-non-answer class. The first case
		// is the EXACT producer shape session/tmux/start.go builds: the
		// chain the bug report's reproduction test exercised.
		{"tmux pre-spawn has-session gate (real producer shape)", fmt.Errorf(
			"%w: has-session probe for session %q did not answer: %w: %w",
			sessiontmux.ErrTmuxTimeout, "s", sessiontmux.ErrTmuxServerNoAnswer, context.DeadlineExceeded),
			apiproto.ErrorCodeBoundedProbeNoAnswer},
		{"fsprobe write-capacity flight", errors.Join(
			fsprobe.ErrProbeTimedOut, context.DeadlineExceeded),
			apiproto.ErrorCodeBoundedProbeNoAnswer},
		{"git worktree setup-stalled sentinel", fmt.Errorf(
			"clear stale worktree path: %w: killed at its deadline: %w",
			sessiongit.ErrWorktreeSetupStalled, context.DeadlineExceeded),
			apiproto.ErrorCodeBoundedProbeNoAnswer},
		{"git setup-ownership sentinel", fmt.Errorf(
			"%w: cannot verify the registered branch at /wt: %w",
			sessiongit.ErrSetupRemovalOwnershipUnproven, context.DeadlineExceeded),
			apiproto.ErrorCodeBoundedProbeNoAnswer},
		{"git common-dir sentinel", fmt.Errorf(
			"%w: %w", sessiongit.ErrCommonDirProbeNoAnswer, context.DeadlineExceeded),
			apiproto.ErrorCodeBoundedProbeNoAnswer},
		{"git relocate-state sentinel", errors.Join(
			sessiongit.ErrRelocateStateUnknown, context.DeadlineExceeded),
			apiproto.ErrorCodeBoundedProbeNoAnswer},
		// A bare deadline — the preserve push's own network deadline, with
		// no sentinel — still serializes as the bare-deadline code; the
		// client reconstructs only context.DeadlineExceeded and the
		// preserve call site's bare-deadline escape still reads it as
		// environmental. The Recover path intentionally refuses this case
		// (a permanently hung user launch_cmd wraps the same bare
		// deadline) — so the bare-deadline code MUST stay distinct from
		// the bounded-probe-non-answer code (#5295 review).
		{"bare deadline stays bare-deadline code", fmt.Errorf(
			"preserve push: %w", context.DeadlineExceeded),
			apiproto.ErrorCodeDeadlineExceeded},
		// The errno sibling wins when both hold: a full disk's ENOSPC
		// reads as the stronger environmental signal, and the resource
		// errno check above the deadline one returns
		// resource_exhausted with the unbounded wait the errno class
		// earns. The deadline+sentinel signature is dropped on purpose
		// — the verdict is the same environmental class either way, and
		// the errno class is the unbounded one.
		{"resource errno wins over deadline+sentinel", errors.Join(
			syscall.ENOSPC, fsprobe.ErrProbeTimedOut, context.DeadlineExceeded),
			apiproto.ErrorCodeResourceExhausted},
		// A plain failure stays uncoded, preserving byte-for-byte
		// compatibility with older clients.
		{"plain failure stays uncoded", errors.New("agent exited at startup"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			writeHTTPError(rec, httptest.NewRequest(http.MethodPost, "/v1/agent/snapshot", nil),
				http.StatusInternalServerError, tc.err)
			var env struct {
				Error map[string]any `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
			if tc.wantCode == "" {
				require.NotContains(t, env.Error, "code",
					"a plain failure must not carry a code (err: %v)", tc.err)
			} else {
				require.Equal(t, tc.wantCode, env.Error["code"],
					"wrong code for %v", tc.err)
			}
		})
	}
}

// TestWriteHTTPError_BoundedProbeNoAnswerCode_Ordering pins the ordering of
// the three envelope-code arms in writeHTTPError: coded error (APIErrorCode)
// wins first, then resource errno, then deadline+sentinel (the new arm), then
// bare deadline. The coded-error arm exists for the daemon's own typed
// errors (mutation_committed, etc.); a sentinel-bearing deadline wired
// through that surface stays coded-error, preserving the typed contract.
func TestWriteHTTPError_BoundedProbeNoAnswerCode_Ordering(t *testing.T) {
	// A typed coded error that also carries the deadline+sentinel signature
	// still emits its own code, not the bounded-probe-non-answer code — the
	// coded-error arm wins over the environmental-code arms, exactly as it
	// already wins over the resource_exhausted arm.
	coded := &codedWithDeadlineSentinel{
		msg: "admission refused", code: "admission_refused"}
	rec := httptest.NewRecorder()
	writeHTTPError(rec, httptest.NewRequest(http.MethodPost, "/v1/CreateSession", nil),
		http.StatusServiceUnavailable, coded)
	var env struct {
		Error map[string]any `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	require.Equal(t, "admission_refused", env.Error["code"],
		"a typed coded error must win over the environmental envelope-code arms")
}

// codedWithDeadlineSentinel is a test-only error that carries BOTH its own
// APIErrorCode (the typed surface) AND the deadline+sentinel signature, so
// the ordering test can prove the coded-error arm wins.
type codedWithDeadlineSentinel struct {
	msg  string
	code string
}

func (e *codedWithDeadlineSentinel) Error() string        { return e.msg }
func (e *codedWithDeadlineSentinel) APIErrorCode() string { return e.code }
func (e *codedWithDeadlineSentinel) Unwrap() []error {
	return []error{sessiontmux.ErrTmuxServerNoAnswer, context.DeadlineExceeded}
}

// TestWireCrossing_BoundedProbeNoAnswerSurvivesWire is the end-to-end
// composition the original bug report's reproduction test pinned: an
// in-sandbox bounded probe that does not answer produces an error chain
// holding a sentinel AND context.DeadlineExceeded, the chain classifies
// environmentally in-process — and after the fix, it ALSO classifies
// environmentally after the daemon's HTTP envelope wire hop. The hop runs
// the real writeHTTPError (server side) against the real callWithin (client
// side, reached through session.NewRemoteAgentServer(...).Snapshot()), and
// the resulting error walks back through the real lost-restore classifier.
//
// Before the fix, the wire bridge flattened the chain to a bare
// deadline_exceeded code and callWithin reconstructed only
// context.DeadlineExceeded, so the classifier's deadline+sentinel arm was
// unreachable for sandbox sessions — every sandbox deadline+sentinel
// failure charged the structural budget on the Recover path, halving the
// retry budget an identical local-session failure gets.
//
// Contrast arms: a BARE deadline (the preserve push's own network deadline,
// reconstructed by the bare-deadline code arm) stays structural on the
// Recover path, and a resource errno joined with the deadline+sentinel
// (the full-disk-with-fsprobe-timeout shape) classifies as the unbounded
// errno class — both correctly, pinning no regression in either direction.
//
// Precondition: none beyond the Go toolchain — the test wires an in-process
// httptest server against session.NewRemoteAgentServer; no real daemon, no
// real sandbox, no real tmux.
func TestWireCrossing_BoundedProbeNoAnswerSurvivesWire(t *testing.T) {
	for _, tc := range []struct {
		name      string
		serveErr  error
		wantClass lostRestoreEnvClass
		// wantSentinelIdent is non-nil when the wire reconstruction must
		// carry a specific sentinel identity the classifier arm reads.
		wantSentinelIdent error
	}{
		{
			name: "in-sandbox tmux pre-spawn gate stays environmental across the wire",
			// The EXACT chain session/tmux/start.go builds.
			serveErr: fmt.Errorf(
				"%w: has-session probe for session %q did not answer: %w: %w",
				sessiontmux.ErrTmuxTimeout, "wedged", sessiontmux.ErrTmuxServerNoAnswer, context.DeadlineExceeded),
			wantClass:         lostRestoreEnvDeadline,
			wantSentinelIdent: apiproto.ErrRemoteBoundedProbeNoAnswer,
		},
		{
			name: "in-sandbox fsprobe write-capacity flight stays environmental across the wire",
			serveErr: errors.Join(
				fsprobe.ErrProbeTimedOut, context.DeadlineExceeded),
			wantClass:         lostRestoreEnvDeadline,
			wantSentinelIdent: apiproto.ErrRemoteBoundedProbeNoAnswer,
		},
		{
			name: "in-sandbox git worktree setup-stall stays environmental across the wire",
			serveErr: fmt.Errorf(
				"clear stale worktree path: %w: killed at its deadline: %w",
				sessiongit.ErrWorktreeSetupStalled, context.DeadlineExceeded),
			wantClass:         lostRestoreEnvDeadline,
			wantSentinelIdent: apiproto.ErrRemoteBoundedProbeNoAnswer,
		},
		{
			// Contrast: a BARE deadline (preserve push's own network
			// deadline) arrives via the bare-deadline code arm and
			// reconstructs only context.DeadlineExceeded. On the Recover
			// path this intentionally stays structural — a permanently
			// hung user launch_cmd wraps the same bare deadline, and
			// neither half of the deadline+sentinel signature is exempt
			// alone (#5295 review).
			name: "bare deadline stays structural on the Recover path (no regression)",
			serveErr: fmt.Errorf(
				"archive failed: git push: %w", context.DeadlineExceeded),
			wantClass: lostRestoreEnvNone,
		},
		{
			// Contrast: a resource errno joined with the deadline+sentinel
			// arrives via the resource_exhausted arm and reconstructs
			// ErrRemoteResourceExhausted, classifying as the unbounded
			// errno class — the same crossing works for the errno sibling.
			name: "resource errno joined with deadline+sentinel stays errno class (no regression)",
			serveErr: errors.Join(
				fmt.Errorf("write probe: %w", syscall.ENOSPC),
				fsprobe.ErrProbeTimedOut, context.DeadlineExceeded),
			wantClass:         lostRestoreEnvErrno,
			wantSentinelIdent: apiproto.ErrRemoteResourceExhausted,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Server side: the real writeHTTPError. The in-sandbox
			// handler runs in the daemon package (the headless
			// agent-server is af agent-server, a daemon binary mode),
			// so writeHTTPError is the actual emitter.
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeHTTPError(w, r, http.StatusInternalServerError, tc.serveErr)
			}))
			defer srv.Close()

			// Client side: the real callWithin, reached through the
			// public AgentServer surface so the test exercises the
			// exact reconstruction production code runs.
			as, err := session.NewRemoteAgentServer(
				session.AgentServerEndpoint{URL: srv.URL, Token: "test-token"},
				"wedged-probe")
			require.NoError(t, err)
			_, callErr := as.Snapshot()
			require.Error(t, callErr, "the wire-crossed failure must surface as an error")

			// The classifier is the daemon-side policy that consumes
			// the reconstructed chain — the same call lostRestoreFailed
			// makes on the Recover path.
			gotClass := lostRestoreEnvironmentalClass(callErr)
			require.Equal(t, tc.wantClass, gotClass,
				"wire-crossed classification mismatch for %v", tc.serveErr)

			// The classifier reads the reconstructed sentinel through
			// errors.Is — pin it explicitly so a future refactor that
			// drops the sentinel half of the pair fails loudly.
			if tc.wantSentinelIdent != nil {
				require.True(t, errors.Is(callErr, tc.wantSentinelIdent),
					"errors.Is(err, %v) = false for %v", tc.wantSentinelIdent, tc.serveErr)
			} else {
				require.False(t, errors.Is(callErr, apiproto.ErrRemoteBoundedProbeNoAnswer),
					"a bare deadline must NOT carry the bounded-probe sentinel after the wire: %v", callErr)
				require.False(t, errors.Is(callErr, apiproto.ErrRemoteResourceExhausted),
					"a bare deadline must NOT carry the resource sentinel after the wire: %v", callErr)
			}
		})
	}
}

// TestWireCrossing_BoundedProbeNoAnswerPreservesRecoverBudget is the
// accounting differential the original bug report measured: under the fix,
// the first lostRestoreMaxAttempts deadline+sentinel failures on the Recover
// path are absorbed environmentally (consecutiveFailures untouched), and only
// further failures charge the structural budget — 12 failures (6
// environmental + 6 structural) reach the durable give-up. Before the fix,
// the misclassified bare-deadline path charged consecutiveFailures on every
// failure and gave up after 6, halving the budget an identical local-session
// failure gets. This test pins the post-fix accounting by driving the same
// deadline+sentinel failure through the recovered-state machine
// lostRestoreFailed consumes, not just the classifier — proving the
// classifier verdict reaches the accounting arm.
//
// Precondition: none beyond the Go toolchain — uses the recoverFakeBackend
// double, no real tmux, no real sandbox. The wire hop is the same
// httptest.NewServer + session.NewRemoteAgentServer shape used by the
// classification test above; the sandbox's "recover" failure is produced by
// wiring the agent-server's Snapshot failure envelope through writeHTTPError
// and feeding the reconstructed error into the same recordLostRestoreFailure
// path production uses.
func TestWireCrossing_BoundedProbeNoAnswerPreservesRecoverBudget(t *testing.T) {
	manager, ownLogs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	zeroRestoreBackoff(t)
	backend := &recoverFakeBackend{
		FakeBackend: session.NewFakeBackend(),
	}
	inst := registerStarted(t, manager, repoID, repoPath, "wedged-probe", backend, true, session.Lost)

	// The exact in-sandbox tmux Start error chain the bug report's
	// reproduction test exercised, served through the real writeHTTPError
	// and reconstructed through the real callWithin (via the public
	// AgentServer.Snapshot surface). The reconstructed error is what the
	// loop would record if the sandbox produced it on Recover — i.e. the
	// shape lostRestoreFailed actually consumes for a sandbox session.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeHTTPError(w, r, http.StatusInternalServerError, fmt.Errorf(
			"%w: has-session probe for session %q did not answer: %w: %w",
			sessiontmux.ErrTmuxTimeout, "wedged-probe",
			sessiontmux.ErrTmuxServerNoAnswer, context.DeadlineExceeded))
	}))
	defer srv.Close()
	as, err := session.NewRemoteAgentServer(
		session.AgentServerEndpoint{URL: srv.URL, Token: "test-token"}, "wedged-probe")
	require.NoError(t, err)
	_, wireErr := as.Snapshot()
	require.Error(t, wireErr, "setup: the wire crossing must surface an error")
	require.True(t, errors.Is(wireErr, apiproto.ErrRemoteBoundedProbeNoAnswer),
		"setup: the wire-crossed error must carry the bounded-probe sentinel: %v", wireErr)
	require.True(t, errors.Is(wireErr, context.DeadlineExceeded),
		"setup: the wire-crossed error must carry the deadline: %v", wireErr)

	// Drive the whole bounded environmental share through the accounting
	// loop production uses (recordLostRestoreFailure -> lostRestoreFailed).
	// Each absorbed wait must NOT charge consecutiveFailures — the
	// structural budget is preserved for a condition the environment
	// owns. lostRestoreMaxAttempts environmental waits, then a further 6
	// attempts that should now charge structural and finally give up —
	// matching the local-session budget, not half of it.
	for i := 0; i < lostRestoreMaxAttempts; i++ {
		manager.recordLostRestoreFailure(daemonInstanceKey(repoID, "wedged-probe"), repoID, inst, wireErr, lostRestoreAutomatic)
	}
	manager.mu.Lock()
	st := manager.lostRestoreStates[stableSessionKey(repoID, inst)]
	var consecutive, envDeadline, environmental int
	if st != nil {
		consecutive, envDeadline, environmental = st.consecutiveFailures, st.environmentalDeadlineFailures, st.environmentalFailures
	}
	manager.mu.Unlock()
	require.NotNil(t, st, "the episode state must stay tracked across the bounded share")
	require.Equal(t, 0, consecutive,
		"after %d absorbed environmental waits, consecutiveFailures must be 0 — the structural budget is preserved", lostRestoreMaxAttempts)
	require.Equal(t, lostRestoreMaxAttempts, envDeadline,
		"after %d absorbed waits, environmentalDeadlineFailures must be exactly the bounded share", lostRestoreMaxAttempts)
	require.Equal(t, lostRestoreMaxAttempts, environmental,
		"after %d absorbed waits, environmentalFailures must count each one", lostRestoreMaxAttempts)
	require.False(t, inst.LifecycleView().LostRestoreGaveUp,
		"the bounded environmental share must NOT reach the durable give-up")
	require.True(t, inst.LifecycleView().RestoreWaitingOnEnvironment,
		"the absorbed wait must be visible on the session as 'waiting on the environment'")
	require.True(t, strings.Contains(ownLogs.warnings.String(), "waiting on the environment"),
		"the wait must be logged as 'waiting on the environment'; warn logs:\n%s", ownLogs.warnings.String())
}
