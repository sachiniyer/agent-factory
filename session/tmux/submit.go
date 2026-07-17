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
// pasted into the pane as a BRACKETED paste, followed by a pause to let terminal
// control sequences drain, then Enter to submit.
//
// Every pane gets the bracketed paste — there is no per-agent exception list.
// See sendKeysPasteBuffer for why that is both necessary and safe (#1956).
func (t *TmuxSession) SendKeysCommand(text string) error {
	return t.sendKeysPasteBuffer(text)
}

// sendKeysPasteBuffer delivers text to the pane through a BRACKETED tmux paste
// buffer (`load-buffer` + `paste-buffer -p`) and then sends Enter to submit.
// Text is streamed via stdin rather than an argv argument so arbitrarily large
// prompts are not bounded by ARG_MAX.
//
// The paste MUST be bracketed (`-p`), for every pane. A plain paste is delivered
// to the application as ordinary KEYSTROKES, indistinguishable from typing, so an
// agent whose composer is modal executes the prompt as EDITOR COMMANDS instead of
// inserting it as text (#1956). With claude's `editorMode: "vim"` the composer
// rests in NORMAL mode, where a prompt beginning "status check…" loses its leading
// `s` to the substitute command, and one beginning "deploy…" runs `d` as the delete
// operator — the prompt mangles the box rather than filling it. Bracketing tells the
// application the bytes are pasted data, not commands. It also stops every `\n` in a
// multi-line prompt from acting as its own Enter, which submitted long prompts in
// fragments.
//
// Unconditional is safe: tmux only emits the bracketed-paste markers when the
// application has itself requested the mode (DECSET 2004), so `-p` is a literal
// no-op for panes that never enabled it. Panes that DO enable it (bash/readline,
// and the agent composers) are precisely the ones that must not be typed at.
// Codex reached this path first for a different reason — its paste-burst detection
// needs an explicit end-of-paste marker before the trailing Enter counts as a
// submit (#1254/#1256) — and keeps working unchanged; it was never special, it was
// just the only agent whose breakage was loud enough to notice.
func (t *TmuxSession) sendKeysPasteBuffer(text string) error {
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

	// `-d` deletes the buffer after pasting, `-p` brackets it (see the doc
	// comment: prompts are pasted DATA, never keystrokes), and `=` forces an
	// exact session match so input never reaches a prefix-matched sibling
	// session (#1006).
	args := []string{"paste-buffer", "-d", "-p", "-b", buf, "-t", exactTarget(t.sanitizedName)}
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
	//
	// Bracketing (above) may make this unnecessary for apps that requested the
	// mode: the `\x1b[201~` end-of-paste marker tells them the paste is complete,
	// which is the whole reason codex needed `-p` rather than a longer sleep
	// (#1254/#1256). It cannot go away unconditionally, though — `-p` is a no-op
	// for apps that never enabled the mode, and those get no marker and still
	// need the drain. Removing it is a separate change with its own evidence;
	// left alone here deliberately (#1956 follow-up).
	time.Sleep(500 * time.Millisecond)

	// Send Enter separately to submit.
	enterCmd := exec.Command("tmux", "send-keys", "-t", exactTarget(t.sanitizedName), "Enter")
	return t.cmdExec.Run(enterCmd)
}
