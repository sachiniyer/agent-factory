package agentproto

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/terminal"
)

// TestControlMessageWireShapes pins the exact JSON each control frame serializes
// to — these are a public client-API contract (§4.2), so the bytes are load-bearing.
func TestControlMessageWireShapes(t *testing.T) {
	cases := []struct {
		name string
		msg  any
		want string
	}{
		{"resize", NewResizeMessage(24, 80), `{"type":"resize","rows":24,"cols":80}`},
		{"exit", NewExitMessage(0), `{"type":"exit","code":0}`},
		{"exit_nonzero", NewExitMessage(137), `{"type":"exit","code":137}`},
		// #2136: the tab-close exit is the SAME frame with an additive reason, and
		// the reasonless session-end exit above must stay byte-identical (omitempty)
		// so no existing client sees a changed wire shape.
		{"exit_tab_closed", NewTabClosedMessage(), `{"type":"exit","code":0,"reason":"tab_closed"}`},
		{"detach", NewDetachMessage(), `{"type":"detach"}`},
		// Input delivery accounting (server → client, additive). The ack carries
		// no payload beyond the type; the nack carries the applied byte count so
		// the client can re-send only the unapplied tail of the failing frame.
		{"input_ack", NewInputAckMessage(), `{"type":"input_ack"}`},
		{"input_nack", NewInputNackMessage(0), `{"type":"input_nack","applied":0}`},
		{"input_nack_partial", NewInputNackMessage(8192), `{"type":"input_nack","applied":8192}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(tc.msg)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("wire shape mismatch:\n got  = %s\n want = %s", got, tc.want)
			}
		})
	}
}

func TestTerminalModesMessageRoundTrip(t *testing.T) {
	want := NewTerminalModesMessageCoveringNextCursor(terminal.Modes{
		AlternateScreen: true,
		MouseTracking:   true,
		MouseButton:     true,
		MouseSGR:        true,
	})
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if typ, err := MessageTypeOf(raw); err != nil || typ != MsgTerminalModes {
		t.Fatalf("MessageTypeOf = %q, %v; want %q", typ, err, MsgTerminalModes)
	}
	var got TerminalModesMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

func TestFreshTerminalModesMessageOmitsCursorCoverage(t *testing.T) {
	raw, err := json.Marshal(NewTerminalModesMessage(terminal.Modes{}))
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(raw), "covers_next_cursor") {
		t.Fatalf("fresh mode snapshot granted recovery cursor coverage: %s", raw)
	}
}

func TestMessageTypeOf(t *testing.T) {
	raw, _ := json.Marshal(NewDetachMessage())
	got, err := MessageTypeOf(raw)
	if err != nil {
		t.Fatalf("MessageTypeOf: %v", err)
	}
	if got != MsgDetach {
		t.Fatalf("MessageTypeOf = %q, want %q", got, MsgDetach)
	}

	// A reader discriminates then unmarshals into the concrete type.
	rawResize, _ := json.Marshal(NewResizeMessage(40, 120))
	if tp, _ := MessageTypeOf(rawResize); tp != MsgResize {
		t.Fatalf("type = %q, want %q", tp, MsgResize)
	}
	var rm ResizeMessage
	if err := json.Unmarshal(rawResize, &rm); err != nil {
		t.Fatalf("unmarshal resize: %v", err)
	}
	if rm.Rows != 40 || rm.Cols != 120 {
		t.Fatalf("resize decoded to %dx%d, want 40x120", rm.Rows, rm.Cols)
	}
}

func TestMessageTypeOfErrors(t *testing.T) {
	if _, err := MessageTypeOf([]byte(`not json`)); err == nil {
		t.Error("MessageTypeOf(bad json) = nil error, want error")
	}
	if _, err := MessageTypeOf([]byte(`{"rows":24}`)); err == nil {
		t.Error("MessageTypeOf(no type) = nil error, want error")
	}
}

// An input nack carries how many bytes of the failing frame were applied, so the
// client can re-send only the unapplied tail rather than the whole backlog.
func TestInputNackRoundTrip(t *testing.T) {
	want := NewInputNackMessage(4096)
	raw, err := json.Marshal(want)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if typ, err := MessageTypeOf(raw); err != nil || typ != MsgInputNack {
		t.Fatalf("MessageTypeOf = %q, %v; want %q", typ, err, MsgInputNack)
	}
	var got InputNackMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Applied != 4096 {
		t.Fatalf("Applied = %d, want 4096", got.Applied)
	}
}

// An input ack acknowledges a single applied frame; it discriminates cleanly and
// carries nothing beyond its type.
func TestInputAckRoundTrip(t *testing.T) {
	raw, err := json.Marshal(NewInputAckMessage())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if typ, err := MessageTypeOf(raw); err != nil || typ != MsgInputAck {
		t.Fatalf("MessageTypeOf = %q, %v; want %q", typ, err, MsgInputAck)
	}
	var got InputAckMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Type != MsgInputAck {
		t.Fatalf("type = %q, want %q", got.Type, MsgInputAck)
	}
}

func TestEventRoundTrip(t *testing.T) {
	// A session.* event carries an opaque projection payload (a marshaled
	// session.InstanceData by contract); agentproto stays a leaf and treats it as
	// raw JSON. Use a stand-in payload to prove the envelope round-trips.
	type projection struct {
		Title    string `json:"title"`
		Liveness string `json:"liveness"`
	}
	in := projection{Title: "root", Liveness: "running"}
	ev, err := NewEvent(EventSessionUpdated, in)
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}

	wire, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}

	var decoded Event
	if err := json.Unmarshal(wire, &decoded); err != nil {
		t.Fatalf("unmarshal event: %v", err)
	}
	if decoded.Type != EventSessionUpdated {
		t.Fatalf("event type = %q, want %q", decoded.Type, EventSessionUpdated)
	}
	var out projection
	if err := json.Unmarshal(decoded.Data, &out); err != nil {
		t.Fatalf("unmarshal event data: %v", err)
	}
	if out != in {
		t.Fatalf("payload round-trip: got %+v, want %+v", out, in)
	}
}

func TestNewEventNilPayload(t *testing.T) {
	ev, err := NewEvent(EventTaskRemoved, nil)
	if err != nil {
		t.Fatalf("NewEvent(nil): %v", err)
	}
	if ev.Data != nil {
		t.Fatalf("nil payload should omit data, got %s", ev.Data)
	}
	got, _ := json.Marshal(ev)
	if string(got) != `{"type":"task.removed"}` {
		t.Fatalf("nil-payload event = %s", got)
	}
}
