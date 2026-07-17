package tmux

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// receiverProgram builds a pane program that stands in for an agent composer at
// the only level that matters here: it requests bracketed-paste mode (DECSET
// 2004) exactly as claude/codex do, and records the RAW bytes the pane actually
// delivers to it. `stty raw -echo` keeps the tty from line-buffering or echoing,
// so the capture is byte-for-byte what the application's stdin saw.
//
// Only sh/stty/cat are used — all present in the toolchain image — so this test
// never skips in CI for want of an agent binary.
func receiverProgram(outPath string) string {
	return fmt.Sprintf(`sh -c 'stty raw -echo; printf "\033[?2004h"; cat > %s'`, outPath)
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

	// Let the pane program reach its read loop and emit ?2004h before pasting;
	// a paste that lands before the app requests the mode would legitimately
	// arrive unbracketed and make this test lie.
	waitFor(t, func() bool {
		content, err := ts.CapturePaneContent()
		return err == nil && content != ""
	}, "receiver pane did not start")
	time.Sleep(300 * time.Millisecond)
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
