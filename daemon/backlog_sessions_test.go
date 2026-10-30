package daemon

import (
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/session"
)

func TestContinueSetsRunningBeforeSend(t *testing.T) {
	m, repoID, inst := startedItem(t)
	inst.DisarmBacklogRun()
	m.backlogRunEnded(repoID, inst.ID)

	var status backlog.Status
	var armed bool
	prev := sendBacklogPrompt
	sendBacklogPrompt = func(*Manager, SendPromptRequest) error {
		status, armed = loadItem(t, repoID, 1).Status, inst.BacklogRunActive()
		return nil
	}
	t.Cleanup(func() { sendBacklogPrompt = prev })

	if _, err := m.StartBacklogItem(backlog.StartRequest{RepoID: repoID, ID: 1}, backlog.StartedByUser); err != nil {
		t.Fatal(err)
	}
	if status != backlog.StatusRunning || !armed {
		t.Fatalf("at send: status %s armed %v", status, armed)
	}
}

func TestContinueRefusedWhileInFlight(t *testing.T) {
	m, repoID, inst := startedItem(t)
	inst.DisarmBacklogRun()
	m.backlogRunEnded(repoID, inst.ID)
	if !m.backlog.beginContinue(repoID, 1) {
		t.Fatal("claim")
	}
	_, err := m.StartBacklogItem(backlog.StartRequest{RepoID: repoID, ID: 1}, backlog.StartedByUser)
	if err == nil || !strings.Contains(err.Error(), "already starting") {
		t.Fatalf("got %v", err)
	}
	if loadItem(t, repoID, 1).Status != backlog.StatusReview {
		t.Fatal("refused continue changed the item")
	}
}

