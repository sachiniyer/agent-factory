package daemon

import (
	"context"
	"errors"
	"fmt"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/session"
)

func (s *controlServer) OpenBacklogOrchestrator(req OpenBacklogOrchestratorRequest, resp *OpenBacklogOrchestratorResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	out, err := s.manager.OpenBacklogOrchestrator(req)
	if err != nil {
		return err
	}
	*resp = out
	return nil
}

type orchOpenResult struct {
	out backlog.OpenOrchestratorResponse
	err error
}

// OpenBacklogOrchestrator names the session to open for group req.ID: its
// orchestrator, restored first when archived, lost or dead; a replacement
// bound in its place when it is gone; or a new session bound to a group with
// none. It holds the group's delivery claim, so no delivery races it. A
// session it starts or restores opens no orchestrator turn: the user opens it,
// so the end of what it does first does not archive it.
func (m *Manager) OpenBacklogOrchestrator(req backlog.OpenOrchestratorRequest) (backlog.OpenOrchestratorResponse, error) {
	svc := m.backlogService()
	ref, err := svc.Resolve(req.RepoID, req.RepoPath)
	if err != nil {
		return backlog.OpenOrchestratorResponse{}, err
	}
	if !m.backlog.beginOrchestrate(ref.ID, req.ID) {
		return backlog.OpenOrchestratorResponse{}, fmt.Errorf("af is already working on the effort session of %s; try again in a moment", backlog.FormatID(req.ID))
	}
	done := make(chan orchOpenResult, 1)
	if !m.launchBackgroundMutation(func(stop <-chan struct{}) {
		defer m.backlog.endOrchestrate(ref.ID, req.ID)
		out, err := m.openOrchestrator(stop, ref, req)
		done <- orchOpenResult{out, svc.Named(ref, err)}
	}) {
		m.backlog.endOrchestrate(ref.ID, req.ID)
		return backlog.OpenOrchestratorResponse{}, errors.New("af is shutting down")
	}
	r := <-done
	return r.out, r.err
}

func (m *Manager) openOrchestrator(stop <-chan struct{}, ref backlog.ProjectRef, req backlog.OpenOrchestratorRequest) (backlog.OpenOrchestratorResponse, error) {
	var none backlog.OpenOrchestratorResponse
	b, err := backlog.Load(ref.ID)
	if err != nil {
		return none, err
	}
	o, err := b.OrchestratorToOpen(req.ID)
	if err != nil {
		return none, err
	}
	if o == nil {
		return m.startOrchestrator(stop, ref, b, req.ID, nil)
	}
	s, ok := m.projectSession(ref.ID, o.SessionID)
	if ok && !s.view.UserKilled && s.view.InFlightOp != session.OpKilling {
		return m.restoreOrchestrator(stop, ref.ID, b, req.ID, o, s)
	}
	if !m.orchestratorReady(ref.ID) {
		return none, errors.New("af is still loading this project's sessions; try again in a moment")
	}
	return m.startOrchestrator(stop, ref, b, req.ID, o)
}

// restoreOrchestrator restores group's orchestrator o when it needs it. One
// that comes back without its agent conversation, or that never had its start
// prompt, gets the start prompt; its pending events wait for the next
// delivery. As the deliverer does, af asks before the restore whether the
// conversation resumes, and marks the session unstarted first, so a start
// prompt that is then held or not delivered is sent in full by that delivery.
func (m *Manager) restoreOrchestrator(stop <-chan struct{}, repoID string, b *backlog.Backlog, group int, o *backlog.Orchestrator, s itemSession) (backlog.OpenOrchestratorResponse, error) {
	var none backlog.OpenOrchestratorResponse
	out := backlog.OpenOrchestratorResponse{SessionID: s.instance.ID, Title: s.instance.Title}
	if !backlogNeedsRestore(s) {
		return out, nil
	}
	kind := o.Unstarted
	if exact, _ := backlogExactResume(s.instance); !exact {
		if kind == "" {
			kind = backlog.OrchUnstartedRestarted
		}
		svc := m.backlogService()
		if err := m.applyRetrying(repoID, stop, func(b *backlog.Backlog) ([]int, error) {
			return []int{group}, b.MarkOrchUnstarted(group, o.SessionID, kind, svc.Now())
		}); err != nil {
			return none, err
		}
	}
	restored, err := restoreBacklogSession(m, RestoreSessionRequest{ID: s.instance.ID, RepoID: repoID})
	if err != nil {
		return none, fmt.Errorf("af could not restore effort session %q: %w", s.instance.Title, err)
	}
	m.publishEvent(agentproto.EventSessionRestored, restored)
	out.Change = backlog.OrchOpenRestored
	if kind != "" {
		m.sendOpenedStartPrompt(stop, repoID, b, group, out.SessionID, kind)
	}
	return out, nil
}

// sendOpenedStartPrompt sends the start prompt, without events, to group's
// orchestrator sessionID, which af restored for the user to open. It opens no
// turn. A confirmed send clears the session's unstarted mark; any other
// outcome leaves it for the next delivery.
func (m *Manager) sendOpenedStartPrompt(stop <-chan struct{}, repoID string, b *backlog.Backlog, group int, sessionID, kind string) {
	s, ok := m.projectSession(repoID, sessionID)
	if !ok {
		return
	}
	prompt := b.OrchStartPrompt(group, proposalDir(repoID, b.Item(group)), kind, nil)
	if err := m.sendOrchestratorPrompt(repoID, group, s.instance, prompt, false); err != nil {
		m.warn().Printf("send the start prompt to restored effort session %q: %v", s.instance.Title, err)
		return
	}
	svc := m.backlogService()
	if err := m.applyRetrying(repoID, stop, func(b *backlog.Backlog) ([]int, error) {
		if !b.OrchDelivered(group, sessionID, nil, svc.Now()) {
			return nil, errBacklogNoChange
		}
		return []int{group}, nil
	}); err != nil && !errors.Is(err, errBacklogNoChange) {
		m.warn().Printf("record the start prompt of effort session %q: %v", s.instance.Title, err)
	}
}

