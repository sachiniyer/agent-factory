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
	stubExactResume(t, true, "")
	m, repoID, inst := startedItem(t)
	if _, err := m.backlogService().PostMessage(backlog.PostMessageRequest{RepoID: repoID, ID: 1,
		Kind: backlog.KindQuestion, Text: "Which db?", SessionID: inst.ID, Then: then}); err != nil {
		t.Fatal(err)
	}
	idleItemSession(t, inst)
	return m, repoID, inst
}

// stubExactResume answers whether af could resume the session's own
// conversation, for sessions whose agent the tests stand in for.
func stubExactResume(t *testing.T, ok bool, why string) {
	t.Helper()
	prev := backlogExactResume
	backlogExactResume = func(*session.Instance) (bool, string) { return ok, why }
	t.Cleanup(func() { backlogExactResume = prev })
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
	var filed *session.BacklogArchiveData
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
	// The archived row is marked as waiting on the user, on its record too, so
	// the TUI shows it.
	if !inst.AwaitsAnswer() || inst.ToInstanceData().BacklogArchive == nil {
		t.Fatal("the archived session is not marked as waiting on the answer")
	}
	v, err := m.backlogService().Get(backlog.GetRequest{RepoID: repoID})
	if err != nil {
		t.Fatal(err)
	}
	if st := v.States[1].State; st != backlog.StateNeedsYou {
		t.Fatalf("state %s, want needs you", st)
	}
	// The answer closes the question, which drops the mark before the restore.
	stubBacklogSend(t, m, repoID)
	prevRestore := restoreBacklogSession
	restoreBacklogSession = func(*Manager, RestoreSessionRequest) (session.InstanceData, error) {
		return inst.ToInstanceData(), errors.New("stand-in restore")
	}
	t.Cleanup(func() { restoreBacklogSession = prevRestore })
	answer(t, m, repoID, "Postgres")
	m.backgroundMutationWG.Wait()
	if inst.AwaitsAnswer() || inst.ToInstanceData().BacklogArchive != nil {
		t.Fatal("the mark outlived the question")
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
	if owed := inst.ToInstanceData().BacklogArchive; owed == nil || owed.Reason != backlog.ArchiveAsk || !owed.ArchivedAt.IsZero() {
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
	m.PauseStatusPollFor(repoID, inst.Title, inst.ID, "")
	m.backlogRunEnded(repoID, inst.ID)
	m.backgroundMutationWG.Wait()
	if len(*results) != 0 || inst.OwedArchive() == nil {
		t.Fatalf("archived while attached: %v, owed %+v", *results, inst.OwedArchive())
	}
	m.ResumeStatusPollFor(repoID, inst.Title, inst.ID, "")
	m.drainBacklogDeferredArchive(repoID, inst)
	m.backgroundMutationWG.Wait()
	if len(*results) != 1 || inst.GetLiveness() != session.LiveArchived {
		t.Fatalf("after detach: archives %v, liveness %v", *results, inst.GetLiveness())
	}
}

// af archives only a session it can restore into its own conversation. A
// session of an agent it cannot resume by id (devin, say) stays live, as with
// then wait, the item says why, and the answer is delivered straight to it.
func TestAskArchiveKeepsLiveASessionAfCannotResume(t *testing.T) {
	m, repoID, inst := askedThen(t, backlog.ThenArchive)
	stubExactResume(t, false, "af cannot resume a devin conversation by its id")
	results := stubGuardedArchive(t, inst, nil)
	m.backlogRunEnded(repoID, inst.ID)
	m.backgroundMutationWG.Wait()
	if len(*results) != 0 || inst.GetLiveness() != session.LiveReady || inst.OwedArchive() != nil {
		t.Fatalf("archives %v, liveness %v, owed %+v", *results, inst.GetLiveness(), inst.OwedArchive())
	}
	it := loadItem(t, repoID, 1)
	const want = "Kept the session live: af cannot resume a devin conversation by its id, so archived it would come back as a fresh agent."
	if got := lastNote(it).Text; got != want {
		t.Fatalf("note %q, want %q", got, want)
	}
	if q := it.OpenQuestion(); q == nil || q.Then != backlog.ThenWait {
		t.Fatalf("question %+v, want then wait", q)
	}
	sent := stubBacklogSend(t, m, repoID)
	prevRestore := restoreBacklogSession
	restoreBacklogSession = func(*Manager, RestoreSessionRequest) (session.InstanceData, error) {
		t.Error("the answer restored a session that stayed live")
		return session.InstanceData{}, errors.New("unexpected restore")
	}
	t.Cleanup(func() { restoreBacklogSession = prevRestore })
	answer(t, m, repoID, "Postgres")
	m.backgroundMutationWG.Wait()
	if sent.count() != 1 {
		t.Fatalf("delivered %d, want 1", sent.count())
	}
}

// An archive that committed with a warning (an on_archive hook failed, or files
// were skipped) is still an archive: the row is marked as waiting on the user
// (Codex P2 on #6475).
func TestACommittedArchiveWarningStillMarksTheRow(t *testing.T) {
	m, repoID, inst := askedThen(t, backlog.ThenArchive)
	prev := archiveSessionForLifecycle
	archiveSessionForLifecycle = func(_ *Manager, _ ArchiveSessionRequest, guard sessionTeardownGuard) error {
		if err := guard(inst); err != nil {
			return err
		}
		if err := inst.Transition(session.BeginArchive()); err != nil {
			return err
		}
		if err := inst.Transition(session.CommitArchive()); err != nil {
			return err
		}
		return archiveCommittedWarning(inst, errors.New("on_archive hook exited 1"))
	}
	t.Cleanup(func() { archiveSessionForLifecycle = prev })
	m.backlogRunEnded(repoID, inst.ID)
	m.backgroundMutationWG.Wait()
	if !inst.AwaitsAnswer() || inst.OwedArchive() != nil {
		t.Fatalf("awaits %v, owed %+v; want the row marked and the filing settled", inst.AwaitsAnswer(), inst.OwedArchive())
	}
}

// A daemon that stopped after the archive committed and before it marked the
// row leaves the filing owed on an archived session: recovery marks the row
// instead of dropping the filing.
func TestRecoveryMarksAnAskArchiveThatCommittedBeforeARestart(t *testing.T) {
	m, repoID, inst := askedThen(t, backlog.ThenArchive)
	m.fileOwedArchive(repoID, inst, backlog.ArchiveAsk)
	if err := inst.Transition(session.BeginArchive()); err != nil {
		t.Fatal(err)
	}
	if err := inst.Transition(session.CommitArchive()); err != nil {
		t.Fatal(err)
	}
	results := stubGuardedArchive(t, inst, nil)
	m.redriveOwedArchives(repoID, m.itemSessions(repoID), nil)
	if len(*results) != 0 || !inst.AwaitsAnswer() || inst.OwedArchive() != nil {
		t.Fatalf("archives %v, awaits %v, owed %+v", *results, inst.AwaitsAnswer(), inst.OwedArchive())
	}
}

// An answer to one of two questions keeps the session marked while the other is
// open, so a restore that fails leaves the mark on the archived row (Codex P2 on
// #6475); the answer to the last one drops it.
func TestTheMarkStaysWhileAQuestionIsOpen(t *testing.T) {
	stubExactResume(t, true, "")
	m, repoID, inst := startedItem(t)
	for _, q := range []string{"Which db?", "Which port?"} {
		if _, err := m.backlogService().PostMessage(backlog.PostMessageRequest{RepoID: repoID, ID: 1,
			Kind: backlog.KindQuestion, Text: q, SessionID: inst.ID, Then: backlog.ThenArchive}); err != nil {
			t.Fatal(err)
		}
	}
	idleItemSession(t, inst)
	stubGuardedArchive(t, inst, nil)
	m.backlogRunEnded(repoID, inst.ID)
	m.backgroundMutationWG.Wait()
	if inst.GetLiveness() != session.LiveArchived || !inst.AwaitsAnswer() {
		t.Fatalf("liveness %v, awaits %v; want archived and marked", inst.GetLiveness(), inst.AwaitsAnswer())
	}
	stubBacklogSend(t, m, repoID)
	prevRestore := restoreBacklogSession
	restoreBacklogSession = func(*Manager, RestoreSessionRequest) (session.InstanceData, error) {
		return inst.ToInstanceData(), errors.New("stand-in restore")
	}
	t.Cleanup(func() { restoreBacklogSession = prevRestore })
	answer(t, m, repoID, "Postgres")
	m.backgroundMutationWG.Wait()
	if !inst.AwaitsAnswer() {
		t.Fatal("the mark was dropped while a question is still open")
	}
	answer(t, m, repoID, "5432")
	m.backgroundMutationWG.Wait()
	if inst.AwaitsAnswer() {
		t.Fatal("the mark outlived the last question")
	}
}

// twoQuestions starts item 1, has its agent ask two questions with then archive,
// and leaves the session idle with its run over.
func twoQuestions(t *testing.T) (*Manager, string, *session.Instance) {
	t.Helper()
	stubExactResume(t, true, "")
	m, repoID, inst := startedItem(t)
	for _, q := range []string{"Which db?", "Which port?"} {
		if _, err := m.backlogService().PostMessage(backlog.PostMessageRequest{RepoID: repoID, ID: 1,
			Kind: backlog.KindQuestion, Text: q, SessionID: inst.ID, Then: backlog.ThenArchive}); err != nil {
			t.Fatal(err)
		}
	}
	idleItemSession(t, inst)
	return m, repoID, inst
}

// An answer to one of two questions that lands after the archive's guard passed
// and before it committed leaves the row marked for the question left (Codex P2
// on #6475): the mark follows the open questions, not the one the decision saw.
func TestAnAnswerDuringTheTeardownKeepsTheMarkForTheQuestionLeft(t *testing.T) {
	m, repoID, inst := twoQuestions(t)
	stubBacklogSend(t, m, repoID)
	prev := archiveSessionForLifecycle
	archiveSessionForLifecycle = func(_ *Manager, _ ArchiveSessionRequest, guard sessionTeardownGuard) error {
		if err := guard(inst); err != nil {
			return err
		}
		answer(t, m, repoID, "Postgres") // after the guard, before the commit
		if err := inst.Transition(session.BeginArchive()); err != nil {
			return err
		}
		return inst.Transition(session.CommitArchive())
	}
	t.Cleanup(func() { archiveSessionForLifecycle = prev })
	m.backlogRunEnded(repoID, inst.ID)
	m.backgroundMutationWG.Wait()
	if inst.GetLiveness() != session.LiveArchived || !inst.AwaitsAnswer() || inst.OwedArchive() != nil {
		t.Fatalf("liveness %v, awaits %v, owed %+v; want archived and marked for the question left",
			inst.GetLiveness(), inst.AwaitsAnswer(), inst.OwedArchive())
	}
}

// A restart that finds an ask archive committed but not marked, with an answer
// to one of two questions still pending, marks the row: the other question
// waits whether or not that answer's restore succeeds.
func TestRecoveryMarksAnArchiveWithAQuestionLeftAndAnAnswerPending(t *testing.T) {
	m, repoID, inst := twoQuestions(t)
	m.fileOwedArchive(repoID, inst, backlog.ArchiveAsk)
	if err := inst.Transition(session.BeginArchive()); err != nil {
		t.Fatal(err)
	}
	if err := inst.Transition(session.CommitArchive()); err != nil {
		t.Fatal(err)
	}
	if _, err := backlog.Update(repoID, func(b *backlog.Backlog) error {
		_, err := b.Answer(1, 0, "Postgres", time.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	results := stubGuardedArchive(t, inst, nil)
	m.redriveOwedArchives(repoID, m.itemSessions(repoID), nil)
	if len(*results) != 0 || !inst.AwaitsAnswer() || inst.OwedArchive() != nil {
		t.Fatalf("archives %v, awaits %v, owed %+v", *results, inst.AwaitsAnswer(), inst.OwedArchive())
	}
}
