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
// tmux refuses a command whose packed argv exceeds its limit — so the paste was
// accepted by the WebSocket and then dropped by tmux, never appearing in the
// terminal.
//
// That limit is platform-specific — and the two platforms we ship do not even
// enforce the same KIND of limit — which is why the tests below assert against
// sendRawKeysArgvBudget rather than any derived constant, and why
// TestSendRawKeysChunkFitsRealTmux measures the real ceiling instead of trusting
// one. Measured: Linux accepts 5,444 input bytes (a byte limit, MAX_IMSGSIZE at
// ~3 bytes per input byte, matching the issue's report); macOS accepts 996,
// which with this command's 4 fixed arguments is exactly 1000 — an argument
// COUNT cap, not a byte one.
//
// The TUI attach path never hit this only because apiclient/attach.go reads
// stdin in 32-byte chunks and so already sends many small frames. Chunking
// inside SendRawKeys gives every caller that same property.

// recordedArgs collects the argv of every tmux command an executor is handed.
// The mutex is not for the tests here, which are single-goroutine: it is so a
// later test that does drive SendRawKeys concurrently can share a recorder
// without the recorder itself being the race it would be trying to find.
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

// packedArgvBytes is the size the command spends against sendRawKeysArgvBudget:
// NUL-terminated argv, excluding argv[0] (the "tmux" binary name exec prepends,
// which is not part of the command tmux packs).
func packedArgvBytes(args []string) int {
	total := 0
	for _, a := range args[1:] {
		total += len(a) + 1
	}
	return total
}

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

// testChunkSize is the chunk the mock-backed tests' session gets.
func testChunkSize() int {
	return NewTmuxSessionFromSanitizedNameWithDeps(
		testPasteSession, "sh", MakePtyFactory(), recordingExecutor(&recordedArgs{})).sendRawKeysChunkSize()
}

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
	copy(payload[ts.sendRawKeysChunkSize()-3:], []byte("\x1b[200~hello\x1b[201~"))

	if err := ts.SendRawKeys(payload); err != nil {
		t.Fatalf("SendRawKeys(6KB): %v", err)
	}

	issued := rec.all()
	if len(issued) < 2 {
		t.Fatalf("6KB paste went out as %d command(s); it must be chunked to stay under tmux's limit", len(issued))
	}
	var got []byte
	for i, args := range issued {
		if n := packedArgvBytes(args); n > sendRawKeysArgvBudget {
			t.Fatalf("chunk %d packs to %d bytes, over the %d-byte budget a command may spend",
				i, n, sendRawKeysArgvBudget)
		}
		got = append(got, decodeHexPayload(t, args)...)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("chunking did not preserve the byte stream: got %d bytes, want %d (first difference at %d)",
			len(got), len(payload), firstDiff(got, payload))
	}
}

