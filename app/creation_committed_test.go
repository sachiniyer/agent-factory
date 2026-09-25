package app

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

// committedCreateServer stands up a real Unix-socket HTTP server that answers
// POST /v1/CreateSession exactly the way the daemon does — encoding the response
// through the shared apiproto.WriteEnvelope, the identical primitive
// daemon/httpserver.go uses — and returns a Client dialing it. Using the real
// envelope writer (not a hand-rolled body) is what makes the round-trip a genuine
// contract proof rather than a mock agreeing with itself, mirroring routeServer
// in apiclient/control_test.go. Only withDaemonHTTPMutation is faked, so the production
// startSessionThroughDaemon seam is what's under test, not a helper.
func committedCreateServer(t *testing.T, handle func(req daemon.CreateSessionRequest) apiproto.Envelope) *apiclient.Client {
	t.Helper()
	sockPath := testguard.SocketPath(t, "daemon-http.sock")
	ln, err := net.Listen("unix", sockPath)
	require.NoError(t, err, "listen unix")

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/CreateSession", func(w http.ResponseWriter, r *http.Request) {
		var req daemon.CreateSessionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")
		_ = apiproto.WriteEnvelope(w, handle(req))
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return apiclient.NewWithSocket(sockPath)
}

// TestStartSessionThroughDaemon_CommittedCreate_PreservesRetainedInstance is the
// regression lock for the seam half of this bug: apiclient.CreateSession keeps
// the payload on a mutation-committed outcome (a retained failed create still
// has a durable row, #3233/#3357), but startSessionThroughDaemon collapsed
// (&resp.Instance, committedErr) back to (nil, err), discarding the very identity
// that contract exists to deliver. Driving the REAL seam against a daemon-shaped
// Unix-socket HTTP server proves the retained row now reaches the handler.
func TestStartSessionThroughDaemon_CommittedCreate_PreservesRetainedInstance(t *testing.T) {
	const warning = `failed to create instance "retained", and its startup outcome could not be determined safely`
	c := committedCreateServer(t, func(req daemon.CreateSessionRequest) apiproto.Envelope {
		return apiproto.Success(daemon.CreateSessionResponse{
			Instance: session.InstanceData{
				Title:       req.Title,
				Program:     req.Program,
				Path:        req.RepoPath,
				BackendType: "docker", // a sandbox create — the daemon's first committed-create branch provisions a sandbox it then can't confirm torn down (daemon/manager_create.go). FromInstanceData rebuilds an inert sandbox backend without touching git, so the retained row materializes from a snapshot-shaped payload the way it does in production.
			},
			MutationOutcome: daemon.MutationOutcome{
				Code:    apiproto.ErrorCodeMutationCommitted,
				Warning: warning,
			},
		})
	})
	prev := withDaemonHTTPMutation
	withDaemonHTTPMutation = func(fn func(*apiclient.Client) error) error { return fn(c) }
	t.Cleanup(func() { withDaemonHTTPMutation = prev })

	inst, err := startSessionThroughDaemon(nil, sessionStartRequest{
		Title:    "retained",
		RepoPath: t.TempDir(),
		Program:  "claude",
	})

	require.Error(t, err, "a committed create must still surface its follow-up error")
	require.True(t, apiclient.IsMutationCommitted(err),
		"the committed marker must propagate through the seam so the handler can classify it, got %T: %v", err, err)
	require.NotNil(t, inst, "the retained row must NOT be discarded on a committed create — that discard was the bug")
	assert.Equal(t, "retained", inst.Title, "the materialized row must carry the daemon's retained identity")
}

