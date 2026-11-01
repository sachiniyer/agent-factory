package backlog

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/planusage"
)

// Host is what the service needs from the daemon.
type Host interface {
	// Facts reports what the host knows about the sessions of a project.
	Facts(repoID string) Facts
	// ProjectName names a project in error text; "" when unknown.
	ProjectName(repoID string) string
	// Changed is called after a write with the ids it changed.
	Changed(repoID string, ids []int)
	// SlotsUsed counts the project's runs that hold a runner slot: starts in
	// flight plus item sessions with an active run or that af will restore.
	SlotsUsed(repoID string) int
	// SetAttention mirrors an item's open question on its session; reason ""
	// clears it. Best effort: the thread is the source of truth. The service
	// calls it under the backlog lock, so mirrors follow the thread's order.
	SetAttention(repoID, sessionID string, questionID int, reason string)
	// AnswerTarget reports whether an answer can reach the session.
	AnswerTarget(repoID, sessionID string) AnswerTarget
	// Usage returns the plan windows from the last metering pass.
	Usage() planusage.Usage
	// DeliverAnswer sends a pending answer to the item's session in the
	// background, restoring the session first when it is not running, and
	// records the outcome on the answer.
	DeliverAnswer(repoID string, a PendingAnswer)
	// Gate reports the project's runner gate that is closed now, from a recent
	// meter pass; the zero Gate when all are open.
	Gate(repoID string) Gate
}

// AnswerTarget is where an answer can go.
type AnswerTarget int

const (
	// TargetGone: the session was killed or never existed.
	TargetGone AnswerTarget = iota
	TargetLive
	// TargetRestore: the session is archived, lost or dead; af restores it.
	TargetRestore
)

// Service runs the backlog routes over the stores.
type Service struct {
	host Host
	now  func() time.Time
}

func NewService(host Host) *Service {
	return &Service{host: host, now: time.Now}
}

type project struct {
	host Host
	id   string
	// fallback names the project when the host does not know it.
	fallback string
}

func (s *Service) resolve(repoID, repoPath string) (*project, error) {
	repoID, repoPath = strings.TrimSpace(repoID), strings.TrimSpace(repoPath)
	switch {
	case repoID != "" && repoPath != "", repoID == "" && repoPath == "":
		return nil, errors.New("name the project with exactly one of repo_id or repo_path")
	case repoID != "":
		if err := config.ValidateRepoID(repoID); err != nil {
			return nil, err
		}
		return &project{host: s.host, id: repoID}, nil
	}
	// The RPC-boundary resolver: ~ expanded, and a relative path refused rather
	// than resolved against the daemon's own working directory (an unrelated
	// checkout). Local CLI calls name the project by id; a remote caller sends an
	// absolute path on the daemon host.
	path, err := config.ResolveDaemonHostPath(repoPath)
	if err != nil {
		return nil, err
	}
	repo, err := config.RepoFromPath(path)
	if err != nil {
		return nil, fmt.Errorf("repo_path %q: %w", repoPath, err)
	}
	return &project{host: s.host, id: repo.ID, fallback: filepath.Base(repo.Root)}, nil
}

// named adds the project name to a not-found error. The name lookup is not
// free, so it runs only here.
func (p *project) named(err error) error {
	var nf *NotFoundError
	if !errors.As(err, &nf) || nf.Project != "" {
		return err
	}
	name := p.host.ProjectName(p.id)
	if name == "" {
		name = p.fallback
	}
	if name == "" {
		return err
	}
	return &NotFoundError{ID: nf.ID, Project: name}
}

func (s *Service) update(p *project, fn func(*Backlog) ([]int, error)) (*Backlog, error) {
	var ids []int
	b, err := Update(p.id, func(b *Backlog) error {
		var err error
		ids, err = fn(b)
		return err
	})
	if err != nil {
		return nil, p.named(err)
	}
	s.host.Changed(p.id, ids)
	return b, nil
}

