package daemon

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// newDeliverCommittedFixture builds a manager whose backend factory installs
// the given backend, mirroring newCreateCommittedFixture. The target session is
// NOT pre-registered, so DeliverPrompt takes the absent-target branch through
// createMissingPromptTarget -> Manager.CreateSession — the same committed-
// producing manager method createSession reaches. The home lives under a socket
// safe temp dir so the same fixture drives both the in-process handler test and
// the real net/rpc socket test.
func newDeliverCommittedFixture(t *testing.T, backend session.Backend) (*Manager, string, string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	restore := session.SetBackendFactoryForTest(func(session.InstanceOptions, string) (session.Backend, error) {
		return backend, nil
	})
	t.Cleanup(restore)

	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return manager, repo.ID, repoPath
}

// TestControlDeliverPrompt_CommittedAutoCreate_FillsEnvelope pins the handler
// half of #3357: DeliverPrompt's absent-target branch routes through
// Manager.CreateSession, which can return a *mutationCommittedError when its
// startup outcome is unknown. The handler MUST record that outcome in the
// response envelope (net/rpc flattens a returned error to a string, which reads
// as failed-nothing-committed) — mirroring createSession — and the retained
// recorded-for-inspection row must survive so the durable session stays
// addressable. Before the fix the handler returned the committed error
// directly, dropping the marker at the transport boundary.
func TestControlDeliverPrompt_CommittedAutoCreate_FillsEnvelope(t *testing.T) {
	backend := &unknownStartBackend{readyFakeBackend: readyFakeBackend{session.NewFakeBackend()}}
	manager, repoID, repoPath := newDeliverCommittedFixture(t, backend)
	cs := &controlServer{manager: manager}

	var resp DeliverPromptResponse
	if err := cs.DeliverPrompt(DeliverPromptRequest{
		Title:    "uncertain-deliver",
		RepoPath: repoPath,
		Program:  "claude",
		Prompt:   "run it",
	}, &resp); err != nil {
		t.Fatalf("a committed retained auto-create must land in the envelope, not be returned as an rpc error: %v", err)
	}
	if resp.MutationOutcome.Code != apiproto.ErrorCodeMutationCommitted {
		t.Fatalf("resp code = %q, want %q", resp.MutationOutcome.Code, apiproto.ErrorCodeMutationCommitted)
	}
	if !strings.Contains(resp.MutationOutcome.Warning, "recorded for inspection") {
		t.Fatalf("the warning must carry the committed auto-create explanation, got %q", resp.MutationOutcome.Warning)
	}
	rec := recordFor(t, repoID, "uncertain-deliver")
	if rec == nil {
		t.Fatal("the retained recorded-for-inspection row must exist")
	}
	if rec.Title != "uncertain-deliver" || !rec.StartupStateUnknown {
		t.Fatalf("retained row = %+v, want title uncertain-deliver with StartupStateUnknown set", rec)
	}
}

