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

	// opLock→repoStartLock is the ordering the kill/archive paths use: the write
	// below persists the whole row, so without the op lock it can checkpoint a
	// handoff's provisional swap state that the swap later reverts in memory.
	opLock := m.opLockFor(daemonInstanceKey(repoID, title))
	opLock.Lock()
	defer opLock.Unlock()
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
	m.queueAttentionClear(instance, seen.SetAt)
}

// answerAttentionByWork clears the open attention request when the agent shows
// its own in-turn indicator after the idle spell the request waited out. epoch
// is the one the observing poll decided at; a request raised later is not
// answered by a frame captured before it.
func (m *Manager) answerAttentionByWork(instance *session.Instance, epoch uint64) {
	if setAt, ok := instance.AttentionAnsweredByWork(epoch); ok {
		m.queueAttentionClear(instance, setAt)
	}
}

// queueAttentionClear schedules a keyed clear of the request set at setAt.
// Keys name the request an answer saw, so a newer key always wins over a queued
// older one: the older clear is already a no-op on the request that replaced
// it, and losing the newer answer leaves an answered request standing. An older
// or equal key — a prompt answer captured before a newer answer landed —
// changes nothing. The worker drains whatever the newest key names.
func (m *Manager) queueAttentionClear(instance *session.Instance, setAt time.Time) {
	for {
		v, ok := m.attentionClears.Load(instance)
		if !ok {
			if _, busy := m.attentionClears.LoadOrStore(instance, setAt); !busy {
				m.runAttentionClearWorker(instance)
				return
			}
			continue
		}
		queued := v.(time.Time)
		if !setAt.After(queued) || m.attentionClears.CompareAndSwap(instance, queued, setAt) {
			return
		}
	}
}

// runAttentionClearWorker drains the newest keyed clear for instance: load the
// key, clear that request under the repo lock, and delete the key only if it is
// still the same one — a newer key meanwhile means there is another to do.
func (m *Manager) runAttentionClearWorker(instance *session.Instance) {
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
	// Same opLock→repoStartLock ordering as SetAttention: the whole-row persist
	// must not interleave with a handoff's provisional swap state.
	opLock := m.opLockFor(daemonInstanceKey(repoID, instance.Title))
	opLock.Lock()
	defer opLock.Unlock()
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
// user submits a line in the agent pane. A keystroke alone does not clear it,
// so focusing the pane and typing a draft keeps the request.
type attentionClearingBinding struct {
	ptyTabBinding
	instance *session.Instance
	clear    func(time.Time)
	// submit follows bracketed-paste state across this connection's frames; nil
	// (a binding built without one) judges each frame alone.
	submit *submitDetector
}

func (b attentionClearingBinding) input(p []byte) error {
	// The submitted line answers the request set when it was sent, so capture it
	// before transport: a request that lands while input is in flight is a
	// different question and must survive this clear.
	var answered time.Time
	if seen := b.instance.Attention(); seen != nil {
		answered = seen.SetAt
	}
	submitted := b.submit.Submitted(p) // every frame, so paste state stays current
	err := b.ptyTabBinding.input(p)
	if err == nil && submitted && !answered.IsZero() {
		b.clear(answered)
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

// isSubmittedInput reports whether one frame, judged alone, ends a line. A real
// connection uses a submitDetector, which carries paste state between frames.
func isSubmittedInput(p []byte) bool { return (&submitDetector{}).Submitted(p) }

// submitDetector decides, frame by frame, whether the user submitted a line. A
// newline inside a bracketed paste is part of the pasted text, and a paste can
// span frames (the CLI attach reads stdin 32 bytes at a time) with its markers
// split across them, so the state lives per connection: whether a paste is open,
// and the tail of a marker that may be completed by the next frame.
// Not safe for concurrent use; a connection feeds it from one reader loop.
type submitDetector struct {
	inPaste bool
	carry   []byte
}

// Submitted feeds one input frame and reports whether it contains a submitting
// newline outside any paste. A nil detector judges the frame alone.
func (d *submitDetector) Submitted(p []byte) bool {
	if d == nil {
		d = &submitDetector{}
	}
	idle := !d.inPaste && len(d.carry) == 0
	startsPaste := len(p) < len(bracketedPasteStart) && bytes.HasPrefix(bracketedPasteStart, p)
	if idle && !startsPaste && !isTypedInput(p) {
		return false // a terminal's own reply (focus, cursor, device query), not the user
	}
	buf := append(d.carry, p...)
	d.carry = nil
	submitted := false
	for i := 0; i < len(buf); {
		rest := buf[i:]
		if d.inPaste {
			j := bytes.Index(rest, bracketedPasteEnd)
			if j < 0 {
				d.carry = partialMarker(rest, bracketedPasteEnd)
				return submitted
			}
			i += j + len(bracketedPasteEnd)
			d.inPaste = false
			continue
		}
		if bytes.HasPrefix(rest, bracketedPasteStart) {
			i += len(bracketedPasteStart)
			d.inPaste = true
			continue
		}
		if len(rest) < len(bracketedPasteStart) && bytes.HasPrefix(bracketedPasteStart, rest) {
			d.carry = append([]byte(nil), rest...) // may become a paste start in the next frame
			return submitted
		}
		if rest[0] == '\r' || rest[0] == '\n' {
			submitted = true
		}
		i++
	}
	return submitted
}

// partialMarker is the longest suffix of b that is a proper prefix of marker.
func partialMarker(b, marker []byte) []byte {
	for n := min(len(marker)-1, len(b)); n > 0; n-- {
		if bytes.HasPrefix(marker, b[len(b)-n:]) {
			return append([]byte(nil), b[len(b)-n:]...)
		}
	}
	return nil
}
