package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// receiverReadyMarker is printed by the receiver AFTER it has put the tty in raw
// mode and emitted DECSET 2004. Seeing it on the pane is proof from the receiver
// itself that both happened — and, because tmux renders the pane in order, proof
// that tmux PROCESSED the DECSET before the marker. That is exactly the
// precondition a bracketed paste depends on, so it is the only sound thing to
// wait for.
const receiverReadyMarker = "AF-RECEIVER-READY"

// receiverProgram builds a pane program that stands in for an agent composer at
// the only level that matters here: it requests bracketed-paste mode (DECSET
// 2004) exactly as claude/codex/aider/gemini/amp/opencode do, and records the RAW
// bytes the pane actually delivers to it. `stty raw -echo` keeps the tty from
// line-buffering or echoing, so the capture is byte-for-byte what the
// application's stdin saw.
//
// The order is load-bearing: raw mode, then the DECSET, then the marker. A test
// that pastes before the DECSET lands would get an unbracketed paste for a
// legitimate reason and blame the code under test.
//
// outPath is shell-quoted (shellQuoteArg, the package's existing POSIX quoter):
// t.TempDir() inherits TMPDIR, so the path can hold a space or a metacharacter
// even though Go sanitizes the subtest name out of it. Unquoted, `cat > /tmp/af
// space dir/received.bin` redirects to `/tmp/af` — the fixture breaks, and it
// breaks as "the submit test failed", sending the reader after the paste logic
// instead of the harness (#1978 class).
//
// tmux runs the command string through a shell itself, so there is deliberately
// no `sh -c '…'` wrapper here: that second level of quoting is what would make a
// correctly-quoted path unquotable (its single quotes would close the wrapper's).
// One shell, one level of quoting, one correct answer.
//
// Only stty/printf/cat are used — all present in the toolchain image — so this
// test never skips in CI for want of an agent binary.
func receiverProgram(outPath string) string {
	return fmt.Sprintf(`stty raw -echo; printf "\033[?2004h%s\r\n"; cat > %s`,
		receiverReadyMarker, shellQuoteArg(outPath))
}

// startReceiverPane brings up a real tmux session running the receiver and
// returns it plus the path its captured bytes land in.
func startReceiverPane(t *testing.T, name string) (*TmuxSession, string) {
	t.Helper()
	testguard.IsolateTmux(t)

	dir := t.TempDir()
	out := filepath.Join(dir, "received.bin")
	ts := NewTmuxSession(name, receiverProgram(out))
	require.NoError(t, ts.Start(dir))
	t.Cleanup(func() { _ = ts.Close() })

	// Wait for the marker the RECEIVER prints, never for the pane merely being
	// non-empty. `capture-pane` on a completely blank pane returns one newline per
	// row — 24 bytes of "\n" for a 24-row pane — so a `content != ""` check is true
	// the instant tmux has a pane at all, before the receiver has run a single
	// line. That check plus a fixed sleep is a race that passes locally and flakes
	// in CI, and a flake HERE reads as "the paste logic is broken" and sends the
	// next person hunting the wrong thing.
	//
	// It is also the very mistake this file exists to catch, one level up: the bug
	// under test is af sending input before establishing that the receiver would
	// take it as text. Waiting on a signal only a live receiver can emit is the
	// structural fix; "the buffer is not empty" is an artifact of tmux, not a fact
	// about the receiver.
	//
	// The marker proves raw mode is set and the DECSET is processed. `cat` may not
	// have been exec'd yet at that instant, which is harmless: the tty input queue
	// holds the pasted bytes until it starts reading. Nothing is lost, and the
	// bracketing decision — the thing under test — is already settled.
	waitFor(t, func() bool {
		content, err := ts.CapturePaneContent()
		return err == nil && strings.Contains(content, receiverReadyMarker)
	}, "receiver never printed "+receiverReadyMarker+": the pane program did not start")
	return ts, out
}

func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal(msg)
}

// readReceived polls until the receiver has flushed the delivery, then returns
// the raw bytes it got.
func readReceived(t *testing.T, path string, want string) string {
	t.Helper()
	var got string
	waitFor(t, func() bool {
		b, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		got = string(b)
		return strings.Contains(got, want)
	}, fmt.Sprintf("receiver never saw %q; got %q", want, got))
	return got
}

