package daemon

import (
	"errors"
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
	if _, err := m.backlogService().PostMessage(backlog.PostMessageRequest{RepoID: repoID, ID: 1,
		Kind: backlog.KindQuestion, Text: "And the port?", SessionID: inst.ID}); err != nil {
		t.Fatal(err)
	}
	m.noteBacklogSubmitted(inst)
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
