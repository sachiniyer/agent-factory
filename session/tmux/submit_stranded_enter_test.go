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

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
)

// stagedDraftPane is a composer state machine for the #4200 remedy. The pane is
// transcript rows above a composer whose first row carries the prompt glyph;
// the cursor sits on the composer's last row. Enter is modelled the way tmux
// delivers it: the boundary frame captured in the same command queue as Enter
// shows the pane BEFORE the application reacts, and the reaction lands after.
//
// swallowedEnters is how many leading Enters the composer absorbs as a literal
// newline — the #4200 defect: the draft stays and gains a blank row. After
// that an Enter submits: the draft leaves the composer and echoes into the
// transcript, which for a chip is the same chip text real Codex draws for a
// pending paste (daemon/configagent.go).
type stagedDraftPane struct {
	mu              sync.Mutex
	pastes          int
	enters          int
	snapshots       int
	lastLoaded      string
	transcript      []string
	composer        []string
	glyph           string
	boxed           bool
	hiddenCursor    bool
	movedCursor     bool
	clearIsNoop     bool
	swallowedEnters int
	// swallowNoNewline, when set, makes a swallowed Enter absorb WITHOUT
	// appending a blank composer row — the no-newline-swallow shape: the
	// draft stays and the cursor remains on the chip's last row instead of
	// dropping to a blank continuation row. Zero value (false) keeps the
	// canonical absorbed-newline behavior every other test relies on.
	swallowNoNewline bool
	render           func(payload string) []string
	// cursorCol, when non-zero, overrides the column cursorLineLocked reports —
	// the hook that lets a test model the cursor moving off the insert column
	// (e.g. a user typing a space onto a blank composer row).
	cursorCol int
	// afterPaste runs inside the paste-buffer command, after this delivery's
	// own render lands — the hook that models input arriving between the
	// pre-paste baseline and the Enter boundary.
	afterPaste func(m *stagedDraftPane)
	// afterSnapshot runs after the n-th atomic cursor+grid snapshot is
	// answered — the hook that models input arriving between two reads.
	afterSnapshot func(m *stagedDraftPane, n int)
	// failSnapshotsAfter fails atomic snapshots beyond this count.
	failSnapshotsAfter int
	// paneWidth, when >0, wraps every logical row into paneWidth-wide physical
	// rows in grid captures. The boundary's -J capture still emits logical
	// rows, so a wrapped transcript line shifts every row index below it —
	// the two capture modes disagree by construction (#4530 review).
	paneWidth int
	// reportedPaneWidth, when >0, is appended to the cursor-state wire as its
	// fourth field — the #{pane_width} a producer carrying the #7901 field
	// answers. Left unset the mock answers the older 3-field shape: width
	// unknown. It is deliberately independent of paneWidth, so a test can
	// report a width that does not match the grid the capture draws (#7901's
	// mismatched-width withhold).
	reportedPaneWidth int
	// height, when >0, caps the pane's visible rows: content that grows past
	// the cap scrolls older transcript rows off the top — a history-less pane.
	height int
	// pollPartial makes the plain delivery-poll captures report a composer
	// holding only the payload's prefix — the paste drained between the last
	// poll and the Enter boundary, so the polls classify absent while the
	// boundary binds the whole draft.
	pollPartial bool
	// placeholder is the hint an agent draws inside its EMPTY input row (Devin's
	// "Guide Devin while it works"). It shows only while the composer holds
	// nothing, the way the real agents draw it (#7023).
	placeholder string
	// echoGlyph, when set, is the glyph the transcript echo row carries instead
	// of the composer glyph — real Codex marks every echoed user prompt "› "
	// even while the Ultra-tier composer prompts with "»" (codex-rs
	// history_cell/messages.rs vs bottom_pane/effort_ignition.rs) — so the
	// echo row is a narrow-set glyph row sitting above the real composer
	// (#7801).
	echoGlyph string
	// footer, when set, is drawn below the composer — the status and shortcut
	// rows an agent leaves a blank row under its unruled composer (the shape
	// composerRegion's post-blank cut exists for). The cursor stays on the
	// composer's last row.
	footer []string
}

func (m *stagedDraftPane) glyphOr() string {
	if m.glyph != "" {
		return m.glyph
	}
	return "›"
}

func (m *stagedDraftPane) echoGlyphOr() string {
	if m.echoGlyph != "" {
		return m.echoGlyph
	}
	return m.glyphOr()
}

