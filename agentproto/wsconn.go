package agentproto

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/coder/websocket"
)

// This file is the only binding between agentproto's wire types and the WebSocket
// transport (github.com/coder/websocket — context-aware, zero-dependency, actively
// maintained; the design's recommended choice, §9.5). The codec (frame.go) and the
// message types (message.go) stay transport-agnostic; these helpers move them over
// a *websocket.Conn so the daemon broker and every client share one read/write
// seam. No connection is opened here — Phase 2 PR1 wires nothing.

// WriteFrame sends a binary PTY frame (PTY_OUT/INPUT/RESIZE) as a WS binary
// message.
func WriteFrame(ctx context.Context, c *websocket.Conn, f Frame) error {
	return c.Write(ctx, websocket.MessageBinary, f.Encode())
}

// WriteControl marshals a JSON control frame (ResizeMessage, ExitMessage,
// DetachMessage) or an events-plane Event and sends it as a WS text message.
func WriteControl(ctx context.Context, c *websocket.Conn, msg any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("agentproto: marshal control frame: %w", err)
	}
	return c.Write(ctx, websocket.MessageText, b)
}

// Message is one decoded frame off the wire: a binary PTY Frame or a JSON control
// frame. Exactly one of Frame/Text is meaningful, selected by Binary.
type Message struct {
	// Binary reports whether this was a binary (PTY) frame; when false it was a
	// text (JSON control) frame.
	Binary bool
	// Frame is the decoded binary frame, valid only when Binary is true.
	Frame Frame
	// Text is the raw JSON payload of a control frame, valid only when Binary is
	// false; pass it to MessageTypeOf to discriminate, then unmarshal.
	Text []byte
}

// ReadMessage reads the next WS frame and decodes it. A binary frame is parsed
// through DecodeFrame; a text frame's raw JSON is returned in Message.Text for the
// caller to discriminate with MessageTypeOf. It errors on the read itself, on a
// malformed binary frame, or on an unexpected WS message type.
func ReadMessage(ctx context.Context, c *websocket.Conn) (Message, error) {
	typ, data, err := c.Read(ctx)
	if err != nil {
		return Message{}, err
	}
	switch typ {
	case websocket.MessageBinary:
		f, derr := DecodeFrame(data)
		if derr != nil {
			return Message{}, derr
		}
		return Message{Binary: true, Frame: f}, nil
	case websocket.MessageText:
		return Message{Binary: false, Text: data}, nil
	default:
		return Message{}, fmt.Errorf("agentproto: unexpected WS message type %v", typ)
	}
}