// TestStartSessionThroughDaemon_CleanFailure_ReturnsNilInstance guards the
// non-committed branch of the seam: a plain daemon refusal carries no durable
// row, so the seam must keep returning (nil, err) exactly as before — only the
// committed path was changed. Without this, a future tweak could start
// fabricating a row from a clean failure.
func TestStartSessionThroughDaemon_CleanFailure_ReturnsNilInstance(t *testing.T) {
	c := committedCreateServer(t, func(daemon.CreateSessionRequest) apiproto.Envelope {
		return apiproto.Failure("the daemon refused this create")
	})
	prev := withDaemonHTTPMutation
	withDaemonHTTPMutation = func(fn func(*apiclient.Client) error) error { return fn(c) }
	t.Cleanup(func() { withDaemonHTTPMutation = prev })

	inst, err := startSessionThroughDaemon(nil, sessionStartRequest{
		Title:    "doomed",
		RepoPath: t.TempDir(),
		Program:  "claude",
	})

	require.Error(t, err)
	assert.Nil(t, inst, "a clean failure has no durable row to surface")
	assert.False(t, apiclient.IsMutationCommitted(err),
		"a plain refusal must not be misclassified as a committed mutation")
}

// TestInstanceStarted_CommittedWarning_KeepsRowSurfacesWarningNoDraftNoRecovery
// is the regression lock for the handler half of this bug. With the seam now
// surfacing the retained row, the instanceStartedMsg handler must classify a
// committed create as "created, with warning" — keeping the started row,
// surfacing the warning through handleError, and NOT removing the row, retaining
// the draft, or showing the "Cannot create session" recovery screen — mirroring
// handleInstanceArchived / handleInstanceRestored. Pre-fix this path removed the
// placeholder, stashed the draft in failedCreate, and showed "Cannot create
// session" for a session the daemon had already durably persisted.
func TestInstanceStarted_CommittedWarning_KeepsRowSurfacesWarningNoDraftNoRecovery(t *testing.T) {
	h := newTestHome(t)
	placeholder := newLoadingInstance(t, "retained-create")
	h.store.AddInstance(placeholder)
	h.sidebar.SelectInstance(placeholder)

	started := newStartedInstance(t, "retained-create")
	const warning = `failed to start instance "retained-create", and its cleanup could not complete safely`
	committedErr := &testMutationCommittedError{msg: warning}
	require.True(t, apiclient.IsMutationCommitted(committedErr),
		"precondition: the stubbed error must classify as mutation-committed")

	req := sessionStartRequest{Title: placeholder.Title, RepoPath: h.repoRoot, Program: "claude", Prompt: "do the thing"}
	_, _ = h.Update(instanceStartedMsg{
		instance:  placeholder,
		started:   started,
		err:       committedErr,
		draft:     &req,
		rawPrompt: "do the thing",
		account:   "",
	})

	// The retained row replaces the Loading placeholder; the row is NOT removed.
	require.True(t, h.store.ContainsInstance(started), "the retained row must replace the placeholder, not be dropped")
	assert.False(t, h.store.ContainsInstance(placeholder), "the Loading placeholder must be replaced by the durable row")
	assert.Contains(t, collectTitles(h.store.GetInstances()), "retained-create",
		"the durable row must remain in the sidebar")

	// The draft is NOT retained for retry — the daemon already committed the
	// create, so inviting a retry would race the title-uniqueness reservation.
	assert.Nil(t, h.failedCreate, "the create draft must NOT be retained on a committed create")

	// No recovery screen / "Cannot create session": the create landed.
	require.Nil(t, h.recovery, "a committed create must not raise the recovery screen")
	assert.Equal(t, stateDefault, h.state, "a committed create must not strand the user in a modal/recovery state")

	// The committed warning surfaces to the user via handleError (transient
	// failure), matching the sibling session-mutation handlers. The errBox carries
	// the daemon's warning text, and the retained notice is categorized as a
	// failure (handleError, not showTransientMessage) — the choice
	// handleInstanceArchived / handleInstanceRestored also make.
	notice, isFailure := h.errBox.RetainedNotice()
	require.NotEmpty(t, notice, "the committed warning must surface to the user")
	assert.True(t, isFailure, "the committed warning surfaces via handleError as a failure-category notice")
	assert.Contains(t, notice, warning, "the daemon's committed warning text must reach the user verbatim")
	assert.Contains(t, notice, "created session", "the warning must frame the outcome as a created session, not a failure")
	assert.Contains(t, notice, "retained-create")
	assert.Contains(t, notice, "done, with a warning", "a committed create surfaces the 'done, with a warning' wording in the shared helper, not 'could not be confirmed'")
	assert.NotContains(t, notice, "could not be confirmed", "a committed create landed; it is not an unknown outcome")
	assert.NotContains(t, notice, "may have done it", "a committed create is known to have landed, never 'may have'")
}

