package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// pasteBufferSeq makes each bracketed-paste buffer name unique per call so two
// concurrent deliveries to the same session can never race on a shared tmux
// buffer (load-buffer overwrite between another call's load and paste). The
// daemon op-lock usually serializes same-session deliveries, but the submit
// path releases the instance lock before these tmux calls, so it must not
// assume serialization.
var pasteBufferSeq atomic.Uint64

// pasteBufferProcessToken uniquely identifies THIS process within a tmux server.
// tmux buffers are server-scoped, and the seq counter above is process-local, so
// two af processes sharing a server and a session name would otherwise mint the
// SAME buffer name — letting one process's load-buffer clobber the other's
// pending buffer, or (with the #1536 failure cleanup) one process's delete-buffer
// remove the other's buffer and lose its prompt. The PID is unique among
// concurrently-live processes, which is exactly the collision window, so keying
// the name on it makes cross-process collision impossible. Resolved once at
// startup.
var pasteBufferProcessToken = fmt.Sprintf("%d", os.Getpid())

// pasteBufferName builds the per-call unique tmux paste-buffer name. It is keyed
// on the process token (cross-process uniqueness on a shared, server-scoped tmux
// buffer namespace), the sanitized session name, and a process-local sequence
// (intra-process uniqueness across concurrent deliveries to the same session).
func pasteBufferName(processToken, sanitizedName string, seq uint64) string {
	return fmt.Sprintf("af_paste_%s_%s_%d", processToken, sanitizedName, seq)
}

// SendKeysCommand sends text to the tmux pane using tmux commands instead of
// writing to the PTY. This is more reliable for headless/scheduled runs where
// the PTY connection may not persist. Text is loaded into a tmux paste buffer,
// pasted into the pane, followed by a pause to let terminal control sequences
// drain, then Enter to submit.
//
// Most panes receive a plain paste. Codex is the exception: its composer runs
// paste-burst detection, so it needs explicit bracketed-paste boundaries before
// the following Enter is unambiguously a submit (#1254/#1256). Submit handling
// is keyed off the agent actually running in the pane (DetectAgentFromCommand),
// mirroring HasUpdated's per-agent prompt heuristics, so a program_overrides
// redirect can't misclassify the pane.
func (t *TmuxSession) SendKeysCommand(text string) error {
	return t.sendKeysPasteBuffer(text, DetectAgentFromCommand(t.programCmd()) == ProgramCodex)
}

// sendKeysPasteBuffer delivers text to the pane through a tmux paste buffer
// (`load-buffer` + `paste-buffer`) and then sends Enter to submit. This avoids
// the literal `send-keys -l` text path whose per-character delivery can leave a
// duplicated wrapped prefix in bash/readline panes (#1292). Text is streamed via
// stdin rather than an argv argument so arbitrarily large prompts are not
// bounded by ARG_MAX.
func (t *TmuxSession) sendKeysPasteBuffer(text string, bracketed bool) error {
	// A per-call unique buffer name: two concurrent deliveries to the same
	// session must not share a buffer, or one call's load-buffer could overwrite
	// the other's content between its load and paste and corrupt the submit. The
	// process token additionally keeps two af processes on a shared tmux server
	// from colliding on the same server-scoped buffer name. `-d` clears the buffer
	// after pasting so buffers never accumulate.
	buf := pasteBufferName(pasteBufferProcessToken, t.sanitizedName, pasteBufferSeq.Add(1))

	loadCmd := exec.Command("tmux", "load-buffer", "-b", buf, "-")
	loadCmd.Stdin = strings.NewReader(text)
	if err := t.cmdExec.Run(loadCmd); err != nil {
		return fmt.Errorf("error loading paste buffer: %w", err)
	}

	// `-d` deletes the buffer after pasting, `=` forces an exact session match
	// so input never reaches a prefix-matched sibling session (#1006).
	//
	// Codex additionally needs `-p`: tmux inserts bracketed-paste markers (when
	// the app requested the mode), giving codex an end-of-paste marker so the
	// following Enter submits instead of being swallowed by paste-burst
	// detection (#1254/#1256). Other panes intentionally use a plain paste to
	// preserve their pre-existing raw-input newline behavior while still
	// avoiding the wrapped-prefix redraw issue (#1292).
	args := []string{"paste-buffer", "-d"}
	if bracketed {
		args = append(args, "-p")
	}
	args = append(args, "-b", buf, "-t", exactTarget(t.sanitizedName))
	pasteCmd := exec.Command("tmux", args...)
	if err := t.cmdExec.Run(pasteCmd); err != nil {
		// `-d` only deletes the buffer once the paste succeeds; a failed paste
		// would otherwise strand the named buffer, and tmux buffers are
		// server-scoped (they outlive the session), so each failed submit would
		// leak one buffer unbounded. Best-effort delete it before returning.
		delCmd := exec.Command("tmux", "delete-buffer", "-b", buf)
		if derr := t.cmdExec.Run(delCmd); derr != nil {
			log.ErrorLog.Printf("failed to delete paste buffer %q after paste error: %v", buf, derr)
		}
		return fmt.Errorf("error pasting buffer: %w", err)
	}

	// Wait for terminal control sequences (e.g. OSC color responses) and the
	// paste to drain before sending Enter, otherwise they can corrupt the input.
	time.Sleep(500 * time.Millisecond)

	// Send Enter separately to submit.
	enterCmd := exec.Command("tmux", "send-keys", "-t", exactTarget(t.sanitizedName), "Enter")
	return t.cmdExec.Run(enterCmd)
}
