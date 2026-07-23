package tmux

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// #2414: the web terminal delivers a paste as ONE xterm.js onData call and
// forwards it as one InputFrame, so SendRawKeys received the whole payload at
// once. It rendered one hex argument per byte into a single `send-keys -H`, and
// tmux rejects a command whose packed argv exceeds its ~16 KB imsg limit — which
// a paste crosses at ~5,446 bytes. The paste was accepted by the WebSocket and
// then dropped by tmux, so it simply never appeared in the terminal.
//
// The TUI attach path never hit this only because apiclient/attach.go reads
// stdin in 32-byte chunks and so already sends many small frames. Chunking
// inside SendRawKeys gives every caller that same property.

// recordedArgs collects the argv of every tmux command an executor is handed.
// It is safe for concurrent use so the concurrency test below can share one.
type recordedArgs struct {
	mu   sync.Mutex
	args [][]string
}

func (r *recordedArgs) record(args []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.args = append(r.args, append([]string(nil), args...))
}

func (r *recordedArgs) all() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][]string, len(r.args))
	copy(out, r.args)
	return out
}

// recordingExecutor captures each command's argv and reports success, standing
// in for a healthy tmux without spawning one.
func recordingExecutor(rec *recordedArgs) cmd.Executor {
	return cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			rec.record(c.Args)
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			rec.record(c.Args)
			return nil, nil
		},
	}
}

// packedArgvBytes models what tmux actually measures. The client packs the
// command into a single MSG_COMMAND imsg as NUL-terminated argv strings, and the
// whole message — payload plus the 16-byte imsg header — must fit in
// MAX_IMSGSIZE (16384). argv[0] here is the "tmux" binary name exec prepends,
// which is not sent, so it is excluded.
func packedArgvBytes(args []string) int {
	total := imsgHeaderBytes + msgCommandHeaderBytes
	for _, a := range args[1:] {
		total += len(a) + 1 // trailing NUL
	}
	return total
}

const (
	imsgHeaderBytes       = 16
	msgCommandHeaderBytes = 8 // struct msg_command_data { int argc; }, padded
	maxIMsgSize           = 16384
)

// decodeHexPayload extracts the input bytes a `send-keys -H` argv carries, and
// asserts the command's shape: the exact-match target and the -H flag must be
// present on EVERY chunk, not just the first.
func decodeHexPayload(t *testing.T, args []string) []byte {
	t.Helper()
	if len(args) < 5 || args[1] != "send-keys" {
		t.Fatalf("not a send-keys command: %v", args[:min(6, len(args))])
	}
	if targetOf(args) != "="+testPasteSession+":" {
		t.Fatalf("chunk lost its exact-match target (#1006): got %q", targetOf(args))
	}
	hexStart := -1
	for i, a := range args {
		if a == "-H" {
			hexStart = i + 1
			break
		}
	}
	if hexStart < 0 {
		t.Fatalf("chunk lost its -H flag: %v", args[:min(6, len(args))])
	}
	var out []byte
	for _, a := range args[hexStart:] {
		b, err := hex.DecodeString(a)
		if err != nil || len(b) != 1 {
			t.Fatalf("bad hex argument %q: %v", a, err)
		}
		out = append(out, b[0])
	}
	return out
}

const testPasteSession = "af2414-paste"

