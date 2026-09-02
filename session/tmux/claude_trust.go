package tmux

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/log"
)

// claudeTrustAffirmativeLabel is the option af must confirm on Claude Code's
// folder-trust dialog. It is matched as a LABEL, never as a position: #3579 is
// what happened when af answered that dialog with a bare Enter and Claude Code
// 2.1.257 made "No, exit" the preselected row, so af's own dismissal quit the
// agent and the create failed with "capture-pane: exit status 1".
const claudeTrustAffirmativeLabel = "Yes, I trust this folder"

// claudeTrustSelectionGlyph is the cursor Claude Code draws on the selected row
// of a launch-time picker (U+276F). Its composer prompt uses the SAME glyph, so
// it is read as a selection cursor only inside a dialog claudeTrustPromptPresent
// has already identified.
const claudeTrustSelectionGlyph = "❯"

// claudeTrustAffordancePrefix is the modal's own footer. Requiring it to be the
// LAST content on screen is what separates a live dialog from a transcript that
// quotes one: the composer of a working agent is always painted below its
// output, so a quoted dialog can never be the last thing in the pane. Measured
// against claude 2.1.258 with af's own capture flags (-p -e -J): rows 15/16 are
// the options, 17 is blank, 18 is this footer, and 19-30 are blank.
//
// The hidden-cursor oracle the codex branch uses is NOT available here. Claude
// Code hides the terminal cursor in the modal (cursor_flag=0) — and in its
// ordinary composer too, also measured — so cursor visibility cannot tell the
// two apart for this agent. Structure is the only discriminator, so it carries
// the whole weight.
const claudeTrustAffordancePrefix = "Enter to confirm"

const (
	// claudeTrustMoveProofWindow is how long af treats a movement key it has not
	// seen land as possibly still in flight. Inside it af sends NOTHING; past
	// it, af concludes the key never reached the agent and may send another.
	//
	// Both directions are real, and the window is what separates them:
	//
	//   - A key that LANDED but has not repainted yet must not be re-sent: the
	//     cursor has already moved, so a second key would push it back off the
	//     affirmative row.
	//   - A key that was DROPPED must be re-sent, or the dialog is never
	//     answered and the session start fails. Claude Code drops keys sent in
	//     the instant after it paints this dialog and before it is reading
	//     input — measured on 2.1.258, roughly one create in six.
	//
	// 3s is the margin between them, and it is measured rather than guessed: on
	// this box, a Down that landed repainted in 31-92ms across every successful
	// run, while a dropped one showed no change through 16 consecutive captures.
	// 3s is ~30x the observed repaint, so a pane still showing the old row after
	// it is evidence about the key, not about the terminal.
	claudeTrustMoveProofWindow = 3 * time.Second
	// claudeTrustMaxMoveAttempts bounds re-sends across polls. A dialog that
	// swallows this many movement keys is not one af can drive, and saying so is
	// worth more than a session that retries until its budget runs out.
	claudeTrustMaxMoveAttempts = 4
	// claudeTrustMaxMovementKeys bounds the Down/Up keys af may inject into one
	// dialog in a single check. Direction is recomputed from a fresh capture
	// after every key, so a picker that moves the cursor somewhere af did not
	// predict converges within a couple of steps — and one that ignores the keys
	// entirely stops here and is REPORTED rather than confirmed blind.
	claudeTrustMaxMovementKeys = 4
	// claudeTrustRepaintWindow bounds how long af waits for the terminal to
	// redraw the row it just moved the cursor to. `send-keys` returns once the
	// key is written to the pty, so an immediate capture races the redraw; a
	// stale frame read as "the cursor did not move" is what would make af send a
	// second movement key and overshoot.
	//
	// It is short on purpose. This runs on the daemon's SEQUENTIAL per-instance
	// poll, so time spent here is time every later instance waits. Half a second
	// covers a terminal redraw with room to spare, and a redraw that misses it
	// costs one poll, not a wrong keystroke: af returns without confirming and
	// the next check re-reads the pane's real state.
	claudeTrustRepaintWindow = 500 * time.Millisecond
	claudeTrustRepaintPoll   = 25 * time.Millisecond
)

