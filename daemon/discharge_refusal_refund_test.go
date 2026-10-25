package daemon

// A task delivery refused because the adoption discharge could not be persisted
// must be classified not-attempted (#4984).
//
// When a task-origin delivery targets a session still carrying an
// owedOnComplete marker, NoteAdoptionDelivery runs the durable discharge FIRST
// and refuses the PTY write when it fails (#4738). That refusal is a definitive
// not-attempted outcome — no byte was written — but it came back as an ordinary
// send error: SendPromptWithStatus wrapped it "failed to send prompt" like any
// ambiguous socket failure ("never sent" vs "sent, reply lost"), so the watch
// path's isNotAttemptedErr never matched and every queued retry whose discharge
// persist failed burned a fresh per-minute rate slot while delivering nothing.
//
// The fix marks the refusal with session.ErrDischargeRefusedDelivery at both
// NoteAdoptionDelivery return points — the caller's own notify failure AND the
// shared in-flight discharge future — and SendPromptWithStatus recognises it
// and re-tags it notAttempted, which guarantees the wire marker that survives
// the control-socket flattening back to the watcher. Nothing else changes: a
// send error that may have followed a successful write still stays charged.
//
// The tests below deliberately assert only the CLASSIFICATION surface that
// exists on unfixed master — errNotAttempted, isNotAttemptedErr, the wire
// marker, the rate-slot count — so this file compiles standalone against
// master and is red there for the behavioral reason the issue reports.

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// failDischargePersist injects a persist failure targeted at the discharge
// write for title only: the filing write carries PendingOnComplete != nil and
// passes; the seed write carries no TaskID and passes; the discharge write is
// the task row with the marker cleared. Mirrors the post-marker race tests.
func failDischargePersist(t *testing.T, title string) {
	t.Helper()
	prev := testHookPersistInstanceData
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		if data.Title == title && data.TaskID != "" && data.PendingOnComplete == nil {
			return errors.New("injected discharge persist failure (disk error)")
		}
		return nil
	}
	t.Cleanup(func() { testHookPersistInstanceData = prev })
}

// registerOwedSession builds the #4984 fixture: a task-spawned session whose
// run ended idle, with the durable on_complete marker filed and the discharge
// notify installed — exactly the completion edge's durable state, without
// launching the lifecycle worker that is irrelevant to delivery accounting.
func registerOwedSession(t *testing.T, manager *Manager, repoID, repoPath, title string, backend session.Backend) *session.Instance {
	t.Helper()
	inst := registerTaskSpawnedSession(t, manager, repoID, repoPath, title, "task-kill")
	if backend != nil {
		inst.SetBackend(backend)
	}
	endRunOnIdleEdge(t, inst)
	manager.fileOwedTaskLifecycle(repoID, inst)
	require.NotNil(t, inst.OwedOnComplete(), "precondition: the durable marker is filed")
	return inst
}

