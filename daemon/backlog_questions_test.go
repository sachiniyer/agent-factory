package daemon

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/session"
)

// askedItem starts item 1, ends its run, and has its agent ask a question.
func askedItem(t *testing.T) (*Manager, string, *session.Instance) {
	t.Helper()
	m, repoID, inst := startedItem(t)
	inst.DisarmBacklogRun()
	if _, err := m.backlogService().PostMessage(backlog.PostMessageRequest{RepoID: repoID, ID: 1,
		Kind: backlog.KindQuestion, Text: "Which db?", SessionID: inst.ID}); err != nil {
		t.Fatal(err)
	}
	return m, repoID, inst
}

type sentPrompts struct {
	mu   sync.Mutex
	reqs []SendPromptRequest
	// paused records whether the session was attached at each send.
	paused []bool
	err    error
}

func (s *sentPrompts) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.reqs)
}

func stubBacklogSend(t *testing.T, m *Manager, repoID string) *sentPrompts {
	t.Helper()
	s := &sentPrompts{}
	prev := sendBacklogPrompt
	sendBacklogPrompt = func(_ *Manager, req SendPromptRequest) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.reqs = append(s.reqs, req)
		s.paused = append(s.paused, m.isPollPaused(repoID, req.Title, req.ID))
		return s.err
	}
	t.Cleanup(func() { sendBacklogPrompt = prev })
	return s
}

// stubAnswerWait makes each hold signal held and wait for one value on resume:
// true continues the hold loop, false is a daemon stop.
func stubAnswerWait(t *testing.T) (held chan struct{}, resume chan bool) {
	t.Helper()
	held, resume = make(chan struct{}, 1), make(chan bool)
	prev := backlogAnswerWait
	backlogAnswerWait = func(<-chan struct{}) bool {
		held <- struct{}{}
		return <-resume
	}
	t.Cleanup(func() { backlogAnswerWait = prev })
	return held, resume
}

func answer(t *testing.T, m *Manager, repoID, text string) backlog.Message {
	t.Helper()
	out, err := m.backlogService().PostMessage(backlog.PostMessageRequest{RepoID: repoID, ID: 1, Kind: backlog.KindAnswer, Text: text})
	if err != nil {
		t.Fatal(err)
	}
	return out.Message
}

func lastNote(it backlog.Item) backlog.Message { return it.Thread[len(it.Thread)-1] }

func TestBacklogAskSetsAttention(t *testing.T) {
	m, repoID, inst := askedItem(t)
	if a := inst.Attention(); a == nil || a.Reason != "Which db?" {
		t.Fatalf("attention %+v", a)
	}
	v, err := m.backlogService().Get(backlog.GetRequest{RepoID: repoID})
	if err != nil {
		t.Fatal(err)
	}
	if st := v.States[1].State; st != backlog.StateNeedsYou {
		t.Fatalf("state %s", st)
	}
}

func TestBacklogAnswerDelivers(t *testing.T) {
	m, repoID, inst := askedItem(t)
	sent := stubBacklogSend(t, m, repoID)
	msg := answer(t, m, repoID, "Postgres")
	m.backgroundMutationWG.Wait()
	if inst.HasAttention() {
		t.Fatal("attention not cleared")
	}
	if len(sent.reqs) != 1 || sent.reqs[0].ID != inst.ID || sent.reqs[0].Prompt != "The user answered your question on B-1: Postgres" {
		t.Fatalf("sent %+v", sent.reqs)
	}
	it := loadItem(t, repoID, 1)
	if it.Status != backlog.StatusRunning || it.OpenQuestion() != nil || !inst.BacklogRunActive() {
		t.Fatalf("item %+v active %v", it, inst.BacklogRunActive())
	}
	for _, x := range it.Thread {
		if x.ID == msg.ID && x.Delivery != backlog.DeliveryDelivered {
			t.Fatalf("answer %+v", x)
		}
	}
	if got := m.backlogSlotsUsed(repoID); got != 1 {
		t.Fatalf("slots %d, want the armed run only", got)
	}
}