// startOrchestrator starts a session for group and binds it: in place of
// prev, or as the group's first orchestrator when prev is nil. The group's
// pending events stay queued for the next delivery. A replacement the
// deliverer started before a restart could bind it is bound instead of a
// second one, and a replacement this starts is recorded before the create, as
// the deliverer records its own, so a daemon that stops before the bind adopts
// it on its next pass.
func (m *Manager) startOrchestrator(stop <-chan struct{}, ref backlog.ProjectRef, b *backlog.Backlog, group int, prev *backlog.Orchestrator) (backlog.OpenOrchestratorResponse, error) {
	var none backlog.OpenOrchestratorResponse
	g := b.Item(group)
	kind, program := backlog.OrchUnstartedNew, g.Program
	if prev != nil {
		kind, program = backlog.OrchUnstartedReplaced, prev.Program
		if prev.Replacing != nil {
			if data, ok := m.adoptableReplacement(ref.ID, b, prev.Replacing); ok {
				next := backlog.Orchestrator{SessionID: data.ID, Title: data.Title, Program: program, Unstarted: backlog.OrchUnstartedReplaced}
				return m.bindOpenedOrchestrator(stop, ref.ID, group, prev, next, false)
			}
		}
	}
	root, err := backlogStartRoot(ref.ID, "")
	if err == nil && program != "" {
		err = validateCreateProgram(program)
	}
	create := CreateSessionRequest{
		TitleBase: orchestratorTitleBase(g),
		RepoPath:  root,
		Program:   program,
		Prompt:    b.OrchStartPrompt(group, proposalDir(ref.ID, g), kind, nil),
		Backend:   string(session.BackendLocal),
	}
	if err == nil {
		err = validateBacklogCreate(create)
	}
	if err != nil {
		return none, fmt.Errorf("af could not start an effort session: %w", err)
	}
	if prev != nil {
		svc := m.backlogService()
		if err := m.applyRetrying(ref.ID, stop, func(b *backlog.Backlog) ([]int, error) {
			return []int{group}, b.BeginOrchReplace(group, prev.SessionID, create.TitleBase, svc.Now())
		}); err != nil {
			return none, err
		}
	}
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
	next := backlog.Orchestrator{Program: program}
	switch {
	case err != nil && isMutationCommitted(err) && data.ID != "" && data.StartupStateUnknown:
		// The create kept a session whose startup it could not confirm: it may be
		// running, so it is bound, unstarted, as the deliverer binds its own.
		m.warn().Printf("effort session %q for %s: %v; binding it, since it may be running", data.Title, backlog.FormatID(group), err)
		next.Unstarted = kind
	case err == nil && data.ID == "":
		err = errors.New("the daemon created no session")
		fallthrough
	case err != nil:
		return none, fmt.Errorf("af could not start an effort session: %w", err)
	case data.LastPromptDeliveryStatus == session.PromptNotDelivered:
		next.Unstarted = kind
	}
	next.SessionID, next.Title = data.ID, data.Title
	return m.bindOpenedOrchestrator(stop, ref.ID, group, prev, next, true)
}

// bindOpenedOrchestrator binds next to group, in place of prev when it is not
// nil. A busy backlog is retried until the write lands or stop closes, which
// leaves the session for the next pass. A session af created and could not
// bind otherwise is killed.
func (m *Manager) bindOpenedOrchestrator(stop <-chan struct{}, repoID string, group int, prev *backlog.Orchestrator, next backlog.Orchestrator, created bool) (backlog.OpenOrchestratorResponse, error) {
	var none backlog.OpenOrchestratorResponse
	svc := m.backlogService()
	err := m.applyRetrying(repoID, stop, func(b *backlog.Backlog) ([]int, error) {
		if prev != nil {
			return []int{group}, b.OrchReplaced(group, prev.SessionID, next, nil, svc.Now())
		}
		return []int{group}, b.BindNewOrchestrator(group, next, svc.Now())
	})
	if errors.Is(err, backlog.ErrBusy) {
		m.warn().Printf("bind effort session %q to %s: %v", next.Title, backlog.FormatID(group), err)
		return none, fmt.Errorf("af stopped before it could bind effort session %q to %s: %w", next.Title, backlog.FormatID(group), err)
	}
	if err != nil {
		if created {
			if killed, kerr := killOrchestratorSession(m, KillSessionRequest{ID: next.SessionID, Title: next.Title, RepoID: repoID}); kerr != nil {
				m.warn().Printf("kill unbound effort session %q: %v", next.Title, kerr)
			} else {
				m.publishEvent(agentproto.EventSessionKilled, killed)
			}
		}
		return none, err
	}
	ev := backlog.OrchestratorEvent{RepoID: repoID, ID: group, SessionID: next.SessionID, Title: next.Title}
	out := backlog.OpenOrchestratorResponse{SessionID: next.SessionID, Title: next.Title, Change: backlog.OrchOpenStarted}
	t := agentproto.EventBacklogOrchestratorBound
	if prev != nil {
		ev.Previous, out.Change, t = prev.SessionID, backlog.OrchOpenReplaced, agentproto.EventBacklogOrchestratorReplaced
	}
	m.publishEvent(t, ev)
	return out, nil
}
