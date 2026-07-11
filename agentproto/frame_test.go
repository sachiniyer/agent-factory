package agentproto

import (
	"bytes"
	"testing"
)

// frameEqual compares two frames, treating nil and empty Data as equal (Encode of
// an empty payload and DecodeFrame of a 1-byte frame differ only in nil-ness).
func frameEqual(a, b Frame) bool {
	return a.Op == b.Op && a.Rows == b.Rows && a.Cols == b.Cols && bytes.Equal(a.Data, b.Data)
}

func TestFrameRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		in   Frame
	}{
		{"pty_out", PTYOutFrame([]byte("hello \x1b[31mworld\x1b[0m\r\n"))},
		{"pty_out_empty", PTYOutFrame(nil)},
		{"pty_out_binary", PTYOutFrame([]byte{0x00, 0x01, 0x02, 0xff, 0xfe})},
		{"input", InputFrame([]byte("ls -la\n"))},
		{"input_ctrl", InputFrame([]byte{0x03})}, // Ctrl-C
		{"resize", ResizeFrame(24, 80)},
		{"resize_zero", ResizeFrame(0, 0)},
		{"resize_max", ResizeFrame(65535, 65535)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DecodeFrame(tc.in.Encode())
			if err != nil {
				t.Fatalf("DecodeFrame(Encode()) error: %v", err)
			}
			if !frameEqual(got, tc.in) {
				t.Fatalf("round-trip mismatch:\n in  = %+v\n got = %+v", tc.in, got)
			}
		})
	}
}

func TestFrameEncodeWireBytes(t *testing.T) {
	// Opcode is the first byte; the wire layout is load-bearing (public contract).
	if b := PTYOutFrame([]byte("ab")).Encode(); !bytes.Equal(b, []byte{0x00, 'a', 'b'}) {
		t.Errorf("PTY_OUT wire bytes = % x", b)
	}
	if b := InputFrame([]byte("ab")).Encode(); !bytes.Equal(b, []byte{0x01, 'a', 'b'}) {
		t.Errorf("INPUT wire bytes = % x", b)
	}
	// RESIZE: opcode, then rows (big-endian uint16), then cols (big-endian uint16).
	if b := ResizeFrame(24, 80).Encode(); !bytes.Equal(b, []byte{0x02, 0x00, 24, 0x00, 80}) {
		t.Errorf("RESIZE wire bytes = % x", b)
	}
	if b := ResizeFrame(258, 259).Encode(); !bytes.Equal(b, []byte{0x02, 0x01, 0x02, 0x01, 0x03}) {
		t.Errorf("RESIZE wire bytes = % x", b)
	}
}

func TestDecodeFrameErrors(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
	}{
		{"empty", nil},
		{"unknown_opcode", []byte{0x7f, 'x'}},
		{"resize_too_short", []byte{byte(OpResize), 0x00, 24}},
		{"resize_too_long", []byte{byte(OpResize), 0, 24, 0, 80, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := DecodeFrame(tc.raw); err == nil {
				t.Fatalf("DecodeFrame(% x) = nil error, want error", tc.raw)
			}
		})
	}
}

func TestOpcodeString(t *testing.T) {
	for _, tc := range []struct {
		op   Opcode
		want string
	}{
		{OpPTYOut, "PTY_OUT"},
		{OpInput, "INPUT"},
		{OpResize, "RESIZE"},
		{Opcode(0x42), "Opcode(0x42)"},
	} {
		if got := tc.op.String(); got != tc.want {
			t.Errorf("Opcode(%#x).String() = %q, want %q", byte(tc.op), got, tc.want)
		}
	}
}
