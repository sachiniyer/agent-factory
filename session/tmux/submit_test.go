package tmux

import (
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	cmdpkg "github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/testguard"
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

// bufferOf returns the `-b <name>` buffer argument from a tmux argv, or "".
func bufferOf(args []string) string {
	for i, a := range args {
		if a == "-b" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

func hasArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
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
	//    the buffer is deleted after (-d) so buffers don't accumulate. The paste
	//    reads back the SAME buffer the load wrote (no cross-talk).
	require.Contains(t, joined[1], "paste-buffer", "second command must paste the buffer; got %v", joined)
	require.Contains(t, joined[1], "-p", "paste must be bracketed (-p) so codex sees the paste boundary")
	require.Contains(t, joined[1], "-d", "paste must delete the buffer afterward (-d)")
	loadBuf, pasteBuf := bufferOf(cmds[0].args), bufferOf(cmds[1].args)
	require.NotEmpty(t, loadBuf, "load-buffer must name a buffer; got %v", joined)
	require.Equal(t, loadBuf, pasteBuf, "paste must read back the buffer the load wrote; got %v", joined)

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

// TestEveryAgentSubmitUsesBracketedPasteBuffer guards the delivery shape shared
// by every pane: load-buffer (payload on stdin) → bracketed paste-buffer → a
// separate Enter to submit.
//
// This test used to be TestNonCodexSubmitUsesPlainPasteBuffer and asserted the
// OPPOSITE — that claude/aider/gemini/amp/overrides must NOT get `-p`. It passed
// continuously while #1956 corrupted real dispatches, because a plain paste is
// delivered to the pane as KEYSTROKES: a claude composer in vim NORMAL mode
// (`editorMode: "vim"`) executed each prompt's leading character as an editor
// command instead of inserting it. The assertion was inverted, not merely
// incomplete, so it is replaced rather than relaxed. See
// submit_bracketed_test.go for the gate that asserts what the pane RECEIVES —
// the property this argv-level test cannot see (#1956).
func TestEveryAgentSubmitUsesBracketedPasteBuffer(t *testing.T) {
	for _, program := range []string{"claude", "aider", "gemini", "amp", "some-custom-shell"} {
		t.Run(program, func(t *testing.T) {
			const prompt = "hello"
			cmds := recordTmuxCommands(t, program, prompt)
			joined := joinedArgs(cmds)

			require.Len(t, cmds, 3, "paste path is load-buffer, paste-buffer, send-keys Enter; got %v", joined)
			require.Contains(t, joined[0], "load-buffer", "first command must load the paste buffer; got %v", joined)
			require.Equal(t, prompt, cmds[0].stdin, "paste text must be streamed on stdin")
			require.Contains(t, joined[1], "paste-buffer", "second command must paste the buffer; got %v", joined)
			require.Contains(t, joined[1], "-d", "paste must delete the buffer afterward (-d)")
			require.True(t, hasArg(cmds[1].args, "-p"),
				"every pane must receive a BRACKETED paste: a plain paste arrives as keystrokes "+
					"and a modal composer executes it as commands (#1956); got %v", joined)
			require.Contains(t, joined[2], "send-keys", "last command must send Enter; got %v", joined)
			require.Contains(t, joined[2], "Enter", "last command must submit with Enter; got %v", joined)

			for _, j := range joined {
				require.NotContains(t, j, "send-keys -t =af_proj: -l",
					"no pane may use the literal send-keys path that redraws wrapped bash input (#1292); got %v", joined)
			}

			loadBuf, pasteBuf := bufferOf(cmds[0].args), bufferOf(cmds[1].args)
			require.NotEmpty(t, loadBuf, "load-buffer must name a buffer; got %v", joined)
			require.Equal(t, loadBuf, pasteBuf, "paste must read back the buffer the load wrote; got %v", joined)

			for _, c := range cmds {
				if tgt := targetOf(c.args); tgt != "" {
					require.Equal(t, "=af_proj:", tgt,
						"submit commands must target by exact match (#1006); got %q in %v", tgt, joined)
				}
			}
		})
	}
}

// TestCodexSubmitConcurrentDeliveriesUseDistinctBuffers is the Greptile
// shared-buffer-race guard (#1256 review): the submit path releases the
// instance lock before its tmux calls, so two concurrent codex deliveries to
// the SAME session can interleave. If they shared one buffer name, one call's
// load-buffer could overwrite the other's content between its load and paste
// and corrupt the submit. Each delivery must therefore use a per-call unique
// buffer, and every paste must read back the buffer its own load wrote.
func TestCodexSubmitConcurrentDeliveriesUseDistinctBuffers(t *testing.T) {
	const workers = 24

	var mu sync.Mutex
	var loadBufs []string
	pasteBufs := map[string]bool{}

	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			joined := strings.Join(c.Args, " ")
			mu.Lock()
			defer mu.Unlock()
			if strings.Contains(joined, "load-buffer") {
				loadBufs = append(loadBufs, bufferOf(c.Args))
			}
			if strings.Contains(joined, "paste-buffer") {
				pasteBufs[bufferOf(c.Args)] = true
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) { return []byte("content"), nil },
	}
	// One shared session — the same sanitizedName for every delivery, which is
	// exactly the case where a fixed `af_paste_<name>` buffer would collide.
	session := newTmuxSession("af_proj", "codex", NewMockPtyFactory(t), cmdExec)

	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			require.NoError(t, session.SendKeysCommand(fmt.Sprintf("prompt %d", i)))
		}(i)
	}
	wg.Wait()

	require.Len(t, loadBufs, workers, "every delivery must load its own buffer")
	unique := map[string]bool{}
	for _, b := range loadBufs {
		require.NotEmpty(t, b, "load-buffer must name a buffer")
		require.False(t, unique[b], "buffer name %q reused across concurrent deliveries — shared-buffer race", b)
		unique[b] = true
	}
	// Every paste targeted a buffer that some load wrote, and no fixed name was
	// shared across all calls.
	require.Equal(t, unique, pasteBufs, "each paste must read back a per-call load buffer")
}

