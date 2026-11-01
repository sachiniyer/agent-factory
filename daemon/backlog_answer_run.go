package daemon

import (
	"time"

	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/session"
)

func (h backlogHost) StartAnswerRun(repoID string, req backlog.AnswerRunRequest) (backlog.Message, error) {
	return h.m.startAnswerRun(backlog.ProjectRef{ID: repoID}, req)
}

// startAnswerRun answers a question whose session is gone with a new run of the
// item, started by the user, as StartBacklogItem starts one: the kept answer and
// the starting run are written in one transaction (BeginAnswerRun), the session
// is created on a tracked goroutine, and the question closes when it starts.
// The new run's first prompt carries af's facts about the previous run, the
// asking agent's handoff, and the question and its answer (StartPrompt).
func (m *Manager) startAnswerRun(ref backlog.ProjectRef, req backlog.AnswerRunRequest) (backlog.Message, error) {
	svc := m.backlogService()
	b, err := backlog.Load(ref.ID)
	if err != nil {
		return backlog.Message{}, err
	}
	// A dry run on the loaded copy refuses early, before origin is asked.
	if _, err := b.BeginAnswerRun(req.ID, req.QuestionID, req.SessionID, req.Text, svc.Now()); err != nil {
		return backlog.Message{}, err
	}
	root, err := backlogStartRoot(ref.ID, req.RepoPath)
	if err != nil {
		return backlog.Message{}, err
	}
	prev := m.prevBacklogRun(ref.ID, root, b.Item(req.ID))

	var (
		create CreateSessionRequest
		out    backlog.AnswerRun
	)
	m.backlog.reserve(ref.ID)
	_, err = svc.Apply(ref, func(b *backlog.Backlog) ([]int, error) {
		var err error
		if out, err = b.BeginAnswerRun(req.ID, req.QuestionID, req.SessionID, req.Text, svc.Now()); err != nil {
			return nil, err
		}
		create, err = backlogCreateRequest(ref.ID, b, req.ID, root, prev)
		return []int{req.ID}, err
	})
	if err != nil {
		m.backlog.release(ref.ID)
		return backlog.Message{}, err
	}
	m.launchBacklogCreate(ref, req.ID, out.Plan.Run.StartedAt, create)
	return out.Answer, nil
}

// prevBacklogRun is what af knows about the item's latest run that had a
// session: its branch, from the run or else from the session's record when it
// is still there, and that branch's last commit on origin.
func (m *Manager) prevBacklogRun(repoID, root string, it *backlog.Item) backlog.PrevRun {
	var prev backlog.PrevRun
	if it == nil {
		return prev
	}
	for i := len(it.Runs) - 1; i >= 0; i-- {
		r := it.Runs[i]
		if r.SessionID == "" {
			continue
		}
		prev.Branch = r.Branch
		if s, ok := m.itemSessions(repoID)[r.SessionID]; ok && prev.Branch == "" {
			prev.Branch = s.instance.GetBranch()
		}
		break
	}
	if prev.Branch == "" {
		return prev
	}
	sha, err := session.OriginBranchHead(root, prev.Branch, prevRunOriginTimeout)
	if err != nil {
		m.warn().Printf("ask origin for %s of %s: %v", prev.Branch, backlog.FormatID(it.ID), err)
		return prev
	}
	prev.SHA, prev.Checked = sha, true
	return prev
}

// prevRunOriginTimeout bounds the origin lookup an answer to a gone session
// waits on.
const prevRunOriginTimeout = 5 * time.Second