func TestSlotsCountOnlyLatestRun(t *testing.T) {
	m, repoID, inst := startedItem(t)
	if !inst.BacklogRunActive() || m.backlogSlotsUsed(repoID) != 1 {
		t.Fatalf("precondition: one slot, got %d", m.backlogSlotsUsed(repoID))
	}
	if _, err := backlog.Update(repoID, func(b *backlog.Backlog) error {
		it := b.Item(1)
		it.Runs = append(it.Runs, backlog.RunRef{State: backlog.RunFailed, StartedAt: time.Now()})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := m.backlogSlotsUsed(repoID); got != 0 {
		t.Fatalf("an older run's session holds a slot: %d", got)
	}
}

func idleItemSession(t *testing.T, inst *session.Instance) {
	t.Helper()
	inst.EndBacklogRunForTest()
	if err := inst.Transition(session.ObserveLiveness(session.LiveReady)); err != nil {
		t.Fatal(err)
	}
}

func stubBacklogArchive(t *testing.T) *[]string {
	t.Helper()
	var got []string
	prev := archiveSessionForLifecycle
	archiveSessionForLifecycle = func(_ *Manager, req ArchiveSessionRequest, guard sessionTeardownGuard) error {
		got = append(got, req.ID)
		return nil
	}
	t.Cleanup(func() { archiveSessionForLifecycle = prev })
	return &got
}

func setOnDoneArchive(t *testing.T, m *Manager, repoID string) {
	t.Helper()
	archive := backlog.OnDoneArchive
	if _, err := m.backlogService().SetSettings(backlog.SetSettingsRequest{RepoID: repoID, OnDone: &archive}); err != nil {
		t.Fatal(err)
	}
}

func TestReviewNeverArchives(t *testing.T) {
	m, repoID, inst := startedItem(t)
	setOnDoneArchive(t, m, repoID)
	archived := stubBacklogArchive(t)
	idleItemSession(t, inst)
	m.backlogRunEnded(repoID, inst.ID)
	m.backgroundMutationWG.Wait()
	if loadItem(t, repoID, 1).Status != backlog.StatusReview || len(*archived) != 0 {
		t.Fatalf("archived %v", *archived)
	}
}

func TestUserDoneArchivesIdleSession(t *testing.T) {
	m, repoID, inst := startedItem(t)
	setOnDoneArchive(t, m, repoID)
	archived := stubBacklogArchive(t)
	idleItemSession(t, inst)
	m.backlogRunEnded(repoID, inst.ID)

	out, err := m.backlogService().SetItemStatus(backlog.SetItemStatusRequest{RepoID: repoID, ID: 1, Status: backlog.StatusDone})
	if err != nil {
		t.Fatal(err)
	}
	m.onBacklogItemDone(repoID, out.Item)
	m.backgroundMutationWG.Wait()
	if len(*archived) != 1 || (*archived)[0] != inst.ID {
		t.Fatalf("archived %v", *archived)
	}
}

func TestUserDoneWaitsForActiveRun(t *testing.T) {
	m, repoID, inst := startedItem(t)
	setOnDoneArchive(t, m, repoID)
	archived := stubBacklogArchive(t)
	out, err := m.backlogService().SetItemStatus(backlog.SetItemStatusRequest{RepoID: repoID, ID: 1, Status: backlog.StatusDone})
	if err != nil {
		t.Fatal(err)
	}
	m.onBacklogItemDone(repoID, out.Item)
	m.backgroundMutationWG.Wait()
	if len(*archived) != 0 {
		t.Fatal("archived mid-run")
	}
	idleItemSession(t, inst)
	m.backlogRunEnded(repoID, inst.ID)
	m.backgroundMutationWG.Wait()
	if len(*archived) != 1 {
		t.Fatalf("idle edge after done: archived %v", *archived)
	}
}

func TestDroppedNeverArchives(t *testing.T) {
	m, repoID, inst := startedItem(t)
	setOnDoneArchive(t, m, repoID)
	archived := stubBacklogArchive(t)
	idleItemSession(t, inst)
	out, err := m.backlogService().SetItemStatus(backlog.SetItemStatusRequest{RepoID: repoID, ID: 1, Status: backlog.StatusDropped})
	if err != nil {
		t.Fatal(err)
	}
	m.onBacklogItemDone(repoID, out.Item)
	m.backlogRunEnded(repoID, inst.ID)
	m.backgroundMutationWG.Wait()
	if len(*archived) != 0 {
		t.Fatalf("archived %v", *archived)
	}
}

func TestBacklogArchiveGuard(t *testing.T) {
	_, _, inst := startedItem(t)
	var fenced *session.Instance
	guard := backlogArchiveGuard(inst.ID, func(cur *session.Instance) { fenced = cur })
	if guard(inst) == nil {
		t.Fatal("guard passed mid-run")
	}
	idleItemSession(t, inst)
	if err := guard(inst); err != nil {
		t.Fatalf("idle session refused: %v", err)
	}
	// The fence is shut while the archive runs (Codex P2 on #6294): input that
	// arrives after the check is refused instead of starting work the archive
	// interrupts. The caller reopens it when the archive is over.
	if fenced != inst {
		t.Fatal("the guard did not report the instance it fenced")
	}
	if err := inst.NoteAdoptionDelivery(); err == nil {
		t.Fatal("a delivery was accepted after the guard passed")
	}
	inst.ReopenAdoptionFence()
	if backlogArchiveGuard("other", func(*session.Instance) {})(inst) == nil {
		t.Fatal("guard passed for another session")
	}
	inst.ArmBacklogRun(time.Now())
	if guard(inst) == nil {
		t.Fatal("guard passed with a new run")
	}
	if err := inst.NoteAdoptionDelivery(); err != nil {
		t.Fatalf("a refusal must reopen the fence: %v", err)
	}
}

func TestRecoverySkipsCurrentDaemonStarts(t *testing.T) {
	m, repoID, inst := startedItem(t)
	prevStart := daemonProcessStart
	t.Cleanup(func() { daemonProcessStart = prevStart })
	daemonProcessStart = inst.CreatedAt.Add(-time.Hour)
	if _, err := backlog.Update(repoID, func(b *backlog.Backlog) error {
		r := &b.Item(1).Runs[0]
		r.State, r.SessionID, r.Title, r.StartedAt = backlog.RunStarting, "", "", time.Now()
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m.recoverBacklog(repoID, nil)
	if r := loadItem(t, repoID, 1).Runs[0]; r.State != backlog.RunStarting {
		t.Fatalf("a start of this daemon was recovered: %+v", r)
	}
}

// A run a restart left starting, whose session persisted idle with its marker
// already cleared, goes to review in the same recovery pass that repairs the
// session link (Codex P2 on #6294).
func TestRecoveryEndsAnIdleRunAfterRepairingItsSessionLink(t *testing.T) {
	m, repoID, inst := startedItem(t)
	inst.DisarmBacklogRun()
	// The session went idle while no daemon watched: recovery counts only a live,
	// Ready session whose marker is clear.
	if err := inst.Transition(session.ObserveLiveness(session.LiveReady)); err != nil {
		t.Fatal(err)
	}
	prevStart := daemonProcessStart
	t.Cleanup(func() { daemonProcessStart = prevStart })
	daemonProcessStart = inst.CreatedAt.Add(time.Hour)
	if _, err := backlog.Update(repoID, func(b *backlog.Backlog) error {
		r := &b.Item(1).Runs[0]
		r.State, r.SessionID, r.Title, r.StartedAt = backlog.RunStarting, "", "", inst.CreatedAt.Add(-time.Second)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !m.recoverBacklog(repoID, nil) {
		t.Fatal("recovery did not settle the project")
	}
	it := loadItem(t, repoID, 1)
	if it.Runs[0].State != backlog.RunStarted || it.Runs[0].SessionID != inst.ID {
		t.Fatalf("the run was not linked: %+v", it.Runs[0])
	}
	if it.Status != backlog.StatusReview {
		t.Fatalf("status = %s, want review: an idle run was left running", it.Status)
	}
}

func TestRecoverySkipsSkippedRepos(t *testing.T) {
	m, repoID, inst := startedItem(t)
	prevStart := daemonProcessStart
	t.Cleanup(func() { daemonProcessStart = prevStart })
	daemonProcessStart = time.Now().Add(time.Hour)
	if _, err := backlog.Update(repoID, func(b *backlog.Backlog) error {
		r := &b.Item(1).Runs[0]
		r.State, r.SessionID, r.Title = backlog.RunStarting, "", ""
		r.StartedAt = inst.CreatedAt.Add(-time.Second)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.skippedRepos = append(m.skippedRepos, SkippedRepo{RepoID: repoID})
	m.mu.Unlock()
	if !m.backlog.beginRecovery() {
		t.Fatal("begin")
	}
	m.recoverBacklogStarts(nil)
	if r := loadItem(t, repoID, 1).Runs[0]; r.State != backlog.RunStarting || m.backlog.isRecovered(repoID) {
		t.Fatalf("skipped repo was recovered: %+v", r)
	}
	if !m.backlog.beginRecovery() {
		t.Fatal("a skipped repo must leave recovery pending")
	}
}

func TestDeferredArchive(t *testing.T) {
	var s backlogState
	s.deferArchive("a")
	if !s.takeDeferredArchive("a") || s.takeDeferredArchive("a") {
		t.Fatal("deferred archive is taken once")
	}
}

// A completion queued for run N must not finish a continue accepted after it
// (Codex P2 on #6294): the continue re-arms the same session, so the stale
// callback sees a newer generation and does nothing.
func TestStaleCompletionDoesNotFinishALaterContinue(t *testing.T) {
	m, repoID, inst := startedItem(t)
	gen := inst.BacklogRunGeneration()
	inst.DisarmBacklogRun() // run N ended on its idle edge; its completion is still queued
	prev := sendBacklogPrompt
	sendBacklogPrompt = func(*Manager, SendPromptRequest) error { return nil }
	t.Cleanup(func() { sendBacklogPrompt = prev })

	if _, err := m.StartBacklogItem(backlog.StartRequest{RepoID: repoID, ID: 1}, backlog.StartedByUser); err != nil {
		t.Fatal(err)
	}
	if inst.BacklogRunGeneration() == gen {
		t.Fatal("the continue did not arm a new run")
	}
	m.backlogRunEndedGen(repoID, inst.ID, gen) // the stale callback fires now

	it := loadItem(t, repoID, 1)
	if it.Status != backlog.StatusRunning {
		t.Fatalf("status = %s: the stale completion finished the new run", it.Status)
	}
	for _, msg := range it.Thread {
		if msg.Text == backlog.RunEndNote {
			t.Fatalf("a stopped-agent note was added for a run that is working: %+v", it.Thread)
		}
	}
}

// An idle edge observed before the session's start record is committed has no
// item to land on: it is held and replayed when the link is recorded, so the item
// is not left running (Codex P2 on #6294).
func TestCompletionBeforeTheStartRecordIsHeldAndReplayed(t *testing.T) {
	m, repoID, inst := startedItem(t)
	gen := inst.BacklogRunGeneration()
	inst.DisarmBacklogRun()
	// Unlink the run, as if finishBacklogStart had not committed yet.
	var run backlog.RunRef
	if _, err := backlog.Update(repoID, func(b *backlog.Backlog) error {
		r := &b.Item(1).Runs[0]
		run = *r
		r.State, r.SessionID, r.Title = backlog.RunStarting, "", ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m.settleRunEnd(repoID, inst.ID, gen, nil)
	if it := loadItem(t, repoID, 1); it.Status != backlog.StatusRunning {
		t.Fatalf("an unlinked session's completion was applied: %s", it.Status)
	}
	// finishBacklogStart commits the link and replays the held edge.
	m.backlog.reserve(repoID)
	m.finishBacklogStart(backlog.ProjectRef{ID: repoID}, 1, run.StartedAt,
		session.InstanceData{ID: inst.ID, Title: inst.Title}, nil, nil)
	if it := loadItem(t, repoID, 1); it.Status != backlog.StatusReview {
		t.Fatalf("status = %s, want review: the held completion was lost", it.Status)
	}
}

// A busy backlog does not drop a completion (Codex P2 on #6294): it is retried.
func TestCompletionIsRetriedWhileTheBacklogIsBusy(t *testing.T) {
	m, repoID, inst := startedItem(t)
	gen := inst.BacklogRunGeneration()
	inst.DisarmBacklogRun()
	prevApply, prevBackoff := applyBacklog, finishStartBackoff
	finishStartBackoff = time.Millisecond
	busy := 3
	applyBacklog = func(svc *backlog.Service, ref backlog.ProjectRef, fn func(*backlog.Backlog) ([]int, error)) (*backlog.Backlog, error) {
		if busy > 0 {
			busy--
			return nil, backlog.ErrBusy
		}
		return prevApply(svc, ref, fn)
	}
	t.Cleanup(func() { applyBacklog, finishStartBackoff = prevApply, prevBackoff })
	m.settleRunEnd(repoID, inst.ID, gen, nil)
	if busy != 0 {
		t.Fatalf("the completion gave up with %d busy answers left", busy)
	}
	if it := loadItem(t, repoID, 1); it.Status != backlog.StatusReview {
		t.Fatalf("status = %s, want review", it.Status)
	}
}

// An item the user marked done during its run, whose session went idle while no
// daemon watched (a crash between the marker clearing and the completion), still
// gets its on_done archive from restart recovery (Codex P2 on #6294).
func TestRecoveryArchivesADoneItemWhoseRunEndedUnobserved(t *testing.T) {
	m, repoID, inst := startedItem(t)
	setOnDoneArchive(t, m, repoID)
	archived := stubBacklogArchive(t)
	if _, err := m.backlogService().SetItemStatus(backlog.SetItemStatusRequest{RepoID: repoID, ID: 1, Status: backlog.StatusDone}); err != nil {
		t.Fatal(err)
	}
	idleItemSession(t, inst) // the marker cleared and the session settled; nobody saw the edge
	if !m.recoverBacklog(repoID, nil) {
		t.Fatal("recovery did not settle the project")
	}
	m.backgroundMutationWG.Wait()
	if len(*archived) != 1 || (*archived)[0] != inst.ID {
		t.Fatalf("archived %v, want the done item's session", *archived)
	}
}

// A session that reports on its item before finishBacklogStart commits the link
// is adopted by the report (Codex P2 on #6294); the late start record is a no-op.
func TestSelfReportBeforeTheStartRecordLinksTheRun(t *testing.T) {
	m, repoID, inst := startedItem(t)
	var run backlog.RunRef
	if _, err := backlog.Update(repoID, func(b *backlog.Backlog) error {
		r := &b.Item(1).Runs[0]
		run = *r
		r.State, r.SessionID, r.Title = backlog.RunStarting, "", ""
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m.adoptStartingRun(repoID, "", inst.ID)
	it := loadItem(t, repoID, 1)
	if it.Runs[0].State != backlog.RunStarted || it.Runs[0].SessionID != inst.ID {
		t.Fatalf("the report did not link the run: %+v", it.Runs[0])
	}
	m.backlog.reserve(repoID)
	m.finishBacklogStart(backlog.ProjectRef{ID: repoID}, 1, run.StartedAt,
		session.InstanceData{ID: inst.ID, Title: it.Runs[0].Title}, nil, nil)
	if loadItem(t, repoID, 1).Runs[0].SessionID != inst.ID {
		t.Fatal("the late start record changed the linked run")
	}
}

// The delivery counter is memory-only; after a restart the persisted prompt
// attempt is what shows a prompt arrived after the run ended (Codex P2 on #6294).
func TestBacklogArchiveCheckUsesDurablePostRunDeliveryEvidence(t *testing.T) {
	_, _, inst := startedItem(t)
	idleItemSession(t, inst)
	after := inst.BacklogRunEndedAt().Add(time.Second)
	if err := backlogArchiveCheck(inst.ID, inst, inst.AdoptionDeliveriesAtRunEnd()); err != nil {
		t.Fatalf("an idle session with no later prompt was refused: %v", err)
	}
	inst.RecordPromptAttempt(session.PromptNotDelivered, after)
	if err := backlogArchiveCheck(inst.ID, inst, inst.AdoptionDeliveriesAtRunEnd()); err != nil {
		t.Fatalf("a proven non-delivery vetoed the archive: %v", err)
	}
	inst.RecordPromptAttempt(session.PromptDelivered, after.Add(time.Second))
	if err := backlogArchiveCheck(inst.ID, inst, inst.AdoptionDeliveriesAtRunEnd()); err == nil {
		t.Fatal("a prompt sent after the run ended did not veto the archive (counter is zero after a restart)")
	}
}
