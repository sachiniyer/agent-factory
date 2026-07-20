package tmux

import (
	"context"
	"fmt"
	"os"
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

	// Clear any draft stranded in the composer BEFORE this paste (#2070/#1982
	// half b). A prior send whose Enter never took leaves its full text sitting in
	// the composer; without a clear, this paste appends to it and the receiver
	// gets STRANDED-DRAFTNEW-PROMPT — one lost Enter silently corrupts the NEXT
	// instruction. #2065 closed the pixel-inference routes as unsound (an agent
	// echoes its prompt, an echo-off pane writes to a file — "not on screen" is
	// not "not submitted"), so we do NOT try to detect whether a strand exists:
	// that check cannot be made soundly from pane content. We clear
	// unconditionally instead, which asserts nothing about the pane. The captures
	// around the clear are for the LOG only (noteClearedDraft) — nothing is gated
	// on them — and the post-clear pane doubles as the delivery baseline below.
	//
	// Clearing unconditionally is only safe because the clear keystroke is inert
	// on a BUSY pane: see clearComposerDraft for why it is C-u and explicitly NOT
	// Escape, which is the agents' INTERRUPT key and would abort a running turn on
	// every scheduled delivery.
	//
	// Cost: this is one capture more than the single (tail-conditional) baseline
	// capture it replaces, and both are unconditional. Each is bounded by
	// tmuxCommandTimeout, so against a WEDGED server the delivery path can now
	// stall one extra bound before giving up. That is the price of the
	// discarded-draft record; if it ever matters, the `before` capture is the
	// droppable half (it feeds only the log, never the baseline).
	before, _ := t.capturePaneForDelivery()
	t.clearComposerDraft()
	after, afterOK := t.capturePaneForDelivery()
	noteClearedDraft(t.sanitizedName, before, after)

	// Baseline the pane AFTER the clear but BEFORE the paste so the post-paste
	// check waits for the prompt's tail to newly APPEAR (a count increase), not
	// merely be present: the daemon re-delivers the same prompt after a limit
	// resume (#1146), so an identical tail could already be on screen. Baselining
	// post-clear is also what lets a re-delivery whose prior attempt stranded an
	// identical tail still confirm — the clear removed that copy, so the tail
	// genuinely re-appears.
	baseline := 0
	if tail != "" && afterOK {
		baseline = strings.Count(normalizeDelivery(after), tail)
	}

	// load-buffer streams the prompt in on stdin, so it needs the stdin-carrying
	// bounded runner rather than the shared one — and deliberately does NOT treat
	// exec.ErrWaitDelay as success, because a force-closed stdin pipe means the
	// payload may be truncated. See runTmuxBoundedStdin.
	loadCtx, loadCancel := tmuxTimeoutContext()
	err := runTmuxBoundedStdin(loadCtx, t.cmdExec, strings.NewReader(text), "load-buffer", "-b", buf, "-")
	loadTimedOut := loadCtx.Err() != nil
	loadCancel()
	if err != nil {
		if loadTimedOut {
			return fmt.Errorf("%w: load-buffer after %s", ErrTmuxTimeout, tmuxCommandTimeout)
		}
		return fmt.Errorf("error loading paste buffer: %w", err)
	}

	// `-d` deletes the buffer after pasting, `-p` brackets it (see the doc
	// comment: prompts are pasted DATA, never keystrokes), and `=` forces an
	// exact session match so input never reaches a prefix-matched sibling
	// session (#1006).
	pasteCtx, pasteCancel := tmuxTimeoutContext()
	pasteErr := t.runTmuxBounded(pasteCtx, "paste-buffer", "-d", "-p", "-b", buf, "-t", exactTarget(t.sanitizedName))
	pasteTimedOut := pasteCtx.Err() != nil
	pasteCancel()
	if pasteErr != nil {
		// `-d` only deletes the buffer once the paste succeeds; a failed paste
		// would otherwise strand the named buffer, and tmux buffers are
		// server-scoped (they outlive the session), so each failed submit would
		// leak one buffer unbounded. Best-effort delete it before returning —
		// bounded too, or the cleanup for a wedge-failed paste would itself wedge
		// on the same server and undo the bound we just added.
		delCtx, delCancel := tmuxTimeoutContext()
		derr := t.runTmuxBounded(delCtx, "delete-buffer", "-b", buf)
		delCancel()
		if derr != nil {
			log.ErrorLog.Printf("failed to delete paste buffer %q after paste error: %v", buf, derr)
		}
		if pasteTimedOut {
			return fmt.Errorf("%w: paste-buffer after %s", ErrTmuxTimeout, tmuxCommandTimeout)
		}
		return fmt.Errorf("error pasting buffer: %w", pasteErr)
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

// sendEnter submits whatever is pending in the pane. Bounded by
// tmuxCommandTimeout (#2099): it is the last step of a submit the daemon drives
// while holding the per-session op lock, so an unbounded stall here leaves the
// session unpromptable rather than merely dropping one Enter.
func (t *TmuxSession) sendEnter() error {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	if err := t.runTmuxBounded(ctx, "send-keys", "-t", exactTarget(t.sanitizedName), "Enter"); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("%w: send-keys Enter after %s", ErrTmuxTimeout, tmuxCommandTimeout)
		}
		return err
	}
	return nil
}