// TestInstanceStarted_CommittedWarning_UserNavigatedAway_KeepsRowSilently proves
// the committed branch fires regardless of whether the user is still watching
// the creating instance (the warning matters either way), and that it does not
// yank the user's selection or pop a help screen onto them — the same
// don't-yank guarantee the success path gives a user who navigated away.
func TestInstanceStarted_CommittedWarning_UserNavigatedAway_KeepsRowSilently(t *testing.T) {
	h := newTestHome(t)
	placeholder := newLoadingInstance(t, "retained-create")
	other := newLoadingInstance(t, "unrelated")
	other.SetStatusForTest(session.Running)
	h.store.AddInstance(placeholder)
	h.store.AddInstance(other)
	h.sidebar.SetSelectedInstance(1) // user moved onto `other`

	started := newStartedInstance(t, "retained-create")
	req := sessionStartRequest{Title: placeholder.Title, RepoPath: h.repoRoot, Program: "claude"}

	_, _ = h.Update(instanceStartedMsg{
		instance:  placeholder,
		started:   started,
		err:       &testMutationCommittedError{msg: "startup outcome could not be determined safely"},
		draft:     &req,
		rawPrompt: "x",
	})

	// The durable row is kept; the user's selection on `other` is preserved.
	assert.True(t, h.store.ContainsInstance(started), "the retained row must be kept even when the user navigated away")
	assert.False(t, h.store.ContainsInstance(placeholder), "the placeholder must be replaced by the durable row")
	assert.Same(t, other, h.sidebar.GetSelectedInstance(),
		"the user's selection must remain on the instance they navigated to")
	assert.Nil(t, h.failedCreate, "no draft retention on a committed create")
	assert.Nil(t, h.recovery, "no recovery screen on a committed create")
	// The warning still surfaces — a committed create is not silent just because
	// the user looked away: the durable row is theirs to address.
	notice, _ := h.errBox.RetainedNotice()
	assert.Contains(t, notice, "created session", "the committed warning must surface even when the user navigated away")
}