// claudeTrustNow and claudeTrustSleep are indirected so a test can drive the
// repaint window without depending on wall-clock timing.
var (
	claudeTrustNow   = time.Now
	claudeTrustSleep = time.Sleep
)

// claudeTrustState is the only cross-poll memory the folder-trust handler needs:
// whether its refusal has already been reported, so an unreadable dialog is
// logged once instead of on every tick of the daemon's per-second poll.
// Access only while inputMu is held.
type claudeTrustState struct {
	refusalLogged bool
	// pendingFrom is the label the cursor sat on when af last sent a movement
	// key whose redraw it did not get to observe. While it is set af sends NO
	// further movement key: `send-keys` returning is not proof the key landed,
	// and a terminal that has not repainted yet is not proof it did not. A
	// second key sent against a cursor that already moved would push it back
	// off the affirmative row, and a redraw landing between the verification
	// and confirmation captures could then let Enter reach the wrong option —
	// which is the very failure #3579 is about.
	//
	// Only a frame showing a DIFFERENT selected label releases it, or a new
	// pane process. This mirrors handleCodexSafetyBuffering's pending-selection
	// discipline: holding costs a session that stays blocked on a dialog it was
	// already blocked on, and that failure is loud and bounded; a movement key
	// af cannot take back is neither.
	pendingFrom  string
	pendingSince time.Time
	pendingPolls int
	// moveAttempts counts movement keys af has sent for the dialog currently on
	// screen. It resets whenever the cursor is observed to move, so it counts
	// keys that achieved nothing, not keys.
	moveAttempts int
}

func (s *claudeTrustState) beginPendingMove(from string) {
	s.pendingFrom, s.pendingSince, s.pendingPolls = from, claudeTrustNow(), 0
	s.moveAttempts++
}

func (s *claudeTrustState) clearPendingMove() {
	s.pendingFrom, s.pendingSince, s.pendingPolls = "", time.Time{}, 0
}

// pendingMoveUnproven reports whether a movement key af sent could still be in
// flight, so af must send nothing.
func (s *claudeTrustState) pendingMoveUnproven() bool {
	return s.pendingFrom != "" && claudeTrustNow().Sub(s.pendingSince) < claudeTrustMoveProofWindow
}

// claudeTrustRow is one visible pane line reduced to what af navigates by: the
// option's LABEL and whether it carries the selection cursor. The numeric
// ordinal Claude Code renders on some builds is picker syntax and is discarded
// here — keying off it is the same assumption #3579 was, one layer down.
type claudeTrustRow struct {
	label    string
	selected bool
	blank    bool
}

// claudeTrustBoxDrawing is the frame some Claude Code builds draw around this
// dialog. Every rune here is chrome: a row made only of them says nothing about
// what is on screen, so it neither separates options nor counts as content
// painted below the footer. claude 2.1.258 draws the dialog unframed, but the
// framed form must parse identically — a rendering change is not a reason to
// stop answering the dialog.
const claudeTrustBoxDrawing = "│┃║╭╮╰╯┌┐└┘╔╗╚╝─━═┄┈├┤┏┓┗┛┬┴┼╠╣╦╩╬▏▕"

func claudeTrustRowOf(line string) claudeTrustRow {
	line = strings.TrimSpace(ansiCSISequence.ReplaceAllString(strings.TrimSuffix(line, "\r"), ""))
	// Strip the side borders so a bordered row and a bare row reduce to the
	// same label, then treat a row that was ONLY frame as blank.
	line = strings.TrimSpace(strings.Trim(line, claudeTrustBoxDrawing))
	if line == "" {
		return claudeTrustRow{blank: true}
	}
	row := claudeTrustRow{}
	if rest, ok := strings.CutPrefix(line, claudeTrustSelectionGlyph); ok {
		row.selected = true
		line = strings.TrimSpace(rest)
	}
	row.label = strings.TrimSpace(trimClaudeTrustOrdinal(line))
	return row
}

