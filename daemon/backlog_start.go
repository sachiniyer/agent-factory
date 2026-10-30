package daemon

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/proposal"
	"github.com/sachiniyer/agent-factory/session"
)

// createBacklogSession is the create an item start performs. A package var so
// tests can stand in for a real session create.
var createBacklogSession = func(m *Manager, ctx context.Context, req CreateSessionRequest) (session.InstanceData, error) {
	return m.CreateSession(ctx, req)
}

// validateBacklogCreate is the pre-create host check. A package var so tests
// that stand in for the create need not have an agent installed.
var validateBacklogCreate = validateCreateFirst

// sendBacklogPrompt sends "continue" to an item's live session.
//
// A runtime that observed the prompt absent reports PromptNotDelivered with a
// nil error; that is a failed continue, not a delivery, so it is returned as an
// error and continueBacklogItem disarms the run and restores the item (Codex P2
// on #6294).
var sendBacklogPrompt = func(m *Manager, req SendPromptRequest) error {
	status, err := m.SendPromptWithStatus(req)
	if err == nil && status == session.PromptNotDelivered {
		return errBacklogPromptNotDelivered
	}
	return err
}

var errBacklogPromptNotDelivered = errors.New("the agent did not take the prompt")

func (s *controlServer) StartBacklogItem(req StartBacklogItemRequest, resp *StartBacklogItemResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	run, err := s.manager.StartBacklogItem(req, backlog.StartedByUser)
	if err != nil {
		return err
	}
	resp.Run = run
	return nil
}

// StartBacklogItem starts item req.ID now. An item with a live session gets
// "continue" in that session; any other item gets a new session, created on a
// tracked goroutine, and the returned run is still starting.
func (m *Manager) StartBacklogItem(req backlog.StartRequest, by string) (backlog.RunRef, error) {
	svc := m.backlogService()
	ref, err := svc.Resolve(req.RepoID, req.RepoPath)
	if err != nil {
		return backlog.RunRef{}, err
	}
	b, err := backlog.Load(ref.ID)
	if err != nil {
		return backlog.RunRef{}, err
	}
	facts := backlogHost{m: m}.Facts(ref.ID)
	plan, err := b.BeginStart(req.ID, by, facts, svc.Now())
	if err != nil {
		return backlog.RunRef{}, svc.Named(ref, err)
	}
	if plan.Continue != "" {
		return m.continueBacklogItem(ref, req.ID, by, plan.Continue)
	}
	root := req.RepoPath
	if root == "" {
		if root, err = registeredProjectRootForRepoID(ref.ID); err != nil {
			return backlog.RunRef{}, err
		}
		if root == "" {
			return backlog.RunRef{}, fmt.Errorf("project %s is not registered; open it in af first", ref.ID)
		}
	}

	var create CreateSessionRequest
	m.backlog.reserve(ref.ID)
	_, err = svc.Apply(ref, func(b *backlog.Backlog) ([]int, error) {
		p, err := b.BeginStart(req.ID, by, facts, svc.Now())
		if err != nil {
			return nil, err
		}
		if p.Continue != "" {
			return nil, fmt.Errorf("%s changed while starting; retry", backlog.FormatID(req.ID))
		}
		plan = p
		it := b.Item(req.ID)
		if it.Program != "" {
			if err := validateCreateProgram(it.Program); err != nil {
				return nil, err
			}
		}
		create = CreateSessionRequest{
			TitleBase:   backlog.SessionTitleBase(it),
			RepoPath:    root,
			Program:     it.Program,
			Prompt:      b.ItemPrompt(it, proposalDir(ref.ID, it)),
			Backend:     string(session.BackendLocal),
			BacklogItem: it.ID,
		}
		// The host-owned checks `af sessions create` runs (the agent is
		// installed, tmux exists): fail the start here, with the actionable
		// message and no run recorded, instead of a session that dies while
		// waiting for an agent that cannot launch (found in the slice 4
		// play-test).
		if err := validateBacklogCreate(create); err != nil {
			return nil, err
		}
		return []int{req.ID}, nil
	})
	if err != nil {
		m.backlog.release(ref.ID)
		return backlog.RunRef{}, svc.Named(ref, err)
	}
	launched := m.launchBackgroundMutation(func(stop <-chan struct{}) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() {
			select {
			case <-stop:
				cancel()
			case <-ctx.Done():
			}
		}()
		data, err := createBacklogSession(m, ctx, create)
		m.finishBacklogStart(ref, req.ID, plan.Run.StartedAt, data, err, stop)
	})
	if !launched {
		m.finishBacklogStart(ref, req.ID, plan.Run.StartedAt, session.InstanceData{}, errors.New("af is shutting down"), nil)
	}
	return plan.Run, nil
}

func proposalDir(repoID string, it *backlog.Item) string {
	if it.Source == nil || it.Source.Proposal == "" {
		return ""
	}
	store, err := proposal.Open(repoID)
	if err != nil {
		return ""
	}
	return store.SlugPath(it.Source.Proposal)
}