// TestInstanceStarted_CleanFailure_StillRemovesRowRetainsDraftAndShowsRecovery is
// the regression guard for the pre-existing failure path: a plain (non-committed)
// error must still remove the placeholder and show the "Cannot create session"
// recovery screen, and — when the user navigated away — retain the draft for
// retry. The committed-warning branch must not weaken the clean-failure behavior
// the existing tests pin (creation_test.go:356, recovery_test.go:93).
func TestInstanceStarted_CleanFailure_StillRemovesRowRetainsDraftAndShowsRecovery(t *testing.T) {
	t.Run("removes placeholder and raises recovery when watching", func(t *testing.T) {
		h := newTestHome(t)
		failing := newLoadingInstance(t, "doomed")
		h.store.AddInstance(failing)
		h.sidebar.SelectInstance(failing)

		// No draft: the existing TestInstanceStarted_Failure_OnFailedInstance
		// shape — a clean failure with nothing to re-arm, so restoreFailedCreate
		// never runs and the placeholder is gone for good.
		_, _ = h.Update(instanceStartedMsg{
			instance: failing,
			err:      &plainCreateError{msg: "The daemon refused this create."},
		})

		assert.False(t, h.store.ContainsInstance(failing), "a clean failure must still remove the placeholder row")
		assert.Empty(t, h.store.GetInstances(), "no row should remain after a clean create failure")
		assert.Nil(t, h.failedCreate, "no draft was supplied, so none is retained")
		require.NotNil(t, h.recovery, "a clean failure must raise the recovery screen")
		assert.Equal(t, "Cannot create session", h.recovery.condition)
		assert.False(t, apiclient.IsMutationCommitted(&plainCreateError{msg: "x"}),
			"sanity: a plain refusal is not mutation-committed, so it must take the failure branch")
	})

	t.Run("retains the draft for retry when the user navigated away", func(t *testing.T) {
		h := newTestHome(t)
		failing := newLoadingInstance(t, "doomed")
		other := newLoadingInstance(t, "unrelated")
		other.SetStatusForTest(session.Running)
		h.store.AddInstance(failing)
		h.store.AddInstance(other)
		h.sidebar.SetSelectedInstance(1) // user moved onto `other`

		req := sessionStartRequest{Title: failing.Title, RepoPath: h.repoRoot, Program: "claude", Prompt: "retry me"}
		_, _ = h.Update(instanceStartedMsg{
			instance:  failing,
			err:       &plainCreateError{msg: "The daemon refused this create."},
			draft:     &req,
			rawPrompt: "retry me",
		})

		// The placeholder is removed; the user's selection on `other` is kept.
		assert.False(t, h.store.ContainsInstance(failing), "a clean failure must remove the failed placeholder")
		assert.True(t, h.store.ContainsInstance(other), "an unrelated row must survive the clean failure")
		assert.Same(t, other, h.sidebar.GetSelectedInstance(), "the user's selection is preserved")
		// The draft is retained for the next create in this project — the
		// user navigated away, so the form is NOT re-armed here; failedCreate
		// holds it. (A committed create must NOT retain the draft — that is the
		// bug; this pins the opposite behavior for a clean failure.)
		require.NotNil(t, h.failedCreate, "a clean failure must retain the draft for retry")
		assert.Same(t, failing, h.failedCreate.instance, "the retained draft must be the failed create's")
		require.NotNil(t, h.recovery, "a clean failure must raise the recovery screen")
		assert.Equal(t, "Cannot create session", h.recovery.condition)
	})
}

// TestInstanceStarted_CommittedWarning_PreservesDaemonLiveness guards the
// liveness-preservation half of the handler fix. A mutation-committed create
// retained the row with the daemon's own liveness (running, indeterminate, or
// tombstoned-for-cleanup-retry). The handler must NOT call Transition(ConfirmLive)
// on that row — doing so would force a tombstoned row to LiveRunning and mislead
// the user until the next snapshot reconciled it. The success-path
// ConfirmLive flip is now scoped to the non-committed (clean-start) path.
func TestInstanceStarted_CommittedWarning_PreservesDaemonLiveness(t *testing.T) {
	h := newTestHome(t)
	placeholder := newLoadingInstance(t, "tombstoned")
	h.store.AddInstance(placeholder)
	h.sidebar.SelectInstance(placeholder)

	// Materialize a retained row the way the daemon records a tombstoned
	// cleanup-retry: LiveDead, started=false. SetStatusForTest(Dead) decomposes
	// onto {liveness=LiveDead, op=OpNone} — the exact state ConfirmLive would
	// legally flip to LiveRunning (transitionTable tkConfirmLive allowedFrom
	// op==OpNone), so this is a faithful probe: if the handler called
	// ConfirmLive on the committed path, liveness would become LiveRunning.
	started, err := session.NewInstance(session.InstanceOptions{
		Title:   "tombstoned",
		Path:    t.TempDir(),
		Program: "claude",
	})
	require.NoError(t, err)
	started.SetStatusForTest(session.Dead)

	_, _ = h.Update(instanceStartedMsg{
		instance: placeholder,
		started:  started,
		err:      &testMutationCommittedError{msg: "cleanup could not complete safely"},
		draft:    &sessionStartRequest{Title: "tombstoned", RepoPath: h.repoRoot, Program: "claude"},
	})

	require.True(t, h.store.ContainsInstance(started), "the retained row must replace the placeholder")
	// The daemon's liveness is preserved — ConfirmLive was NOT called on the
	// committed path, so the row stays Dead rather than being resurrected to
	// LiveRunning. (The next snapshot reconciles either way; the point is the
	// handler must not lie about liveness in the interim.)
	assert.Equal(t, session.LiveDead, started.GetLiveness(),
		"a committed retained row must keep the daemon's liveness, not be forced to LiveRunning")
	assert.Equal(t, session.OpNone, started.GetInFlightOp(),
		"the committed row's op axis must be untouched")
}