// TestDeliverPrompt_RealControlSocket_CommittedOutcomeReachesCLIClassifier is
// the end-to-end half of #3357: it drives the FULL control-socket transport — a
// real net/rpc gob round trip via the public daemon.DeliverPromptWithStatus
// (-> callDaemon -> dial -> handler -> envelope -> gob back ->
// CommittedOutcome() reconstitution into *rpcMutationCommittedError) — against
// a manager whose auto-create startup outcome is unknown, and asserts the
// error the caller would receive is classified committed by the rule
// apiclient.IsMutationCommitted delegates to (apiproto.IsMutationCommitted).
// Before the fix the handler returned the committed error directly, net/rpc
// flattened it to a plain rpc.ServerError whose string matched neither the
// legacy task-CRUD prefixes nor the CommittedOutcome() carrier, so
// IsMutationCommitted returned false and a cron/watch/trigger re-fire treated
// the durable recorded row as a clean, freely-retryable failure.
func TestDeliverPrompt_RealControlSocket_CommittedOutcomeReachesCLIClassifier(t *testing.T) {
	backend := &unknownStartBackend{readyFakeBackend: readyFakeBackend{session.NewFakeBackend()}}
	manager, repoID, repoPath := newDeliverCommittedFixture(t, backend)

	// callDaemon runs EnsureDaemon, which would spawn a daemon process if the
	// socket were not already dialed. The in-process control server IS bound,
	// so stub the launcher to a no-op — Ping reaches the bound socket and
	// EnsureDaemon returns nil — the way TestKillSession_RealControlSocket
	// CommittedOutcomeReachesCLIClassifier does.
	prevLaunch := launchDaemonProcessFn
	launchDaemonProcessFn = func() error { return nil }
	t.Cleanup(func() { launchDaemonProcessFn = prevLaunch })

	closeServer, err := startControlServer(manager, nil, nil, nil)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	t.Cleanup(func() { _ = closeServer() })

	// The public DeliverPromptWithStatus goes through callDaemon -> the real
	// gob control socket -> the handler -> createMissingPromptTarget ->
	// Manager.CreateSession, which retains the row and wraps the failing
	// startup as a committed mutation.
	_, _, deliverErr := DeliverPromptWithStatus(DeliverPromptRequest{
		Title:    "committed-socket-deliver",
		RepoPath: repoPath,
		Program:  "claude",
		Prompt:   "run it",
	})
	if deliverErr == nil {
		t.Fatal("expected DeliverPrompt over the control socket to surface the committed auto-create failure")
	}

	// THIS is the guarantee the bug broke: the committed marker must survive the
	// real transport so apiclient.IsMutationCommitted(err) classifies it as
	// durable (the recorded row exists) rather than a clean, freely-retryable
	// failure. The rule apiclient.IsMutationCommitted delegates to is
	// apiproto.IsMutationCommitted.
	if !apiproto.IsMutationCommitted(deliverErr) {
		t.Fatalf("control-socket DeliverPrompt error = %T %v, want a committed-mutation marker "+
			"apiclient.IsMutationCommitted would classify (the recorded-for-inspection row "+
			"is durable, so a re-fire must not treat it as a clean failure)", deliverErr, deliverErr)
	}
	if !strings.Contains(deliverErr.Error(), "recorded for inspection") {
		t.Fatalf("committed warning must carry the recorded-for-inspection text, got: %v", deliverErr)
	}

	// The durable recorded-for-inspection row must survive so a re-fire that
	// branches on the committed marker can address it rather than provisioning a
	// duplicate against a title this record still owns.
	rec := recordFor(t, repoID, "committed-socket-deliver")
	if rec == nil || !rec.StartupStateUnknown {
		t.Fatalf("the retained recorded-for-inspection row must survive the committed auto-create, got %+v", rec)
	}
}

// TestDeliverPrompt_RealControlSocket_HappyPathStartsAndNotCommitted pins the
// regression: a normal auto-create (session started) over the real control
// socket returns status "started" with no error and no committed marker. The
// envelope change must not misclassify the happy path, and the started session
// must be persisted exactly as before.
func TestDeliverPrompt_RealControlSocket_HappyPathStartsAndNotCommitted(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installInstantBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	prevLaunch := launchDaemonProcessFn
	launchDaemonProcessFn = func() error { return nil }
	t.Cleanup(func() { launchDaemonProcessFn = prevLaunch })

	closeServer, err := startControlServer(manager, nil, nil, nil)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	t.Cleanup(func() { _ = closeServer() })

	status, _, deliverErr := DeliverPromptWithStatus(DeliverPromptRequest{
		Title:    "happy-deliver",
		RepoPath: repoPath,
		Program:  "claude",
		Prompt:   "run it",
	})
	if deliverErr != nil {
		t.Fatalf("happy-path DeliverPrompt over the control socket: %v", deliverErr)
	}
	if status != "started" {
		t.Fatalf("status = %q, want started", status)
	}
	rec := recordFor(t, repo.ID, "happy-deliver")
	if rec == nil {
		t.Fatal("the started session must be persisted")
	}
}