// TestSendRawKeysChunkSizeAccountsForTheSessionName pins the part of the budget
// that is not payload. The session name is an argument too, and a repo-scoped
// name has no fixed length, so a long project path must shrink the chunk rather
// than silently push the command over.
func TestSendRawKeysChunkSizeAccountsForTheSessionName(t *testing.T) {
	short := NewTmuxSessionFromSanitizedNameWithDeps("af_x", "sh", MakePtyFactory(), recordingExecutor(&recordedArgs{}))
	long := NewTmuxSessionFromSanitizedNameWithDeps(
		"af_"+strings.Repeat("verylongprojectname", 8), "sh", MakePtyFactory(), recordingExecutor(&recordedArgs{}))

	if long.sendRawKeysChunkSize() >= short.sendRawKeysChunkSize() {
		t.Fatalf("a longer session name must buy a smaller chunk: short=%d long=%d",
			short.sendRawKeysChunkSize(), long.sendRawKeysChunkSize())
	}
	for _, ts := range []*TmuxSession{short, long} {
		rec := &recordedArgs{}
		sized := NewTmuxSessionFromSanitizedNameWithDeps(ts.sanitizedName, "sh", MakePtyFactory(), recordingExecutor(rec))
		if err := sized.SendRawKeys(bytes.Repeat([]byte("q"), 4*sendRawKeysArgvBudget)); err != nil {
			t.Fatalf("SendRawKeys: %v", err)
		}
		for i, args := range rec.all() {
			if n := packedArgvBytes(args); n > sendRawKeysArgvBudget {
				t.Fatalf("session %q chunk %d packs to %d bytes, over the %d-byte budget",
					ts.sanitizedName, i, n, sendRawKeysArgvBudget)
			}
		}
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
		{"at the chunk limit", bytes.Repeat([]byte("x"), testChunkSize())},
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

	err := ts.SendRawKeys(bytes.Repeat([]byte("y"), 5*testChunkSize()))
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

// TestSendRawKeysChunkFitsRealTmux is the guard that keeps sendRawKeysArgvBudget
// a checked fact rather than an assumption, and it exists because the assumption
// was wrong once already.
//
// The budget was first derived from MAX_IMSGSIZE (16384), which the issue's Linux
// measurement corroborates exactly. macOS refuses far sooner — a 4096-byte chunk,
// ~12 KB of argv and comfortably inside 16384, fails there — so no single
// platform's constant can be reasoned from. Only asking the tmux that is actually
// installed settles it, and it must be asked on every platform we ship rather
// than on whichever one a developer happens to use.
//
// On failure it binary-searches the real ceiling and reports it, so the fix is
// the measured number rather than another guess. It logs that number on success
// too: the headroom is the thing worth watching, and a budget that has quietly
// crept up to the edge should be visible before it starts dropping pastes.
func TestSendRawKeysChunkFitsRealTmux(t *testing.T) {
	testguard.IsolateTmux(t)

	// The pane runs `cat >/dev/null` rather than a shell: the search below sends
	// tens of KB of filler, and a shell would echo every byte back through the
	// pane and its line editor for no benefit to what is being measured.
	const name = "af2414-budget"
	ex := cmd.MakeExecutor()
	if out, err := exec.Command("tmux", "new-session", "-d", "-s", name, "cat >/dev/null").CombinedOutput(); err != nil {
		t.Fatalf("new-session: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = ex.Run(exec.Command("tmux", "kill-session", "-t", "="+name)) })

	ts := NewTmuxSessionFromSanitizedNameWithDeps(name, "cat", MakePtyFactory(), ex)
	chunk := ts.sendRawKeysChunkSize()

	// One command carrying a full chunk: exactly what SendRawKeys emits for any
	// input at or over the chunk size.
	err := ts.sendRawKeysChunk(bytes.Repeat([]byte("x"), chunk))
	// Search once, and report either way. The bound is generous enough to reveal
	// a ceiling well above ours (Linux measures 5444) without spending the search
	// on sizes no budget would ever want.
	ceiling := maxAcceptedChunk(ts, 8192)
	if err != nil {
		t.Fatalf("this tmux rejects a full %d-byte chunk (argv budget %d): %v\n"+
			"largest chunk it actually accepts is %d bytes — lower sendRawKeysArgvBudget to about %d",
			chunk, sendRawKeysArgvBudget, err, ceiling, 3*ceiling)
	}
	t.Logf("tmux accepted a full %d-byte chunk (argv budget %d); largest accepted chunk here is %d bytes",
		chunk, sendRawKeysArgvBudget, ceiling)
}

// maxAcceptedChunk binary-searches the largest single send-keys chunk this tmux
// accepts, between 0 and hi. Reported rather than asserted on: the number is
// platform-specific, and the point is to inform the budget, not to pin it.
func maxAcceptedChunk(ts *TmuxSession, hi int) int {
	lo := 0
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if ts.sendRawKeysChunk(bytes.Repeat([]byte("x"), mid)) == nil {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	return lo
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