func TestBacklogAnswerHeldWhileAttached(t *testing.T) {
	m, repoID, inst := askedItem(t)
	sent := stubBacklogSend(t, m, repoID)
	held, resume := stubAnswerWait(t)

	m.PauseStatusPoll(repoID, inst.Title, inst.ID)
	answer(t, m, repoID, "yes")
	<-held
	if sent.count() != 0 {
		t.Fatal("sent while attached")
	}
	if inst.BacklogRunActive() {
		t.Fatal("the run was armed while the answer was held: its completion gate would be open with nothing sent (Codex P2 on #6295)")
	}
	v, err := m.backlogService().Get(backlog.GetRequest{RepoID: repoID})
	if err != nil {
		t.Fatal(err)
	}
	if st := v.States[1].State; st != backlog.StateWorking || m.backlogSlotsUsed(repoID) != 1 {
		t.Fatalf("held answer shows %s with %d slots", st, m.backlogSlotsUsed(repoID))
	}
	m.ResumeStatusPoll(repoID, inst.Title, inst.ID)
	resume <- true
	m.backgroundMutationWG.Wait()
	if len(sent.reqs) != 1 || sent.paused[0] {
		t.Fatalf("sent %+v paused %v", sent.reqs, sent.paused)
	}
	if !inst.BacklogRunActive() {
		t.Fatal("the run was not armed when the answer was sent")
	}
}