func (s *Service) Get(req GetRequest) (*View, error) {
	p, err := s.resolve(req.RepoID, req.RepoPath)
	if err != nil {
		return nil, err
	}
	b, err := Load(p.id)
	if err != nil {
		return nil, err
	}
	states := b.DeriveAll(s.host.Facts(p.id))
	var gate Gate
	if b.Settings.Runner {
		gate = s.host.Gate(p.id)
	}
	runner := b.PlanRunner(RunnerInput{States: states, Used: s.host.SlotsUsed(p.id), Gate: gate, Limit: -1}).View
	return &View{Backlog: b, States: states, Runner: runner}, nil
}

// USD formats dollars the way every surface shows them: $10.40.
func USD(v float64) string { return fmt.Sprintf("$%.2f", v) }

func (s *Service) Add(req AddRequest) (*AddResponse, error) {
	p, err := s.resolve(req.RepoID, req.RepoPath)
	if err != nil {
		return nil, err
	}
	var ids map[string]int
	var order []int
	b, err := s.update(p, func(b *Backlog) ([]int, error) {
		now := s.now()
		if req.Before != 0 || req.After != 0 {
			it, err := addOne(b, req, now)
			if err != nil {
				return nil, err
			}
			ids = map[string]int{req.Items[0].Key: it.ID}
			order = []int{it.ID}
			return order, nil
		}
		existed := map[int]bool{}
		for _, it := range b.Items {
			existed[it.ID] = true
		}
		var err error
		if ids, err = b.AddBatch(Batch{Proposal: req.Proposal, Items: req.Items}, now); err != nil {
			return nil, err
		}
		touched := map[int]bool{}
		for _, id := range ids {
			touched[id] = true
		}
		for _, it := range b.Items {
			if !existed[it.ID] || touched[it.ID] {
				order = append(order, it.ID)
			}
		}
		return order, nil
	})
	if err != nil {
		return nil, err
	}
	out := &AddResponse{IDs: ids, Items: []Item{}}
	for _, id := range order {
		out.Items = append(out.Items, *b.Item(id))
	}
	return out, nil
}

// addOne adds a single item next to a sibling. Refs must be item ids, since
// there is no batch to hold keys.
func addOne(b *Backlog, req AddRequest, now time.Time) (*Item, error) {
	if len(req.Items) != 1 || req.Proposal != "" {
		return nil, errors.New("before and after place one item; they cannot go with a batch or a proposal")
	}
	x := req.Items[0]
	if x.Parent.Key != "" {
		return nil, fmt.Errorf("parent %s must be an item id", x.Parent)
	}
	in := NewItem{Title: x.Title, Body: x.Body, Parent: x.Parent.ID, Program: x.Program,
		Sequential: x.Sequential, Before: req.Before, After: req.After}
	for _, r := range x.WaitsOn {
		if r.Key != "" {
			return nil, fmt.Errorf("waits on %s must be an item id", r)
		}
		in.WaitsOn = append(in.WaitsOn, r.ID)
	}
	return b.Add(in, now)
}

func (s *Service) UpdateItem(req UpdateItemRequest) (*ItemResponse, error) {
	p, err := s.resolve(req.RepoID, req.RepoPath)
	if err != nil {
		return nil, err
	}
	b, err := s.update(p, func(b *Backlog) ([]int, error) {
		_, err := b.Edit(req.ID, Edit{Title: req.Title, Body: req.Body, Program: req.Program,
			WaitsOn: req.WaitsOn, Sequential: req.Sequential, Paused: req.Paused, Archived: req.Archived}, s.now())
		return []int{req.ID}, err
	})
	if err != nil {
		return nil, err
	}
	return &ItemResponse{Item: *b.Item(req.ID)}, nil
}

func (s *Service) Move(req MoveRequest) error {
	p, err := s.resolve(req.RepoID, req.RepoPath)
	if err != nil {
		return err
	}
	_, err = s.update(p, func(b *Backlog) ([]int, error) {
		it, err := b.get(req.ID)
		if err != nil {
			return nil, err
		}
		from := it.Parent
		mv := Move{Before: req.Before, After: req.After, Into: req.Into, Top: req.Top, Bottom: req.Bottom}
		if err := b.Move(req.ID, mv, s.now()); err != nil {
			return nil, err
		}
		ids := []int{req.ID}
		for _, parent := range []int{from, b.Item(req.ID).Parent} {
			if parent != 0 && !slices.Contains(ids, parent) {
				ids = append(ids, parent)
			}
		}
		return ids, nil
	})
	return err
}

