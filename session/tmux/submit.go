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

// minDistinctiveFragment is the shortest payload fragment treated as
// self-evidently prompt-specific. A very short prompt ("ok", "y") yields text
// that unrelated pane churn could plausibly emit, so a single completion
// sighting is weak evidence and cannot serve as a render witness. Below this
// length the delivery check additionally requires the completion to be seen in
// TWO consecutive captures, so a one-frame coincidence cannot confirm delivery
// early and let Enter race the paste after all.
const minDistinctiveFragment = 8

// deliveryOutcome is deliberately THREE-valued. "The pane did not show the
// prompt" is an observed absence only when the terminal capture proves THIS
// payload began rendering but its completion tail did not. A collapsed paste,
// a composer frame, and a failed capture all mean delivery could not be
// observed; agent-wide rendering habits may never manufacture a negative.
type deliveryOutcome int

const (
	// deliveryObservedLanded: the pasted text was observed to arrive.
	deliveryObservedLanded deliveryOutcome = iota
	// deliveryObservedAbsent: capture succeeded at the deadline, a new prefix
	// from this exact prompt was rendered, and its completion tail was not. This
	// is the genuine #1982 signal and must stay loud.
	deliveryObservedAbsent
	// deliveryCouldNotObserve: capture failed, the prompt had no distinctive
	// text, or the pane never rendered prompt-specific evidence. Nothing may be
	// concluded about delivery.
	deliveryCouldNotObserve
)

// deliveryObservation keeps the classification and the exact terminal pane
// frame together. An observed-absent diagnostic must print the same evidence
// that authorized the decision, not recapture after Enter and describe a newer,
// unrelated frame (#2255/#2266).
type deliveryObservation struct {
	outcome deliveryOutcome
	pane    string
}