func TestBacklogAnswerStopWhileHeld(t *testing.T) {
	m, repoID, inst := askedItem(t)
	sent := stubBacklogSend(t, m, repoID)
	held, resume := stubAnswerWait(t)
	if _, err := backlog.Update(repoID, func(b *backlog.Backlog) error {
		b.Item(1).Status = backlog.StatusReview
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m.PauseStatusPoll(repoID, inst.Title, inst.ID)
	answer(t, m, repoID, "yes")
	<-held
	resume <- false
	m.backgroundMutationWG.Wait()
	it := loadItem(t, repoID, 1)
	if sent.count() != 0 || inst.BacklogRunActive() || it.Status != backlog.StatusReview {
		t.Fatalf("sent %d active %v status %s", sent.count(), inst.BacklogRunActive(), it.Status)
	}
	if n := lastNote(it); n.Author != backlog.AuthorAF || !strings.Contains(n.Text, "af stopped") {
		t.Fatalf("note %+v", n)
	}
}

func TestBacklogAnswerSendFails(t *testing.T) {
	m, repoID, inst := askedItem(t)
	sent := stubBacklogSend(t, m, repoID)
	sent.err = errors.New("pane gone")
	if _, err := backlog.Update(repoID, func(b *backlog.Backlog) error {
		b.Item(1).Status = backlog.StatusReview
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	answer(t, m, repoID, "yes")
	m.backgroundMutationWG.Wait()
	it := loadItem(t, repoID, 1)
	if inst.BacklogRunActive() || it.Status != backlog.StatusReview || !strings.Contains(lastNote(it).Text, "pane gone") {
		t.Fatalf("active %v item %+v", inst.BacklogRunActive(), it)
	}
	if m.backlogSlotsUsed(repoID) != 0 {
		t.Fatal("slot leaked")
	}
}

func TestBacklogAnswerRestoreFails(t *testing.T) {
	m, repoID, _ := askedItem(t)
	sent := stubBacklogSend(t, m, repoID)
	prevNeeds := backlogNeedsRestore
	backlogNeedsRestore = func(itemSession) bool { return true }
	t.Cleanup(func() { backlogNeedsRestore = prevNeeds })
	prev := restoreBacklogSession
	restoreBacklogSession = func(*Manager, RestoreSessionRequest) (session.InstanceData, error) {
		return session.InstanceData{}, errors.New("worktree removed")
	}
	t.Cleanup(func() { restoreBacklogSession = prev })
	if _, err := backlog.Update(repoID, func(b *backlog.Backlog) error {
		b.Item(1).Status = backlog.StatusReview
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	answer(t, m, repoID, "yes")
	m.backgroundMutationWG.Wait()
	it := loadItem(t, repoID, 1)
	if sent.count() != 0 || it.Status != backlog.StatusReview || !strings.Contains(lastNote(it).Text, "worktree removed") {
		t.Fatalf("sent %d item %+v", sent.count(), it)
	}
	if m.backlogSlotsUsed(repoID) != 0 {
		t.Fatal("slot leaked")
	}
}

func TestBacklogAnswerGoneSession(t *testing.T) {
	m, repoID, _ := askedItem(t)
	if _, err := backlog.Update(repoID, func(b *backlog.Backlog) error {
		b.Item(1).Runs[0].SessionID = "gone"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err := m.backlogService().PostMessage(backlog.PostMessageRequest{RepoID: repoID, ID: 1, Kind: backlog.KindAnswer, Text: "x"})
	if err == nil || !strings.HasPrefix(err.Error(), "the session for B-1 is gone") {
		t.Fatal(err)
	}
	it := loadItem(t, repoID, 1)
	if last := lastNote(it); last.Kind != backlog.KindAnswer {
		t.Fatalf("answer not kept: %+v", it.Thread)
	}
}

func TestBacklogRedeliversPendingAnswer(t *testing.T) {
	m, repoID, inst := askedItem(t)
	sent := stubBacklogSend(t, m, repoID)
	if _, err := backlog.Update(repoID, func(b *backlog.Backlog) error {
		if _, err := b.Answer(1, 0, "Postgres", true, daemonProcessStart.Add(-time.Minute)); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m.recoverBacklog(repoID, nil)
	m.backgroundMutationWG.Wait()
	it := loadItem(t, repoID, 1)
	if len(sent.reqs) != 1 || sent.reqs[0].ID != inst.ID || it.Status != backlog.StatusRunning {
		t.Fatalf("sent %+v item %+v", sent.reqs, it)
	}
	if len(mustLoad(t, repoID).PendingAnswers()) != 0 {
		t.Fatal("answer still pending")
	}
}

func mustLoad(t *testing.T, repoID string) *backlog.Backlog {
	t.Helper()
	b, err := backlog.Load(repoID)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestBacklogAnsweredInSessionOnSubmit(t *testing.T) {
	m, repoID, inst := askedItem(t)
	// The question the user's line answers is the one open when it is submitted;
	// the agent posts the next one while the line is in flight.
	answered := m.backlogSeenQuestion(inst)
	if answered == "" {
		t.Fatal("no open question to capture")
	}
	if _, err := m.backlogService().PostMessage(backlog.PostMessageRequest{RepoID: repoID, ID: 1,
		Kind: backlog.KindQuestion, Text: "And the port?", SessionID: inst.ID}); err != nil {
		t.Fatal(err)
	}
	m.noteBacklogSubmitted(inst, answered)
	m.backgroundMutationWG.Wait()
	it := loadItem(t, repoID, 1)
	if it.Thread[0].Closed != backlog.ClosedAnsweredInSession || it.Thread[1].Closed != "" {
		t.Fatalf("only the oldest question closes: %+v", it.Thread)
	}
	if a := inst.Attention(); a == nil || a.Reason != "And the port?" {
		t.Fatalf("attention %+v, want the next question", a)
	}
	if !inst.BacklogRunActive() {
		t.Fatal("a running item's run is armed after an answer in session")
	}
}

func TestBacklogInputWithoutSubmitKeepsQuestion(t *testing.T) {
	m, repoID, inst := askedItem(t)
	m.clearAttentionOnInput(inst)
	m.backgroundMutationWG.Wait()
	if it := loadItem(t, repoID, 1); it.OpenQuestion() == nil || inst.BacklogRunActive() {
		t.Fatalf("a keystroke answered the question: %+v", it.Thread)
	}
}

func TestIsSubmittedInput(t *testing.T) {
	for in, want := range map[string]bool{"yes\r": true, "\r": true, "a": false, "\x1b[A": false, "\x1b[13;2u\r": false, "": false} {
		if got := isSubmittedInput([]byte(in)); got != want {
			t.Errorf("%q: got %v", in, got)
		}
	}
}

func TestBacklogDoneClearsAttention(t *testing.T) {
	m, repoID, inst := askedItem(t)
	if _, err := m.backlogService().SetItemStatus(backlog.SetItemStatusRequest{RepoID: repoID, ID: 1, Status: backlog.StatusDone}); err != nil {
		t.Fatal(err)
	}
	if inst.HasAttention() {
		t.Fatal("done left the attention request")
	}
	if c := loadItem(t, repoID, 1).Thread[0].Closed; c != backlog.ClosedDone {
		t.Fatal(c)
	}
}

// Delivering an answer must not clear the attention of the NEXT open question
// (Codex P2 on #6295): the backlog mirrors attention itself, so its sends are
// not "the user answering whatever is set". A plain send still clears it.
func TestBacklogSendKeepsTheNextQuestionsAttention(t *testing.T) {
	drain := queuedAttentionClears(t)
	for _, tc := range []struct {
		name          string
		backlogOrigin bool
		wantKept      bool
	}{{"backlog answer delivery", true, true}, {"a user's send-prompt", false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			manager, repoID, repoPath := newStatusTestManager(t)
			backend := observedPromptBackend{
				readyFakeBackend: readyFakeBackend{FakeBackend: session.NewFakeBackend()},
				status:           session.PromptDelivered,
			}
			inst := registerStarted(t, manager, repoID, repoPath, "worker", backend, true, session.Ready)
			if _, err := inst.SetAttention("the second question", time.Now()); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.SendPromptWithStatus(SendPromptRequest{Title: "worker", RepoID: repoID, Prompt: "answer", BacklogOrigin: tc.backlogOrigin}); err != nil {
				t.Fatal(err)
			}
			// The clear a delivered user prompt queues runs after the send returns
			// (a goroutine in production); drain it as the worker would.
			drain()
			if inst.HasAttention() != tc.wantKept {
				t.Fatalf("attention kept = %v, want %v", inst.HasAttention(), tc.wantKept)
			}
		})
	}
}

// The write that records a delivered answer is retried while the backlog is busy
// (Codex P2 on #6295): left pending, the answer would be sent again after a
// restart and restart work the agent already did.
func TestAnswerDeliveredWriteIsRetriedWhileTheBacklogIsBusy(t *testing.T) {
	m, repoID, inst := askedItem(t)
	stubBacklogSend(t, m, repoID)
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
	msg := answer(t, m, repoID, "Postgres")
	m.backgroundMutationWG.Wait()
	_ = inst
	if busy != 0 {
		t.Fatalf("the outcome write gave up with %d busy answers left", busy)
	}
	for _, x := range loadItem(t, repoID, 1).Thread {
		if x.ID == msg.ID && x.Delivery != backlog.DeliveryDelivered {
			t.Fatalf("answer %+v is still %q: a restart would send it again", x, x.Delivery)
		}
	}
}

// A failed additional answer leaves the run that was already underway alone
// (Codex P2 on #6295): only a run this send armed from idle is disarmed.
func TestFailedAdditionalAnswerKeepsTheActiveRun(t *testing.T) {
	m, repoID, inst := askedItem(t)
	sent := stubBacklogSend(t, m, repoID)
	if !inst.ArmBacklogRun(time.Now()) { // the first answer's run, still being worked on
		t.Fatal("precondition: arm")
	}
	sent.err = errors.New("pane gone")
	if err := m.sendBacklogAnswer(repoID, inst, "second answer"); err == nil {
		t.Fatal("the send was expected to fail")
	}
	if !inst.BacklogRunActive() {
		t.Fatal("a failed additional answer closed the run already underway")
	}
	inst.DisarmBacklogRun()
	if err := m.sendBacklogAnswer(repoID, inst, "from idle"); err == nil {
		t.Fatal("the send was expected to fail")
	}
	if inst.BacklogRunActive() {
		t.Fatal("a run armed by the failed send itself must be disarmed")
	}
}

// An answer whose delivery cannot be confirmed keeps the run marker and counts as
// delivered, so a restart does not send it again (Codex P2 on #6294, invariant 1
// of the run lifecycle model).
func TestAnswerWithAnUncertainDeliveryKeepsTheRunAndIsNotResent(t *testing.T) {
	m, repoID, inst := askedItem(t)
	sent := stubBacklogSend(t, m, repoID)
	sent.err = fmt.Errorf("%w: tmux send timed out", errBacklogDeliveryUncertain)
	msg := answer(t, m, repoID, "Postgres")
	m.backgroundMutationWG.Wait()
	if !inst.BacklogRunActive() {
		t.Fatal("the run marker was dropped for an answer that may have landed")
	}
	it := loadItem(t, repoID, 1)
	for _, x := range it.Thread {
		if x.ID == msg.ID && x.Delivery != backlog.DeliveryDelivered {
			t.Fatalf("answer %+v is %q: a restart would send it again", x, x.Delivery)
		}
	}
	if n := it.Thread[len(it.Thread)-1]; !strings.Contains(n.Text, "could not confirm") {
		t.Fatalf("no note about the unconfirmed delivery: %+v", n)
	}
}

// If the answered run goes idle before the delivered-outcome write lands, the
// pending answer held its end back; recording the delivery settles it (Codex P2
// on #6295), so the item is not left running with nothing to end it.
func TestAnswerDeliveredReplaysACompletionItSuppressed(t *testing.T) {
	m, repoID, inst := askedItem(t)
	prev := sendBacklogPrompt
	sendBacklogPrompt = func(*Manager, SendPromptRequest) error {
		idleItemSession(t, inst) // the agent finished before the outcome is recorded
		return nil
	}
	t.Cleanup(func() { sendBacklogPrompt = prev })
	answer(t, m, repoID, "Postgres")
	m.backgroundMutationWG.Wait()
	if it := loadItem(t, repoID, 1); it.Status != backlog.StatusReview {
		t.Fatalf("status = %s, want review: the suppressed completion was never replayed", it.Status)
	}
}

// Recovery can offer the same pending answer on every retry; only one delivery
// worker runs per answer (Codex P2 on #6295).
func TestOnePendingAnswerGetsOneDeliveryWorker(t *testing.T) {
	m, repoID, _ := askedItem(t)
	a := backlog.PendingAnswer{ID: 1, AnswerID: 9, SessionID: "s", Prompt: "p"}
	if !m.backlog.claimAnswer(repoID, a) {
		t.Fatal("the first claim must succeed")
	}
	if m.backlog.claimAnswer(repoID, a) {
		t.Fatal("a second worker was allowed for the same answer")
	}
	m.backlog.releaseAnswer(repoID, a)
	if !m.backlog.claimAnswer(repoID, a) {
		t.Fatal("a released answer can be claimed again")
	}
}

// A line the user answers in the pane of an item in review is new work (Codex P2
// on #6295): the item goes back to running, holds a live answer and has its run
// armed in the same transaction.
func TestTerminalAnswerOnAnItemInReviewTracksNewWork(t *testing.T) {
	m, repoID, inst := askedItem(t)
	idleItemSession(t, inst)
	if _, err := backlog.Update(repoID, func(b *backlog.Backlog) error {
		b.Item(1).Status = backlog.StatusReview
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	m.noteBacklogSubmitted(inst, m.backlogSeenQuestion(inst))
	m.backgroundMutationWG.Wait()
	it := loadItem(t, repoID, 1)
	if it.Status != backlog.StatusRunning || it.LiveAnswers != 1 || !inst.BacklogRunActive() {
		t.Fatalf("status %s live answers %d armed %v", it.Status, it.LiveAnswers, inst.BacklogRunActive())
	}
}

// An answer whose delivery fails after the previous run already went idle replays
// the completion the pending answer had held back (Codex P2 on #6295).
func TestFailedAnswerReplaysASkippedCompletion(t *testing.T) {
	m, repoID, inst := askedItem(t)
	prev := sendBacklogPrompt
	sendBacklogPrompt = func(*Manager, SendPromptRequest) error {
		idleItemSession(t, inst)
		return errors.New("pane gone")
	}
	t.Cleanup(func() { sendBacklogPrompt = prev })
	answer(t, m, repoID, "Postgres")
	m.backgroundMutationWG.Wait()
	if it := loadItem(t, repoID, 1); it.Status != backlog.StatusReview {
		t.Fatalf("status = %s, want review: the held-back completion was not replayed after the failure", it.Status)
	}
}

// Dismissing the attention chip does not close the backlog question: the text the
// session was shown is still the question a terminal line answers (Codex P2 on
// #6295).
func TestBacklogSeenQuestionSurvivesADismissedChip(t *testing.T) {
	m, _, inst := askedItem(t)
	want := m.backlogSeenQuestion(inst)
	if want == "" {
		t.Fatal("no open question")
	}
	a := inst.Attention()
	inst.ClearAttention(a.SetAt)
	if inst.Attention() != nil {
		t.Fatal("chip not dismissed")
	}
	if got := m.backlogSeenQuestion(inst); got != want {
		t.Fatalf("seen question after dismissal = %q, want %q", got, want)
	}
}

// After a restart the remembered question comes back from the backlog, without
// restoring a dismissed chip (Codex P2 on #6299).
func TestRebuildAskedQuestionsFromTheBacklog(t *testing.T) {
	m, repoID, inst := askedItem(t)
	want := m.backlogSeenQuestion(inst)
	inst.ClearAttention(inst.Attention().SetAt)
	m.backlog.asked.Delete(inst.ID) // a fresh daemon
	if m.backlogSeenQuestion(inst) != "" {
		t.Fatal("nothing should be remembered yet")
	}
	b, err := backlog.Load(repoID)
	if err != nil {
		t.Fatal(err)
	}
	m.rebuildAskedQuestions(b)
	if got := m.backlogSeenQuestion(inst); got != want {
		t.Fatalf("rebuilt question = %q, want %q", got, want)
	}
	if inst.Attention() != nil {
		t.Fatal("the dismissed chip must stay dismissed")
	}
}

// A question longer than the attention cap is remembered, and compared, in the one
// normalized form: with the chip dismissed (and after a restart's rebuild) a
// terminal answer still closes it.
func TestLongQuestionIsAnsweredInSessionWithADismissedChip(t *testing.T) {
	m, repoID, inst := startedItem(t)
	inst.DisarmBacklogRun()
	long := strings.Repeat("Which database should the migration target? ", 20)
	if len([]rune(long)) <= session.MaxAttentionReasonLen {
		t.Fatal("fixture question is not over the cap")
	}
	if _, err := m.backlogService().PostMessage(backlog.PostMessageRequest{RepoID: repoID, ID: 1,
		Kind: backlog.KindQuestion, Text: long, SessionID: inst.ID}); err != nil {
		t.Fatal(err)
	}
	inst.ClearAttention(inst.Attention().SetAt)
	for _, rebuilt := range []bool{false, true} {
		if rebuilt {
			m.backlog.asked.Delete(inst.ID)
			b, err := backlog.Load(repoID)
			if err != nil {
				t.Fatal(err)
			}
			m.rebuildAskedQuestions(b)
		}
		seen := m.backlogSeenQuestion(inst)
		if seen == "" || seen != session.NormalizeAttentionReason(long) {
			t.Fatalf("rebuilt=%v: seen question %d runes, want the normalized form", rebuilt, len([]rune(seen)))
		}
	}
	m.noteBacklogSubmitted(inst, m.backlogSeenQuestion(inst))
	m.backgroundMutationWG.Wait()
	if it := loadItem(t, repoID, 1); it.OpenQuestion() != nil {
		t.Fatalf("the long question stayed open: %+v", it.Thread)
	}
}
