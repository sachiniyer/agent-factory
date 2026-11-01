package backlog

import (
	"errors"
	"fmt"
)

// PostMessage adds a note, an agent's question, or the user's answer to an
// item's thread. A question sets the session's attention; an answer is
// delivered to the session.
func (s *Service) PostMessage(req PostMessageRequest) (*MessageResponse, error) {
	if req.Kind != KindQuestion && req.Handoff != "" {
		return nil, errors.New("a handoff goes with a question")
	}
	if req.Kind != KindQuestion && req.Then != "" {
		return nil, errors.New("then goes with a question")
	}
	switch req.Kind {
	case KindNote, KindQuestion, KindAnswer:
	default:
		return nil, fmt.Errorf("kind %q is not one of %s, %s and %s", req.Kind, KindNote, KindQuestion, KindAnswer)
	}
	p, err := s.resolve(req.RepoID, req.RepoPath)
	if err != nil {
		return nil, err
	}
	switch req.Kind {
	case KindQuestion:
		return s.ask(p, req)
	case KindAnswer:
		return s.answer(p, req)
	}
	var msg Message
	_, err = s.update(p, func(b *Backlog) ([]int, error) {
		author := AuthorUser
		if req.SessionID != "" {
			it, err := b.get(req.ID)
			if err != nil {
				return nil, err
			}
			if !it.hasRun(req.SessionID) {
				return nil, fmt.Errorf("session %s is not a run of %s", req.SessionID, FormatID(req.ID))
			}
			author = "agent:" + req.SessionID
		}
		m, err := b.AddNote(req.ID, author, req.Text, s.now())
		if err != nil {
			return nil, err
		}
		msg = *m
		return []int{req.ID}, nil
	})
	if err != nil {
		return nil, err
	}
	return &MessageResponse{Message: msg}, nil
}

func (s *Service) ask(p *project, req PostMessageRequest) (*MessageResponse, error) {
	if req.SessionID == "" {
		return nil, errors.New("a question needs session_id: only the agent of the item's session asks — use \"af backlog ask --self\"")
	}
	if req.QuestionID != 0 {
		return nil, errors.New("question_id goes with an answer, not a question")
	}
	var msg Message
	_, err := s.update(p, func(b *Backlog) ([]int, error) {
		m, err := b.Ask(req.ID, req.SessionID, Question{Text: req.Text, Then: req.Then, Handoff: req.Handoff}, s.now())
		if err != nil {
			return nil, err
		}
		msg = *m
		qid, qtext := b.OpenQuestionRef(req.ID)
		s.host.SetAttention(p.id, req.SessionID, qid, qtext)
		return []int{req.ID}, nil
	})
	if err != nil {
		return nil, err
	}
	return &MessageResponse{Message: msg}, nil
}

func (s *Service) answer(p *project, req PostMessageRequest) (*MessageResponse, error) {
	if req.SessionID != "" {
		return nil, errors.New("an answer comes from the user; it cannot carry session_id")
	}
	b, err := Load(p.id)
	if err != nil {
		return nil, err
	}
	it := b.Item(req.ID)
	if it == nil {
		return nil, p.named(&NotFoundError{ID: req.ID})
	}
	sessionID, target := "", TargetGone
	if last := it.lastRun(); last != nil {
		if last.State == RunStarting {
			return nil, fmt.Errorf("%s is starting a new session; answer once it is up", FormatID(req.ID))
		}
		if last.State == RunStarted && last.SessionID != "" {
			sessionID = last.SessionID
			target = s.host.AnswerTarget(p.id, sessionID)
		}
	}
	if target == TargetGone {
		// No session can take the answer: the host keeps it and starts a new run
		// whose first prompt carries it. A done or dropped item refuses.
		msg, err := s.host.StartAnswerRun(p.id, AnswerRunRequest{RepoPath: req.RepoPath, SessionID: sessionID,
			ID: req.ID, QuestionID: req.QuestionID, Text: req.Text})
		if err != nil {
			return nil, p.named(err)
		}
		return &MessageResponse{Message: msg}, nil
	}
	var out Answered
	var pending PendingAnswer
	b, err = s.update(p, func(b *Backlog) ([]int, error) {
		var err error
		// The target was read outside this transaction. If the item's session changed
		// in between (a killed session replaced), answering now would close the
		// question against the wrong target and lose it; refuse before touching the
		// thread so the retry sees the replacement.
		current := ""
		if cur := b.Item(req.ID); cur != nil {
			if last := cur.lastRun(); last != nil && last.State == RunStarted {
				current = last.SessionID
			}
		}
		if current != sessionID {
			return nil, fmt.Errorf("%s changed while answering; retry", FormatID(req.ID))
		}
		if out, err = b.Answer(req.ID, req.QuestionID, req.Text, s.now()); err != nil {
			return nil, err
		}
		if out.SessionID != sessionID {
			return nil, fmt.Errorf("%s changed while answering; retry", FormatID(req.ID))
		}
		// The next open question's id goes to the host for any target, so 0
		// always means no question is left; only a live session mirrors its text.
		next, nextID := "", 0
		if out.Next != nil {
			nextID = out.Next.ID
			if target == TargetLive {
				next = out.Next.Text
			}
		}
		s.host.SetAttention(p.id, sessionID, nextID, next)
		it := b.Item(req.ID)
		pending = PendingAnswer{ID: req.ID, AnswerID: out.Answer.ID, SessionID: sessionID,
			Prompt: AnswerPrompt(it, out.Answer), Handoff: it.Handoff(out.Answer.Answers)}
		return []int{req.ID}, nil
	})
	if err != nil {
		return nil, err
	}
	s.host.DeliverAnswer(p.id, pending)
	return &MessageResponse{Message: out.Answer}, nil
}