func (m *stagedDraftPane) rowsLocked() (rows []string, cursorRow int) {
	rows = append(rows, m.transcript...)
	if m.boxed {
		rows = append(rows, "╭────────────────╮")
	}
	composer := m.composer
	if len(composer) == 0 {
		composer = []string{m.placeholder}
	}
	for i, c := range composer {
		line := "  " + c
		if i == 0 {
			line = m.glyphOr() + " " + c
		}
		if m.boxed {
			line = "│ " + line + " │"
		}
		rows = append(rows, line)
	}
	if m.boxed {
		rows = append(rows, "╰────────────────╯")
	}
	rows = append(rows, m.footer...)
	if m.height > 0 && len(rows) > m.height {
		rows = rows[len(rows)-m.height:]
	}
	cursorRow = len(rows) - 1 - len(m.footer)
	if m.boxed {
		// The cursor rests on the composer's last row, not the box's bottom edge.
		cursorRow--
	}
	return rows, cursorRow
}

// gridRowsLocked wraps the logical rows into physical pane rows at paneWidth —
// what `capture-pane` without -J returns when a line wraps.
func (m *stagedDraftPane) gridRowsLocked() []string {
	rows, _ := m.rowsLocked()
	if m.paneWidth <= 0 {
		return rows
	}
	var grid []string
	for _, r := range rows {
		rs := []rune(r)
		if len(rs) == 0 {
			grid = append(grid, r)
			continue
		}
		for len(rs) > m.paneWidth {
			grid = append(grid, string(rs[:m.paneWidth]))
			rs = rs[m.paneWidth:]
		}
		grid = append(grid, string(rs))
	}
	return grid
}

// paneLocked is the physical-grid capture (capture-pane -p).
func (m *stagedDraftPane) paneLocked() string {
	return strings.Join(m.gridRowsLocked(), "\n") + "\n"
}

// joinedPaneLocked is the joined capture (capture-pane -p -J): logical rows.
func (m *stagedDraftPane) joinedPaneLocked() string {
	rows, _ := m.rowsLocked()
	return strings.Join(rows, "\n") + "\n"
}

func (m *stagedDraftPane) cursorLineLocked() string {
	// The cursor rests on the composer's last row: the last grid row minus
	// the footer rows an agent draws below it.
	row := len(m.gridRowsLocked()) - 1
	for _, f := range m.footer {
		rs := []rune(f)
		if m.paneWidth > 0 && len(rs) > m.paneWidth {
			row -= (len(rs) + m.paneWidth - 1) / m.paneWidth
		} else {
			row--
		}
	}
	flag := 1
	if m.hiddenCursor {
		flag = 0
	}
	// The cursor rests at the composer block's insert column: 2 on an unboxed
	// composer (› / "  " prefixes) and 4 inside a "│ › … │" box.
	col := 2
	if m.boxed {
		col = 4
	}
	if m.cursorCol != 0 {
		col = m.cursorCol
	}
	if m.movedCursor {
		row, col = 0, 0
	}
	line := fmt.Sprintf("%d %d %d", row, col, flag)
	if m.reportedPaneWidth > 0 {
		line += fmt.Sprintf(" %d", m.reportedPaneWidth)
	}
	return line
}

// userPastes models an attached user pasting a multiline draft into the
// composer: the agent collapses it into a chip exactly like our own paste.
func (m *stagedDraftPane) userPastes() {
	m.composer = append(m.composer[:len(m.composer):len(m.composer)], "[Pasted text #2 +12 lines]")
}

// userPastesChip appends a chip row the way a concurrent user paste would draw
// it, with the caller's declared size.
func (m *stagedDraftPane) userPastesChip(chars int) {
	m.composer = append(m.composer[:len(m.composer):len(m.composer)],
		fmt.Sprintf("[Pasted Content %d chars]", chars))
}