// TestSendRawKeysChunksLargePastes is the #2414 regression. A 6 KB paste — an
// ordinary pasted stack trace or code block — must reach the pane. Before the
// fix SendRawKeys emitted ONE command carrying 6144 hex arguments, ~18.5 KB
// packed, which tmux rejects outright; the assertion below on packed size is
// what fails first.
//
// The reassembly check is the other half, and the one that matters for
// correctness rather than mere delivery: chunking is only safe because it
// preserves the byte stream exactly, so concatenating the chunks in order must
// reproduce the input byte-for-byte. That is what keeps bracketed-paste markers
// (ESC[200~ … ESC[201~) intact even when a chunk boundary falls inside one — the
// receiving application parses a stream, not discrete messages.
func TestSendRawKeysChunksLargePastes(t *testing.T) {
	rec := &recordedArgs{}
	ts := NewTmuxSessionFromSanitizedNameWithDeps(testPasteSession, "sh", MakePtyFactory(), recordingExecutor(rec))

	// Deliberately not uniform filler: a repeating pattern with high bytes and
	// control characters would let a chunker that dropped or reordered a chunk
	// still produce a byte-identical result.
	payload := make([]byte, 6*1024)
	for i := range payload {
		payload[i] = byte(i % 251)
	}
	// Straddle a chunk boundary with bracketed-paste markers so a chunker that
	// tried to be clever about escape sequences is caught here.
	copy(payload[sendRawKeysMaxChunk-3:], []byte("\x1b[200~hello\x1b[201~"))

	if err := ts.SendRawKeys(payload); err != nil {
		t.Fatalf("SendRawKeys(6KB): %v", err)
	}

	issued := rec.all()
	if len(issued) < 2 {
		t.Fatalf("6KB paste went out as %d command(s); it must be chunked to stay under tmux's limit", len(issued))
	}
	var got []byte
	for i, args := range issued {
		if n := packedArgvBytes(args); n >= maxIMsgSize {
			t.Fatalf("chunk %d packs to %d bytes, at or over tmux's %d-byte command limit — tmux rejects this",
				i, n, maxIMsgSize)
		}
		got = append(got, decodeHexPayload(t, args)...)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("chunking did not preserve the byte stream: got %d bytes, want %d (first difference at %d)",
			len(got), len(payload), firstDiff(got, payload))
	}
}

func firstDiff(a, b []byte) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i
		}
	}
	return min(len(a), len(b))
}

// TestSendRawKeysKeepsSmallInputInOneCommand guards the other direction. Ordinary
// interactive typing is one or a few bytes per call and is by far the hottest
// path through here; chunking must not turn a keystroke into extra tmux
// round-trips.
func TestSendRawKeysKeepsSmallInputInOneCommand(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   []byte
	}{
		{"single keystroke", []byte("a")},
		{"arrow key", []byte("\x1b[A")},
		{"at the chunk limit", bytes.Repeat([]byte("x"), sendRawKeysMaxChunk)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recordedArgs{}
			ts := NewTmuxSessionFromSanitizedNameWithDeps(testPasteSession, "sh", MakePtyFactory(), recordingExecutor(rec))
			if err := ts.SendRawKeys(tc.in); err != nil {
				t.Fatalf("SendRawKeys: %v", err)
			}
			issued := rec.all()
			if len(issued) != 1 {
				t.Fatalf("got %d commands for a %d-byte input, want 1", len(issued), len(tc.in))
			}
			if got := decodeHexPayload(t, issued[0]); !bytes.Equal(got, tc.in) {
				t.Fatalf("payload changed: got %q want %q", got, tc.in)
			}
		})
	}
}

// TestSendRawKeysEmptyInputSendsNothing pins the documented no-op.
func TestSendRawKeysEmptyInputSendsNothing(t *testing.T) {
	rec := &recordedArgs{}
	ts := NewTmuxSessionFromSanitizedNameWithDeps(testPasteSession, "sh", MakePtyFactory(), recordingExecutor(rec))
	if err := ts.SendRawKeys(nil); err != nil {
		t.Fatalf("SendRawKeys(nil): %v", err)
	}
	if n := len(rec.all()); n != 0 {
		t.Fatalf("empty input issued %d commands, want 0", n)
	}
}

