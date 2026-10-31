package backlog

import (
	"errors"
	"fmt"
)

// PostMessage adds a note, an agent's question, or the user's answer to an
// item's thread. A question sets the session's attention; an answer is
// delivered to the session.
func (s *Service) PostMessage(req PostMessageRequest) (*MessageResponse, error) {
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
		m, err := b.Ask(req.ID, req.SessionID, req.Text, s.now())
		if err != nil {
			return nil, err
		}
		msg = *m
		s.host.SetAttention(p.id, req.SessionID, b.OpenQuestionText(req.ID))
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
	var out Answered
	var pending PendingAnswer
	b, err = s.update(p, func(b *Backlog) ([]int, error) {
		var err error
		if out, err = b.Answer(req.ID, req.QuestionID, req.Text, target != TargetGone, s.now()); err != nil {
			return nil, err
		}
		if target == TargetGone {
			return []int{req.ID}, nil
		}
		if out.SessionID != sessionID {
			return nil, fmt.Errorf("%s changed while answering; retry", FormatID(req.ID))
		}
		next := ""
		if out.Next != nil && target == TargetLive {
			next = out.Next.Text
		}
		s.host.SetAttention(p.id, sessionID, next)
		pending = PendingAnswer{ID: req.ID, AnswerID: out.Answer.ID, SessionID: sessionID,
			Prompt: AnswerPrompt(b.Item(req.ID), out.Answer)}
		return []int{req.ID}, nil
	})
	if err != nil {
		return nil, err
	}
	if target == TargetGone {
		return nil, GoneError(req.ID)
	}
	s.host.DeliverAnswer(p.id, pending)
	return &MessageResponse{Message: out.Answer}, nil
}