func (m *stagedDraftPane) exec() cmd_test.MockCmdExec {
	return cmd_test.MockCmdExec{
		RunFunc: func(c *exec.Cmd) error {
			joined := strings.Join(c.Args, " ")
			var stdin string
			if c.Stdin != nil {
				b, _ := io.ReadAll(c.Stdin)
				stdin = string(b)
			}
			m.mu.Lock()
			defer m.mu.Unlock()
			switch {
			case strings.Contains(joined, "load-buffer"):
				m.lastLoaded = stdin
			case strings.Contains(joined, "send-keys") && hasArg(c.Args, "C-u"):
				if !m.clearIsNoop {
					m.composer = nil
				}
			case strings.Contains(joined, "paste-buffer"):
				m.pastes++
				m.composer = append(m.composer, m.render(m.lastLoaded)...)
				if m.afterPaste != nil {
					m.afterPaste(m)
				}
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			joined := strings.Join(c.Args, " ")
			m.mu.Lock()
			defer m.mu.Unlock()
			if isDeliveryBoundaryCommand(c) {
				m.enters++
				frame := m.paneLocked()
				joinedFrame := m.joinedPaneLocked()
				if m.enters <= m.swallowedEnters {
					if !m.swallowNoNewline {
						m.composer = append(m.composer, "")
					}
				} else if len(m.composer) > 0 {
					m.transcript = append(m.transcript, m.echoGlyphOr()+" "+strings.TrimSpace(strings.Join(m.composer, " ")))
					m.composer = nil
				}
				return []byte(deliveryBoundarySentinel + "\n" + joinedFrame +
					boundaryGridMarker(c.Args) + "\n" + frame), nil
			}
			if strings.Contains(joined, "display-message") && strings.Contains(joined, "capture-pane") {
				m.snapshots++
				if m.failSnapshotsAfter > 0 && m.snapshots > m.failSnapshotsAfter {
					return nil, exec.ErrNotFound
				}
				out := m.cursorLineLocked() + "\n" + m.paneLocked()
				if m.afterSnapshot != nil {
					m.afterSnapshot(m, m.snapshots)
				}
				return []byte(out), nil
			}
			if strings.Contains(joined, "display-message") {
				return []byte(m.cursorLineLocked()), nil
			}
			if m.pollPartial && m.pastes > 0 {
				// The delivery polls run while the paste is still draining:
				// they see the composer holding only the payload's prefix.
				n := []rune(normalizeDelivery(m.lastLoaded))
				if len(n) > 40 {
					n = n[:40]
				}
				return []byte(m.glyphOr() + " " + string(n) + "\n"), nil
			}
			return []byte(m.paneLocked()), nil
		},
	}
}

// boundaryGridMarker returns the grid delimiter this boundary command asked
// tmux to print between the two captures — which is per-capture now, so the
// mock must replay the exact argv marker the way real tmux echoes its
// display-message argument.
func boundaryGridMarker(args []string) string {
	for i := 0; i+2 < len(args); i++ {
		if args[i] == "display-message" && args[i+1] == "-p" &&
			strings.HasPrefix(args[i+2], deliveryBoundaryGridSentinel) {
			return args[i+2]
		}
	}
	return deliveryBoundaryGridSentinel
}

func (m *stagedDraftPane) counts() (pastes, enters int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pastes, m.enters
}

func literalRender(payload string) []string { return []string{payload} }

func codexChipRender(payload string) []string {
	return []string{fmt.Sprintf("[Pasted Content %d chars]", len([]rune(payload)))}
}

func sendStaged(t *testing.T, m *stagedDraftPane, prompt string) (PromptDeliveryStatus, int, int) {
	t.Helper()
	defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()
	session := newTmuxSession("af_proj", ProgramCodex, NewMockPtyFactory(t), m.exec())
	status, err := session.SendKeysCommandObserved(prompt)
	require.NoError(t, err)
	pastes, enters := m.counts()
	return status, pastes, enters
}

// TestPaneEchoedDelimiterCannotForgeBoundarySplit: the visible pane carries a
// line equal to the grid delimiter — and a second wearing the nonce format —
// because the submitted prompt discussed this very mechanism (the #4530
// review case). Pane content must never be able to stand in for the delimiter
// tmux prints between the two boundary captures: a forged split truncates the
// joined frame and hands the remedy a "grid" frame that is really joined-mode
// leftovers plus the real delimiter plus the grid, so the row indexes it
// compares are nonsense and the chip strand wrongly stands down — or the
// corrupted frame seeds the monitor baseline. The real delimiter carries a
// per-capture nonce the pane cannot guess, so the strand still remedies.
func TestPaneEchoedDelimiterCannotForgeBoundarySplit(t *testing.T) {
	m := &stagedDraftPane{swallowedEnters: 1, render: codexChipRender}
	m.transcript = []string{
		"agent quoting the source it was asked about:",
		deliveryBoundaryGridSentinel,
		deliveryBoundaryGridSentinel + "-0123456789abcdef0123456789abcdef",
		// Codex draws its composer a blank row under the transcript; the
		// still-chip proof requires that transcript gap above the anchor.
		"",
	}
	status, pastes, enters := sendStaged(t, m, redeliverPrompt)
	require.Equal(t, PromptSentUnverified, status)
	require.Equal(t, 1, pastes, "the remedy must never re-paste")
	require.Equal(t, 2, enters, "a delimiter forged in pane content must not break the boundary split")
}

// TestChipStrandGetsExactlyOneRemedyEnter is the one remediable #4200 shape:
// the paste landed as a collapsed chip that declares this payload's size, the
// composer absorbed Enter as a newline (the draft stays, the cursor drops to a
// blank row under it), and nothing else moved a byte of the composer. The
// remedy sends exactly one more Enter — never a re-paste — and the report
// stays sent-unverified, because a chip never proves which text it held.
func TestChipStrandGetsExactlyOneRemedyEnter(t *testing.T) {
	m := &stagedDraftPane{swallowedEnters: 1, render: codexChipRender}
	status, pastes, enters := sendStaged(t, m, redeliverPrompt)
	require.Equal(t, PromptSentUnverified, status)
	require.Equal(t, 1, pastes, "the remedy must never re-paste")
	require.Equal(t, 2, enters, "a swallowed Enter on a bound chip gets exactly one remedy Enter")
}

// TestChipStrandAboveAnAgentFooterStillRemedies is the positive #7850 R3
// control: the composer holds only this delivery's chip, and the row below the
// blank gap is provably the agent's own footer — Codex's model/warning status
// line — not draft text. The remedy still sends its one Enter.
func TestChipStrandAboveAnAgentFooterStillRemedies(t *testing.T) {
	m := &stagedDraftPane{
		swallowedEnters: 1,
		render:          codexChipRender,
		footer: []string{
			"",
			"  GPT-6.1 Terra ultra · ~/sahara/sahara-cheaper-ci · ⚠ 1 warning · f2 to view",
		},
	}
	status, pastes, enters := sendStaged(t, m, redeliverPrompt)
	require.Equal(t, PromptSentUnverified, status)
	require.Equal(t, 1, pastes, "the remedy must never re-paste")
	require.Equal(t, 2, enters, "a still chip-only composer above the agent's own footer gets its one remedy Enter")
}

// TestLiteralStrandStandsDown: a draft rendered as literal text is bound by
// its completion tail, so the remedy knows it is still staged — but the
// narrowed remedy only remediates the chip shape. It withholds the second
// Enter and reports sent-unverified.
func TestLiteralStrandStandsDown(t *testing.T) {
	m := &stagedDraftPane{swallowedEnters: 1, render: literalRender}
	status, pastes, enters := sendStaged(t, m, redeliverPrompt)
	require.Equal(t, PromptSentUnverified, status)
	require.Equal(t, 1, pastes)
	require.Equal(t, 1, enters, "a literal strand must never receive the remedy Enter")
}

// TestSecondChipInStrandedComposerStandsDown: our chip was bound at the Enter
// boundary, and a SECOND chip — the user's own paste, or any render we cannot
// attribute — appeared before the grace read. The composer no longer holds one
// content row, so it fails byte-identical stillness: no remedy Enter.
func TestSecondChipInStrandedComposerStandsDown(t *testing.T) {
	m := &stagedDraftPane{swallowedEnters: 1, render: codexChipRender}
	mock := m.exec()
	base := mock.OutputFunc
	mock.OutputFunc = func(c *exec.Cmd) ([]byte, error) {
		out, err := base(c)
		if isDeliveryBoundaryCommand(c) {
			m.mu.Lock()
			m.userPastes()
			m.mu.Unlock()
		}
		return out, err
	}
	defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()
	session := newTmuxSession("af_proj", ProgramCodex, NewMockPtyFactory(t), mock)
	status, err := session.SendKeysCommandObserved(redeliverPrompt)
	require.NoError(t, err)
	_, enters := m.counts()
	require.Equal(t, 1, enters, "a composer holding a foreign chip must never receive our Enter")
	require.Equal(t, PromptSentUnverified, status)
}

// TestSameSizeChipOnAnotherRowStandsDown: a second chip that declares OUR
// payload's size — or any other text — on a different composer row still adds
// a content row the boundary did not hold. Stillness fails either way.
func TestSameSizeChipOnAnotherRowStandsDown(t *testing.T) {
	for name, foreign := range map[string]func(*stagedDraftPane){
		"same size":      func(m *stagedDraftPane) { m.userPastesChip(len([]rune(redeliverPrompt))) },
		"different text": func(m *stagedDraftPane) { m.composer = append(m.composer, "foreign text") },
	} {
		t.Run(name, func(t *testing.T) {
			m := &stagedDraftPane{swallowedEnters: 1, render: codexChipRender}
			mock := m.exec()
			base := mock.OutputFunc
			mock.OutputFunc = func(c *exec.Cmd) ([]byte, error) {
				out, err := base(c)
				if isDeliveryBoundaryCommand(c) {
					m.mu.Lock()
					foreign(m)
					m.mu.Unlock()
				}
				return out, err
			}
			defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()
			session := newTmuxSession("af_proj", ProgramCodex, NewMockPtyFactory(t), mock)
			status, err := session.SendKeysCommandObserved(redeliverPrompt)
			require.NoError(t, err)
			_, enters := m.counts()
			require.Equal(t, 1, enters)
			require.Equal(t, PromptSentUnverified, status)
		})
	}
}

// TestComposerByteChangeStandsDown covers every composer byte change between
// the Enter boundary and the grace read — a user keystroke, whitespace, or a
// pasted row. Each changes the composer region's bytes, so stillness fails and
// the remedy Enter never fires.
func TestComposerByteChangeStandsDown(t *testing.T) {
	for name, mutate := range map[string]func(*stagedDraftPane){
		"keystroke":  func(m *stagedDraftPane) { m.composer = append(m.composer, "x") },
		"whitespace": func(m *stagedDraftPane) { m.composer = append(m.composer, "   typed space runs") },
		"paste":      func(m *stagedDraftPane) { m.composer = append(m.composer, "a pasted line", "and another") },
	} {
		t.Run(name, func(t *testing.T) {
			m := &stagedDraftPane{swallowedEnters: 1, render: codexChipRender}
			mock := m.exec()
			base := mock.OutputFunc
			mock.OutputFunc = func(c *exec.Cmd) ([]byte, error) {
				out, err := base(c)
				if isDeliveryBoundaryCommand(c) {
					m.mu.Lock()
					mutate(m)
					m.mu.Unlock()
				}
				return out, err
			}
			defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()
			session := newTmuxSession("af_proj", ProgramCodex, NewMockPtyFactory(t), mock)
			status, err := session.SendKeysCommandObserved(redeliverPrompt)
			require.NoError(t, err)
			_, enters := m.counts()
			require.Equal(t, 1, enters, "a changed composer must never receive the remedy Enter")
			require.Equal(t, PromptSentUnverified, status)
		})
	}
}

// TestInvisibleComposerByteChangeStandsDown is the byte-identical rule's one
// deliberate gap closed by the cursor check: a space typed onto the composer's
// blank continuation row is a whitespace-only row either way, but it moves the
// cursor off the insert column. The remedy must not fire.
func TestInvisibleComposerByteChangeStandsDown(t *testing.T) {
	m := &stagedDraftPane{swallowedEnters: 1, render: codexChipRender, cursorCol: 3}
	status, pastes, enters := sendStaged(t, m, redeliverPrompt)
	require.Equal(t, 1, enters, "a composer whose cursor moved under unseen input must not get our Enter")
	require.Equal(t, 1, pastes)
	require.Equal(t, PromptSentUnverified, status)
}

// TestMovedCursorStandsDown: the composer bytes are identical but the cursor
// has left the positions a swallowed-Enter draft can rest at — input or
// navigation we cannot see moved it. The remedy Enter must not fire.
func TestMovedCursorStandsDown(t *testing.T) {
	m := &stagedDraftPane{swallowedEnters: 1, render: codexChipRender, movedCursor: true}
	status, _, enters := sendStaged(t, m, redeliverPrompt)
	require.Equal(t, 1, enters)
	require.Equal(t, PromptSentUnverified, status)
}

// TestHiddenCursorStandsDown: a composer that hides the cursor (Claude's
// ordinary composer reports cursor_flag=0) cannot prove position, so the chip
// strand still withholds the remedy Enter.
func TestHiddenCursorStandsDown(t *testing.T) {
	m := &stagedDraftPane{swallowedEnters: 1, glyph: claudeComposerGlyph, hiddenCursor: true, render: codexChipRender}
	status, _, enters := sendStaged(t, m, redeliverPrompt)
	require.Equal(t, 1, enters)
	require.Equal(t, PromptSentUnverified, status)
}

// TestChipSizeMismatchStandsDown: the pane shows a chip, but its declared size
// is not this payload's — it is a foreign render (or a truncated draw), not
// our paste. It cannot bind, so it is treated like any unbound pane: the
// observation stands and no remedy Enter is considered.
func TestChipSizeMismatchStandsDown(t *testing.T) {
	m := &stagedDraftPane{
		swallowedEnters: 1,
		render:          func(string) []string { return []string{"[Pasted Content 999 chars]"} },
	}
	status, _, enters := sendStaged(t, m, redeliverPrompt)
	require.Equal(t, PromptSentUnverified, status)
	require.Equal(t, 1, enters)
}

// TestConcurrentUserPasteChipIsNotBound is the Codex P2 on chip provenance:
// the user's own paste collapsed into a chip in the same window ours did, so
// the boundary frame holds TWO new chips — one of which is not this payload's.
// Growth > 0 cannot tell them apart, and the user's chip declares a size that
// is not ours, so neither can be bound to this delivery: no remedy Enter.
func TestConcurrentUserPasteChipIsNotBound(t *testing.T) {
	m := &stagedDraftPane{
		swallowedEnters: 1,
		render:          codexChipRender,
		afterPaste:      func(m *stagedDraftPane) { m.userPastes() },
	}
	status, pastes, enters := sendStaged(t, m, redeliverPrompt)
	require.Equal(t, 1, enters, "a chip that might be the user's must never receive our Enter")
	require.Equal(t, 1, pastes)
	require.Equal(t, PromptSentUnverified, status)
}

// TestUnclearedUserChipIsNotBoundEvidence: C-u could not clear a user's draft
// (a vim-NORMAL composer) and our paste drew nothing new. The chip on the
// composer predates this paste — the baseline already counted it — so it
// cannot authorize an Enter, however still the pane is.
func TestUnclearedUserChipIsNotBoundEvidence(t *testing.T) {
	m := &stagedDraftPane{
		swallowedEnters: 1,
		clearIsNoop:     true,
		composer:        []string{"[Pasted text #1 +40 lines]"},
		render:          func(string) []string { return nil },
	}
	status, pastes, enters := sendStaged(t, m, redeliverPrompt)
	require.Equal(t, PromptSentUnverified, status)
	require.Equal(t, 1, pastes)
	require.Equal(t, 1, enters)
}

// TestDispatchedDraftGetsNoRemedyEnter: when the first Enter submits, the echo
// keeps the payload's tail — or chip — in the transcript, but the composer is
// a fresh bare glyph row and the bound evidence is no longer staged. No second
// keystroke may fire.
func TestDispatchedDraftGetsNoRemedyEnter(t *testing.T) {
	for name, render := range map[string]func(string) []string{
		"literal": literalRender,
		"chip":    codexChipRender,
	} {
		t.Run(name, func(t *testing.T) {
			m := &stagedDraftPane{render: render}
			status, pastes, enters := sendStaged(t, m, redeliverPrompt)
			require.Equal(t, 1, pastes)
			require.Equal(t, 1, enters)
			if name == "literal" {
				require.Equal(t, PromptDelivered, status)
			} else {
				require.Equal(t, PromptSentUnverified, status)
			}
		})
	}
}

// TestUserPasteAfterSubmitGetsNoRemedyEnter is the Codex P2 on the chip path:
// our Enter submitted the prompt, and an attached user pasted their own
// multiline draft during the grace window. Its chip sits in the composer,
// where a positional check could read it as our staged paste and send an Enter
// that would have submitted the user's draft. The chip was not in our Enter's
// boundary frame, so it cannot bind — and it arrives after that boundary.
func TestUserPasteAfterSubmitGetsNoRemedyEnter(t *testing.T) {
	for name, render := range map[string]func(string) []string{
		"literal paste": literalRender,
		"chip paste":    codexChipRender,
	} {
		t.Run(name, func(t *testing.T) {
			m := &stagedDraftPane{render: render}
			m.transcript = []string{"booted"}
			// The user pastes right after our Enter is answered.
			mock := m.exec()
			base := mock.OutputFunc
			mock.OutputFunc = func(c *exec.Cmd) ([]byte, error) {
				out, err := base(c)
				if isDeliveryBoundaryCommand(c) {
					m.mu.Lock()
					m.userPastes()
					m.mu.Unlock()
				}
				return out, err
			}
			defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()
			session := newTmuxSession("af_proj", ProgramCodex, NewMockPtyFactory(t), mock)
			_, err := session.SendKeysCommandObserved(redeliverPrompt)
			require.NoError(t, err)
			_, enters := m.counts()
			require.Equal(t, 1, enters, "the user's own draft must never receive our Enter")
		})
	}
}

// TestDraftSurvivingRemedyEnterReportsUnverified: a draft still staged after
// both Enters is a deeper wedge. It must be reported sent-unverified, never
// delivered, and the remedy must not loop.
func TestDraftSurvivingRemedyEnterReportsUnverified(t *testing.T) {
	m := &stagedDraftPane{swallowedEnters: 2, render: codexChipRender}
	status, pastes, enters := sendStaged(t, m, redeliverPrompt)
	require.Equal(t, PromptSentUnverified, status)
	require.Equal(t, 1, pastes)
	require.Equal(t, 2, enters, "one remedy Enter, not a retry loop")
}

// TestRemedyWithUnobservableOutcomeReportsUnverified: the remedy Enter was
// sent and the settle snapshot cannot be read. The prompt was last seen
// unsubmitted, so delivered is not available.
func TestRemedyWithUnobservableOutcomeReportsUnverified(t *testing.T) {
	m := &stagedDraftPane{swallowedEnters: 1, failSnapshotsAfter: 1, render: codexChipRender}
	status, pastes, enters := sendStaged(t, m, redeliverPrompt)
	require.Equal(t, PromptSentUnverified, status)
	require.Equal(t, 1, pastes)
	require.Equal(t, 2, enters)
}

// TestWrappedTranscriptStillRemedies is the capture-mode P1: a transcript line
// above the composer wraps in the physical grid, so the -J boundary and the
// post-grace grid capture disagree on every row index below it. Row indexes
// must come from the grid capture the boundary now carries alongside -J —
// otherwise a genuinely still chip strand is rejected by a phantom "anchor
// moved" and the remedy never fires.
func TestWrappedTranscriptStillRemedies(t *testing.T) {
	m := &stagedDraftPane{
		swallowedEnters: 1,
		render:          codexChipRender,
		paneWidth:       20,
		transcript:      []string{"a much longer transcript row that wraps", ""},
	}
	status, _, enters := sendStaged(t, m, redeliverPrompt)
	require.Equal(t, 2, enters, "a wrapped transcript row must not defeat the still-chip remedy")
	require.Equal(t, PromptSentUnverified, status)
}

// TestScrolledOffIdenticalCopyStillBinds covers the history-less pane where an
// older render of the same tail scrolls off the top exactly as this paste
// lands: the completion count can never grow, so only positional binding —
// the tail sitting in the live composer at the Enter boundary — keeps the
// still-staged draft from being reported delivered. It is a literal strand, so
// it still stands down: sent-unverified, no remedy Enter.
func TestScrolledOffIdenticalCopyStillBinds(t *testing.T) {
	completion := newDeliveryProbe(redeliverPrompt).completion
	splitRender := func(payload string) []string {
		rs := []rune(payload)
		return []string{string(rs[:len(rs)/2]), string(rs[len(rs)/2:])}
	}
	m := &stagedDraftPane{
		swallowedEnters: 1,
		render:          splitRender,
		// Three visible rows: the baseline shows the older identical tail,
		// the paste pushes it off the top exactly as the new render lands,
		// and the absorbed Enter's blank row must not scroll the glyph away.
		height:     3,
		transcript: []string{"older reply " + completion, "another older line"},
	}
	status, _, enters := sendStaged(t, m, redeliverPrompt)
	require.Equal(t, PromptSentUnverified, status,
		"a still-staged draft must never be reported delivered just because its tail's count could not grow")
	require.Equal(t, 1, enters)
}

// TestAbsentThenBoundaryBoundChipRemedies is the absent-poll recheck: the
// delivery polls ran while the paste was still draining, so they saw only the
// payload's prefix and classified absent. The draft drained into the gap and
// the Enter boundary binds the completed chip — the remedy must run on the
// bound boundary, not the stale classification, and the strand gets its one
// Enter. The report stays sent-unverified: the remedied submit cannot claim
// the absent observation's delivery either.
func TestAbsentThenBoundaryBoundChipRemedies(t *testing.T) {
	m := &stagedDraftPane{
		swallowedEnters: 1,
		render:          codexChipRender,
		pollPartial:     true,
	}
	status, _, enters := sendStaged(t, m, redeliverPrompt)
	require.Equal(t, 2, enters, "a draft that positively binds at the Enter boundary must reach the remedy even after an absent poll")
	require.Equal(t, PromptSentUnverified, status)
}

// TestAbsentThenBoundaryBoundChipNeverRedelivers is the same stale-absent
// bound shape as above, but the chip binds positionally and the draft is a
// literal tail strand that must stand down — the boundary's bound evidence
// vetoes the redelivery proof so no second paste is ever authorized for a
// draft the Enter boundary already saw whole.
func TestAbsentBoundLiteralStandsDownWithoutRedelivery(t *testing.T) {
	m := &stagedDraftPane{
		swallowedEnters: 1,
		render:          literalRender,
		pollPartial:     true,
	}
	status, pastes, enters := sendStaged(t, m, redeliverPrompt)
	require.Equal(t, PromptSentUnverified, status)
	require.Equal(t, 1, pastes, "a bound-at-boundary draft must never be re-pasted")
	require.Equal(t, 1, enters, "a literal strand stands down even when it was bound at the boundary")
}

// TestInputBetweenCheckAndRemedyReportsUnverified covers the check-then-act
// gap: input lands after the grace snapshot and before the remedy Enter. It
// cannot be prevented, and the report stays sent-unverified rather than
// delivered.
func TestInputBetweenCheckAndRemedyReportsUnverified(t *testing.T) {
	m := &stagedDraftPane{
		swallowedEnters: 1,
		render:          codexChipRender,
		afterSnapshot: func(m *stagedDraftPane, n int) {
			if n == 1 {
				m.composer = append(m.composer, "typed in the gap")
			}
		},
	}
	status, _, enters := sendStaged(t, m, redeliverPrompt)
	require.Equal(t, PromptSentUnverified, status)
	require.Equal(t, 2, enters, "input in the check-then-act gap cannot be prevented; the report must not round up")
}

// wrappedTailCol returns the content-end column of the last physical grid row
// when line wraps at paneWidth — where an undisturbed draft parks the cursor for
// a chip whose text wraps across physical rows.
func wrappedTailCol(line string, paneWidth int) int {
	rs := []rune(line)
	for len(rs) > paneWidth {
		rs = rs[paneWidth:]
	}
	return contentEndCol(string(rs))
}

// TestWrappedChipTailCursorNoNewlineSwallowStillRemedies is the end-to-end shape
// the unit-level TestStillChipComposerAcceptsWrappedChipTailCursor pins: a paste
// chip long enough to wrap across physical grid rows, whose Enter was swallowed
// WITHOUT inserting a newline (the draft stays, no blank continuation row), with
// the visible cursor parked at the content-end of the chip's LAST wrapped row.
// This is exactly where an undisturbed draft leaves the cursor for a wrapped
// chip, so the strand must receive its one remedy Enter (#4200 wrapped-chip fix).
// The default harness models cursor-at-insert and absorbed-newline Enter; the
// swallowNoNewline mode plus cursorCol=pasted-chip tail model this shape here.
func TestWrappedChipTailCursorNoNewlineSwallowStillRemedies(t *testing.T) {
	chipRow := "› " + codexChipRender(redeliverPrompt)[0]
	m := &stagedDraftPane{
		swallowedEnters:  1,
		swallowNoNewline: true,
		render:           codexChipRender,
		paneWidth:        20,
		cursorCol:        wrappedTailCol(chipRow, 20),
	}
	status, pastes, enters := sendStaged(t, m, redeliverPrompt)
	require.Equal(t, 1, pastes, "the remedy must never re-paste")
	require.Equal(t, 2, enters, "a still wrapped chip with the cursor at the wrapped tail (no-newline swallow) gets exactly one remedy Enter")
	require.Equal(t, PromptSentUnverified, status)
}

// TestExactFillWrappedChipTailStillRemedies is the #7901 shape end-to-end: the
// chip's last wrapped row fills the pane exactly ("› [Pasted Content 89
// chars]" is 27 runes — at pane width 9 its tail row "89 chars]" renders all
// 9 cells), and the agent draws its composer cursor with an explicit
// cursor-move — codex-rs emits a CUP for the frame's cursor on every draw —
// which tmux clamps to the last cell at paneWidth-1, one column inside the
// one-past-end column a natural write leaves (that column is master's own
// content-end accept). The cursor-state wire carries tmux's own
// #{pane_width}, which proves the fill, so the strand gets its one remedy
// Enter. Master withholds this frame: the clamped column never equalled
// contentEndCol, and no pane width rode the cursor answer.
func TestExactFillWrappedChipTailStillRemedies(t *testing.T) {
	chipRow := "› " + codexChipRender(redeliverPrompt)[0]
	m := &stagedDraftPane{
		swallowedEnters:   1,
		swallowNoNewline:  true,
		render:            codexChipRender,
		paneWidth:         9,
		reportedPaneWidth: 9,
		cursorCol:         wrappedTailCol(chipRow, 9) - 1,
	}
	status, pastes, enters := sendStaged(t, m, redeliverPrompt)
	require.Equal(t, 1, pastes, "the remedy must never re-paste")
	require.Equal(t, 2, enters, "an exact-fill chip tail parked at paneWidth-1 with tmux's matching width gets exactly one remedy Enter")
	require.Equal(t, PromptSentUnverified, status)
}

// TestExactFillWrappedChipTailWidthGateWithholds bounds the #7901 widening
// end-to-end: the cursor sitting one column inside the tail row's content end
// — the cursor-move park of an exact fill — is accepted ONLY when tmux's
// reported pane width proves that row fills the pane. A short tail row, an
// unreported width (the older 3-field answer), and a mismatched width each
// keep master's withhold — that one park is the only shape the new accept
// exists for.
func TestExactFillWrappedChipTailWidthGateWithholds(t *testing.T) {
	chipRow := "› " + codexChipRender(redeliverPrompt)[0]
	for name, m := range map[string]*stagedDraftPane{
		// At pane width 7 the chip's last wrapped row is "chars]" — 6
		// cells of a 7-pane, one column short of filling it.
		"tail row one column short of the pane width": {
			swallowedEnters:   1,
			swallowNoNewline:  true,
			render:            codexChipRender,
			paneWidth:         7,
			reportedPaneWidth: 7,
			cursorCol:         wrappedTailCol(chipRow, 7) - 1,
		},
		// The exact-fill tail row with the older 3-field cursor answer —
		// tmux reported no width, so nothing proves the fill.
		"pane width unreported": {
			swallowedEnters:  1,
			swallowNoNewline: true,
			render:           codexChipRender,
			paneWidth:        9,
			cursorCol:        wrappedTailCol(chipRow, 9) - 1,
		},
		// tmux's reported width does not equal the row's rendered width —
		// the proof compares the wire to the row, not to the cursor.
		"pane width mismatched": {
			swallowedEnters:   1,
			swallowNoNewline:  true,
			render:            codexChipRender,
			paneWidth:         9,
			reportedPaneWidth: 10,
			cursorCol:         wrappedTailCol(chipRow, 9) - 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			status, pastes, enters := sendStaged(t, m, redeliverPrompt)
			require.Equal(t, 1, pastes, "the remedy must never re-paste")
			require.Equal(t, 1, enters, "without a proved exact fill the remedy Enter stays withheld")
			require.Equal(t, PromptSentUnverified, status)
		})
	}
}
