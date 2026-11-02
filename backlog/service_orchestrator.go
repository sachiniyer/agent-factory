package backlog

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

// What a host reports of a session's state.
const (
	SessionLive     = "live"
	SessionArchived = "archived"
	SessionLost     = "lost"
	SessionDead     = "dead"
	SessionLimited  = "limited"
	// SessionGone: the session was killed or never existed.
	SessionGone = "gone"
)

// SessionInfo is what the host knows of one session of a project. Item is the
// backlog item the session was started for, or 0.
type SessionInfo struct {
	Title   string
	Program string
	Item    int
	State   string
}

// ErrOrchestrateFromItemSession refuses an item session as an orchestrator.
var ErrOrchestrateFromItemSession = errors.New("run this from the session that should orchestrate, not from an item session")

type SetOrchestratorRequest struct {
	RepoID    string `json:"repo_id,omitempty"`
	RepoPath  string `json:"repo_path,omitempty"`
	ID        int    `json:"id"`
	SessionID string `json:"session_id,omitempty"`
	// Clear unbinds the group; it cannot go with SessionID.
	Clear bool `json:"clear,omitempty"`
	// Replace takes the group over from another session.
	Replace bool `json:"replace,omitempty"`
	// Handoff, when set, also stores notes for a replacement session.
	Handoff *string `json:"handoff,omitempty"`
}

// What a bind did to a group's orchestrator.
const (
	OrchestratorBound    = "bound"
	OrchestratorReplaced = "replaced"
)

// OrchestratorResponse is a group and its orchestrator; Orchestrator is nil
// when the group has none. Change is OrchestratorBound or
// OrchestratorReplaced when the bind changed the session, and Previous names
// the session it replaced.
type OrchestratorResponse struct {
	ID           int               `json:"id"`
	Orchestrator *OrchestratorView `json:"orchestrator"`
	Change       string            `json:"change,omitempty"`
	Previous     string            `json:"previous_session_id,omitempty"`
}

type SetOrchestratorHandoffRequest struct {
	RepoID    string `json:"repo_id,omitempty"`
	RepoPath  string `json:"repo_path,omitempty"`
	SessionID string `json:"session_id"`
	Handoff   string `json:"handoff"`
}

// OrchestratorView is a group's orchestrator as the routes show it: the
// binding with the session's state and the count of undelivered events.
type OrchestratorView struct {
	SessionID string    `json:"session_id"`
	Title     string    `json:"title"`
	Program   string    `json:"program,omitempty"`
	BoundAt   time.Time `json:"bound_at"`
	State     string    `json:"state"`
	Pending   int       `json:"pending"`
	Handoff   string    `json:"handoff,omitempty"`
}

// OrchestratorEvent is the payload of a backlog.orchestrator.bound or
// backlog.orchestrator.replaced event.
type OrchestratorEvent struct {
	RepoID    string `json:"repo_id"`
	ID        int    `json:"id"`
	SessionID string `json:"session_id"`
	Title     string `json:"title"`
	Previous  string `json:"previous_session_id,omitempty"`
}

func (s *Service) orchestratorView(repoID string, o *Orchestrator) *OrchestratorView {
	if o == nil {
		return nil
	}
	state := SessionGone
	if info, ok := s.host.Session(repoID, o.SessionID); ok && info.State != "" {
		state = info.State
	}
	return &OrchestratorView{SessionID: o.SessionID, Title: o.Title, Program: o.Program, BoundAt: o.BoundAt,
		State: state, Pending: len(o.Events), Handoff: o.Handoff}
}

func (s *Service) orchestratorViews(repoID string, b *Backlog) map[int]OrchestratorView {
	var out map[int]OrchestratorView
	for _, it := range b.Items {
		if it.Orchestrator == nil {
			continue
		}
		if out == nil {
			out = map[int]OrchestratorView{}
		}
		out[it.ID] = *s.orchestratorView(repoID, it.Orchestrator)
	}
	return out
}

// newOrchestrator checks that sessionID is a session of the project that may
// orchestrate, and returns its binding.
func (s *Service) newOrchestrator(p *project, sessionID string) (Orchestrator, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return Orchestrator{}, errors.New("session_id names the session to bind")
	}
	info, ok := s.host.Session(p.id, sessionID)
	if !ok || info.State == SessionGone {
		return Orchestrator{}, fmt.Errorf("session %q is not a session of this project", sessionID)
	}
	if info.Item != 0 {
		return Orchestrator{}, ErrOrchestrateFromItemSession
	}
	return Orchestrator{SessionID: sessionID, Title: info.Title, Program: info.Program}, nil
}

type bindResult struct {
	change   string
	previous string
	handoff  bool
}