func (s *Service) SetItemStatus(req SetItemStatusRequest) (*ItemResponse, error) {
	p, err := s.resolve(req.RepoID, req.RepoPath)
	if err != nil {
		return nil, err
	}
	facts := s.host.Facts(p.id)
	runActive := func(it *Item) bool {
		r := it.lastRun()
		return r != nil && facts != nil && facts(r.SessionID).RunActive
	}
	var asked map[int]string
	var askedRef map[int]openQuestion
	b, err := s.update(p, func(b *Backlog) ([]int, error) {
		asked = b.questionSessions()
		askedRef = map[int]openQuestion{}
		for id := range asked {
			qid, text := b.OpenQuestionRef(id)
			askedRef[id] = openQuestion{qid, text}
		}
		by := ActorUser
		if req.SessionID != "" {
			if err := checkAgentReview(b, req); err != nil {
				return nil, err
			}
			by = ActorAgent
		} else if req.PR != "" {
			return nil, errors.New("pr goes with session_id: only the agent's done sets it")
		}
		changed, err := b.SetStatus(req.ID, req.Status, by, runActive, s.now())
		if err != nil {
			return nil, err
		}
		if req.PR != "" {
			b.Item(req.ID).PR = req.PR
		}
		still := b.questionSessions()
		for id, sessionID := range asked {
			if _, open := still[id]; !open {
				s.host.SetAttention(p.id, sessionID, 0, "")
				continue
			}
			// The oldest open question changed (send-to-review closed af's own
			// question and left the agent's): the mirror and the remembered
			// identity follow it, or a terminal answer fails the identity check.
			if qid, text := b.OpenQuestionRef(id); (openQuestion{qid, text}) != askedRef[id] {
				s.host.SetAttention(p.id, sessionID, qid, text)
			}
		}
		return changed, nil
	})
	if err != nil {
		return nil, err
	}
	return &ItemResponse{Item: *b.Item(req.ID)}, nil
}

// openQuestion is the identity of an item's oldest open question.
type openQuestion struct {
	id   int
	text string
}

func checkAgentReview(b *Backlog, req SetItemStatusRequest) error {
	it, err := b.get(req.ID)
	if err != nil {
		return err
	}
	if req.Status != StatusReview {
		return fmt.Errorf("an item session can only send its item to review, not set it to %s", req.Status)
	}
	if r := it.lastRun(); r == nil || r.SessionID != req.SessionID {
		return fmt.Errorf("session %s is not the latest run of %s", req.SessionID, FormatID(req.ID))
	}
	return nil
}

func (it *Item) hasRun(sessionID string) bool {
	for _, r := range it.Runs {
		if r.SessionID == sessionID {
			return true
		}
	}
	return false
}

func (s *Service) DeleteItem(req DeleteItemRequest) error {
	p, err := s.resolve(req.RepoID, req.RepoPath)
	if err != nil {
		return err
	}
	_, err = s.update(p, func(b *Backlog) ([]int, error) {
		return b.Delete(req.ID)
	})
	return err
}

func (s *Service) SetSettings(req SetSettingsRequest) (*SettingsResponse, error) {
	p, err := s.resolve(req.RepoID, req.RepoPath)
	if err != nil {
		return nil, err
	}
	b, err := s.update(p, func(b *Backlog) ([]int, error) {
		return []int{}, b.ApplySettings(SettingsChange{Runner: req.Runner, MaxRunning: req.MaxRunning, OnDone: req.OnDone})
	})
	if err != nil {
		return nil, err
	}
	return &SettingsResponse{Settings: b.Settings}, nil
}

func (s *Service) GetBudget(GetBudgetRequest) (*BudgetResponse, error) {
	b, err := LoadBudget()
	if err != nil {
		return nil, err
	}
	return &BudgetResponse{Budget: *b, Usage: s.host.Usage()}, nil
}

func (s *Service) SetBudget(req SetBudgetRequest) (*BudgetResponse, error) {
	b, err := UpdateBudget(req.Apply)
	if err != nil {
		return nil, err
	}
	return &BudgetResponse{Budget: *b, Usage: s.host.Usage()}, nil
}