// clearComposerDraft best-effort removes any text stranded in the pane's
// composer before a new paste, so a draft whose Enter never took (#1982) cannot
// fuse with the next prompt into STRANDED-DRAFTNEW-PROMPT (#2070).
//
// It sends C-u as a KEYSTROKE — never a bracketed paste. A clear is by
// definition a command, which is exactly what `-p` exists to prevent for the
// PROMPT (#1956): the prompt is pasted DATA, the clear is typed control input.
//
// C-u ALONE, and deliberately NOT Escape. Escape looks like the natural way to
// resolve a modal composer to a known state, and it is disqualified three times
// over — measured against the real codex 0.144.6 binary in a container:
//
//   - Escape is the INTERRUPT key. Both codex and claude advertise "esc to
//     interrupt" in the footer of a working pane, and codex ships an
//     `interrupt_turn` action. Delivery to a busy agent is routine — cron and
//     watch tasks fire on schedule regardless of agent state, and the #1146
//     limit-resume re-delivery targets a session that may have resumed on its
//     own — so an Escape here would abort in-flight work on a scheduled
//     delivery. Killing a running turn is far worse than the fusion this fixes:
//     fusion corrupts one prompt, that would silently destroy real work.
//   - Escape does not even clear. Sent to a codex composer holding a stranded
//     draft, the draft was still there afterward. It buys nothing.
//   - Escape is destructive at a modal picker. Sent to codex's trust dialog it
//     dismissed the dialog and tore the session down, so a delivery arriving
//     while any picker is up would answer it by cancelling.
//
// Escape is also counterproductive for the modal case it was meant to serve: a
// vim composer rests in NORMAL mode (see sendKeysPasteBuffer on claude's
// `editorMode`), where C-u is half-page-scroll rather than kill-line. Escape
// forces the composer INTO that mode, making the clear strictly less likely to
// work. So it is pure downside on every axis.
//
// C-u carries none of that: it is not bound to interrupt in any supported agent,
// it is the POSIX tty KILL character (so readline/bash-class panes erase the
// pending line), and against real codex it cleared a stranded draft outright.
// A vim-NORMAL composer is still not cleared — that residual gap is unchanged
// from before this fix, and closing it would need the per-agent clear matrix
// #2070 warns against.
//
// Best-effort: a failed clear must NOT block delivery — the paste is what
// matters, and a session that could not be cleared is no worse off than before
// this fix — so the error is logged and swallowed. Bounded by tmuxCommandTimeout
// like every other tmux call on this path (#2099/#2105): it runs on the delivery
// path the daemon drives under the per-session op lock, so an unbounded stall
// against a wedged server would leave the session unpromptable rather than
// merely skipping one clear.
func (t *TmuxSession) clearComposerDraft() {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	// send-keys discards output, so runTmuxBounded (which normalizes
	// exec.ErrWaitDelay to success) is the right runner — the stdin-streaming
	// caveat that makes load-buffer use its own helper does not apply here.
	if err := t.runTmuxBounded(ctx, "send-keys", "-t", exactTarget(t.sanitizedName), "C-u"); err != nil {
		if ctx.Err() != nil {
			log.ErrorLog.Printf("submit: clearing composer for session %q timed out after %s (continuing to deliver)",
				t.sanitizedName, tmuxCommandTimeout)
			return
		}
		log.ErrorLog.Printf("submit: could not clear composer for session %q before delivery (continuing): %v",
			t.sanitizedName, err)
	}
}

