package agentproto

import (
	"encoding/json"
	"fmt"
)

// MessageType is the discriminator of a JSON control frame carried as a WS text
// frame on the PTY stream (§4.2). Binary frames (frame.go) carry the hot path;
// these text frames carry size/lifecycle control.
//
// Multi-writer model (Sachin, supersedes the design doc's lease sections §3-Q3 /
// §4.2): af is single-owner, so there is NO attach lease and no interactive/observer
// distinction — every WS subscriber is an equal read-write client, and the server
// accepts INPUT/RESIZE from any of them. The only genuine multi-client conflict is
// terminal size (one PTY, one size), handled by last-resize-wins + an authoritative
// echo (MsgResize), not a lease. A lease could be re-introduced later as purely
// additive advisory frames without reshaping anything defined here; it is
// deliberately not built now.
type MessageType string

const (
	// MsgResize (server → client) is the authoritative size echo. The server sizes
	// the PTY to the MOST RECENT RESIZE it received (last-resize-wins) and echoes
	// that authoritative size so every other client reflows its emulator to match.
	MsgResize MessageType = "resize"
	// MsgExit (server → client) reports that the agent/PTY ended.
	MsgExit MessageType = "exit"
	// MsgDetach (client → server) is a clean-close signal; also implicit on socket
	// close.
	MsgDetach MessageType = "detach"
)

// ResizeMessage is the server's authoritative size echo (last-resize-wins, §6.2).
type ResizeMessage struct {
	Type MessageType `json:"type"`
	Rows uint16      `json:"rows"`
	Cols uint16      `json:"cols"`
}

// NewResizeMessage builds a size echo.
func NewResizeMessage(rows, cols uint16) ResizeMessage {
	return ResizeMessage{Type: MsgResize, Rows: rows, Cols: cols}
}

// ExitMessage reports the agent/PTY end code.
type ExitMessage struct {
	Type MessageType `json:"type"`
	Code int         `json:"code"`
}

// NewExitMessage builds an exit notice.
func NewExitMessage(code int) ExitMessage {
	return ExitMessage{Type: MsgExit, Code: code}
}

// DetachMessage is a client's clean-close signal.
type DetachMessage struct {
	Type MessageType `json:"type"`
}

// NewDetachMessage builds a detach request.
func NewDetachMessage() DetachMessage {
	return DetachMessage{Type: MsgDetach}
}

// MessageTypeOf peeks the "type" discriminator of a JSON control frame so a reader
// can pick the concrete struct to unmarshal into.
func MessageTypeOf(raw []byte) (MessageType, error) {
	var env struct {
		Type MessageType `json:"type"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return "", fmt.Errorf("agentproto: decode control frame: %w", err)
	}
	if env.Type == "" {
		return "", fmt.Errorf("agentproto: control frame missing type")
	}
	return env.Type, nil
}

// EventType is the discriminator of an events-plane message (§4.3), the WS/JSON
// fan-out of session/task state changes served at GET /v1/events. It lets a client
// replace Snapshot polling with push.
type EventType string

const (
	EventSessionCreated  EventType = "session.created"
	EventSessionUpdated  EventType = "session.updated"
	EventSessionKilled   EventType = "session.killed"
	EventSessionArchived EventType = "session.archived"
	EventSessionRestored EventType = "session.restored"
	// EventProjectsChanged signals that the set of "active projects" (repos with
	// live sessions or a root_agents opt-in) changed as a whole — e.g. a
	// DeleteProject archived a repo's sessions and dropped its opt-in (#1735). It
	// carries no payload: the project list is a derivation over the session
	// projection, so a client re-derives it from a fresh Snapshot rather than
	// patching a single row. Distinct from the per-session archived events the
	// same delete also emits (those update the rail); this one is the signal a
	// client keying a projects view resyncs on.
	EventProjectsChanged EventType = "projects.changed"
	EventTaskCreated     EventType = "task.created"
	EventTaskUpdated     EventType = "task.updated"
	EventTaskRemoved     EventType = "task.removed"
)

// Event is one message on the events plane. Data carries the same projection the
// existing RPCs return, encoded — deliberately as a raw payload rather than a
// typed field, so agentproto stays a leaf (no session/task import). By contract a
// session.* event's Data is a marshaled session.InstanceData (the Snapshot
// projection) and a task.* event's Data is a marshaled task.Task; the daemon
// encodes it and the client decodes it into those authoritative types.
type Event struct {
	Type EventType       `json:"type"`
	Data json.RawMessage `json:"data,omitempty"`
}

// NewEvent builds an event, marshaling payload into Data. A nil payload yields an
// event with no data member (e.g. a delete signalled by id in the type's
// convention, or a bare heartbeat).
func NewEvent(t EventType, payload any) (Event, error) {
	if payload == nil {
		return Event{Type: t}, nil
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return Event{}, fmt.Errorf("agentproto: marshal %s event: %w", t, err)
	}
	return Event{Type: t, Data: data}, nil
}
