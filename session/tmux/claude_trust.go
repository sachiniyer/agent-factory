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

const (
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

func claudeTrustRowOf(line string) claudeTrustRow {
	line = strings.TrimSpace(ansiCSISequence.ReplaceAllString(strings.TrimSuffix(line, "\r"), ""))
	// Claude Code draws this dialog inside a box on most terminals. The border
	// is chrome, not content, so a bordered row and a bare row must reduce to
	// the same label.
	line = strings.TrimSpace(strings.Trim(line, "│┃"))
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
		if strings.HasPrefix(row.label, claudeTrustAffirmativeLabel) {
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
	case !claudeTrustRowsAdjacent(rows, dialog.cursor, dialog.affirmative):
		return claudeFolderTrustDialog{}, fmt.Errorf(
			"the selected row %q and the %q row are separated by a blank line, so they are not two options of one picker",
			rows[dialog.cursor].label, claudeTrustAffirmativeLabel)
	}
	return dialog, nil
}

// claudeTrustRowsAdjacent reports whether two rows sit in the same run of
// non-blank lines.
func claudeTrustRowsAdjacent(rows []claudeTrustRow, a, b int) bool {
	if a > b {
		a, b = b, a
	}
	for idx := a + 1; idx < b; idx++ {
		if rows[idx].blank {
			return false
		}
	}
	return true
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
	dialog, err := parseClaudeFolderTrustDialog(content)
	if err != nil {
		t.refuseClaudeFolderTrust(err)
		return true
	}

	for moves := 0; !dialog.onAffirmative(); moves++ {
		if moves >= claudeTrustMaxMovementKeys {
			t.refuseClaudeFolderTrust(fmt.Errorf(
				"the cursor stayed on %q after %d movement keys and never reached %q",
				dialog.selectedLabel(), moves, claudeTrustAffirmativeLabel))
			return true
		}
		key := dialog.keyTowardAffirmative()
		if err := t.tapPromptKeys(key); err != nil {
			log.ErrorLog.Printf("could not move the %s cursor onto %q for session %q: %v",
				claudeFolderTrustDialogName, claudeTrustAffirmativeLabel, t.sanitizedName, err)
			return true
		}
		t.noteDialogKeystroke(claudeFolderTrustDialogName, claudeTrustAffirmativeLabel, key)

		moved, ok := t.awaitClaudeFolderTrustRepaint(dialog)
		if !ok {
			// af has no proof of where the cursor is now, so it must not confirm.
			// Holding costs one poll against a dialog the session was already
			// blocked on; confirming an unknown row cannot be taken back.
			log.InfoLog.Printf("session %q: %s did not repaint within %s after af sent %s; re-reading on the next check",
				t.sanitizedName, claudeFolderTrustDialogName, claudeTrustRepaintWindow, key)
			return true
		}
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
	t.claudeTrust.refusalLogged = false
	return true
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