// TestDeliverPrompt_RealControlSocket_PreFlightFailureNotCommitted guards the
// other side of #3357: a pre-flight failure that delivers nothing must still
// surface as a plain, non-committed error over the control socket. The envelope
// change must not misclassify a clean failure (the not-attempted empty-prompt
// branch) as a committed mutation, which would let a genuine no-op retry claim a
// durable row that does not exist.
func TestDeliverPrompt_RealControlSocket_PreFlightFailureNotCommitted(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installInstantBackend(t)
	repoPath := setupControlRepo(t)
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	prevLaunch := launchDaemonProcessFn
	launchDaemonProcessFn = func() error { return nil }
	t.Cleanup(func() { launchDaemonProcessFn = prevLaunch })

	closeServer, err := startControlServer(manager, nil, nil, nil)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	t.Cleanup(func() { _ = closeServer() })

	_, _, deliverErr := DeliverPromptWithStatus(DeliverPromptRequest{
		Title:    "preflight-deliver",
		RepoPath: repoPath,
		Program:  "claude",
		Prompt:   "", // pre-flight: prompt is required, nothing is delivered
	})
	if deliverErr == nil {
		t.Fatal("expected a pre-flight failure for an empty prompt")
	}
	if apiproto.IsMutationCommitted(deliverErr) {
		t.Fatalf("a pre-flight not-attempted failure must NOT be classified committed, got: %v", deliverErr)
	}
}

// TestDeliverTaskPromptOutcome_CommittedAutoCreate_PreservesMarker pins the
// deliverTaskPromptOutcome half of #3357: once the committed marker survives
// the control socket, the daemon's own re-fire path must branch on it rather
// than wrap it as a generic delivery failure. A committed auto-create is a
// durable mutation, so the marker must reach RunTask's caller (the scheduler /
// af tasks trigger) verbatim for isMutationCommitted to classify it — instead
// of being prefixed with "failed to deliver prompt to target session", which
// reads as a clean, freely-retryable delivery failure.
func TestDeliverTaskPromptOutcome_CommittedAutoCreate_PreservesMarker(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupTaskRepo(t)

	committedErr := &mutationCommittedError{err: fmt.Errorf(
		"failed to auto-create target session %q: failed to start instance %q, "+
			"and its startup outcome could not be determined safely, so its workspace "+
			"was left in place; the session is recorded for inspection and no "+
			"automatic cleanup will run: %w",
		"captain", "captain", session.ErrPaneMayBeLive)}
	origDeliver := deliverPromptForTask
	deliverPromptForTask = func(req DeliverPromptRequest) (taskPromptDeliveryResult, error) {
		return taskPromptDeliveryResult{}, committedErr
	}
	t.Cleanup(func() { deliverPromptForTask = origDeliver })

	tsk := &task.Task{
		ID:            "ffff0060",
		Name:          "gh-issues",
		Prompt:        "triage",
		CronExpr:      "0 3 * * *",
		TargetSession: "captain",
		ProjectPath:   repoPath,
		Program:       "claude",
		Enabled:       true,
	}
	_, _, err := deliverTaskPromptOutcome(tsk, tsk.Prompt, true)
	if err == nil {
		t.Fatal("expected deliverTaskPromptOutcome to surface the committed auto-create error")
	}
	if !isMutationCommitted(err) {
		t.Fatalf("deliverTaskPromptOutcome error = %T %v, want the committed marker preserved "+
			"(not wrapped away as a generic delivery failure)", err, err)
	}
	if !strings.Contains(err.Error(), "recorded for inspection") {
		t.Fatalf("committed warning must survive verbatim, got: %v", err)
	}
	// The committed branch must NOT prefix the error with the generic
	// delivery-failure wording, which reads as a clean, retryable failure.
	if strings.Contains(err.Error(), "failed to deliver prompt to target session") {
		t.Fatalf("committed auto-create must not be wrapped as a generic delivery failure, got: %v", err)
	}
}