// TestMutationOutcomeError_CommittedRendersDoneWithWarning pins the shared
// helper's committed-with-warning wording at two committed-capable sites — the
// create action and the archive action — so a mutationCommittedError renders
// "<action> — done, with a warning: <post-commit error>" everywhere
// mutationOutcomeError is called, not "could not be confirmed". On master this
// committed branch still reads "went through, but the daemon reported a
// follow-up problem", so the "done, with a warning" assertions fail there and
// pass here; the uncertain control keeps #4904's "could not be confirmed"
// wording unchanged.
func TestMutationOutcomeError_CommittedRendersDoneWithWarning(t *testing.T) {
	const postCommit = "its VS Code editor did not stop in time, so the tombstoned record was kept for a retry"
	committedErr := &testMutationCommittedError{msg: postCommit}
	require.True(t, apiclient.IsMutationCommitted(committedErr),
		"precondition: the stubbed error must classify as mutation-committed")

	t.Run("create action", func(t *testing.T) {
		got := mutationOutcomeError(fmt.Sprintf("created session %q", "x"), "the sidebar", committedErr)
		assert.Contains(t, got.Error(), `created session "x"`)
		assert.Contains(t, got.Error(), "done, with a warning", "a committed create renders the 'done, with a warning' wording, not 'went through'")
		assert.Contains(t, got.Error(), postCommit, "the post-commit error must be wrapped verbatim")
		assert.NotContains(t, got.Error(), "could not be confirmed", "a committed outcome is not unknown")
		assert.NotContains(t, got.Error(), "may have done it", "a committed outcome is known to have landed, never 'may have'")
	})

	t.Run("archive action", func(t *testing.T) {
		got := mutationOutcomeError(fmt.Sprintf("archiving session '%s'", "y"), "the sidebar", committedErr)
		assert.Contains(t, got.Error(), "archiving session 'y'")
		assert.Contains(t, got.Error(), "done, with a warning", "the shared helper renders 'done, with a warning' at every committed-capable site, not just create")
		assert.Contains(t, got.Error(), postCommit)
		assert.NotContains(t, got.Error(), "could not be confirmed")
	})

	t.Run("uncertain outcome keeps the unknown wording", func(t *testing.T) {
		got := mutationOutcomeError(fmt.Sprintf("created session %q", "x"), "the sidebar", replyLost())
		assert.Contains(t, got.Error(), "could not be confirmed", "#4904's uncertain wording is unchanged")
		assert.NotContains(t, got.Error(), "done, with a warning", "the committed wording must not leak into the uncertain branch")
	})
}

// plainCreateError is a non-committed error — the shape a plain daemon refusal
// takes (apiclient.interpretEnvelopeError returns a bare message when the
// envelope code is empty). It must NOT satisfy apiproto.MutationCommittedError,
// so the handler treats it as a clean failure.
type plainCreateError struct{ msg string }

func (e *plainCreateError) Error() string { return e.msg }