// trimClaudeTrustOrdinal drops a leading "N. " picker ordinal. Mirrors
// codexPickerRow: the ordinal is parsed only so it cannot be mistaken for part
// of the label.
func trimClaudeTrustOrdinal(line string) string {
	dot := strings.Index(line, ". ")
	if dot <= 0 || dot > 3 {
		return line
	}
	for _, r := range line[:dot] {
		if r < '0' || r > '9' {
			return line
		}
	}
	return line[dot+2:]
}

// claudeFolderTrustDialog is a parsed folder-trust modal: every visible row,
// plus the index of the row that owns the cursor and the index of the row af
// must confirm.
type claudeFolderTrustDialog struct {
	rows        []claudeTrustRow
	cursor      int
	affirmative int
}

func (d claudeFolderTrustDialog) onAffirmative() bool {
	return d.cursor >= 0 && d.cursor == d.affirmative
}

// keyTowardAffirmative picks the direction by RENDER ORDER, so it is correct
// for both orders the dialog has shipped with — and for any future one.
func (d claudeFolderTrustDialog) keyTowardAffirmative() string {
	if d.affirmative > d.cursor {
		return "Down"
	}
	return "Up"
}

func (d claudeFolderTrustDialog) selectedLabel() string {
	if d.cursor < 0 || d.cursor >= len(d.rows) {
		return ""
	}
	return d.rows[d.cursor].label
}

// claudeFolderTrustDialogPresent reports whether the capture shows the dialog
// whose affirmative row af locates by label. The MCP-server prompt and the
// legacy "Do you trust the files in this folder?" wording render no such row;
// they keep the historical Enter tap.
func claudeFolderTrustDialogPresent(content string) bool {
	return strings.Contains(ansiCSISequence.ReplaceAllString(content, ""), claudeTrustAffirmativeLabel)
}

// parseClaudeFolderTrustDialog locates the cursor row and the affirmative row.
//
// Both must be unambiguous and both must belong to the same block of adjacent
// non-blank rows, because that is the only evidence that they are two options
// of ONE picker rather than a live dialog plus a quoted mention of its label
// somewhere else on screen. Anything less is reported as an error: af refuses to
// answer a dialog it cannot read, which costs a retry, where guessing costs the
// agent (#3579).
func parseClaudeFolderTrustDialog(content string) (claudeFolderTrustDialog, error) {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	rows := make([]claudeTrustRow, 0, len(lines))
	for _, line := range lines {
		rows = append(rows, claudeTrustRowOf(line))
	}

	dialog := claudeFolderTrustDialog{rows: rows, cursor: -1, affirmative: -1}
	cursors, affirmatives := 0, 0
	for idx, row := range rows {
		if row.selected {
			dialog.cursor, cursors = idx, cursors+1
		}
		// EXACT, not a prefix: "Yes, I trust this folder (quoted from the docs)"
		// is prose about the dialog, not an option of one.
		if row.label == claudeTrustAffirmativeLabel {
			dialog.affirmative, affirmatives = idx, affirmatives+1
		}
	}

	switch {
	case cursors == 0 && affirmatives == 0:
		return claudeFolderTrustDialog{}, fmt.Errorf("no row carries the %q selection cursor and none is labelled %q",
			claudeTrustSelectionGlyph, claudeTrustAffirmativeLabel)
	case cursors == 0:
		return claudeFolderTrustDialog{}, fmt.Errorf("no row carries the %q selection cursor", claudeTrustSelectionGlyph)
	case cursors > 1:
		return claudeFolderTrustDialog{}, fmt.Errorf("%d rows carry the %q selection cursor, so af cannot tell which option is selected",
			cursors, claudeTrustSelectionGlyph)
	case affirmatives == 0:
		return claudeFolderTrustDialog{}, fmt.Errorf("no row is labelled %q", claudeTrustAffirmativeLabel)
	case affirmatives > 1:
		return claudeFolderTrustDialog{}, fmt.Errorf("%d rows are labelled %q", affirmatives, claudeTrustAffirmativeLabel)
	}
	if err := claudeTrustPickerStructure(rows, dialog.cursor, dialog.affirmative); err != nil {
		return claudeFolderTrustDialog{}, err
	}
	return dialog, nil
}