func TestBashWrappedSubmitDoesNotDuplicateCommandPrefix(t *testing.T) {
	testguard.IsolateTmux(t)
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("bash not available: %v", err)
	}

	session := newTmuxSession(
		toTmuxName("wrap-submit", ""),
		"bash --noprofile --norc -i",
		MakePtyFactory(),
		cmdpkg.MakeExecutor(),
	)
	require.NoError(t, session.Start(t.TempDir()))
	t.Cleanup(func() {
		_, closeErrX := session.Close()
		require.NoError(t, closeErrX)
	})

	resizeCmd := exec.Command("tmux", "resize-window", "-t", exactTarget(session.sanitizedName), "-x", "24", "-y", "10")
	require.NoError(t, session.cmdExec.Run(resizeCmd))

	time.Sleep(100 * time.Millisecond)

	const command = "printf '%s\\n' AF1292_DONE"
	require.NoError(t, session.SendKeysCommand(command))

	require.Eventually(t, func() bool {
		content, err := captureRawPane(session)
		return err == nil && strings.Contains(content, "\nAF1292_DONE\n")
	}, 2*time.Second, 50*time.Millisecond, "wrapped bash command did not run")

	content, err := captureRawPane(session)
	require.NoError(t, err)
	require.Equal(t, 1, strings.Count(content, "printf '%s"),
		"wrapped command prefix must be captured once, not duplicated:\n%s", content)
}

// TestSubmitDeletesBufferWhenPasteFails guards the #1536 leak: `paste-buffer -d`
// only deletes the named buffer once the paste SUCCEEDS, so a failed paste
// strands it. tmux buffers are server-scoped (they outlive the session), so each
// failed submit would leak one buffer unbounded. When paste-buffer errors the
// submit path must best-effort `delete-buffer` the same buffer before returning.
func TestSubmitDeletesBufferWhenPasteFails(t *testing.T) {
	var loadBuf, deletedBuf string
	var pasteErr = fmt.Errorf("boom: no client")
	cmdExec := cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			joined := strings.Join(c.Args, " ")
			switch {
			case strings.Contains(joined, "load-buffer"):
				loadBuf = bufferOf(c.Args)
				return nil
			case strings.Contains(joined, "paste-buffer"):
				return pasteErr
			case strings.Contains(joined, "delete-buffer"):
				deletedBuf = bufferOf(c.Args)
				return nil
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) { return []byte("content"), nil },
	}

	session := newTmuxSession("af_proj", "claude", NewMockPtyFactory(t), cmdExec)
	err := session.SendKeysCommand("a prompt that fails to paste")
	require.Error(t, err, "a failed paste-buffer must surface as an error")
	require.ErrorIs(t, err, pasteErr, "the paste error must be wrapped and returned")

	require.NotEmpty(t, loadBuf, "load-buffer must have named a buffer")
	require.Equal(t, loadBuf, deletedBuf,
		"a failed paste must delete-buffer the same buffer it loaded, not leak it (#1536)")
}

// TestPasteBufferNameProcessTokenPreventsCrossProcessCollision is the #1536
// Greptile guard: tmux buffers are server-scoped and the seq counter is
// process-local, so two af processes sharing a tmux server and a session name
// must NOT mint the same buffer name. If they did, one process's load-buffer
// would clobber — or its failure-cleanup delete-buffer would remove — the other's
// pending buffer and lose its prompt. The per-process token makes that
// impossible while the seq still keeps one process's concurrent deliveries apart.
func TestPasteBufferNameProcessTokenPreventsCrossProcessCollision(t *testing.T) {
	const session = "af_proj"

	// Same session + same seq, two different process tokens → distinct names.
	a := pasteBufferName("1234", session, 1)
	b := pasteBufferName("5678", session, 1)
	require.NotEqual(t, a, b,
		"different process tokens must not collide on the same session/seq (#1536)")
	require.Contains(t, a, "1234", "the buffer name must carry the process token")
	require.Contains(t, b, "5678", "the buffer name must carry the process token")

	// Within one process, the seq still keeps concurrent same-session deliveries
	// distinct.
	require.NotEqual(t, pasteBufferName("1234", session, 1), pasteBufferName("1234", session, 2),
		"the process-local seq must still disambiguate concurrent deliveries")

	// The live process token is non-empty, so real submits are always namespaced.
	require.NotEmpty(t, pasteBufferProcessToken, "the process token must be resolved at startup")
}

func captureRawPane(session *TmuxSession) (string, error) {
	cmd := exec.Command("tmux", "capture-pane", "-p", "-t", exactTarget(session.sanitizedName))
	out, err := session.cmdExec.Output(cmd)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
