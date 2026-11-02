package daemon

import (
	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/session"
)

func (h backlogHost) Session(repoID, sessionID string) (backlog.SessionInfo, bool) {
	s, ok := h.m.projectSession(repoID, sessionID)
	if !ok {
		return backlog.SessionInfo{}, false
	}
	return backlog.SessionInfo{Title: s.instance.Title, Program: s.instance.Program, Item: s.instance.BacklogItem,
		State: backlogSessionState(s.view)}, true
}

// projectSession finds any session of the project by id, read under one lock.
func (m *Manager) projectSession(repoID, sessionID string) (itemSession, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, inst := range m.instances {
		if inst == nil || inst.ID != sessionID {
			continue
		}
		if rid, _ := splitDaemonInstanceKey(key); rid == repoID {
			return itemSession{instance: inst, view: inst.LifecycleView()}, true
		}
	}
	return itemSession{}, false
}

func backlogSessionState(v session.LifecycleView) string {
	if v.UserKilled || v.InFlightOp == session.OpKilling {
		return backlog.SessionGone
	}
	switch v.Liveness {
	case session.LiveArchived:
		return backlog.SessionArchived
	case session.LiveDead:
		return backlog.SessionDead
	case session.LiveLost:
		return backlog.SessionLost
	case session.LiveLimitReached:
		return backlog.SessionLimited
	}
	return backlog.SessionLive
}

// publishOrchestratorChange sends backlog.orchestrator.bound or .replaced
// when a bind changed the group's session.
func (m *Manager) publishOrchestratorChange(repoID string, out *backlog.OrchestratorResponse) {
	if out == nil || out.Orchestrator == nil {
		return
	}
	var t agentproto.EventType
	switch out.Change {
	case backlog.OrchestratorBound:
		t = agentproto.EventBacklogOrchestratorBound
	case backlog.OrchestratorReplaced:
		t = agentproto.EventBacklogOrchestratorReplaced
	default:
		return
	}
	m.publishEvent(t, backlog.OrchestratorEvent{RepoID: repoID, ID: out.ID,
		SessionID: out.Orchestrator.SessionID, Title: out.Orchestrator.Title, Previous: out.Previous})
}

func (s *controlServer) SetBacklogOrchestrator(req SetBacklogOrchestratorRequest, resp *SetBacklogOrchestratorResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	svc := s.manager.backlogService()
	out, err := svc.SetOrchestrator(req)
	if err != nil {
		return err
	}
	*resp = *out
	if out.Change != "" {
		if ref, err := svc.Resolve(req.RepoID, req.RepoPath); err == nil {
			s.manager.publishOrchestratorChange(ref.ID, out)
		}
	}
	return nil
}

func (s *controlServer) SetBacklogOrchestratorHandoff(req SetBacklogOrchestratorHandoffRequest, resp *SetBacklogOrchestratorHandoffResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	out, err := s.manager.backlogService().SetOrchestratorHandoff(req)
	if err != nil {
		return err
	}
	*resp = *out
	return nil
}
