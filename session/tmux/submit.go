package tmux

import (
	"fmt"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
)

// pasteBufferSeq makes each bracketed-paste buffer name unique per call so two
// concurrent deliveries to the same session can never race on a shared tmux
// buffer (load-buffer overwrite between another call's load and paste). The
// daemon op-lock usually serializes same-session deliveries, but the submit
// path releases the instance lock before these tmux calls, so it must not
// assume serialization.
var pasteBufferSeq atomic.Uint64

// SendKeysCommand sends text to the tmux pane using the `tmux send-keys` command
// instead of writing to the PTY. This is more reliable for headless/scheduled runs
// where the PTY connection may not persist. Text is sent literally (with -l flag)
// followed by a pause to let terminal control sequences drain, then Enter to submit.
//
// Codex is the exception: its composer runs paste-burst detection, so a large
// run of characters delivered via `send-keys -l` is treated as an in-progress
// paste and a following Enter is absorbed into it as a newline rather than
// submitting. The prompt lands in the input box but never sends until a human
// presses Enter (#1254). Codex therefore takes the bracketed-paste path, which
// gives it an explicit end-of-paste marker so the trailing Enter is
// unambiguously a submit. Submit handling is keyed off the agent actually
// running in the pane (DetectAgentFromCommand), mirroring HasUpdated's
// per-agent prompt heuristics, so a program_overrides redirect can't
// misclassify the pane.
func (t *TmuxSession) SendKeysCommand(text string) error {
	if DetectAgentFromCommand(t.programCmd()) == ProgramCodex {
		return t.sendKeysBracketedPaste(text)
	}

	// Send text literally to avoid key name interpretation. `=` forces an
	// exact session match so input is never sent to a prefix-matched sibling
	// session if the agent session has died (#1006).
	textCmd := exec.Command("tmux", "send-keys", "-t", exactTarget(t.sanitizedName), "-l", text)
	if err := t.cmdExec.Run(textCmd); err != nil {
		return fmt.Errorf("error sending text via send-keys: %w", err)
	}

	// Wait for terminal control sequences (e.g. OSC color responses) to drain
	// before sending Enter, otherwise they can corrupt the input
	time.Sleep(500 * time.Millisecond)

	// Send Enter separately to submit
	enterCmd := exec.Command("tmux", "send-keys", "-t", exactTarget(t.sanitizedName), "Enter")
	return t.cmdExec.Run(enterCmd)
}

// sendKeysBracketedPaste delivers text to the pane as a bracketed paste (`tmux
// load-buffer` + `paste-buffer -p`) and then sends Enter to submit. tmux's `-p`
// only wraps the buffer in bracketed-paste control codes when the running
// application has actually requested bracketed-paste mode, so this is safe even
// mid-dialog (it degrades to a plain paste). The end-of-paste marker lets codex
// finalize its input buffer immediately, so the following Enter submits instead
// of being swallowed by paste-burst detection (#1254). Text is streamed via
// stdin rather than an argv argument so arbitrarily large prompts are not
// bounded by ARG_MAX.
func (t *TmuxSession) sendKeysBracketedPaste(text string) error {
	// A per-call unique buffer name: two concurrent deliveries to the same
	// session must not share a buffer, or one call's load-buffer could overwrite
	// the other's content between its load and paste and corrupt the submit.
	// `-d` clears the buffer after pasting so buffers never accumulate.
	buf := fmt.Sprintf("af_paste_%s_%d", t.sanitizedName, pasteBufferSeq.Add(1))

	loadCmd := exec.Command("tmux", "load-buffer", "-b", buf, "-")
	loadCmd.Stdin = strings.NewReader(text)
	if err := t.cmdExec.Run(loadCmd); err != nil {
		return fmt.Errorf("error loading paste buffer: %w", err)
	}

	// `-p` inserts bracketed-paste markers (when the app requested the mode),
	// `-d` deletes the buffer after pasting, `=` forces an exact session match
	// so input never reaches a prefix-matched sibling session (#1006).
	pasteCmd := exec.Command("tmux", "paste-buffer", "-d", "-p", "-b", buf, "-t", exactTarget(t.sanitizedName))
	if err := t.cmdExec.Run(pasteCmd); err != nil {
		return fmt.Errorf("error pasting buffer: %w", err)
	}

	// Wait for terminal control sequences (e.g. OSC color responses) and the
	// paste to drain before sending Enter, otherwise they can corrupt the input.
	time.Sleep(500 * time.Millisecond)

	// Send Enter separately to submit.
	enterCmd := exec.Command("tmux", "send-keys", "-t", exactTarget(t.sanitizedName), "Enter")
	return t.cmdExec.Run(enterCmd)
}
