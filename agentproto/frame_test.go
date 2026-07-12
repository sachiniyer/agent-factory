package agentproto

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"testing"
)

// frameEqual compares two frames, treating nil and empty Data as equal (Encode of
// an empty payload and DecodeFrame of a 1-byte frame differ only in nil-ness).
func frameEqual(a, b Frame) bool {
	return a.Op == b.Op && a.Rows == b.Rows && a.Cols == b.Cols && a.Seq == b.Seq && bytes.Equal(a.Data, b.Data)
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
		{"hello_zero", HelloFrame(0)},
		{"hello", HelloFrame(4294967297)},
		{"hello_max", HelloFrame(18446744073709551615)},
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
	// HELLO: opcode 0x04, then the start seq as a big-endian uint64.
	if b := HelloFrame(0).Encode(); !bytes.Equal(b, []byte{0x04, 0, 0, 0, 0, 0, 0, 0, 0}) {
		t.Errorf("HELLO(0) wire bytes = % x", b)
	}
	if b := HelloFrame(4294967297).Encode(); !bytes.Equal(b, []byte{0x04, 0, 0, 0, 1, 0, 0, 0, 1}) {
		t.Errorf("HELLO(2^32+1) wire bytes = % x", b)
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
		{"hello_too_short", []byte{byte(OpHello), 0, 0, 0, 0, 0, 0, 1}},
		{"hello_too_long", []byte{byte(OpHello), 0, 0, 0, 0, 0, 0, 0, 1, 2}},
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
		{OpRepaint, "REPAINT"},
		{OpHello, "HELLO"},
		{Opcode(0x42), "Opcode(0x42)"},
	} {
		if got := tc.op.String(); got != tc.want {
			t.Errorf("Opcode(%#x).String() = %q, want %q", byte(tc.op), got, tc.want)
		}
	}
}

// frameVector mirrors one entry of testdata/frame_vectors.json — the fixture the
// TypeScript codec (web/src/frame.test.ts) validates against too, so the browser
// and daemon codecs are pinned to byte-identical framing.
type frameVector struct {
	Name    string `json:"name"`
	Op      string `json:"op"`
	DataHex string `json:"dataHex"`
	Rows    uint16 `json:"rows"`
	Cols    uint16 `json:"cols"`
	Seq     string `json:"seq"` // decimal uint64 (string so JS BigInt keeps it exact)
	WireHex string `json:"wireHex"`
}

// frame reconstructs the logical Frame a vector describes.
func (v frameVector) frame(t *testing.T) Frame {
	t.Helper()
	switch v.Op {
	case "PTY_OUT":
		return PTYOutFrame(mustHex(t, v.DataHex))
	case "INPUT":
		return InputFrame(mustHex(t, v.DataHex))
	case "REPAINT":
		return RepaintFrame(mustHex(t, v.DataHex))
	case "RESIZE":
		return ResizeFrame(v.Rows, v.Cols)
	case "HELLO":
		seq, err := strconv.ParseUint(v.Seq, 10, 64)
		if err != nil {
			t.Fatalf("vector %q: bad seq %q: %v", v.Name, v.Seq, err)
		}
		return HelloFrame(seq)
	default:
		t.Fatalf("vector %q: unknown op %q", v.Name, v.Op)
		return Frame{}
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex %q: %v", s, err)
	}
	return b
}

// TestFrameGoldenVectors is the Go half of the cross-language codec contract: it
// asserts Encode produces the fixture's exact wire bytes and DecodeFrame reparses
// them back to the same Frame. web/src/frame.test.ts asserts the identical property
// against the SAME file, so the Go and TS codecs cannot silently diverge. On a
// mismatch this prints the actual bytes — the way to (re)derive a fixture entry.
func TestFrameGoldenVectors(t *testing.T) {
	raw, err := os.ReadFile("testdata/frame_vectors.json")
	if err != nil {
		t.Fatalf("read fixtures: %v", err)
	}
	var fixture struct {
		Vectors []frameVector `json:"vectors"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("parse fixtures: %v", err)
	}
	if len(fixture.Vectors) == 0 {
		t.Fatal("no vectors in fixture")
	}
	for _, v := range fixture.Vectors {
		t.Run(v.Name, func(t *testing.T) {
			f := v.frame(t)
			wantWire := mustHex(t, v.WireHex)
			if got := f.Encode(); !bytes.Equal(got, wantWire) {
				t.Fatalf("Encode mismatch:\n got  = %x\n want = %s (%q)", got, v.WireHex, v.Name)
			}
			back, err := DecodeFrame(wantWire)
			if err != nil {
				t.Fatalf("DecodeFrame(%s) error: %v", v.WireHex, err)
			}
			if !frameEqual(back, f) {
				t.Fatalf("DecodeFrame round-trip mismatch:\n in  = %+v\n got = %+v", f, back)
			}
		})
	}
}

// TestStreamConsumerToleratesHello is the additive-safety property (#1592 Phase 5
// PR1): an existing stream consumer shaped exactly like the TUI/apiclient path
// (app/live_stream.go apiStream.Recv, integration ws_pty readPump) — which switches
// on Frame.Op and skips anything it doesn't handle — ignores the new OpHello frame
// with NO behavior change. OpHello decodes cleanly (it is a known op, so ReadMessage
// never errors), the consumer's switch falls through to its skip branch, no bytes are
// rendered, and the replay cursor is untouched. This is why the TUI is byte-for-byte
// unaffected by the broker now emitting a leading hello.
func TestStreamConsumerToleratesHello(t *testing.T) {
	// The exact wire sequence the broker now sends a fresh subscriber: hello, then a
	// repaint, then live output.
	wire := [][]byte{
		HelloFrame(42).Encode(),
		RepaintFrame([]byte("screen")).Encode(),
		PTYOutFrame([]byte("live")).Encode(),
	}
	// consume mirrors the TUI/apiclient frame switch: collect PTY_OUT bytes only,
	// render repaint without counting it, ignore everything else. A decode error
	// would model the consumer choking — which must NOT happen for OpHello.
	var out []byte
	var repainted bool
	for _, raw := range wire {
		f, err := DecodeFrame(raw)
		if err != nil {
			t.Fatalf("consumer decode error on % x: %v (a consumer would drop the stream)", raw, err)
		}
		switch f.Op {
		case OpPTYOut:
			out = append(out, f.Data...)
		case OpRepaint:
			repainted = true
		default:
			// INPUT/RESIZE/HELLO and any future op: skipped, exactly as apiStream.Recv
			// and the integration readPump do. The hello lands here — a no-op.
		}
	}
	if string(out) != "live" {
		t.Fatalf("PTY output after a leading hello = %q, want %q", out, "live")
	}
	if !repainted {
		t.Fatal("repaint frame was not delivered through the leading hello")
	}
}
