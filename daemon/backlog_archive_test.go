package daemon

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/session"
)

// askedThen starts item 1, has its agent ask with then, and leaves the session
// idle with its run over.
func askedThen(t *testing.T, then string) (*Manager, string, *session.Instance) {
	t.Helper()
	m, repoID, inst := startedItem(t)
	if _, err := m.backlogService().PostMessage(backlog.PostMessageRequest{RepoID: repoID, ID: 1,
		Kind: backlog.KindQuestion, Text: "Which db?", SessionID: inst.ID, Then: then}); err != nil {
		t.Fatal(err)
	}
	idleItemSession(t, inst)
	return m, repoID, inst
}

// stubGuardedArchive stands in for the teardown the way the real one runs: it
// asks the guard about the instance (under the fence, in the real archive) and
// on a pass commits the archive on it. before runs first, inside the archive.
func stubGuardedArchive(t *testing.T, inst *session.Instance, before func()) *[]error {
	t.Helper()
	var got []error
	prev := archiveSessionForLifecycle
	archiveSessionForLifecycle = func(_ *Manager, req ArchiveSessionRequest, guard sessionTeardownGuard) error {
		if before != nil {
			before()
		}
		err := guard(inst)
		got = append(got, err)
		if err != nil {
			return err
		}
		if err := inst.Transition(session.BeginArchive()); err != nil {
			return err
		}
		return inst.Transition(session.CommitArchive())
	}
	t.Cleanup(func() { archiveSessionForLifecycle = prev })
	return &got
}

func keptLiveNotes(it backlog.Item) int {
	n := 0
	for _, m := range it.Thread {
		if m.Kind == backlog.KindNote && strings.HasPrefix(m.Text, "Kept the session live: ") {
			n++
		}
	}
	return n
}

// The run ends on a question asked with then archive: af files the archive on
// the session, archives it, and settles the filing; the item still needs you.
func TestAskThenArchiveArchivesTheSessionWhenItsRunEnds(t *testing.T) {
	m, repoID, inst := askedThen(t, backlog.ThenArchive)
	var filed *session.OwedArchiveData
	results := stubGuardedArchive(t, inst, func() { filed = inst.OwedArchive() })
	m.backlogRunEnded(repoID, inst.ID)
	m.backgroundMutationWG.Wait()
	if len(*results) != 1 || (*results)[0] != nil {
		t.Fatalf("archive guard results %v, want one pass", *results)
	}
	if filed == nil || filed.Reason != backlog.ArchiveAsk {
		t.Fatalf("the archive was not filed on the session before it ran: %+v", filed)
	}
	if inst.GetLiveness() != session.LiveArchived || inst.OwedArchive() != nil {
		t.Fatalf("liveness %v, owed %+v; want archived and settled", inst.GetLiveness(), inst.OwedArchive())
	}
	v, err := m.backlogService().Get(backlog.GetRequest{RepoID: repoID})
	if err != nil {
		t.Fatal(err)
	}
	if st := v.States[1].State; st != backlog.StateNeedsYou {
		t.Fatalf("state %s, want needs you", st)
	}
}

// --then wait, and a question from before then existed, keep today's behavior:
// the session stays live and nothing is filed.
func TestAskThenWaitKeepsTheSessionLive(t *testing.T) {
	for _, then := range []string{backlog.ThenWait, ""} {
		t.Run("then="+then, func(t *testing.T) {
			m, repoID, inst := askedThen(t, then)
			results := stubGuardedArchive(t, inst, nil)
			m.backlogRunEnded(repoID, inst.ID)
			m.backgroundMutationWG.Wait()
			if len(*results) != 0 || inst.GetLiveness() != session.LiveReady || inst.OwedArchive() != nil {
				t.Fatalf("archives %v, liveness %v, owed %+v", *results, inst.GetLiveness(), inst.OwedArchive())
			}
			if it := loadItem(t, repoID, 1); it.Status != backlog.StatusRunning || keptLiveNotes(it) != 0 {
				t.Fatalf("item %+v", it)
			}
		})
	}
}