// TestSendRawKeysStopsOnChunkFailure is the failure-mode half. A chunked send is
// no longer atomic, so a mid-stream failure means an unknown PREFIX landed. The
// one thing that must NOT happen is the silent-truncation class of #1982/#2099:
// the error has to propagate so the caller sees a failed send rather than a
// success that delivered half a paste. It must also stop rather than keep
// hammering a session that just reported gone.
func TestSendRawKeysStopsOnChunkFailure(t *testing.T) {
	rec := &recordedArgs{}
	boom := errors.New("send-keys refused")
	calls := 0
	ex := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			// has-session is the ExistsOrUnknown probe on the error path; let it
			// answer "exists" so the failure stays a plain send error.
			if len(c.Args) > 1 && c.Args[1] == "has-session" {
				return nil
			}
			rec.record(c.Args)
			calls++
			if calls == 2 {
				return boom
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) { return nil, nil },
	}
	ts := NewTmuxSessionFromSanitizedNameWithDeps(testPasteSession, "sh", MakePtyFactory(), ex)

	err := ts.SendRawKeys(bytes.Repeat([]byte("y"), 5*sendRawKeysMaxChunk))
	if err == nil {
		t.Fatal("a failed chunk must not report success — that is silent paste truncation")
	}
	if !errors.Is(err, boom) {
		t.Fatalf("underlying tmux error must survive wrapping: got %v", err)
	}
	if n := len(rec.all()); n != 2 {
		t.Fatalf("issued %d send-keys commands, want 2 (stop at the first failure)", n)
	}
}

// TestRealSendRawKeysDeliversLargePaste drives a REAL tmux. The mock above
// asserts the argv we build stays inside the limit we believe tmux enforces; only
// a real server proves that belief. It is the report's own repro: before the fix
// this returns "failed to send command" for a 6 KB payload.
//
// The payload is delivered into `cat > file` rather than to a shell prompt, and
// is newline-terminated in 64-byte lines: the pane is a real tty in canonical
// mode, where a single line longer than the terminal's edit buffer would be
// truncated by the LINE DISCIPLINE — a limit that has nothing to do with the
// tmux one under test and would otherwise confound it.
func TestRealSendRawKeysDeliversLargePaste(t *testing.T) {
	testguard.IsolateTmux(t)

	const name = "af2414-real-paste"
	ex := cmd.MakeExecutor()
	if err := ex.Run(exec.Command("tmux", "new-session", "-d", "-s", name, "sh")); err != nil {
		t.Fatalf("new-session: %v", err)
	}
	t.Cleanup(func() { _ = ex.Run(exec.Command("tmux", "kill-session", "-t", "="+name)) })

	ts := NewTmuxSessionFromSanitizedNameWithDeps(name, "sh", MakePtyFactory(), ex)
	out := filepath.Join(t.TempDir(), "pasted.txt")

	// Let the shell reach its prompt before redirecting, so the redirect command
	// is not swallowed by a still-initializing pane.
	if err := ts.SendRawKeys([]byte("cat > " + out + "\n")); err != nil {
		t.Fatalf("start cat: %v", err)
	}
	waitFor(t, func() bool {
		_, err := os.Stat(out)
		return err == nil
	}, "cat never opened "+out+": the pane program did not start")

	var payload strings.Builder
	for i := 0; payload.Len() < 6*1024; i++ {
		fmt.Fprintf(&payload, "line %04d %s\n", i, strings.Repeat("z", 48))
	}
	want := payload.String()

	if err := ts.SendRawKeys([]byte(want)); err != nil {
		t.Fatalf("SendRawKeys(%d bytes) against a real tmux: %v — large pastes must be chunked", len(want), err)
	}
	// Ctrl-D closes cat's stdin so it flushes and exits.
	if err := ts.SendRawKeys([]byte{0x04}); err != nil {
		t.Fatalf("send EOT: %v", err)
	}

	waitFor(t, func() bool {
		got, err := os.ReadFile(out)
		return err == nil && len(got) >= len(want)
	}, fmt.Sprintf("only part of the %d-byte paste reached the pane", len(want)))
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read pasted file: %v", err)
	}
	if string(got) != want {
		t.Fatalf("paste arrived corrupted: got %d bytes want %d (first difference at %d)",
			len(got), len(want), firstDiff(got, []byte(want)))
	}
}
