package agentproto

import (
	"encoding/binary"
	"fmt"
)

// Opcode is the first byte of a binary WS frame on the PTY stream (§4.2). The
// opcode discriminates the payload so the hot path carries raw PTY bytes without
// the base64/JSON bloat a text frame would impose.
type Opcode byte

const (
	// OpPTYOut (server → client) prefixes verbatim PTY output bytes. The client
	// feeds them to its terminal emulator (ui/termpane, xterm.js); the server
	// never renders a grid.
	OpPTYOut Opcode = 0x00
	// OpInput (client → server) prefixes raw key bytes. Multi-writer: the server
	// accepts input from ANY connected client — there is no lease gating (§4.2,
	// superseded by Sachin's multi-writer decision).
	OpInput Opcode = 0x01
	// OpResize (client → server) carries a rows,cols uint16 pair (see
	// resizePayloadLen) from any connected client; the server applies
	// last-resize-wins and echoes the authoritative size back as a ResizeMessage.
	OpResize Opcode = 0x02
)

// resizePayloadLen is the fixed body size of an OpResize frame: two big-endian
// uint16s, rows then cols.
const resizePayloadLen = 4

// String renders an opcode for diagnostics.
func (o Opcode) String() string {
	switch o {
	case OpPTYOut:
		return "PTY_OUT"
	case OpInput:
		return "INPUT"
	case OpResize:
		return "RESIZE"
	default:
		return fmt.Sprintf("Opcode(0x%02x)", byte(o))
	}
}

// Frame is a decoded binary PTY frame. For OpPTYOut and OpInput the payload is in
// Data; for OpResize the size is in Rows/Cols and Data is nil. Build frames with
// PTYOutFrame/InputFrame/ResizeFrame and serialize with Encode; DecodeFrame is the
// inverse, so DecodeFrame(f.Encode()) round-trips f.
type Frame struct {
	Op   Opcode
	Data []byte // OpPTYOut, OpInput: raw payload. OpResize: nil.
	Rows uint16 // OpResize only.
	Cols uint16 // OpResize only.
}

// PTYOutFrame wraps verbatim PTY output bytes (server → client).
func PTYOutFrame(b []byte) Frame { return Frame{Op: OpPTYOut, Data: b} }

// InputFrame wraps raw key bytes (client → server, accepted from any client).
func InputFrame(b []byte) Frame { return Frame{Op: OpInput, Data: b} }

// ResizeFrame wraps a terminal size (client → server, accepted from any client;
// last-resize-wins).
func ResizeFrame(rows, cols uint16) Frame {
	return Frame{Op: OpResize, Rows: rows, Cols: cols}
}

// Encode serializes the frame to its opcode-prefixed wire form.
func (f Frame) Encode() []byte {
	switch f.Op {
	case OpResize:
		out := make([]byte, 1+resizePayloadLen)
		out[0] = byte(OpResize)
		binary.BigEndian.PutUint16(out[1:3], f.Rows)
		binary.BigEndian.PutUint16(out[3:5], f.Cols)
		return out
	default:
		out := make([]byte, 1+len(f.Data))
		out[0] = byte(f.Op)
		copy(out[1:], f.Data)
		return out
	}
}

// DecodeFrame parses an opcode-prefixed binary frame. It errors on an empty frame,
// an unknown opcode, or a malformed OpResize body.
func DecodeFrame(raw []byte) (Frame, error) {
	if len(raw) == 0 {
		return Frame{}, fmt.Errorf("agentproto: empty binary frame")
	}
	op := Opcode(raw[0])
	body := raw[1:]
	switch op {
	case OpPTYOut, OpInput:
		return Frame{Op: op, Data: body}, nil
	case OpResize:
		if len(body) != resizePayloadLen {
			return Frame{}, fmt.Errorf("agentproto: RESIZE frame body is %d bytes, want %d", len(body), resizePayloadLen)
		}
		return Frame{
			Op:   OpResize,
			Rows: binary.BigEndian.Uint16(body[0:2]),
			Cols: binary.BigEndian.Uint16(body[2:4]),
		}, nil
	default:
		return Frame{}, fmt.Errorf("agentproto: unknown opcode 0x%02x", byte(op))
	}
}