// Someone worked in the session after its run ended: it stays live, the item
// says why, and its question turns to then wait so nothing tries again.
func TestAskArchiveKeepsASessionSomeoneWorkedIn(t *testing.T) {
	m, repoID, inst := askedThen(t, backlog.ThenArchive)
	inst.RecordPromptAttempt(session.PromptDelivered, inst.BacklogRunEndedAt().Add(time.Second))
	results := stubGuardedArchive(t, inst, nil)
	m.backlogRunEnded(repoID, inst.ID)
	m.backgroundMutationWG.Wait()
	if len(*results) != 0 || inst.GetLiveness() != session.LiveReady || inst.OwedArchive() != nil {
		t.Fatalf("archives %v, liveness %v, owed %+v", *results, inst.GetLiveness(), inst.OwedArchive())
	}
	it := loadItem(t, repoID, 1)
	if got := lastNote(it).Text; got != "Kept the session live: a prompt was sent to the session after its run ended." {
		t.Fatalf("note %q", got)
	}
	if q := it.OpenQuestion(); q == nil || q.Then != backlog.ThenWait {
		t.Fatalf("question %+v, want then wait", q)
	}
	m.backlogRunEnded(repoID, inst.ID)
	m.backgroundMutationWG.Wait()
	if n := keptLiveNotes(loadItem(t, repoID, 1)); n != 1 {
		t.Fatalf("%d kept-live notes after a second run end, want 1", n)
	}
}

// An answer recorded between the decision and the fence cancels the archive:
// the guard reads the backlog again, the session stays, there is no note, and
// the answer is delivered.
func TestAnAnswerBeforeTheFenceCancelsTheArchive(t *testing.T) {
	m, repoID, inst := askedThen(t, backlog.ThenArchive)
	sent := stubBacklogSend(t, m, repoID)
	results := stubGuardedArchive(t, inst, func() { answer(t, m, repoID, "Postgres") })
	m.backlogRunEnded(repoID, inst.ID)
	m.backgroundMutationWG.Wait()
	if len(*results) != 1 || (*results)[0] == nil {
		t.Fatalf("archive guard results %v, want one refusal", *results)
	}
	if inst.GetLiveness() == session.LiveArchived || inst.OwedArchive() != nil {
		t.Fatalf("liveness %v, owed %+v", inst.GetLiveness(), inst.OwedArchive())
	}
	it := loadItem(t, repoID, 1)
	if keptLiveNotes(it) != 0 || sent.count() != 1 {
		t.Fatalf("kept-live notes %d, sends %d; want none and one", keptLiveNotes(it), sent.count())
	}
}

// An answer whose send meets the archive in progress is not failed: it waits
// for the archive, restores the session and sends again, once.
func TestAnAnswerThatMeetsAnArchiveRestoresTheSessionAndSendsAgain(t *testing.T) {
	m, repoID, inst := askedItem(t)
	idleItemSession(t, inst)
	sent := stubBacklogSend(t, m, repoID)
	archived, restores, sends := false, 0, 0
	prevSend := sendBacklogPrompt
	sendBacklogPrompt = func(m *Manager, req SendPromptRequest) error {
		sends++
		if sends == 1 {
			archived = true // the archive took the session as this send arrived
			return fmt.Errorf("%w: %w: target session %q is being deleted; prompt not delivered",
				errBacklogPromptNotDelivered, errBacklogTargetTearingDown, req.Title)
		}
		return prevSend(m, req)
	}
	prevNeeds := backlogNeedsRestore
	backlogNeedsRestore = func(itemSession) bool { return archived }
	prevRestore := restoreBacklogSession
	restoreBacklogSession = func(*Manager, RestoreSessionRequest) (session.InstanceData, error) {
		restores++
		archived = false
		return inst.ToInstanceData(), nil
	}
	prevWait := backlogAnswerWait
	backlogAnswerWait = func(<-chan struct{}) bool { return true }
	t.Cleanup(func() {
		sendBacklogPrompt, backlogNeedsRestore, restoreBacklogSession, backlogAnswerWait = prevSend, prevNeeds, prevRestore, prevWait
	})
	msg := answer(t, m, repoID, "Postgres")
	m.backgroundMutationWG.Wait()
	if sends != 2 || restores != 1 || sent.count() != 1 {
		t.Fatalf("sends %d, restores %d, delivered %d; want 2, 1, 1", sends, restores, sent.count())
	}
	it := loadItem(t, repoID, 1)
	for _, x := range it.Thread {
		if x.ID == msg.ID && x.Delivery != backlog.DeliveryDelivered {
			t.Fatalf("answer %+v, want delivered", x)
		}
		if x.Kind == backlog.KindNote && strings.Contains(x.Text, "not delivered") {
			t.Fatalf("note %q", x.Text)
		}
	}
}