// TestSendPrompt_DischargeRefusalIsNotAttempted pins the #4984 classification
// at the manager boundary: the discharge refusal crosses no socket on its way
// out of NoteAdoptionDelivery, so the sentinel reaches SendPromptWithStatus
// typed, is re-tagged notAttempted, and its wire text keeps the marker that
// re-mints the classification after net/rpc flattens it on the control socket.
func TestSendPrompt_DischargeRefusalIsNotAttempted(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installInstantBackend(t)
	testguard.IsolateTmux(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	rec := &promptRecorder{}
	inst := registerOwedSession(t, manager, repo.ID, repoPath, "nightly",
		recordingBackend{readyFakeBackend{session.NewFakeBackend()}, rec})
	stubTaskLifecycle(t, "task-kill", task.OnCompleteKill)
	failDischargePersist(t, "nightly")

	_, err = manager.SendPromptWithStatus(SendPromptRequest{
		Title: "nightly", RepoID: repo.ID, Prompt: "adopted", TaskOrigin: true,
	})
	require.Error(t, err, "the durable discharge did not land — the delivery must be refused")
	assert.Contains(t, err.Error(), "discharge for session",
		"precondition: this is the discharge refusal, not another send failure")
	assert.Empty(t, rec.snapshot(),
		"precondition: the refusal provably precedes the write — no byte reached the backend")
	require.NotNil(t, inst.OwedOnComplete(),
		"the in-memory marker is restored so a re-attempt re-runs the durable clear")

	// THE FIX: a refusal that provably sent nothing is classified not-attempted
	// in-process, and its text carries the wire marker that re-mints the
	// classification after net/rpc flattens the type on the control socket —
	// the hop the watch path actually takes (taskrun.go's re-mint).
	assert.True(t, errors.Is(err, errNotAttempted),
		"the discharge refusal must be tagged notAttempted so the watch path refunds the rate slot")
	flattened := fmt.Errorf("%s", err.Error())
	require.False(t, errors.Is(flattened, errNotAttempted),
		"precondition: flattening destroys the type — only the marker text can carry the classification")
	assert.True(t, isNotAttemptedErr(flattened),
		"the flattened refusal must carry %q so the watcher can re-mint and refund (#4984)", notDeliveredMarker)
}

// TestSendPrompt_ParkedOnFailedStandDownDischargeIsNotAttempted covers the
// OTHER refusal return point: a SendPrompt that arrives while a stand-down's
// durable clear is in flight parks on the shared discharge future and is
// refused with ITS result — a hand-off through discharge.err, not the notify's
// own return. The stand-down path holds no op-lock, so this is a genuinely
// reachable ordering for a task delivery, and it is the same
// provably-before-write refusal: it must classify not-attempted identically.
func TestSendPrompt_ParkedOnFailedStandDownDischargeIsNotAttempted(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installInstantBackend(t)
	testguard.IsolateTmux(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	rec := &promptRecorder{}
	inst := registerOwedSession(t, manager, repo.ID, repoPath, "nightly",
		recordingBackend{readyFakeBackend{session.NewFakeBackend()}, rec})
	stubTaskLifecycle(t, "task-kill", task.OnCompleteKill)

	// Gate the stand-down's discharge persist inside the hook so the SendPrompt
	// below arrives while it is still in flight: by then the stand-down has
	// cleared the in-memory marker and installed the discharge future, so the
	// send parks on it instead of writing past a durable marker the stand-down
	// is about to fail to clear.
	persistStarted := make(chan struct{})
	persistBlocked := make(chan error, 1)
	prev := testHookPersistInstanceData
	testHookPersistInstanceData = func(_ string, data session.InstanceData) error {
		if data.Title == "nightly" && data.TaskID != "" && data.PendingOnComplete == nil {
			select {
			case <-persistStarted:
				return errors.New("injected discharge persist failure (disk error)")
			default:
				close(persistStarted)
				return <-persistBlocked
			}
		}
		return nil
	}
	t.Cleanup(func() { testHookPersistInstanceData = prev })

	standDone := make(chan struct{})
	go func() {
		manager.dischargeOwedTaskLifecycle(repo.ID, inst.ID, inst.Title)
		close(standDone)
	}()
	<-persistStarted

	sendErr := make(chan error, 1)
	go func() {
		_, serr := manager.SendPromptWithStatus(SendPromptRequest{
			Title: "nightly", RepoID: repo.ID, Prompt: "adopted", TaskOrigin: true,
		})
		sendErr <- serr
	}()

	// The send bumps the delivery count and parks on the stand-down's in-flight
	// discharge future rather than proceeding to the PTY write.
	require.Eventually(t, func() bool {
		return inst.AdoptionDeliveries() == 1
	}, 5*time.Second, 25*time.Millisecond,
		"the send must reach NoteAdoptionDelivery and park on the stand-down's in-flight discharge")

	// The stand-down's durable clear fails: the shared discharge future closes
	// with the persist error, the in-memory marker is restored, and the parked
	// send is refused with the same verdict — no byte was written for it either.
	persistBlocked <- errors.New("injected discharge persist failure (disk error)")
	<-standDone

	err = <-sendErr
	require.Error(t, err,
		"a send parked on a failed stand-down discharge shares the refusal — its PTY write never happens")
	assert.Contains(t, err.Error(), "discharge",
		"precondition: the refusal is the shared discharge failure, not another send failure")
	assert.Empty(t, rec.snapshot(), "the refused send wrote no bytes")
	require.NotNil(t, inst.OwedOnComplete(),
		"the in-memory marker is restored so a re-attempt re-runs the durable clear")

	assert.True(t, errors.Is(err, errNotAttempted),
		"a send refused through the shared discharge future must also classify not-attempted")
	flattened := fmt.Errorf("%s", err.Error())
	assert.True(t, isNotAttemptedErr(flattened),
		"the flattened shared-discharge refusal must carry %q so the watcher refunds (#4984)", notDeliveredMarker)
}

// TestSendPrompt_SendFailureAfterDischargeStaysCharged pins the other half of
// the rule on this exact code path: once the discharge LANDS and the send
// itself fails, the error is the ordinary ambiguous kind — the prompt may have
// reached the pane before the reply was lost — and the attempt stays charged.
// Only the pre-write refusal is re-tagged; this fails if SendPromptWithStatus
// broadens the classification past it.
func TestSendPrompt_SendFailureAfterDischargeStaysCharged(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installInstantBackend(t)
	testguard.IsolateTmux(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	backend := &failingPromptBackend{readyFakeBackend: readyFakeBackend{session.NewFakeBackend()}}
	inst := registerOwedSession(t, manager, repo.ID, repoPath, "nightly", backend)
	stubTaskLifecycle(t, "task-kill", task.OnCompleteKill)
	// No injected failure: the discharge persist lands, and the send itself is
	// what fails — the ambiguous case that must stay charged.

	_, err = manager.SendPromptWithStatus(SendPromptRequest{
		Title: "nightly", RepoID: repo.ID, Prompt: "adopted", TaskOrigin: true,
	})
	require.Error(t, err)
	assert.Equal(t, 1, backend.sent, "precondition: the discharge landed and the send was attempted")
	assert.Nil(t, inst.OwedOnComplete(), "the discharge landed — the marker is cleared")
	assert.False(t, errors.Is(err, errNotAttempted),
		"a send that may have written bytes must stay charged")
	assert.False(t, isNotAttemptedErr(fmt.Errorf("%s", err.Error())),
		"an ambiguous send failure must not pick up the wire marker")
}

// TestWatcherHandleEvent_RefundsRateSlotWhenDischargeRefuses is the end-to-end
// #4984 regression at the layer where the slot is actually spent: the real
// deliverWatchEvent → deliverTaskPrompt → deliverPromptWithOutcome path, with
// the manager's refusal reconstituted from TEXT only — exactly what net/rpc
// does on the daemon's own control socket. Before the fix the refusal carried
// no classification across that hop, so every queued retry whose discharge
// persist failed burned a fresh per-minute slot while writing nothing.
func TestWatcherHandleEvent_RefundsRateSlotWhenDischargeRefuses(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installInstantBackend(t)
	testguard.IsolateTmux(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	rec := &promptRecorder{}
	registerOwedSession(t, manager, repo.ID, repoPath, "nightly",
		recordingBackend{readyFakeBackend{session.NewFakeBackend()}, rec})
	stubTaskLifecycle(t, "task-kill", task.OnCompleteKill)
	failDischargePersist(t, "nightly")

	require.NoError(t, task.AddTask(task.Task{
		ID:            "cafe4984",
		Name:          "gh-issues",
		Prompt:        "Triage: {{line}}",
		WatchCmd:      "watch.sh",
		TargetSession: "nightly",
		ProjectPath:   repoPath,
		Enabled:       true,
		CreatedAt:     time.Now(),
	}))

	// The daemon's own control socket, faithfully: the watch path reaches the
	// manager over net/rpc, which rebuilds the refusal from its text alone and
	// destroys any in-process tag on the way back.
	var wire string
	origDeliver := deliverPromptForTask
	deliverPromptForTask = func(req DeliverPromptRequest) (taskPromptDeliveryResult, error) {
		status, deliveryStatus, promptRetained, err := manager.deliverPromptWithOutcome(req)
		if err == nil {
			return taskPromptDeliveryResult{status: status, deliveryStatus: deliveryStatus, promptRetained: promptRetained}, nil
		}
		wire = err.Error()
		return taskPromptDeliveryResult{}, fmt.Errorf("%s", wire)
	}
	t.Cleanup(func() { deliverPromptForTask = origDeliver })

	w := newRateSlotWatcher(t, "cafe4984", deliverWatchEvent)
	close(w.stopCh) // no drainer behind the assertions

	w.handleEvent("new issue #9", &tailBuffer{})

	require.Contains(t, wire, "discharge for session",
		"precondition: the delivery took the discharge-refusal path; wire text: %q", wire)
	assert.Empty(t, rec.snapshot(), "the refused attempt wrote no bytes")
	if got := spentSlots(w); got != 0 {
		t.Fatalf("rate slots spent = %d, want 0.\n\n"+
			"The discharge refusal was not classified not-attempted, so the watcher charged a slot for a "+
			"delivery that wrote nothing — every queued retry of a failing discharge drains the per-minute "+
			"budget (#4984).\nwire text: %q", got, wire)
	}
	assert.Equal(t, 1, w.queue.pendingCount(),
		"a refused delivery is queued for replay — refunding must not drop it")
}

// TestWatcherDrain_RefundsRateSlotWhenDischargeRefuses pins the same refund on
// the replay arm, where the leak actually compounds: the drainer retries for as
// long as storage stays unhappy, so an unrefunded discharge refusal burns one
// slot PER RETRY for the whole outage. The drainer spends through the same
// sup.deliver wiring, so the flattened refusal arrives with the same
// classification — this proves the drain loop's own refund arm honours it.
func TestWatcherDrain_RefundsRateSlotWhenDischargeRefuses(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	installInstantBackend(t)
	testguard.IsolateTmux(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)

	rec := &promptRecorder{}
	registerOwedSession(t, manager, repo.ID, repoPath, "nightly",
		recordingBackend{readyFakeBackend{session.NewFakeBackend()}, rec})
	stubTaskLifecycle(t, "task-kill", task.OnCompleteKill)
	failDischargePersist(t, "nightly")

	require.NoError(t, task.AddTask(task.Task{
		ID:            "cafe4985",
		Name:          "gh-issues",
		Prompt:        "Triage: {{line}}",
		WatchCmd:      "watch.sh",
		TargetSession: "nightly",
		ProjectPath:   repoPath,
		Enabled:       true,
		CreatedAt:     time.Now(),
	}))

	var wire string
	origDeliver := deliverPromptForTask
	deliverPromptForTask = func(req DeliverPromptRequest) (taskPromptDeliveryResult, error) {
		status, deliveryStatus, promptRetained, err := manager.deliverPromptWithOutcome(req)
		if err == nil {
			return taskPromptDeliveryResult{status: status, deliveryStatus: deliveryStatus, promptRetained: promptRetained}, nil
		}
		wire = err.Error()
		return taskPromptDeliveryResult{}, fmt.Errorf("%s", wire)
	}
	t.Cleanup(func() { deliverPromptForTask = origDeliver })

	attempted := make(chan struct{})
	var once sync.Once
	w := newRateSlotWatcher(t, "cafe4985", func(taskID, line string) error {
		err := deliverWatchEvent(taskID, line)
		once.Do(func() { close(attempted) })
		return err
	})
	// Long backoffs: exactly one replay attempt happens before the stop below,
	// so the assertion counts one attempt's slot, not a race with retries.
	w.sup.drainBaseBackoff = 10 * time.Second
	w.sup.drainMaxBackoff = 10 * time.Second
	require.NoError(t, w.queue.enqueue("new issue #9"))

	w.wg.Add(1)
	go w.drainLoop()
	select {
	case <-attempted:
	case <-time.After(10 * time.Second):
		t.Fatal("drainer never attempted the queued event")
	}
	w.stopOnce.Do(func() { close(w.stopCh) })
	w.wg.Wait()

	require.Contains(t, wire, "discharge for session",
		"precondition: the replay took the discharge-refusal path; wire text: %q", wire)
	assert.Empty(t, rec.snapshot(), "the refused replay wrote no bytes")
	if got := spentSlots(w); got != 0 {
		t.Fatalf("rate slots spent after a refused replay = %d, want 0.\n\n"+
			"The drainer charged a slot for a delivery that wrote nothing — an unrefunded retry "+
			"burns the per-minute budget once per replay for the whole storage outage (#4984).\n"+
			"wire text: %q", got, wire)
	}
	assert.Equal(t, 1, w.queue.pendingCount(),
		"a refused replay stays queued for the next attempt")
}