// bind makes o's session the orchestrator of group id. A group bound to
// another session is refused unless replace; a repeat bind of the same
// session changes nothing. A non-nil handoff is stored in the same write.
func (s *Service) bind(b *Backlog, id int, o Orchestrator, replace bool, handoff *string) (bindResult, error) {
	var r bindResult
	if b.ItemForSession(o.SessionID) != nil {
		return r, ErrOrchestrateFromItemSession
	}
	it, err := b.get(id)
	if err != nil {
		return r, err
	}
	prev := it.Orchestrator
	switch {
	case prev != nil && prev.SessionID == o.SessionID:
	case prev != nil && !replace:
		return r, fmt.Errorf("%s is orchestrated by session %q; pass --replace to take it over", FormatID(id), prev.SessionID)
	default:
		if err := b.BindOrchestrator(id, o, s.now()); err != nil {
			return r, err
		}
		r.change = OrchestratorBound
		if prev != nil {
			r.change, r.previous = OrchestratorReplaced, prev.SessionID
		}
	}
	if handoff != nil {
		if _, err := b.SetOrchestratorHandoff(o.SessionID, *handoff, s.now()); err != nil {
			return r, err
		}
		r.handoff = true
	}
	return r, nil
}

func (s *Service) clear(b *Backlog, id int) error {
	if _, err := b.get(id); err != nil {
		return err
	}
	if !b.isGroup(id) {
		return fmt.Errorf("%s is not a top-level parent; only groups have an orchestrator — clear its group, %s", FormatID(id), FormatID(b.rootOf(id)))
	}
	prev, err := b.ClearOrchestrator(id, s.now())
	if err != nil {
		return err
	}
	if prev == nil {
		return errNoChange
	}
	return nil
}

// SetOrchestrator binds a session to a group as its orchestrator, or with
// Clear unbinds it.
func (s *Service) SetOrchestrator(req SetOrchestratorRequest) (*OrchestratorResponse, error) {
	p, err := s.resolve(req.RepoID, req.RepoPath)
	if err != nil {
		return nil, err
	}
	var o Orchestrator
	switch {
	case req.Clear && (req.SessionID != "" || req.Replace || req.Handoff != nil):
		return nil, errors.New("clear unbinds the group; it cannot go with session_id, replace or handoff")
	case !req.Clear:
		if o, err = s.newOrchestrator(p, req.SessionID); err != nil {
			return nil, err
		}
	}
	var r bindResult
	b, err := s.update(p, func(b *Backlog) ([]int, error) {
		if req.Clear {
			return []int{req.ID}, s.clear(b, req.ID)
		}
		var err error
		if r, err = s.bind(b, req.ID, o, req.Replace, req.Handoff); err != nil {
			return nil, err
		}
		if r.change == "" && !r.handoff {
			return nil, errNoChange
		}
		return []int{req.ID}, nil
	})
	if err != nil {
		return nil, err
	}
	return &OrchestratorResponse{ID: req.ID, Orchestrator: s.orchestratorView(p.id, b.Item(req.ID).Orchestrator),
		Change: r.change, Previous: r.previous}, nil
}

// SetOrchestratorHandoff stores the notes for a session that replaces the
// orchestrator.
func (s *Service) SetOrchestratorHandoff(req SetOrchestratorHandoffRequest) (*OrchestratorResponse, error) {
	p, err := s.resolve(req.RepoID, req.RepoPath)
	if err != nil {
		return nil, err
	}
	var id int
	b, err := s.update(p, func(b *Backlog) ([]int, error) {
		var err error
		id, err = b.SetOrchestratorHandoff(req.SessionID, req.Handoff, s.now())
		return []int{id}, err
	})
	if err != nil {
		return nil, err
	}
	return &OrchestratorResponse{ID: id, Orchestrator: s.orchestratorView(p.id, b.Item(id).Orchestrator)}, nil
}

// batchGroup is the one top-level item the batch's items sit under. items is
// every item the batch added or updated, keyed or not: a batch without a
// proposal may leave its items unkeyed, so its key map does not name them all
// (Codex P2 on #6559).
func batchGroup(b *Backlog, items []int) (int, error) {
	roots := map[int]bool{}
	var order []int
	for _, id := range items {
		if r := b.rootOf(id); !roots[r] {
			roots[r] = true
			order = append(order, r)
		}
	}
	if len(order) != 1 {
		names := make([]string, len(order))
		for i, r := range order {
			names[i] = FormatID(r)
		}
		return 0, fmt.Errorf("the batch is under %d top-level items (%s); orchestrate needs it under one group", len(order), strings.Join(names, ", "))
	}
	return order[0], nil
}