// finishBacklogStart records a create's outcome on its starting run, then
// releases the slot the create held. A busy backlog is retried with backoff
// until the write lands or stop closes, since a run left starting holds the
// item until the next daemon start. A nil stop retries until the write lands.
func (m *Manager) finishBacklogStart(ref backlog.ProjectRef, id int, startedAt time.Time, data session.InstanceData, createErr error, stop <-chan struct{}) {
	defer m.backlog.release(ref.ID)
	if createErr == nil && data.ID == "" {
		createErr = errors.New("the daemon created no session")
	}
	svc := m.backlogService()
	delay := finishStartBackoff
	for {
		_, err := svc.Apply(ref, func(b *backlog.Backlog) ([]int, error) {
			return []int{id}, b.FinishStart(id, startedAt, data.ID, data.Title, createErr, svc.Now())
		})
		if err == nil {
			return
		}
		if !errors.Is(err, backlog.ErrBusy) {
			m.warn().Printf("record start of %s: %v", backlog.FormatID(id), err)
			return
		}
		select {
		case <-stop:
			m.warn().Printf("record start of %s: %v; restart recovery will settle it", backlog.FormatID(id), err)
			return
		case <-time.After(delay):
		}
		delay = min(delay*2, 5*time.Second)
	}
}

// finishStartBackoff is the first wait after a busy backlog. A var so tests can
// shorten it.
var finishStartBackoff = 100 * time.Millisecond

// continueBacklogItem sends "continue" to the item's live session. The item is
// set running first, under the backlog lock, so a second start sees it; then
// the session's run marker is armed and the prompt sent. A send that fails
// disarms the marker and puts the status back.
func (m *Manager) continueBacklogItem(ref backlog.ProjectRef, id int, by, sessionID string) (backlog.RunRef, error) {
	svc := m.backlogService()
	if !m.backlog.beginContinue(ref.ID, id) {
		return backlog.RunRef{}, svc.Named(ref, fmt.Errorf("%s is already starting", backlog.FormatID(id)))
	}
	defer m.backlog.endContinue(ref.ID, id)
	s, ok := m.itemSessions(ref.ID)[sessionID]
	if !ok {
		return backlog.RunRef{}, fmt.Errorf("the session for %s is gone; retry \"af backlog start %d\" to start a new one", backlog.FormatID(id), id)
	}
	inst := s.instance
	facts := backlogHost{m: m}.Facts(ref.ID)
	var (
		prev   backlog.Status
		prompt string
		armed  bool
	)
	out, err := svc.Apply(ref, func(b *backlog.Backlog) ([]int, error) {
		p, err := b.BeginStart(id, by, facts, svc.Now())
		if err != nil {
			return nil, err
		}
		if p.Continue != sessionID {
			return nil, fmt.Errorf("%s changed while starting; retry", backlog.FormatID(id))
		}
		it := b.Item(id)
		prev, prompt = it.Status, backlog.ContinuePrompt(it)
		if err := b.Continued(id, sessionID, svc.Now()); err != nil {
			return nil, err
		}
		// Armed inside the transaction, so a completion queued for the previous
		// run (backlogRunEndedGen) sees the new run or the old generation, never
		// the gap between accepting the continue and arming it.
		if !inst.ArmBacklogRun(time.Now()) {
			return nil, fmt.Errorf("session %q is not an item session", inst.Title)
		}
		armed = true
		return []int{id}, nil
	})
	if err != nil {
		return backlog.RunRef{}, svc.Named(ref, err)
	}
	var sendErr error
	if armed {
		m.persistAndPublishInstance(ref.ID, inst)
		sendErr = m.sendBacklogPromptLocked(SendPromptRequest{ID: inst.ID, Title: inst.Title, RepoID: ref.ID, Prompt: prompt})
		if sendErr != nil {
			m.disarmBacklogRun(ref.ID, inst)
			sendErr = fmt.Errorf("send continue to session %q: %w", inst.Title, sendErr)
		}
	}
	if sendErr != nil {
		if _, err := svc.Apply(ref, func(b *backlog.Backlog) ([]int, error) {
			return []int{id}, b.ContinueFailed(id, prev, sendErr, svc.Now())
		}); err != nil {
			m.warn().Printf("restore %s after a failed continue: %v", backlog.FormatID(id), err)
		}
		return backlog.RunRef{}, sendErr
	}
	runs := out.Item(id).Runs
	return runs[len(runs)-1], nil
}

// sendBacklogPromptLocked sends under the per-target lock task deliveries
// take, so backlog prompts never interleave with them.
func (m *Manager) sendBacklogPromptLocked(req SendPromptRequest) error {
	unlock := m.lockTarget(req.RepoID, req.Title)
	defer unlock()
	return sendBacklogPrompt(m, req)
}

func (m *Manager) disarmBacklogRun(repoID string, inst *session.Instance) {
	if inst.DisarmBacklogRun() {
		m.persistAndPublishInstance(repoID, inst)
	}
}
