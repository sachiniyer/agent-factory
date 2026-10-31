package daemon

import (
	"errors"
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/proposal"
	"github.com/sachiniyer/agent-factory/session"
)

func (h proposalHost) SendPrompt(sessionID, prompt string) (proposal.Delivery, error) {
	status, err := h.m.SendPromptWithStatus(SendPromptRequest{ID: sessionID, Prompt: prompt})
	switch {
	case err != nil && isNotAttemptedErr(err):
		return proposal.DeliveryFailed, err
	case err != nil:
		return proposal.DeliveryUnconfirmed, err
	case status == session.PromptDelivered:
		return proposal.DeliveryConfirmed, nil
	case status == session.PromptNotDelivered:
		return proposal.DeliveryFailed, fmt.Errorf("the agent did not take the prompt; %s", notDeliveredMarker)
	}
	return proposal.DeliveryUnconfirmed, nil
}

func (h proposalHost) PublishPush(st proposal.PushStatus) {
	h.m.publishEvent(agentproto.EventProposalPush, st)
}

func (m *Manager) pushTargetOf(repoID string, instance *session.Instance) proposal.PushTarget {
	t := proposal.PushTarget{
		RepoID:    repoID,
		SessionID: instance.ID,
		Ready:     instance.GetLiveness() == session.LiveReady,
		Attached:  m.isPollPaused(repoID, instance.Title, instance.ID),
	}
	for _, tab := range instance.GetTabs() {
		if tab.Kind != session.TabKindDocs || tab.Proposal == "" {
			continue
		}
		if t.Modes == nil {
			t.Modes = map[string]proposal.PushMode{}
		}
		if _, seen := t.Modes[tab.Proposal]; !seen {
			mode, err := proposal.ParsePushMode(tab.PushMode)
			if err != nil {
				mode = proposal.PushAuto
			}
			t.Modes[tab.Proposal] = mode
		}
	}
	return t
}

func (m *Manager) pushTarget(sessionID string) (proposal.PushTarget, error) {
	if strings.TrimSpace(sessionID) == "" {
		return proposal.PushTarget{}, errors.New("session_id is required")
	}
	instance, repoID, title, _, _, err := m.resolveActionSession(sessionID, "", "")
	if err != nil {
		return proposal.PushTarget{}, err
	}
	if instance == nil {
		return proposal.PushTarget{}, fmt.Errorf("session %q is not loaded by the daemon", title)
	}
	return m.pushTargetOf(repoID, instance), nil
}

// pushProposals runs the push rules for every loaded session. The poll loop
// calls it after ScanProposals.
func (m *Manager) pushProposals() {
	type entry struct {
		repoID   string
		instance *session.Instance
	}
	m.mu.Lock()
	var instances []entry
	for key, instance := range m.instances {
		if instance.ID == "" {
			continue
		}
		rid, _ := splitDaemonInstanceKey(key)
		instances = append(instances, entry{rid, instance})
	}
	m.mu.Unlock()
	targets := make([]proposal.PushTarget, 0, len(instances))
	for _, e := range instances {
		targets = append(targets, m.pushTargetOf(e.repoID, e.instance))
	}
	m.proposalService().PushTick(targets)
}

// setProposalPushMode stores mode on the session's Docs tabs for its proposal
// and persists the roster.
func (m *Manager) setProposalPushMode(sessionID string, mode proposal.PushMode) (*proposal.PushStatus, error) {
	if strings.TrimSpace(sessionID) == "" {
		return nil, errors.New("session_id is required")
	}
	slug, err := m.proposalService().AttachedSlug(sessionID)
	if err != nil {
		return nil, err
	}
	instance, repoID, title, release, err := m.tabMutationTarget(sessionID, "", "",
		tabMutationLabels{action: "change the push mode", op: "push mode change"})
	if err != nil {
		return nil, err
	}
	previous := ""
	for _, tab := range instance.GetTabs() {
		if tab.Kind == session.TabKindDocs && tab.Proposal == slug {
			previous = tab.PushMode
			break
		}
	}
	if instance.SetDocsTabsPushMode(slug, string(mode)) == 0 {
		release()
		return nil, fmt.Errorf("session %q has no Proposal tab for proposal %s: run af proposal attach %s", title, slug, slug)
	}
	data := instance.ToInstanceData()
	if err := persistInstanceData(repoID, data); err != nil {
		instance.SetDocsTabsPushMode(slug, previous)
		release()
		return nil, fmt.Errorf("failed to persist the push mode: %w", err)
	}
	m.publishEvent(agentproto.EventSessionUpdated, data)
	release()
	t, err := m.pushTarget(sessionID)
	if err != nil {
		return nil, err
	}
	return m.proposalService().SetPushMode(t, mode)
}

func (s *controlServer) SetProposalPushMode(req SetProposalPushModeRequest, resp *SetProposalPushModeResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	mode, err := proposal.ParsePushMode(req.Mode)
	if err != nil || req.Mode == "" {
		return fmt.Errorf("invalid push mode %q: use auto or manual", req.Mode)
	}
	out, err := s.manager.setProposalPushMode(req.SessionID, mode)
	if err != nil {
		return err
	}
	*resp = *out
	return nil
}

func (s *controlServer) HoldProposalPush(req HoldProposalPushRequest, resp *HoldProposalPushResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	t, err := s.manager.pushTarget(req.SessionID)
	if err != nil {
		return err
	}
	out, err := s.manager.proposalService().HoldPush(t)
	if err != nil {
		return err
	}
	*resp = *out
	return nil
}

func (s *controlServer) SendProposalChanges(req SendProposalChangesRequest, resp *SendProposalChangesResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	t, err := s.manager.pushTarget(req.SessionID)
	if err != nil {
		return err
	}
	out, err := s.manager.proposalService().SendNow(t)
	if err != nil {
		return err
	}
	*resp = *out
	return nil
}
