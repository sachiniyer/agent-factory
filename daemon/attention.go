package daemon

import (
	"bytes"
	"context"
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
)

// SetAttentionRequest sets or clears a session's attention request. ID is the
// stable id and wins over {Title, RepoID} when set. Clear removes the request;
// otherwise Reason is an optional short note for the user. SetAt, with Clear,
// clears the request only while it is still the one set at that RFC 3339 time.
type SetAttentionRequest struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	RepoID string `json:"repo_id"`
	Reason string `json:"reason"`
	Clear  bool   `json:"clear"`
	SetAt  string `json:"set_at,omitempty"`
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
	return s.setAttention(context.Background(), req, resp)
}

// setAttention is the ctx-aware form the HTTP route calls: a sandbox callback
// credential may speak only for its own session, so the owner replaces every
// selector the request carried.
func (s *controlServer) setAttention(ctx context.Context, req SetAttentionRequest, resp *SetAttentionResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	if owner, isSandbox := sandboxOwner(ctx); isSandbox {
		req = SetAttentionRequest{ID: owner, Reason: req.Reason, Clear: req.Clear, SetAt: req.SetAt}
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
	var setAt time.Time
	if req.Clear && req.SetAt != "" {
		if setAt, err = time.Parse(time.RFC3339Nano, req.SetAt); err != nil {
			return "", "", fmt.Errorf("set_at %q is not an RFC 3339 time: %w", req.SetAt, err)
		}
	}

	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	defer repoStartLock.Unlock()

	var previous session.AttentionUndo
	if req.Clear {
		var cleared bool
		previous, cleared = instance.ClearAttention(setAt)
		if !cleared {
			return id, title, nil
		}
	} else {
		previous, err = instance.SetAttention(req.Reason, nowFunc())
		if err != nil {
			return "", "", fmt.Errorf("session %q is lost, dead, archived, or at a usage limit: %w", title, err)
		}
	}
	data := instance.ToInstanceData()
	if err := persistInstanceData(repoID, data); err != nil {
		instance.RestoreAttention(previous)
		return "", "", fmt.Errorf("save attention for session %q: %w", title, err)
	}
	m.publishEvent(agentproto.EventSessionUpdated, data)
	return id, title, nil
}

// runAttentionClear runs an input-driven clear off the keystroke path. Tests
// replace it to run the clear at a chosen moment.
var runAttentionClear = func(clear func()) { go clear() }

// clearAttentionOnInput clears the attention request when the user types into
// the agent pane. It is on the keystroke path, so it never waits: the common
// case is one read lock and a return, and the clear itself runs elsewhere,
// because it takes the repo start lock that a sibling's create holds for its
// whole run.
func (m *Manager) clearAttentionOnInput(instance *session.Instance) {
	if instance == nil {
		return
	}
	seen := instance.Attention()
	if seen == nil {
		return
	}
	// The map holds the newest request a keystroke saw. A key that lands while a
	// clear is in flight updates it, and the worker loops until it has cleared
	// the request it last read.
	if _, busy := m.attentionClears.Swap(instance, seen.SetAt); busy {
		return
	}
	runAttentionClear(func() {
		for {
			v, ok := m.attentionClears.Load(instance)
			if !ok {
				return
			}
			setAt := v.(time.Time)
			m.clearAttentionNow(instance, setAt)
			if m.attentionClears.CompareAndDelete(instance, setAt) {
				return
			}
		}
	})
}

// clearAttentionNow clears and persists the request set at setAt, under the
// repo start lock. A request set after the keystroke stays.
func (m *Manager) clearAttentionNow(instance *session.Instance, setAt time.Time) {
	repoID, ok := m.repoIDForInstance(instance)
	if !ok {
		return
	}
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	defer repoStartLock.Unlock()
	previous, cleared := instance.ClearAttention(setAt)
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

// The markers around a paste when the agent enabled bracketed-paste mode.
var (
	bracketedPasteStart = []byte("\x1b[200~")
	bracketedPasteEnd   = []byte("\x1b[201~")
)

// isTypedInput rejects frames that start with ESC, except a bracketed paste. The
// browser terminal answers focus, cursor, and device queries on its own with
// escape sequences, and those replies must not count as the user answering.
func isTypedInput(p []byte) bool {
	if rest, ok := bytes.CutPrefix(p, bracketedPasteStart); ok {
		return len(rest) > 0 && !bytes.Equal(rest, bracketedPasteEnd)
	}
	return len(p) > 0 && p[0] != 0x1b
}