// TestDeliverPrompt_RealControlSocket_CommittedUnknownCleanup covers the
// :241/:341 committed branches of Manager.CreateSession (Evidence §9 of the
// report): startup failed with a KNOWN cause, but the cleanup's outcome is
// unknown, so the tombstoned row is retained for the poll's cleanup retry —
// durable state with auto-clearing semantics distinct from the :305
// unknown-startup branch the socket test above pins. Uses unsafeKillBackend
// (Start fails outright, Kill reports ErrPaneMayBeLive) to drive the same
// committed-producing CreateSession from DeliverPrompt's absent-target branch,
// confirming the fix is not branch-specific: the marker survives the gob round
// trip and the tombstoned record survives for the asynchronous reap.
func TestDeliverPrompt_RealControlSocket_CommittedUnknownCleanup(t *testing.T) {
	backend := unsafeKillBackend{readyFakeBackend: readyFakeBackend{session.NewFakeBackend()}}
	manager, repoID, repoPath := newDeliverCommittedFixture(t, backend)

	prevLaunch := launchDaemonProcessFn
	launchDaemonProcessFn = func() error { return nil }
	t.Cleanup(func() { launchDaemonProcessFn = prevLaunch })

	closeServer, err := startControlServer(manager, nil, nil, nil)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	t.Cleanup(func() { _ = closeServer() })

	_, _, deliverErr := DeliverPromptWithStatus(DeliverPromptRequest{
		Title:    "committed-cleanup-deliver",
		RepoPath: repoPath,
		Program:  "claude",
		Prompt:   "run it",
	})
	if deliverErr == nil {
		t.Fatal("expected DeliverPrompt over the control socket to surface the committed auto-create failure")
	}
	if !apiproto.IsMutationCommitted(deliverErr) {
		t.Fatalf("control-socket DeliverPrompt error = %T %v, want a committed-mutation marker "+
			"(the :241/:341 cleanup-pending branch is a durable mutation, so a re-fire must "+
			"not treat it as a clean failure)", deliverErr, deliverErr)
	}
	// The committed warning must name the retained record + the daemon's
	// automatic cleanup retry — the :241/:341 branch's own wording (distinct
	// from the :305 "recorded for inspection...no automatic cleanup" branch
	// the unknown-startup socket test pins, and from the kill path's
	// "retried automatically").
	if !strings.Contains(deliverErr.Error(), "keep retrying the cleanup") {
		t.Fatalf("committed warning must carry the retained-record/cleanup-retry text, got: %v", deliverErr)
	}

	// The tombstoned record must survive for the asynchronous reap — the
	// :241/:341 auto-clearing contract. A re-fire that branches on the
	// committed marker addresses this row rather than provisioning a duplicate.
	rec := recordFor(t, repoID, "committed-cleanup-deliver")
	if rec == nil || !rec.UserKilled {
		t.Fatalf("the retained tombstoned record must survive the committed auto-create, got %+v", rec)
	}
}