// TestReceiverProgramQuotesOutputPath keeps the fixture honest about its own
// input. t.TempDir() inherits TMPDIR, so the receiver's output path can contain a
// space or a shell metacharacter on a perfectly ordinary machine. Unquoted, the
// redirect splits and the fixture dies — reported as "the submit test failed",
// which sends the reader hunting the paste logic instead of the harness.
//
// That failure mode is this PR's own headline finding in miniature: a test whose
// breakage teaches people to distrust the test rather than the code. The submit
// path's test must not be the least reliable thing on the submit path (#1978 class).
func TestReceiverProgramQuotesOutputPath(t *testing.T) {
	for _, path := range []string{
		"/tmp/af space dir/received.bin",
		"/tmp/semi;colon/received.bin",
		"/tmp/it's/received.bin",
		"/tmp/dollar$VAR/received.bin",
	} {
		t.Run(path, func(t *testing.T) {
			prog := receiverProgram(path)
			// The redirect target must survive as ONE shell word. Round-trip it
			// through a real shell rather than asserting on the string: `sh -c`
			// echoing the quoted arg answers the only question that matters —
			// what the shell resolves it to.
			out, err := exec.Command("sh", "-c", "printf %s "+shellQuoteArg(path)).Output()
			require.NoError(t, err)
			require.Equal(t, path, string(out),
				"shell must resolve the quoted path back to exactly one intact word")
			require.Contains(t, prog, shellQuoteArg(path),
				"receiver program must embed the QUOTED path, never the raw one")
			require.NotContains(t, prog, "cat > "+path,
				"receiver program must not splice the raw path into the redirect")
		})
	}
}

// TestSendKeysCommandDeliversPromptAsPasteNotKeystrokes is the #1956 regression
// gate, and it asserts on what the AGENT RECEIVED rather than on the argv we
// built. That distinction is the whole point: the pre-fix code sent its paste
// successfully every time, so every test that asserted the send passed while the
// bug was live and corrupting real dispatches.
//
// A plain (unbracketed) tmux paste reaches the application as ordinary
// KEYSTROKES, indistinguishable from typing. An agent whose composer is modal —
// claude with `editorMode: "vim"`, which rests in NORMAL mode — then EXECUTES the
// prompt as editor commands instead of inserting it: "sentinel-alpha" loses its
// leading `s` to the substitute command and arrives as "entinel-alpha", and a
// prompt starting "deploy…" runs `d` as the delete operator. Verified against
// real vim: plain paste → "entinel-alpha", bracketed paste → "sentinel-alpha".
//
// The invariant that prevents all of it is that the pane receives the prompt
// wrapped in bracketed-paste markers — i.e. tagged as DATA, not commands. That is
// what this asserts, for every agent, with no exception list.
func TestSendKeysCommandDeliversPromptAsPasteNotKeystrokes(t *testing.T) {
	// The leading rune of each prompt is a destructive vim NORMAL-mode command:
	// s substitutes, d deletes, x deletes a char, : opens the command line, u
	// undoes. These are the prompts that mangle a live agent's composer today.
	for _, prompt := range []string{
		"sentinel-alpha",
		"deploy the fix",
		"x marks the spot",
		":wq is not a prompt",
		"undraft it and merge once Build is green",
	} {
		t.Run(prompt, func(t *testing.T) {
			ts, out := startReceiverPane(t, "af_submit_1956")
			require.NoError(t, ts.SendKeysCommand(prompt))

			got := readReceived(t, out, prompt)

			require.Contains(t, got, "\x1b[200~"+prompt+"\x1b[201~",
				"prompt must reach the pane BRACKETED (as pasted data). Received %q — "+
					"unbracketed means the agent gets it as keystrokes, and a modal "+
					"composer executes the leading character as a command (#1956)", got)
		})
	}
}

// TestSendKeysCommandBracketsForEveryAgent pins the no-exception-list rule from
// #1956: bracketing is not a codex special case (#1254/#1256), it is how every
// prompt is delivered. A per-agent conditional here is what let claude regress
// silently for months while codex was fine, so an agent added later must not be
// able to opt out by omission.
func TestSendKeysCommandBracketsForEveryAgent(t *testing.T) {
	for _, program := range []string{
		"claude", "codex", "aider", "gemini", "amp",
		"bash",                 // program_overrides / __shell panes
		"/home/foo/bin/claude", // legacy free-form persisted Program (#677)
	} {
		t.Run(program, func(t *testing.T) {
			cmds := recordTmuxCommands(t, program, "sentinel-alpha")

			var pasted bool
			for _, c := range cmds {
				if len(c.args) > 1 && c.args[1] == "paste-buffer" {
					pasted = true
					require.True(t, hasArg(c.args, "-p"),
						"%s: paste-buffer must carry -p; a plain paste is delivered as "+
							"keystrokes and a modal composer executes them (#1956). argv: %v",
						program, c.args)
				}
			}
			require.True(t, pasted, "%s: expected a paste-buffer delivery, got %v", program, joinedArgs(cmds))
		})
	}
}
