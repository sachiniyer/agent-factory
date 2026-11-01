package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/session"
)

// goneAsker is item 1 whose agent asked with a handoff and whose session is gone
// (killed): the run names a session the daemon no longer has, on a recorded
// branch. It returns the project's root too, which an answer names as the CLI
// and the web do.
func goneAsker(t *testing.T) (*Manager, string, string) {
	t.Helper()
	m, repoID, inst := askedItem(t)
	repoPath := inst.Path
	if _, err := backlog.Update(repoID, func(b *backlog.Backlog) error {
		it := b.Item(1)
		it.Runs[0].SessionID, it.Runs[0].Branch = "gone", "af/b-1-web-view"
		it.Thread[0].Handoff = "Schema drafted in db/schema.sql."
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	prev := session.OriginBranchHead
	session.OriginBranchHead = func(_, branch string, _ time.Duration) (string, error) {
		if branch == "af/b-1-web-view" {
			return "abc1234", nil
		}
		return "", nil
	}
	t.Cleanup(func() { session.OriginBranchHead = prev })
	return m, repoID, repoPath
}

func answerAt(t *testing.T, m *Manager, repoPath, text string) backlog.Message {
	t.Helper()
	out, err := m.backlogService().PostMessage(backlog.PostMessageRequest{RepoPath: repoPath, ID: 1, Kind: backlog.KindAnswer, Text: text})
	if err != nil {
		t.Fatal(err)
	}
	return out.Message
}

// captureCreates records each item session create's prompt and makes it with
// err, or for real when err is nil.
func captureCreates(t *testing.T, err error) *[]string {
	t.Helper()
	var prompts []string
	prev := createBacklogSession
	createBacklogSession = func(m *Manager, ctx context.Context, req CreateSessionRequest) (session.InstanceData, error) {
		prompts = append(prompts, req.Prompt)
		if err != nil {
			return session.InstanceData{}, err
		}
		return prev(m, ctx, req)
	}
	t.Cleanup(func() { createBacklogSession = prev })
	return &prompts
}

// An answer to a question whose session is gone starts a new session for the
// item, as the user's start: its first prompt carries af's facts about the
// previous run, the asking agent's notes, and the question and answer. The
// question closes when the session starts.
func TestAnAnswerToAGoneSessionStartsANewRun(t *testing.T) {
	m, repoID, repoPath := goneAsker(t)
	prompts := captureCreates(t, nil)
	msg := answerAt(t, m, repoPath, "Postgres")
	if msg.Delivery != backlog.DeliveryNewRun {
		t.Fatalf("answer %+v, want kept for a new run", msg)
	}
	m.backgroundMutationWG.Wait()
	if len(*prompts) != 1 {
		t.Fatalf("creates %d, want 1", len(*prompts))
	}
	for _, want := range []string{
		"An earlier session worked on this item, asked the user a question, and is gone now.",
		"- Previous branch: af/b-1-web-view\n- Last commit of that branch on origin: abc1234\n",
		"Schema drafted in db/schema.sql.",
		"The question the previous agent asked:\n\n```markdown\nWhich db?\n```",
		"The user's answer:\n\n```markdown\nPostgres\n```",
	} {
		if !strings.Contains((*prompts)[0], want) {
			t.Errorf("the new run's prompt lacks %q:\n%s", want, (*prompts)[0])
		}
	}
	it := loadItem(t, repoID, 1)
	last := it.Runs[len(it.Runs)-1]
	if len(it.Runs) != 2 || last.State != backlog.RunStarted || last.StartedBy != backlog.StartedByUser || last.Answer != msg.ID {
		t.Fatalf("runs %+v", it.Runs)
	}
	if it.OpenQuestion() != nil || it.Status != backlog.StatusRunning {
		t.Fatalf("the question is still open, or the item is %s: %+v", it.Status, it.Thread)
	}
	if _, ok := m.itemSessions(repoID)[last.SessionID]; !ok {
		t.Fatalf("no item session %q", last.SessionID)
	}
}

// A failed start keeps the answer and its question open, says how to send it,
// and "af backlog start" sends it to the next session.
func TestAFailedAnswerRunKeepsTheAnswerForTheNextStart(t *testing.T) {
	m, repoID, repoPath := goneAsker(t)
	prompts := captureCreates(t, errors.New("no worktree"))
	answerAt(t, m, repoPath, "Postgres")
	m.backgroundMutationWG.Wait()
	it := loadItem(t, repoID, 1)
	if it.OpenQuestion() == nil || it.KeptAnswer() == nil {
		t.Fatalf("the failed start did not keep the answer: %+v", it.Thread)
	}
	if note := lastNote(it).Text; !strings.Contains(note, "no worktree") || !strings.Contains(note, `"af backlog start 1" sends it`) {
		t.Fatalf("note %q", note)
	}
	*prompts = nil
	if _, err := m.StartBacklogItem(backlog.StartRequest{RepoPath: repoPath, ID: 1}, backlog.StartedByUser); err != nil {
		t.Fatal(err)
	}
	m.backgroundMutationWG.Wait()
	if len(*prompts) != 1 || !strings.Contains((*prompts)[0], "The user's answer:\n\n```markdown\nPostgres\n```") {
		t.Fatalf("the start after the failure does not carry the kept answer: %q", *prompts)
	}
}

// A done item refuses an answer to its gone session: it must be reopened first.
func TestAnAnswerToAGoneSessionOfADoneItemIsRefused(t *testing.T) {
	m, repoID, repoPath := goneAsker(t)
	prompts := captureCreates(t, nil)
	if _, err := backlog.Update(repoID, func(b *backlog.Backlog) error {
		b.Item(1).Status = backlog.StatusDone
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err := m.backlogService().PostMessage(backlog.PostMessageRequest{RepoPath: repoPath, ID: 1, Kind: backlog.KindAnswer, Text: "x"})
	if err == nil || !strings.Contains(err.Error(), `B-1 is done; reopen it with "af backlog reopen 1"`) {
		t.Fatal(err)
	}
	m.backgroundMutationWG.Wait()
	if len(*prompts) != 0 || len(loadItem(t, repoID, 1).Runs) != 1 {
		t.Fatal("a refused answer started a run")
	}
}

// A session that comes back without its agent conversation gets the asking
// agent's notes with the answer; one restored into its own conversation does
// not need them.
func TestARestoreWithoutTheConversationSendsTheHandoff(t *testing.T) {
	for _, exact := range []bool{false, true} {
		t.Run(map[bool]string{false: "fresh", true: "exact"}[exact], func(t *testing.T) {
			m, repoID, inst := askedItem(t)
			if _, err := backlog.Update(repoID, func(b *backlog.Backlog) error {
				b.Item(1).Thread[0].Handoff = "Schema drafted in db/schema.sql."
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			stubExactResume(t, exact, "af cannot resume a devin conversation by its id")
			sent := stubBacklogSend(t, m, repoID)
			archived := true
			prevNeeds, prevRestore := backlogNeedsRestore, restoreBacklogSession
			backlogNeedsRestore = func(itemSession) bool { return archived }
			restoreBacklogSession = func(*Manager, RestoreSessionRequest) (session.InstanceData, error) {
				archived = false
				return inst.ToInstanceData(), nil
			}
			t.Cleanup(func() { backlogNeedsRestore, restoreBacklogSession = prevNeeds, prevRestore })
			answer(t, m, repoID, "Postgres")
			m.backgroundMutationWG.Wait()
			if sent.count() != 1 {
				t.Fatalf("sent %d", sent.count())
			}
			got := sent.reqs[0].Prompt
			has := strings.Contains(got, "This session restarted without its earlier conversation.") && strings.Contains(got, "Schema drafted in db/schema.sql.")
			if has == exact {
				t.Fatalf("exact resume %v, handoff in the answer %v:\n%s", exact, has, got)
			}
		})
	}
}
