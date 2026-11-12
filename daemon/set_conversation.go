package daemon

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
)

// SetConversationRequest asks the daemon to record a provider conversation id
// on a session's Agent tab — the operator's repair for a Codex conversation
// that af never captured or captured stale (#6769). The verb validates the id
// against the session's own rollout store before writing, so a request can
// only land a conversation the store itself proves belongs to the session.
//
// Title+RepoID is the CLI lookup shape; ID is the session's stable id for
// clients that hold one. ConversationID is the provider id to record — for
// Codex, the UUID in its rollout-…-<uuid>.jsonl filename.
type SetConversationRequest struct {
	Title          string `json:"title"`
	RepoID         string `json:"repo_id"`
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
}

type SetConversationResponse struct {
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
	// The write is the mutation: a transport error after persist must still
	// read as committed, never as a refusal worth retrying blindly.
	MutationOutcome
}

func (s *controlServer) SetConversation(req SetConversationRequest, resp *SetConversationResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	return s.setConversationGuarded(req, resp, nil)
}

// setConversationGuarded runs the verb behind an optional guard, re-checked
// inside the op lock on the instance the manager holds right now. Sandbox
// scoping passes sandboxSessionGuard so a row replaced between authorization
// and commit is refused rather than written under a stale identity.
func (s *controlServer) setConversationGuarded(req SetConversationRequest, resp *SetConversationResponse, guard func(*session.Instance, string) error) error {
	err := s.manager.setConversation(req, guard)
	resp.OK = err == nil
	if !resp.MutationOutcome.record(err) {
		return err
	}
	if !resp.OK && resp.Reason == "" {
		resp.Reason = "the session changed before the conversation could be recorded"
	}
	return nil
}

// setConversation writes req's proven conversation id onto the resolved
// session's Agent tab. Live rows are allowed — the write only changes what af
// records, never the running pane — EXCEPT while an operation or a committed
// account swap is in flight, whose own conversation bookkeeping must settle
// first; the refusal names the owner so the operator knows what to wait for.
func (m *Manager) setConversation(req SetConversationRequest, guard func(*session.Instance, string) error) error {
	if strings.TrimSpace(req.ConversationID) == "" {
		return fmt.Errorf("a conversation id is required")
	}
	instance, repoID, title, _, _, err := m.resolveActionSession(req.ID, req.Title, req.RepoID)
	if err != nil {
		return err
	}
	if instance == nil {
		return fmt.Errorf("session %q not found", title)
	}
	key := daemonInstanceKey(repoID, instance.Title)
	opLock := m.opLockFor(key)
	opLock.Lock()
	defer opLock.Unlock()

	m.mu.Lock()
	current := m.instances[key]
	m.mu.Unlock()
	if current != instance {
		return fmt.Errorf("session %q changed while its operation lock was taken — retry", title)
	}
	if guard != nil {
		if err := guard(instance, repoID); err != nil {
			return err
		}
	}
	if op := instance.GetInFlightOp(); op != session.OpNone {
		return fmt.Errorf("session %q has a %s in flight — wait for it to settle, then retry", title, setConversationOpName(op))
	}
	if from, to, pending := instance.PendingAccountSwap(); pending {
		return fmt.Errorf("session %q has an account swap (%s → %s) in flight — wait for it to settle, then retry", title, from, to)
	}
	conv, err := session.ValidateSettableConversation(instance, req.ConversationID)
	if err != nil {
		return err
	}
	prev := instance.AgentConversation()
	if prev.Agent == conv.Agent && prev.ID == conv.ID {
		return nil // already recorded: same id is a no-op success
	}
	if !instance.SetAgentConversation(conv) {
		return nil
	}
	stampConversationRepair(instance, conv)

	// opLock→repoStartLock is the ordering SetAttention and the kill/archive
	// paths use: the write persists the whole row, so it must not checkpoint a
	// sibling operation's provisional state.
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	defer repoStartLock.Unlock()
	data := instance.ToInstanceData()
	if err := persistInstanceData(repoID, data); err != nil {
		instance.SetAgentConversation(prev)
		return fmt.Errorf("save conversation for session %q: %w", title, err)
	}
	m.publishEvent(agentproto.EventSessionUpdated, data)
	return nil
}

func setConversationOpName(op session.InFlightOp) string {
	switch op {
	case session.OpCreating:
		return "create"
	case session.OpKilling:
		return "kill"
	case session.OpArchiving:
		return "archive"
	case session.OpRestoring:
		return "restore"
	case session.OpReplacing:
		return "handoff"
	case session.OpRespawning:
		return "respawn"
	}
	return fmt.Sprintf("operation %d", int(op))
}