// deliveryProbe binds every observation to one payload. completion is the
// suffix whose appearance proves the whole paste drained; renderWitness is a
// disjoint prefix whose appearance proves the pane DID render this payload and
// can therefore support a terminal negative when completion is still absent.
// Baselines prefer the capture after the pre-submit clear (and conservatively
// fall back to the pre-clear frame if that capture fails), so old prompt text
// in scrollback cannot be mistaken for evidence from this paste. If neither
// capture succeeds, baselineCaptured stays false and no count comparison may
// claim either positive delivery or terminal absence.
type deliveryProbe struct {
	baselineCaptured      bool
	completion            string
	completionBaseline    int
	renderWitness         string
	renderWitnessBaseline int
}

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
	t.submitMu.Lock()
	defer t.submitMu.Unlock()
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

	probe := newDeliveryProbe(text)

	// Load the payload BEFORE touching the live composer. A load failure means
	// there is nothing we can deliver, so clearing first would destroy a user's
	// draft and then return without replacing it (#2178 review). Streaming via
	// stdin also avoids ARG_MAX for arbitrarily large prompts.
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

	// Clear any draft stranded in the composer BEFORE this paste (#2070/#1982
	// half b). A prior send whose Enter never took leaves its full text sitting in
	// the composer; without a clear, this paste appends to it and the receiver
	// gets STRANDED-DRAFTNEW-PROMPT — one lost Enter silently corrupts the NEXT
	// instruction. #2065 closed the pixel-inference routes as unsound (an agent
	// echoes its prompt, an echo-off pane writes to a file — "not on screen" is
	// not "not submitted"), so we do NOT try to detect whether a strand exists:
	// that check cannot be made soundly from pane content. We clear
	// unconditionally instead, which asserts nothing about the pane. The captures
	// around the clear are for the LOG only (noteClearedComposerContent) — nothing
	// is gated on them — and the post-clear pane doubles as the delivery baseline
	// below. The log itself requires an exact visible removal; an arbitrary pane
	// redraw is not evidence that the composer held a draft (#2225).
	//
	// Clearing unconditionally is only safe because the clear keystroke is inert
	// on a BUSY pane: see clearComposerDraft for why it is C-u and explicitly NOT
	// Escape, which is the agents' INTERRUPT key and would abort a running turn on
	// every scheduled delivery.
	//
	// Cost: this is one capture more than the single (tail-conditional) baseline
	// capture it replaces, and both are unconditional. Each is bounded by
	// tmuxCommandTimeout, so against a WEDGED server the delivery path can now
	// stall one extra bound before giving up. That is the price of recording a
	// demonstrably non-empty clear; if it ever matters, the `before` capture is
	// the droppable half (it feeds only the log, never the baseline).
	priorTail := t.lastPastedTail
	before, beforeOK := t.capturePaneForDelivery()
	beforeCursor, beforeCursorOK := TerminalState{}, false
	if beforeOK {
		beforeCursor, beforeCursorOK = t.previousPasteCursor(before, priorTail)
	}
	t.clearComposerDraft()
	after, afterOK := t.capturePaneForDelivery()
	afterCursor, afterCursorOK := TerminalState{}, false
	if beforeCursorOK && afterOK {
		var err error
		afterCursor, err = t.ReadTerminalState()
		afterCursorOK = err == nil
	}
	noteClearedComposerContent(
		t.sanitizedName,
		priorTail,
		composerClearObservation{pane: before, cursor: beforeCursor, cursorKnown: beforeCursorOK},
		composerClearObservation{pane: after, cursor: afterCursor, cursorKnown: afterCursorOK},
	)

	// Baseline the pane AFTER the clear but BEFORE the paste so the post-paste
	// check waits for the prompt's tail to newly APPEAR (a count increase), not
	// merely be present: the daemon re-delivers the same prompt after a limit
	// resume (#1146), so an identical tail could already be on screen. Baselining
	// post-clear is also what lets a re-delivery whose prior attempt stranded an
	// identical tail still confirm — the clear removed that copy, so the tail
	// genuinely re-appears.
	switch {
	case afterOK:
		probe = probe.withBaseline(after)
	case beforeOK:
		// A failed post-clear capture must not erase the usable pre-clear
		// baseline. Reusing it is conservative: content the clear removed can
		// only make confirmation harder, never create a false success.
		probe = probe.withBaseline(before)
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
	// Remember only a payload tmux actually accepted. On the next submit this
	// exact tail is the provenance that distinguishes prior pasted input from a
	// spinner, footer, or unrelated pane redraw. submitMu owns the field.
	t.lastPastedTail = tail

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
	observation := t.waitForPasteDelivered(probe)

	// Send Enter separately to submit.
	if err := t.sendEnter(); err != nil {
		return err
	}

	if observation.outcome == deliveryObservedAbsent {
		// This is deliberately the only ERROR emitted by delivery observation:
		// unlike could-not-observe, the final capture contains a new prefix from
		// this prompt but not its completion tail. Keep the best-effort Enter, but
		// make this signal actionable and unhedged so genuine #1982 truncation is
		// not buried. The pane tail is load-bearing diagnostic evidence (#2266).
		log.ErrorLog.Printf("submit: prompt delivery observed absent for session %q after %s; the pane rendered this prompt's prefix but not its completion tail; "+
			"Enter sent best-effort. Pane tail: %s",
			t.sanitizedName, pasteDeliveryMaxWait, oneLineTail(observation.pane))
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

type composerClearObservation struct {
	pane        string
	cursor      TerminalState
	cursorKnown bool
}

// noteClearedComposerContent records only content from the previous successful
// paste disappearing at the live input cursor. A whole-pane difference, a
// glyph-like row prefix, or text in a neighboring footer is not provenance for
// a discarded draft (#2225). Ambiguous and failed observations stay silent,
// following deliveryCouldNotObserve.
func noteClearedComposerContent(
	sessionName string,
	previousPasteTail string,
	before, after composerClearObservation,
) {
	if !paneShowsPreviousPasteRemoved(previousPasteTail, before, after) {
		return
	}
	log.ErrorLog.Printf("submit: pre-delivery clear removed visible previous-paste content at the input cursor "+
		"for session %q; this can indicate a stranded draft. Prior pane tail: %s",
		sessionName, oneLineTail(before.pane))
}

// previousPasteCursor returns a cursor observation only when a distinctive tail
// from this TmuxSession's previous paste ends on that exact cursor row. It is a
// cheap no-op unless the tail appears somewhere in the pane; the extra bounded
// tmux read is therefore reserved for a plausible stranded-paste candidate.
func (t *TmuxSession) previousPasteCursor(pane, previousPasteTail string) (TerminalState, bool) {
	if !distinctivePreviousPasteTail(previousPasteTail) ||
		!strings.Contains(normalizeDelivery(pane), previousPasteTail) {
		return TerminalState{}, false
	}
	cursor, err := t.ReadTerminalState()
	if err != nil || !cursor.CursorVisible {
		return TerminalState{}, false
	}
	row, ok := paneRowAt(pane, cursor.CursorRow)
	if !ok || !strings.HasSuffix(normalizeDelivery(row), previousPasteTail) {
		return TerminalState{}, false
	}
	return cursor, true
}

// paneShowsPreviousPasteRemoved recognizes one deliberately narrow transition:
// a distinctive tail that THIS session previously pasted ends on the visible
// cursor row; C-u leaves the cursor on that same row but moves it backward; and
// the row loses a suffix while retaining a non-text anchor above the same
// decorative composer boundary. Comparing only the cursor row makes neighboring
// redraw text impossible to absorb into the match, while the live cursor, paste
// provenance, and boundary prevent a glyph-prefixed status row from qualifying.
func paneShowsPreviousPasteRemoved(
	previousPasteTail string,
	before, after composerClearObservation,
) bool {
	if !distinctivePreviousPasteTail(previousPasteTail) ||
		!before.cursorKnown || !after.cursorKnown ||
		!before.cursor.CursorVisible || !after.cursor.CursorVisible ||
		before.cursor.CursorRow != after.cursor.CursorRow ||
		before.cursor.CursorCol <= after.cursor.CursorCol ||
		!sameComposerBoundaryBelow(before, after) {
		return false
	}

	beforeRow, beforeOK := paneRowAt(before.pane, before.cursor.CursorRow)
	afterRow, afterOK := paneRowAt(after.pane, after.cursor.CursorRow)
	if !beforeOK || !afterOK {
		return false
	}

	normalizedBefore := normalizeDelivery(beforeRow)
	normalizedAfter := normalizeDelivery(afterRow)
	if normalizedAfter == "" || hasLetterOrNumber(normalizedAfter) ||
		!strings.HasPrefix(normalizedBefore, normalizedAfter) {
		return false
	}

	removed := strings.TrimPrefix(normalizedBefore, normalizedAfter)
	return strings.HasSuffix(removed, previousPasteTail)
}

// sameComposerBoundaryBelow requires the cursor row to sit immediately above
// an unchanged, non-empty row made only of whitespace/box drawing. This is the
// generic structure of the supported agents' idle composer divider/bottom edge;
// a busy spinner or arbitrary symbol-prefixed status line has no such evidence.
// If an agent changes that geometry, the diagnostic goes silent rather than
// guessing from a prompt glyph.
func sameComposerBoundaryBelow(before, after composerClearObservation) bool {
	row := before.cursor.CursorRow + 1
	beforeBoundary, beforeOK := paneRowAt(before.pane, row)
	afterBoundary, afterOK := paneRowAt(after.pane, row)
	if !beforeOK || !afterOK {
		return false
	}
	beforeBoundary = strings.TrimSpace(beforeBoundary)
	afterBoundary = strings.TrimSpace(afterBoundary)
	return beforeBoundary != "" &&
		beforeBoundary == afterBoundary &&
		normalizeDelivery(beforeBoundary) == ""
}

func distinctivePreviousPasteTail(tail string) bool {
	return len([]rune(tail)) >= minDistinctiveTail && hasLetterOrNumber(tail)
}

func paneRowAt(pane string, row int) (string, bool) {
	if row < 0 {
		return "", false
	}
	// capture-pane terminates its grid with one newline. Remove that terminator,
	// not every trailing newline: an actually blank bottom row must retain its
	// index so tmux's 0-based cursor_y still names the same row.
	rows := strings.Split(strings.TrimSuffix(pane, "\n"), "\n")
	if row >= len(rows) {
		return "", false
	}
	return rows[row], true
}

func hasLetterOrNumber(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool {
		return unicode.IsLetter(r) || unicode.IsNumber(r)
	}) >= 0
}

// newDeliveryProbe derives two disjoint, payload-specific witnesses. The suffix
// confirms the WHOLE paste landed before submitting: a racing Enter drops the
// tail, so the tail is exactly what must be visible. A disjoint prefix can prove
// that this same payload began rendering even when the tail never arrived — the
// high-signal #1982 truncation case.
//
// Prefer a 32-rune completion and a 24-rune prefix. For prompts longer than the
// completion but shorter than the preferred pair, shorten the completion only
// enough to reserve a distinctive prefix: otherwise a 33–55-rune payload could
// visibly render prompt-specific text yet lose #1982's terminal negative merely
// because the preferred pair does not fit. Prompts of 32 runes or fewer keep
// their entire text as the completion and therefore cannot produce a terminal
// negative. The existing two-capture rule still protects weak short strings.
func newDeliveryProbe(text string) deliveryProbe {
	n := []rune(normalizeDelivery(text))
	const (
		completionRunes = 32
		witnessRunes    = 24
	)
	if len(n) == 0 {
		return deliveryProbe{}
	}

	completionLen := len(n)
	availablePrefix := 0
	if len(n) > completionRunes {
		completionLen = completionRunes
		availablePrefix = len(n) - completionLen
		if availablePrefix < minDistinctiveFragment {
			completionLen = len(n) - minDistinctiveFragment
			availablePrefix = minDistinctiveFragment
		}
	}

	probe := deliveryProbe{completion: string(n[len(n)-completionLen:])}
	if availablePrefix >= minDistinctiveFragment {
		if availablePrefix > witnessRunes {
			availablePrefix = witnessRunes
		}
		probe.renderWitness = string(n[:availablePrefix])
	}
	return probe
}

// withBaseline records that the pane was actually measured and how many copies
// of each witness were already visible before this paste. A later observation
// is evidence only when its count grows from this captured baseline.
func (p deliveryProbe) withBaseline(content string) deliveryProbe {
	normalized := normalizeDelivery(content)
	p.baselineCaptured = true
	if p.completion != "" {
		p.completionBaseline = strings.Count(normalized, p.completion)
	}
	if p.renderWitness != "" {
		p.renderWitnessBaseline = strings.Count(normalized, p.renderWitness)
	}
	return p
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
// a failed capture only ever means "not confirmed yet". The capture stays a
// GRID (no `-J`) so cursor_y can identify the one row eligible for the #2225
// cleared-content diagnostic. normalizeDelivery strips row breaks, so terminal
// and app-drawn wrapping remain irrelevant to the delivery-tail matcher.
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
	out, err := t.outputTmuxBounded(ctx, "capture-pane", "-p", "-t", exactTarget(t.sanitizedName))
	if err != nil {
		return "", false
	}
	return string(out), true
}

// waitForPasteDelivered blocks until this payload's completion tail newly
// appears in the pane, or pasteDeliveryMaxWait elapses. A terminal negative
// requires a new, disjoint render witness from THIS payload in the final
// successful capture. Agent identity, earlier frames, generic composer chrome,
// and collapsed-paste placeholders cannot grant that authority (#2266).
//
// The caller always sends Enter afterward, so delivery is never worse than the
// fixed sleep this replaced (#1982).
func (t *TmuxSession) waitForPasteDelivered(probe deliveryProbe) deliveryObservation {
	if probe.completion == "" {
		// Nothing distinctive to confirm (empty/all-whitespace prompt). There is
		// no positive check to make, so keep the ORIGINAL 500ms drain rather than
		// quietly shortening it.
		time.Sleep(emptyPromptDrain)
		return deliveryObservation{outcome: deliveryCouldNotObserve}
	}
	deadline := time.Now().Add(pasteDeliveryMaxWait)
	streak := 0
	// A short tail is weak evidence, so require two consecutive sightings before
	// trusting it (see minDistinctiveFragment).
	needed := 1
	if len([]rune(probe.completion)) < minDistinctiveFragment {
		needed = 2
	}
	lastObservation := deliveryObservation{outcome: deliveryCouldNotObserve}
	for {
		// If the prior capture succeeded and the following poll interval carried
		// us across the deadline, that prior read is the terminal observation. Do
		// not launch an already-expired capture just to turn success into unknown.
		if time.Until(deadline) <= 0 {
			return lastObservation
		}

		// Bound each capture by what is LEFT of this loop's budget, so a wedged
		// server cannot push a single capture past the deadline below (#2099).
		if content, ok := t.capturePaneForDeliveryWithin(time.Until(deadline)); ok {
			normalized := normalizeDelivery(content)
			if probe.baselineCaptured &&
				strings.Count(normalized, probe.completion) > probe.completionBaseline {
				streak++
				if streak >= needed {
					return deliveryObservation{outcome: deliveryObservedLanded, pane: content}
				}
				// One weak short-tail sighting is neither confirmed delivery nor
				// absence. A later failure must not combine with it.
				lastObservation = deliveryObservation{outcome: deliveryCouldNotObserve, pane: content}
			} else if probe.baselineCaptured && probe.renderWitness != "" &&
				strings.Count(normalized, probe.renderWitness) > probe.renderWitnessBaseline {
				streak = 0
				lastObservation = deliveryObservation{outcome: deliveryObservedAbsent, pane: content}
			} else {
				// A successful capture is not automatically a negative. Unless it
				// newly renders this prompt's prefix, it says only that the pane was
				// readable — a collapsed placeholder and an empty composer are both
				// deliberate examples of could-not-observe (#2266).
				streak = 0
				lastObservation = deliveryObservation{outcome: deliveryCouldNotObserve, pane: content}
			}
		} else {
			// Observation continuity is load-bearing. The paste can land after an
			// earlier absent frame, and a failed frame can separate two coincidental
			// short-tail sightings, so unreadability invalidates both signals.
			streak = 0
			lastObservation = deliveryObservation{outcome: deliveryCouldNotObserve}
		}
		if time.Now().After(deadline) {
			// Only a terminal successful capture with this prompt's new render
			// witness supports a negative. Earlier evidence followed by an
			// unbound or unreadable frame is unknown: the pane may have elided or
			// drained the paste after that frame.
			return lastObservation
		}
		time.Sleep(pasteDeliveryPollInterval)
	}
}

// oneLineTail condenses pane content to a short, single-line excerpt for a log
// line: the last few non-blank rows, whitespace collapsed, row breaks marked
// with ⏎, and truncated. Shared by the terminal delivery observation and
// noteClearedComposerContent so both diagnostics use the same escaping policy.
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