// A send refused by the adoption fence (a teardown shut it) provably sent
// nothing: it is not delivered, and marked as a teardown to wait out, never
// uncertain (which would record the answer as delivered).
func TestAFencedSendIsATeardownNotAnUncertainDelivery(t *testing.T) {
	err := classifyBacklogSend(session.PromptNotDelivered, fmt.Errorf("failed to send prompt: %w", session.ErrAdoptionFenced))
	if !errors.Is(err, errBacklogPromptNotDelivered) || !errors.Is(err, errBacklogTargetTearingDown) || errors.Is(err, errBacklogDeliveryUncertain) {
		t.Fatalf("fenced: %v", err)
	}
	err = classifyBacklogSend(session.PromptCouldNotConfirm, notAttempted(tearingDownError{title: "B-1 x"}))
	if !errors.Is(err, errBacklogTargetTearingDown) || errors.Is(err, errBacklogDeliveryUncertain) {
		t.Fatalf("claimed: %v", err)
	}
	if got := (tearingDownError{title: "B-1 x"}).Error(); got != `target session "B-1 x" is being deleted; prompt not delivered` {
		t.Fatalf("the refusal's text changed: %q", got)
	}
}

// The archive is filed on the session's record before it runs, so a daemon
// that stops in between finishes it on restart; recovery clears one that is no
// longer owed.
func TestRecoveryFinishesAnOwedArchiveAndClearsOneNoLongerOwed(t *testing.T) {
	m, repoID, inst := askedThen(t, backlog.ThenArchive)
	m.fileOwedArchive(repoID, inst, backlog.ArchiveAsk)
	if owed := inst.ToInstanceData().OwedArchive; owed == nil || owed.Reason != backlog.ArchiveAsk {
		t.Fatalf("the filing is not on the record: %+v", owed)
	}
	results := stubGuardedArchive(t, inst, nil)
	m.redriveOwedArchives(repoID, m.itemSessions(repoID), nil)
	m.backgroundMutationWG.Wait()
	if len(*results) != 1 || inst.GetLiveness() != session.LiveArchived || inst.OwedArchive() != nil {
		t.Fatalf("archives %v, liveness %v, owed %+v", *results, inst.GetLiveness(), inst.OwedArchive())
	}

	m2, repoID2, inst2 := askedThen(t, backlog.ThenArchive)
	m2.fileOwedArchive(repoID2, inst2, backlog.ArchiveAsk)
	if _, err := backlog.Update(repoID2, func(b *backlog.Backlog) error {
		b.Item(1).Thread = append(b.Item(1).Thread, backlog.Message{ID: 99, Kind: backlog.KindAnswer, Answers: 1, Text: "pg",
			Delivery: backlog.DeliveryPending, RunCount: 1})
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	results2 := stubGuardedArchive(t, inst2, nil)
	m2.redriveOwedArchives(repoID2, m2.itemSessions(repoID2), nil)
	if len(*results2) != 0 || inst2.OwedArchive() != nil || inst2.GetLiveness() == session.LiveArchived {
		t.Fatalf("archives %v, owed %+v, liveness %v", *results2, inst2.OwedArchive(), inst2.GetLiveness())
	}
}

// While a client is attached the archive waits, keeping its reason, and the
// first normal poll after it detaches archives the session.
func TestDeferredAskArchiveKeepsItsReason(t *testing.T) {
	m, repoID, inst := askedThen(t, backlog.ThenArchive)
	results := stubGuardedArchive(t, inst, nil)
	m.PauseStatusPoll(repoID, inst.Title, inst.ID)
	m.backlogRunEnded(repoID, inst.ID)
	m.backgroundMutationWG.Wait()
	if len(*results) != 0 || inst.OwedArchive() == nil {
		t.Fatalf("archived while attached: %v, owed %+v", *results, inst.OwedArchive())
	}
	m.ResumeStatusPoll(repoID, inst.Title, inst.ID)
	m.drainBacklogDeferredArchive(repoID, inst)
	m.backgroundMutationWG.Wait()
	if len(*results) != 1 || inst.GetLiveness() != session.LiveArchived {
		t.Fatalf("after detach: archives %v, liveness %v", *results, inst.GetLiveness())
	}
}