// TestTriggerTask_RealControlSocket_CommittedOutcomeReachesCLIClassifier pins
// the OUTER RPC boundary of #3357 the DeliverPrompt socket test above leaves
// open: `af tasks trigger` lands on controlServer.TriggerTask -> RunTask -> the
// deliver path, whose absent-target auto-create can return a
// *mutationCommittedError. Before the fix the handler returned that error
// directly, so net/rpc sent no TriggerTaskResponse body and callDaemon could
// only match the legacy task-CRUD prefixes — which a trigger's deliver outcome
// does not carry — leaving the CLI with a plain rpc.ServerError it would treat
// as a clean, freely-retryable failure against a durable recorded row.
// TriggerTask now records the committed outcome in its response envelope
// (mirroring DeliverPrompt), so callDaemon reconstitutes the marker and the
// CLI's apiclient.IsMutationCommitted classifies it.
func TestTriggerTask_RealControlSocket_CommittedOutcomeReachesCLIClassifier(t *testing.T) {
	backend := &unknownStartBackend{readyFakeBackend: readyFakeBackend{session.NewFakeBackend()}}
	manager, repoID, repoPath := newDeliverCommittedFixture(t, backend)

	// A targeted cron task whose TargetSession does not exist takes the
	// absent-target branch through DeliverPrompt -> createMissingPromptTarget ->
	// Manager.CreateSession — the committed-producing manager method.
	const target = "committed-trigger-target"
	if err := task.AddTask(task.Task{
		ID:            "ffff0050",
		Name:          "trigger-committed",
		Prompt:        "run it",
		CronExpr:      "0 3 * * *",
		TargetSession: target,
		ProjectPath:   repoPath,
		Program:       "claude",
		Enabled:       true,
		CreatedAt:     time.Now(),
	}); err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	prevLaunch := launchDaemonProcessFn
	launchDaemonProcessFn = func() error { return nil }
	t.Cleanup(func() { launchDaemonProcessFn = prevLaunch })

	closeServer, err := startControlServer(manager, nil, nil, nil)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	t.Cleanup(func() { _ = closeServer() })

	// The public TriggerTask goes through callDaemon -> the real gob control
	// socket -> TriggerTask -> RunTask -> the deliver path -> a committed
	// auto-create, which the handler must record in the envelope rather than
	// return directly.
	triggerErr := TriggerTask("ffff0050", task.ProjectExpectation{})
	if triggerErr == nil {
		t.Fatal("expected TriggerTask over the control socket to surface the committed auto-create failure")
	}

	// THIS is the guarantee the bug broke: the committed marker must survive the
	// real transport so apiclient.IsMutationCommitted(err) classifies it as
	// durable (the recorded row exists) rather than a clean, freely-retryable
	// failure.
	if !apiproto.IsMutationCommitted(triggerErr) {
		t.Fatalf("control-socket TriggerTask error = %T %v, want a committed-mutation marker "+
			"apiclient.IsMutationCommitted would classify (the recorded-for-inspection row "+
			"is durable, so a re-fire must not treat it as a clean failure)", triggerErr, triggerErr)
	}
	if !strings.Contains(triggerErr.Error(), "recorded for inspection") {
		t.Fatalf("committed warning must carry the recorded-for-inspection text, got: %v", triggerErr)
	}

	// The durable recorded-for-inspection row must survive so a re-fire that
	// branches on the committed marker can address it rather than provisioning a
	// duplicate against a title this record still owns.
	rec := recordFor(t, repoID, target)
	if rec == nil || !rec.StartupStateUnknown {
		t.Fatalf("the retained recorded-for-inspection row must survive the committed auto-create, got %+v", rec)
	}
}

// TestTriggerTask_RealControlSocket_HappyPathStartsAndNotCommitted pins the
// TriggerTask regression side: a normal auto-create (session started) over the
// real control socket returns OK with no error and no committed marker, so the
// envelope change does not misclassify the happy path.
func TestTriggerTask_RealControlSocket_HappyPathStartsAndNotCommitted(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installInstantBackend(t)
	repoPath := setupControlRepo(t)
	if err := task.AddTask(task.Task{
		ID:            "ffff0051",
		Name:          "trigger-happy",
		Prompt:        "run it",
		CronExpr:      "0 3 * * *",
		TargetSession: "happy-trigger-target",
		ProjectPath:   repoPath,
		Program:       "claude",
		Enabled:       true,
		CreatedAt:     time.Now(),
	}); err != nil {
		t.Fatalf("AddTask: %v", err)
	}

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	prevLaunch := launchDaemonProcessFn
	launchDaemonProcessFn = func() error { return nil }
	t.Cleanup(func() { launchDaemonProcessFn = prevLaunch })

	closeServer, err := startControlServer(manager, nil, nil, nil)
	if err != nil {
		t.Fatalf("startControlServer: %v", err)
	}
	t.Cleanup(func() { _ = closeServer() })

	if err := TriggerTask("ffff0051", task.ProjectExpectation{}); err != nil {
		t.Fatalf("happy-path TriggerTask over the control socket: %v", err)
	}
}