// noteClearedDraft logs when the pre-delivery clear visibly changed the pane —
// evidence the composer held pending content that would otherwise have fused
// with this prompt (#2070). It stays SOUND because nothing is gated on it: this
// is a record for the operator, not a decision. A normalized difference between
// the pane captured before and after the clear is the observable EFFECT of our
// own C-u, not an inference about composer state from ambiguous pixels — the
// unsound move #2065 closed. A pane merely mid-render can also differ, so the
// message hedges rather than asserting a strand. Empty/failed captures produce
// no log (nb == "").
func noteClearedDraft(sessionName, before, after string) {
	nb := normalizeDelivery(before)
	if nb == "" || nb == normalizeDelivery(after) {
		return
	}
	log.ErrorLog.Printf("submit: cleared composer for session %q before delivery; the pane changed "+
		"across the clear, so a stranded draft was likely discarded (or the pane was mid-render). "+
		"Prior pane tail: %s", sessionName, oneLineTail(before))
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
// blind probe condemn a perfectly good delivery. A capture killed on a deadline
// is exactly such a failure, so a wedged server yields false — never a negative.
//
// This form is for callers with no deadline of their own and takes the package's
// standard tmuxCommandTimeout; waitForPasteDelivered's poll loop uses
// capturePaneForDeliveryWithin instead. Both were unbounded before #2099.
func (t *TmuxSession) capturePaneForDelivery() (string, bool) {
	ctx, cancel := tmuxTimeoutContext()
	defer cancel()
	return t.capturePaneForDeliveryContext(ctx)
}

// capturePaneForDeliveryWithin is capturePaneForDelivery bounded by a caller's
// own REMAINING budget rather than the flat tmuxCommandTimeout.
//
// This is what makes waitForPasteDelivered actually honor pasteDeliveryMaxWait
// (#2099). The loop's whole budget is 2s; bounding the capture inside it at
// tmuxCommandTimeout (10s) would let ONE stalled capture overshoot that budget 5x
// and leave the loop's own `time.Now().After(deadline)` check — the code that
// decides to stop waiting and send Enter best-effort — unreachable for the whole
// 10s. Fixing the hang without this would only half-fix the reported bug: the
// call would return eventually, but the deadline it is supposed to respect still
// would not hold.
func (t *TmuxSession) capturePaneForDeliveryWithin(budget time.Duration) (string, bool) {
	ctx, cancel := tmuxTimeoutContextWithin(budget)
	defer cancel()
	return t.capturePaneForDeliveryContext(ctx)
}

// capturePaneForDeliveryContext is the shared body: one bounded capture-pane,
// with any failure (including a tripped deadline) reported as "could not look".
func (t *TmuxSession) capturePaneForDeliveryContext(ctx context.Context) (string, bool) {
	out, err := t.outputTmuxBounded(ctx, "capture-pane", "-p", "-J", "-t", exactTarget(t.sanitizedName))
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
		// Bound each capture by what is LEFT of this loop's budget, so a wedged
		// server cannot push a single capture past the deadline below (#2099).
		if content, ok := t.capturePaneForDeliveryWithin(time.Until(deadline)); ok {
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
	return oneLineTail(content)
}

// oneLineTail condenses pane content to a short, single-line excerpt for a log
// line: the last few non-blank rows, whitespace collapsed, row breaks marked
// with ⏎, and truncated. Shared by paneTailForLog and noteClearedDraft so a
// discarded-draft record and a delivery-failure record read the same.
func oneLineTail(content string) string {
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