// claudeTrustPickerStructure requires the two rows to be options of a LIVE
// picker rather than two lines that happen to be on screen together.
//
// Text alone cannot decide that. This handler runs on the daemon's continuous
// poll against arbitrary agent output — including output that quotes this very
// dialog, or this very file — and injecting Down/Up/Enter into a working
// agent's composer is not recoverable. So the shape is required, all of it:
//
//   - the two rows sit in one unbroken run of non-blank rows (one picker),
//   - that run holds at least two rows (a picker has options, plural),
//   - the modal's footer is the next content below the run, and
//   - that footer is the LAST content in the pane.
//
// The last requirement is the load-bearing one, and it is the same shape
// CodexTrustPromptPresent's `affordance == last` rule already relies on: a
// working agent paints its composer beneath its output, so a quoted dialog has
// something after it and a live one does not.
func claudeTrustPickerStructure(rows []claudeTrustRow, cursor, affirmative int) error {
	first, last := claudeTrustRunBounds(rows, affirmative)
	if cursor < first || cursor > last {
		return fmt.Errorf("the selected row %q and the %q row are not in one unbroken block, so they are not two options of one picker",
			rows[cursor].label, claudeTrustAffirmativeLabel)
	}
	if last-first+1 < 2 {
		return fmt.Errorf("%q is the only row in its block; a picker af can navigate has more than one option",
			claudeTrustAffirmativeLabel)
	}
	footer := claudeTrustNextContentRow(rows, last+1)
	if footer < 0 || !strings.HasPrefix(rows[footer].label, claudeTrustAffordancePrefix) {
		return fmt.Errorf("the option block is not followed by the modal's %q footer", claudeTrustAffordancePrefix)
	}
	if claudeTrustNextContentRow(rows, footer+1) >= 0 {
		return fmt.Errorf("content is painted below the %q footer, so this is a dialog being quoted rather than one waiting for input",
			claudeTrustAffordancePrefix)
	}
	return nil
}

// claudeTrustRunBounds returns the inclusive bounds of the unbroken run of
// non-blank rows containing idx.
func claudeTrustRunBounds(rows []claudeTrustRow, idx int) (first, last int) {
	first, last = idx, idx
	for first > 0 && !rows[first-1].blank {
		first--
	}
	for last < len(rows)-1 && !rows[last+1].blank {
		last++
	}
	return first, last
}

// claudeTrustNextContentRow returns the first non-blank row at or after from,
// or -1 when the rest of the pane is blank.
func claudeTrustNextContentRow(rows []claudeTrustRow, from int) int {
	for idx := from; idx < len(rows); idx++ {
		if !rows[idx].blank {
			return idx
		}
	}
	return -1
}

// answerClaudeTrustPrompt answers a Claude Code launch gate that
// claudeTrustPromptPresent has already identified. It always reports true: the
// dialog was positively identified, so every outcome — accepted, unproven, or
// refused — leaves the caller owing another check before it may type into the
// pane (#3302).
func (t *TmuxSession) answerClaudeTrustPrompt(content string) bool {
	if !claudeFolderTrustDialogPresent(content) {
		// The MCP-server trust prompt, or the legacy folder-trust wording.
		// Neither renders a row af can locate by label and neither is what
		// regressed in 2.1.257, so both keep the historical Enter tap on
		// whatever Claude Code preselected.
		if err := t.TapEnter(); err != nil {
			log.ErrorLog.Printf("could not tap enter on trust/MCP screen: %v", err)
			return true
		}
		t.noteDialogKeystroke(claudeLaunchGateDialogName, "", "Enter")
		return true
	}
	return t.selectClaudeFolderTrust(content)
}

