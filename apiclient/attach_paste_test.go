package apiclient

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/sachiniyer/agent-factory/agentproto"
)

// readInputBytes collects INPUT payloads from the server side until it has at
// least want bytes, and returns them concatenated in arrival order. A paste
// larger than the stdin pump's read buffer necessarily arrives as SEVERAL INPUT
// frames; what must hold is that concatenating them reproduces the paste exactly
// — every byte, once, in order.
func readInputBytes(t *testing.T, conn *websocket.Conn, want int) []byte {
	t.Helper()
	var got []byte
	deadline := time.Now().Add(10 * time.Second)
	for len(got) < want {
		if time.Now().After(deadline) {
			t.Fatalf("timed out with %d of %d input bytes: %q", len(got), want, got)
		}
		msg := readServerMsg(t, conn)
		if !msg.Binary || msg.Frame.Op != agentproto.OpInput {
			t.Fatalf("expected INPUT frame, got %+v", msg)
		}
		got = append(got, msg.Frame.Data...)
	}
	return got
}

// TestAttachStream_PasteForwardedByteForByte pins the stdin pump's half of
// #2157: a paste is bytes on stdin like any other input, and the pump must
// forward ALL of them, in order, whatever the size — its 32-byte read buffer is
// a chunk size, never a cap. The sizes straddle that buffer deliberately: one
// byte over it, a non-multiple, the reported 128-character line, and a payload
// two orders of magnitude larger than the buffer.
//
// The byte loss reported in #2157 was NOT here — it was a second reader on the
// same terminal (see app.releaseTerminalToAttach) — which is exactly why this
// lock belongs in the tree: the pump is the obvious suspect for a byte-loss
// report, and the next person to read this code should find the property
// asserted rather than have to re-derive it.
func TestAttachStream_PasteForwardedByteForByte(t *testing.T) {
	for _, size := range []int{33, 100, 128, 4096} {
		t.Run(fmt.Sprintf("%dbytes", size), func(t *testing.T) {
			payload := make([]byte, size)
			for i := range payload {
				payload[i] = byte('a' + i%26)
			}
			server, stdinW, _, done := startDriver(t)
			if _, err := stdinW.Write(payload); err != nil {
				t.Fatalf("write paste: %v", err)
			}
			got := readInputBytes(t, server, size)
			if !bytes.Equal(got, payload) {
				t.Fatalf("paste corrupted on the wire\n got (%d bytes): %q\nwant (%d bytes): %q",
					len(got), got, size, payload)
			}
			_ = server.Close(websocket.StatusNormalClosure, "")
			waitClosed(t, done)
		})
	}
}

// TestAttachStream_SingleKeystrokeForwardedImmediately guards the other side of
// the paste fix: nothing may batch or delay interactive input while waiting for
// a fuller buffer. Each keystroke leaves as its own INPUT frame, as it is typed.
func TestAttachStream_SingleKeystrokeForwardedImmediately(t *testing.T) {
	server, stdinW, _, done := startDriver(t)
	for _, key := range []string{"a", "b", "\x1b", "[", "A"} {
		if _, err := stdinW.Write([]byte(key)); err != nil {
			t.Fatalf("write %q: %v", key, err)
		}
		msg := readServerMsg(t, server)
		if !msg.Binary || msg.Frame.Op != agentproto.OpInput || string(msg.Frame.Data) != key {
			t.Fatalf("expected INPUT %q as its own frame, got %+v", key, msg)
		}
	}
	_ = server.Close(websocket.StatusNormalClosure, "")
	waitClosed(t, done)
}
