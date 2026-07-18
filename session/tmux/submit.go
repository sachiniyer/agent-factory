package tmux

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/sachiniyer/agent-factory/log"
)

// pasteDeliveryMaxWait bounds how long the submit path waits for a pasted prompt
// to appear in the pane before sending Enter, and pasteDeliveryPollInterval is
// how often it re-checks. They are package vars so tests can tighten them. The
// defaults confirm an idle pane in a single poll (well under the old fixed
// 500ms) while still tolerating a pane that is mid-render and drains slower than
// 500ms — the exact case that stranded prompts (#1982).
var (
	pasteDeliveryMaxWait      = 2 * time.Second
	pasteDeliveryPollInterval = 50 * time.Millisecond
	// emptyPromptDrain is the fallback for a prompt that normalizes to nothing,
	// where there is no text to positively confirm. It keeps the ORIGINAL 500ms
	// drain rather than silently shortening it.
	emptyPromptDrain = 500 * time.Millisecond
)

// minDistinctiveTail is the shortest tail treated as self-evidently distinctive.
// A very short prompt ("ok", "y") yields a tail that unrelated pane churn could
// plausibly emit, so a single sighting is weak evidence. Below this length the
// delivery check additionally requires the tail to be seen in TWO consecutive
// captures, so a one-frame coincidence cannot confirm delivery early and let
// Enter race the paste after all.
const minDistinctiveTail = 8

// deliveryOutcome is deliberately THREE-valued. A probe that cannot see the pane
// must never manufacture a negative — the failure mode this repo keeps
// re-learning — so "I could not look" stays distinct from "I looked and it was
// not there". Only the latter is evidence of a failed delivery.
type deliveryOutcome int

const (
	// deliveryConfirmed: the pasted text was observed to arrive.
	deliveryConfirmed deliveryOutcome = iota
	// deliveryNotObserved: captures were working and the text never appeared on
	// screen. Note the name — this is NOT "it did not arrive". A pane is free to
	// consume input without echoing it (anything with echo off, e.g. a password
	// prompt; the #1956 receiver-pane gate is exactly such a program, and its
	// bytes provably arrive while the screen only ever shows AF-RECEIVER-READY).
	// Arrival and echo are different facts, so this stays a loud WARNING and
	// never an error: treating it as a delivery failure would condemn every
	// non-echoing pane.
	deliveryNotObserved
	// deliveryUnknown: capture itself failed (headless/transient), or there was
	// nothing assertable. Nothing may be concluded.
	deliveryUnknown
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

	tail := deliveryTail(text)

	// Baseline the pane BEFORE delivery so the post-paste check waits for the
	// prompt's tail to newly APPEAR (a count increase), not merely be present:
	// the daemon re-delivers the same prompt after a limit resume (#1146), so an
	// identical tail could already be on screen.
	baseline := 0
	if tail != "" {
		if content, ok := t.capturePaneForDelivery(); ok {
			baseline = strings.Count(normalizeDelivery(content), tail)
		}
	}

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

	// Confirm the paste actually LANDED in the pane before sending Enter, instead
	// of sleeping a fixed 500ms and hoping it drained. Enter is a separate
	// `send-keys`, and when it overtakes an as-yet-undrained paste it either
	// truncates the command or — on a bracketed-paste composer that has not yet
	// processed the `\x1b[201~` end-of-paste marker — is absorbed as a literal
	// newline, stranding the prompt in the composer unsubmitted while the send
	// still reports success (#1982). The fixed sleep undershot precisely when the
	// pane was mid-render (a spinner/animation shortly after a turn), which is
	// where the strands clustered. Waiting for the pasted tail to appear ties the
	// Enter to observed delivery: it returns as soon as the paste is visible
	// (usually well under the old 500ms) and, on the rare pane where capture
	// cannot confirm it, falls back to sending Enter after the cap so delivery is
	// never worse than the old blind sleep.
	delivered := t.waitForPasteDelivered(tail, baseline)

	// Send Enter separately to submit.
	if err := t.sendEnter(); err != nil {
		return err
	}

	if delivered == deliveryNotObserved {
		// Loud, but NOT an error — see deliveryNotObserved. This is the "silent
		// success" half of #1982 addressed as far as this layer soundly can:
		// the operator gets a record that the prompt was never seen to land,
		// without a delivery failure being invented for every non-echoing pane.
		log.ErrorLog.Printf("submit: prompt for session %q was never observed on screen; "+
			"if this pane echoes input, the prompt may be unsubmitted. Pane tail: %s",
			t.sanitizedName, paneTailForLog(t))
	}
	return nil
}

// sendEnter submits whatever is pending in the pane.
func (t *TmuxSession) sendEnter() error {
	enterCmd := exec.Command("tmux", "send-keys", "-t", exactTarget(t.sanitizedName), "Enter")
	return t.cmdExec.Run(enterCmd)
}