// selectClaudeFolderTrust moves the cursor onto the affirmative row and
// confirms it — reading the pane at every step instead of assuming a preselected
// option or a fixed key sequence.
//
// The shape is the one the codex safety-check handler beside it already uses:
// look, act, then look again before the irreversible key. Enter is sent only
// after a FRESH capture proves the affirmative row owns the cursor and the
// dialog is still the thing on screen; without that second proof, a dialog that
// closed underneath af would take the Enter into the agent's composer.
func (t *TmuxSession) selectClaudeFolderTrust(content string) bool {
	state := &t.claudeTrust
	dialog, err := parseClaudeFolderTrustDialog(content)
	if err != nil {
		if state.pendingMoveUnproven() {
			// A frame af cannot read is not evidence that a movement key it
			// already sent did nothing either. Keep holding.
			state.pendingPolls++
			return true
		}
		t.refuseClaudeFolderTrust(err)
		return true
	}

	// A movement key af sent but never saw land keeps this handler read-only
	// while it could still be in flight. See claudeTrustMoveProofWindow.
	if state.pendingFrom != "" {
		switch {
		case dialog.selectedLabel() != state.pendingFrom:
			// The cursor moved: the key landed, and af may act on what it sees.
			state.clearPendingMove()
			state.moveAttempts = 0
		case state.pendingMoveUnproven():
			state.pendingPolls++
			return true
		default:
			// Far past any repaint and the cursor never moved, so the key never
			// reached the agent — Claude Code drops keys sent in the instant
			// after it paints this dialog. Re-sending is now the safe action;
			// it is the one thing af must NOT do while the key might still be
			// in flight.
			log.WarningLog.Printf(
				"session %q: %s did not act on the %d key(s) af sent in %s (cursor still on %q); re-sending",
				t.sanitizedName, claudeFolderTrustDialogName, state.moveAttempts,
				claudeTrustNow().Sub(state.pendingSince).Round(time.Millisecond), state.pendingFrom)
			state.clearPendingMove()
		}
	}

	for moves := 0; !dialog.onAffirmative(); moves++ {
		if moves >= claudeTrustMaxMovementKeys || state.moveAttempts >= claudeTrustMaxMoveAttempts {
			t.refuseClaudeFolderTrust(fmt.Errorf(
				"the cursor stayed on %q after %d movement keys and never reached %q",
				dialog.selectedLabel(), state.moveAttempts, claudeTrustAffirmativeLabel))
			return true
		}
		key := dialog.keyTowardAffirmative()
		// Recorded BEFORE the send: a send-keys that errors — or times out — is
		// not proof the key failed to reach the pane, so it must hold too.
		state.beginPendingMove(dialog.selectedLabel())
		if err := t.tapPromptKeys(key); err != nil {
			log.ErrorLog.Printf("could not move the %s cursor onto %q for session %q: %v",
				claudeFolderTrustDialogName, claudeTrustAffirmativeLabel, t.sanitizedName, err)
			return true
		}
		t.noteDialogKeystroke(claudeFolderTrustDialogName, claudeTrustAffirmativeLabel, key)

		moved, ok := t.awaitClaudeFolderTrustRepaint(dialog)
		if !ok {
			// af has no proof of where the cursor is now, so it must not confirm
			// — and the pending record above is what stops the next poll from
			// re-sending this key against a cursor that may already have moved.
			log.InfoLog.Printf("session %q: %s did not repaint within %s after af sent %s; sending nothing further for %s, in case the key is still in flight",
				t.sanitizedName, claudeFolderTrustDialogName, claudeTrustRepaintWindow, key, claudeTrustMoveProofWindow)
			return true
		}
		state.clearPendingMove()
		state.moveAttempts = 0
		dialog = moved
	}

	confirmed, err := t.captureClaudeFolderTrustDialog()
	if err != nil {
		log.InfoLog.Printf("session %q: could not confirm the %s selection before accepting it: %v",
			t.sanitizedName, claudeFolderTrustDialogName, err)
		return true
	}
	if !confirmed.onAffirmative() {
		log.InfoLog.Printf("session %q: %s shows %q selected, not %q; not confirming this frame",
			t.sanitizedName, claudeFolderTrustDialogName, confirmed.selectedLabel(), claudeTrustAffirmativeLabel)
		return true
	}
	if err := t.tapPromptKeys("Enter"); err != nil {
		log.ErrorLog.Printf("could not confirm %q on the %s dialog for session %q: %v",
			claudeTrustAffirmativeLabel, claudeFolderTrustDialogName, t.sanitizedName, err)
		return true
	}
	t.noteDialogKeystroke(claudeFolderTrustDialogName, claudeTrustAffirmativeLabel, "Enter")
	state.refusalLogged = false
	return true
}

