package tmux

import (
	"io"
	"os/exec"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/stretchr/testify/require"
)

// recordedCmd is a captured tmux invocation: the argv plus, for `load-buffer -`,
// whatever was streamed on stdin (so the test can assert the paste payload).
type recordedCmd struct {
	args  []string
	stdin string
}

func recordTmuxCommands(t *testing.T, program string, text string) []recordedCmd {
	t.Helper()
	var cmds []recordedCmd
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			rec := recordedCmd{args: c.Args}
			if c.Stdin != nil {
				b, _ := io.ReadAll(c.Stdin)
				rec.stdin = string(b)
			}
			cmds = append(cmds, rec)
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) { return []byte("content"), nil },
	}

	session := newTmuxSession("af_proj", program, NewMockPtyFactory(t), cmdExec)
	require.NoError(t, session.SendKeysCommand(text))
	return cmds
}

func joinedArgs(cmds []recordedCmd) []string {
	out := make([]string, len(cmds))
	for i, c := range cmds {
		out[i] = strings.Join(c.args, " ")
	}
	return out
}

// TestCodexSubmitUsesBracketedPaste is the #1254 regression: codex's composer
// swallows the trailing Enter after a plain `send-keys -l` paste (paste-burst
// detection), so the prompt lands but never submits. Codex must instead deliver
// the text as a bracketed paste (`load-buffer` + `paste-buffer -p`) which gives
// codex an explicit end-of-paste marker, and only THEN send Enter to submit.
func TestCodexSubmitUsesBracketedPaste(t *testing.T) {
	const prompt = "a big multi-line dispatch prompt"
	cmds := recordTmuxCommands(t, "codex", prompt)
	joined := joinedArgs(cmds)

	require.Len(t, cmds, 3, "codex submit is load-buffer, paste-buffer, send-keys Enter; got %v", joined)

	// 1. Text is streamed into a buffer via stdin (not an argv arg → no ARG_MAX
	//    ceiling for large prompts) and never as `send-keys -l`.
	require.Contains(t, joined[0], "load-buffer", "first command must load the paste buffer; got %v", joined)
	require.Equal(t, prompt, cmds[0].stdin, "paste text must be streamed on stdin")
	for _, j := range joined {
		require.NotContains(t, j, "send-keys -t =af_proj: -l",
			"codex must not use the plain literal send-keys path that gets swallowed (#1254); got %v", joined)
	}

	// 2. The paste is bracketed (-p) so codex gets an end-of-paste marker, and
	//    the buffer is deleted after (-d) so buffers don't accumulate.
	require.Contains(t, joined[1], "paste-buffer", "second command must paste the buffer; got %v", joined)
	require.Contains(t, joined[1], "-p", "paste must be bracketed (-p) so codex sees the paste boundary")
	require.Contains(t, joined[1], "-d", "paste must delete the buffer afterward (-d)")

	// 3. Enter is a SEPARATE command issued last — this is what actually submits.
	require.Contains(t, joined[2], "send-keys", "last command must send Enter; got %v", joined)
	require.Contains(t, joined[2], "Enter", "last command must send Enter to submit; got %v", joined)

	// Every targeted command keeps the #1006 exact-match target.
	for _, c := range cmds {
		if tgt := targetOf(c.args); tgt != "" {
			require.Equal(t, "=af_proj:", tgt,
				"codex submit commands must target by exact match (#1006); got %q in %v", tgt, joined)
		}
	}
}

// TestNonCodexSubmitUsesLiteralSendKeys guards against regressing the agents
// that already submit fine (claude/aider/gemini and unknown/override panes):
// they keep the plain `send-keys -l` + Enter path and must not switch to
// bracketed paste.
func TestNonCodexSubmitUsesLiteralSendKeys(t *testing.T) {
	for _, program := range []string{"claude", "aider", "gemini", "some-custom-shell"} {
		t.Run(program, func(t *testing.T) {
			cmds := recordTmuxCommands(t, program, "hello")
			joined := joinedArgs(cmds)

			require.Len(t, cmds, 2, "literal path is send-keys -l then send-keys Enter; got %v", joined)
			require.Contains(t, joined[0], "send-keys", "first command must be send-keys; got %v", joined)
			require.Contains(t, joined[0], "-l", "non-codex must send text literally; got %v", joined)
			require.Contains(t, joined[0], "hello", "literal text must be passed as an argv arg; got %v", joined)
			require.Contains(t, joined[1], "Enter", "second command must submit with Enter; got %v", joined)

			for _, j := range joined {
				require.NotContains(t, j, "paste-buffer",
					"non-codex agents must not use the bracketed-paste path; got %v", joined)
				require.NotContains(t, j, "load-buffer",
					"non-codex agents must not use the bracketed-paste path; got %v", joined)
			}
		})
	}
}
