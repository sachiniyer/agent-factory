package daemon

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
)

// SetAttentionRequest sets or clears a session's attention request. ID is the
// stable id and wins over {Title, RepoID} when set. Clear removes the request;
// otherwise Reason is an optional short note for the user.
type SetAttentionRequest struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	RepoID string `json:"repo_id"`
	Reason string `json:"reason"`
	Clear  bool   `json:"clear"`
}

// SetAttentionResponse names the session the daemon resolved. OK is always
// true on a nil error.
type SetAttentionResponse struct {
	OK    bool   `json:"ok"`
	ID    string `json:"id"`
	Title string `json:"title"`
}

// SetAttention records or clears an agent's request for the user and
// publishes the row so every open client repaints it.
func (s *controlServer) SetAttention(req SetAttentionRequest, resp *SetAttentionResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	id, title, err := s.manager.SetAttention(req)
	if err != nil {
		return err
	}
	resp.OK = true
	resp.ID = id
	resp.Title = title
	return nil
}

// SetAttention applies req to the tracked session it resolves to.
func (m *Manager) SetAttention(req SetAttentionRequest) (string, string, error) {
	instance, repoID, title, id, _, err := m.resolveActionSession(req.ID, req.Title, req.RepoID)
	if err != nil {
		return "", "", err
	}
	if instance == nil {
		return "", "", fmt.Errorf("session %q is not loaded by the daemon", title)
	}
	switch instance.GetLiveness() {
	case session.LiveArchived, session.LiveLost, session.LiveDead:
		if !req.Clear {
			return "", "", fmt.Errorf("session %q is not running; attention applies only to a live agent", title)
		}
	}

	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	defer repoStartLock.Unlock()

	var previous *session.Attention
	if req.Clear {
		var cleared bool
		previous, cleared = instance.ClearAttention()
		if !cleared {
			return id, title, nil
		}
	} else {
		previous = instance.SetAttention(req.Reason, nowFunc())
	}
	data := instance.ToInstanceData()
	if err := persistInstanceData(repoID, data); err != nil {
		instance.RestoreAttention(previous)
		return "", "", fmt.Errorf("save attention for session %q: %w", title, err)
	}
	m.publishEvent(agentproto.EventSessionUpdated, data)
	return id, title, nil
}

// clearAttentionOnInput clears the attention request when the user types into
// the agent pane. It is on the keystroke path, so the common case is one
// read lock and a return.
func (m *Manager) clearAttentionOnInput(instance *session.Instance) {
	if instance == nil || !instance.HasAttention() {
		return
	}
	repoID, ok := m.repoIDForInstance(instance)
	if !ok {
		return
	}
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	defer repoStartLock.Unlock()
	previous, cleared := instance.ClearAttention()
	if !cleared {
		return
	}
	data := instance.ToInstanceData()
	if err := persistInstanceData(repoID, data); err != nil {
		instance.RestoreAttention(previous)
		m.warn().Printf("could not clear the attention request on session %q: %v", instance.Title, err)
		return
	}
	m.publishEvent(agentproto.EventSessionUpdated, data)
}

// attentionClearingBinding clears the session's attention request when the
// user types into the agent pane.
type attentionClearingBinding struct {
	ptyTabBinding
	clear func()
}

func (b attentionClearingBinding) input(p []byte) error {
	err := b.ptyTabBinding.input(p)
	if err == nil && isTypedInput(p) {
		b.clear()
	}
	return err
}

// isTypedInput rejects frames that start with ESC. The browser terminal answers
// focus, cursor, and device queries on its own with escape sequences, and those
// replies must not count as the user answering.
func isTypedInput(p []byte) bool {
	return len(p) > 0 && p[0] != 0x1b
}