// resetClaudeTrustState drops this handler's cross-poll memory at a proven
// runtime boundary. A movement key af sent to the PREVIOUS pane process cannot
// be pending against a new one, and a refusal reported for the old pane must not
// silence the same refusal for its replacement.
func (t *TmuxSession) resetClaudeTrustState() {
	t.inputMu.Lock()
	defer t.inputMu.Unlock()
	t.claudeTrust = claudeTrustState{}
}

// awaitClaudeFolderTrustRepaint waits for the pane to show the cursor on a
// different option than prev did, bounded by claudeTrustRepaintWindow. The
// comparison is by LABEL, not by line index: a redraw may reflow the rows.
func (t *TmuxSession) awaitClaudeFolderTrustRepaint(prev claudeFolderTrustDialog) (claudeFolderTrustDialog, bool) {
	deadline := claudeTrustNow().Add(claudeTrustRepaintWindow)
	for {
		dialog, err := t.captureClaudeFolderTrustDialog()
		if err == nil && dialog.selectedLabel() != prev.selectedLabel() {
			return dialog, true
		}
		if !claudeTrustNow().Before(deadline) {
			return claudeFolderTrustDialog{}, false
		}
		claudeTrustSleep(claudeTrustRepaintPoll)
	}
}

// captureClaudeFolderTrustDialog re-reads the pane and parses it, requiring the
// dialog to still be the thing on screen. A capture that no longer shows the
// modal is an error and not an empty dialog: it is exactly the state in which
// injecting a key would reach the agent's composer instead.
func (t *TmuxSession) captureClaudeFolderTrustDialog() (claudeFolderTrustDialog, error) {
	content, err := t.CapturePaneContent()
	if err != nil {
		return claudeFolderTrustDialog{}, err
	}
	if !claudeTrustPromptPresent(content) || !claudeFolderTrustDialogPresent(content) {
		return claudeFolderTrustDialog{}, errors.New("the folder-trust dialog is no longer on screen")
	}
	return parseClaudeFolderTrustDialog(content)
}

// refuseClaudeFolderTrust reports a dialog af will not answer, once per dialog
// rather than once per poll. Refusing surfaces as the create's trust-prompt
// budget expiring, which is a loud, recoverable failure; the alternative —
// pressing Enter on whatever happens to be selected — is how #3579 quit users'
// agents silently.
func (t *TmuxSession) refuseClaudeFolderTrust(reason error) {
	if t.claudeTrust.refusalLogged {
		return
	}
	t.claudeTrust.refusalLogged = true
	log.ErrorLog.Printf(
		"refusing to answer the %s dialog for session %q: %v; af will not press a key it cannot aim at %q",
		claudeFolderTrustDialogName, t.sanitizedName, reason, claudeTrustAffirmativeLabel)
}