// deliveryTail returns a distinctive whitespace/box-free suffix of text used to
// confirm the WHOLE paste landed before submitting: a racing Enter drops the
// TAIL, so the tail is exactly what must be visible. Whitespace and box-drawing
// glyphs are removed so an agent composer that wraps the prompt inside its
// border box (claude/aider render one) still reads back as one contiguous run.
// A short tail keeps it within a single composer line so a wrap can't split it.
func deliveryTail(text string) string {
	n := []rune(normalizeDelivery(text))
	const tailRunes = 32
	if len(n) > tailRunes {
		n = n[len(n)-tailRunes:]
	}
	return string(n)
}

// normalizeDelivery strips whitespace and box-drawing / block-element glyphs so
// a prompt wrapped inside a composer box reads back as one contiguous run,
// independent of the pane's width or the agent's framing.
func normalizeDelivery(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if unicode.IsSpace(r) || (r >= 0x2500 && r <= 0x259F) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// capturePaneForDelivery returns the pane's visible content, or "" if capture
// fails (a transient/headless miss just means "not confirmed yet" — the caller
// keeps polling and ultimately falls back to a best-effort Enter).
//
// It deliberately does NOT reuse CapturePaneContent, which is shaped for prompt
// detection rather than text matching: that helper passes `-e` to PRESERVE ANSI
// escapes, and a colourized composer would then interleave escape bytes through
// the very text being matched. It also probes ExistsOrUnknown() on failure,
// spawning a second tmux subprocess — wasted work inside the submit path, where
// a failed capture only ever means "not confirmed yet". `-J` is kept, so a line
// the TERMINAL wrapped is rejoined by tmux itself; app-drawn wrapping inside a
// composer's border box is handled by normalizeDelivery instead.
// The bool reports whether the capture itself SUCCEEDED. Callers must keep that
// separate from "the text was not there": a failed capture means the pane could
// not be inspected at all, and collapsing that into a negative would let a
// blind probe condemn a perfectly good delivery.
func (t *TmuxSession) capturePaneForDelivery() (string, bool) {
	cmd := exec.Command("tmux", "capture-pane", "-p", "-J", "-t", exactTarget(t.sanitizedName))
	out, err := t.cmdExec.Output(cmd)
	if err != nil {
		return "", false
	}
	return string(out), true
}

// waitForPasteDelivered blocks until the pasted prompt's tail newly appears in
// the pane (count exceeds the pre-paste baseline), or pasteDeliveryMaxWait
// elapses. On expiry it logs and returns so Enter is still sent best-effort —
// delivery is never worse than the fixed sleep it replaces (#1982).
func (t *TmuxSession) waitForPasteDelivered(tail string, baseline int) deliveryOutcome {
	if tail == "" {
		// Nothing distinctive to confirm (empty/all-whitespace prompt). There is
		// no positive check to make, so keep the ORIGINAL 500ms drain rather than
		// quietly shortening it.
		time.Sleep(emptyPromptDrain)
		return deliveryUnknown
	}
	deadline := time.Now().Add(pasteDeliveryMaxWait)
	sawCapture := false
	streak := 0
	// A short tail is weak evidence, so require two consecutive sightings before
	// trusting it (see minDistinctiveTail).
	needed := 1
	if len([]rune(tail)) < minDistinctiveTail {
		needed = 2
	}
	for {
		if content, ok := t.capturePaneForDelivery(); ok {
			sawCapture = true
			if strings.Count(normalizeDelivery(content), tail) > baseline {
				streak++
				if streak >= needed {
					return deliveryConfirmed
				}
			} else {
				streak = 0
			}
		}
		if time.Now().After(deadline) {
			if !sawCapture {
				// Never saw the pane at all — unknown, not absent.
				log.ErrorLog.Printf("submit: could not capture pane for session %q while confirming delivery; sending Enter best-effort",
					t.sanitizedName)
				return deliveryUnknown
			}
			log.ErrorLog.Printf("submit: paste delivery for session %q NOT observed within %s (the pane may simply not echo input); sending Enter best-effort",
				t.sanitizedName, pasteDeliveryMaxWait)
			return deliveryNotObserved
		}
		time.Sleep(pasteDeliveryPollInterval)
	}
}

// paneTailForLog returns a short, single-line excerpt of the pane so a failure
// says what the pane actually looked like instead of only that something failed.
func paneTailForLog(t *TmuxSession) string {
	content, ok := t.capturePaneForDelivery()
	if !ok {
		return "<pane not capturable>"
	}
	lines := strings.Split(strings.TrimRight(content, "\n \t"), "\n")
	keep := lines
	if len(keep) > 3 {
		keep = keep[len(keep)-3:]
	}
	joined := strings.Join(keep, " ⏎ ")
	joined = strings.Join(strings.Fields(joined), " ")
	if len([]rune(joined)) > 200 {
		joined = string([]rune(joined)[:200]) + "…"
	}
	if joined == "" {
		return "<pane empty>"
	}
	return joined
}
