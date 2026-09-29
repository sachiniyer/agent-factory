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
	clearIsNoop     bool
	swallowedEnters int
	render          func(payload string) []string
	// afterSnapshot runs after the n-th atomic cursor+grid snapshot is
	// answered — the hook that models input arriving between two reads.
	afterSnapshot func(m *stagedDraftPane, n int)
	// failSnapshotsAfter fails atomic snapshots beyond this count.
	failSnapshotsAfter int
}

func (m *stagedDraftPane) glyphOr() string {
	if m.glyph != "" {
		return m.glyph
	}
	return "›"
}

func (m *stagedDraftPane) rowsLocked() (rows []string, cursorRow int) {
	rows = append(rows, m.transcript...)
	if m.boxed {
		rows = append(rows, "╭────────────────╮")
	}
	composer := m.composer
	if len(composer) == 0 {
		composer = []string{""}
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
	cursorRow = len(rows) - 1
	if m.boxed {
		rows = append(rows, "╰────────────────╯")
	}
	return rows, cursorRow
}

func (m *stagedDraftPane) paneLocked() string {
	rows, _ := m.rowsLocked()
	return strings.Join(rows, "\n") + "\n"
}

func (m *stagedDraftPane) cursorLineLocked() string {
	_, row := m.rowsLocked()
	flag := 1
	if m.hiddenCursor {
		flag = 0
	}
	return fmt.Sprintf("%d 2 %d", row, flag)
}

// userPastes models an attached user pasting a multiline draft into the
// composer: the agent collapses it into a chip exactly like our own paste.
func (m *stagedDraftPane) userPastes() {
	m.composer = append(m.composer[:len(m.composer):len(m.composer)], "[Pasted text #2 +12 lines]")
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
				if m.enters <= m.swallowedEnters {
					m.composer = append(m.composer, "")
				} else if len(m.composer) > 0 {
					m.transcript = append(m.transcript, m.glyphOr()+" "+strings.TrimSpace(strings.Join(m.composer, " ")))
					m.composer = nil
				}
				return []byte(deliveryBoundarySentinel + "\n" + frame), nil
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
			return []byte(m.paneLocked()), nil
		},
	}
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

// TestSwallowedEnterGetsOneRemedyAndReportsDelivered is the #4200 repair: the
// paste lands whole, the composer absorbs Enter as a newline (the draft stays,
// the cursor drops to a blank row under it), and nothing else is drawn. The
// remedy sends exactly one more Enter — never a re-paste — and the prompt the
// caller saw land is reported delivered once it has left the composer.
func TestSwallowedEnterGetsOneRemedyAndReportsDelivered(t *testing.T) {
	m := &stagedDraftPane{swallowedEnters: 1, render: literalRender}
	status, pastes, enters := sendStaged(t, m, redeliverPrompt)
	require.Equal(t, PromptDelivered, status)
	require.Equal(t, 1, pastes, "the remedy must never re-paste")
	require.Equal(t, 2, enters, "a swallowed Enter gets exactly one remedy Enter")
}

// TestCodexChipStrandGetsRemedyAndStaysUnverified is the shape in the #4200
// report: real Codex collapsed a 3 KB prompt into "› [Pasted Content N chars]"
// and the Enter never submitted it. The chip is bound to this delivery (it
// appeared between the baseline and our Enter) and the pane stood still, so
// the remedy submits it — but a chip never proved which text it held, so the
// report stays sent-unverified.
func TestCodexChipStrandGetsRemedyAndStaysUnverified(t *testing.T) {
	m := &stagedDraftPane{swallowedEnters: 1, render: codexChipRender}
	status, pastes, enters := sendStaged(t, m, redeliverPrompt)
	require.Equal(t, PromptSentUnverified, status)
	require.Equal(t, 1, pastes)
	require.Equal(t, 2, enters)
}

// TestDraftSurvivingRemedyEnterReportsUnverified: a draft still staged after
// both Enters is a deeper wedge. It must be reported sent-unverified, never
// delivered, and the remedy must not loop.
func TestDraftSurvivingRemedyEnterReportsUnverified(t *testing.T) {
	m := &stagedDraftPane{swallowedEnters: 2, render: literalRender}
	status, pastes, enters := sendStaged(t, m, redeliverPrompt)
	require.Equal(t, PromptSentUnverified, status)
	require.Equal(t, 1, pastes)
	require.Equal(t, 2, enters, "one remedy Enter, not a retry loop")
}

// TestDispatchedDraftGetsNoRemedyEnter: when the first Enter submits, the echo
// keeps the payload's tail in the transcript, but the composer is empty and
// the pane changed. No second keystroke may fire.
func TestDispatchedDraftGetsNoRemedyEnter(t *testing.T) {
	m := &stagedDraftPane{render: literalRender}
	status, pastes, enters := sendStaged(t, m, redeliverPrompt)
	require.Equal(t, PromptDelivered, status)
	require.Equal(t, 1, pastes)
	require.Equal(t, 1, enters)
}

// TestUserPasteAfterSubmitGetsNoRemedyEnter is the Codex P2 on the chip path:
// our Enter submitted the prompt, and an attached user pasted their own
// multiline draft during the grace window. Its chip sits on the cursor row,
// where the old check read it as our staged paste and sent an Enter that would
// have submitted the user's draft. The chip was not in our Enter's boundary
// frame, so it cannot bind, and the pane changed since that Enter besides.
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

// TestUserInputIntoStrandedDraftWithholdsRemedy: the Enter was swallowed, but
// the user typed into the composer before the grace check. The composer now
// holds more than our first Enter was sent to submit, so the remedy must not
// fire. The status is not asserted: the cursor row holds the user's text, not
// ours, so the remedy finds no staged evidence and leaves the observation as
// master reports it.
func TestUserInputIntoStrandedDraftWithholdsRemedy(t *testing.T) {
	m := &stagedDraftPane{swallowedEnters: 1, render: literalRender}
	mock := m.exec()
	base := mock.OutputFunc
	mock.OutputFunc = func(c *exec.Cmd) ([]byte, error) {
		out, err := base(c)
		if isDeliveryBoundaryCommand(c) {
			m.mu.Lock()
			m.composer = append(m.composer, "and also delete the cache")
			m.mu.Unlock()
		}
		return out, err
	}
	defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()
	session := newTmuxSession("af_proj", ProgramCodex, NewMockPtyFactory(t), mock)
	_, err := session.SendKeysCommandObserved(redeliverPrompt)
	require.NoError(t, err)
	_, enters := m.counts()
	require.Equal(t, 1, enters, "the composer holds the user's text too; our Enter must not submit it")
}

// TestUnclearedUserChipIsNotBoundEvidence: C-u could not clear a user's draft
// (a vim-NORMAL composer) and our paste drew nothing new. The chip on the
// cursor row predates this paste — the baseline already counted it — so it
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

// TestTrailingNewlineDraftGetsRemedy is the Codex P2 on the row anchor: a
// prompt ending in "\n" renders its tail one row ABOVE the cursor, and the
// absorbed Enter adds one more blank row. The check steps up over exactly
// those blank composer rows to reach the tail.
func TestTrailingNewlineDraftGetsRemedy(t *testing.T) {
	m := &stagedDraftPane{
		swallowedEnters: 1,
		boxed:           true,
		render:          func(p string) []string { return strings.Split(p, "\n") },
	}
	status, pastes, enters := sendStaged(t, m, redeliverPrompt+"\n")
	require.Equal(t, PromptDelivered, status)
	require.Equal(t, 1, pastes)
	require.Equal(t, 2, enters)
}

// TestBlankRowStepStopsAtComposerBorder: stepping up over blank rows must not
// leave the composer. Here the composer is empty (the prompt submitted and
// echoed ABOVE the top border), the cursor row is blank — nothing may be read
// through the border as staged.
func TestBlankRowStepStopsAtComposerBorder(t *testing.T) {
	pane := "› " + redeliverPrompt + "\n╭──────╮\n│      │\n╰──────╯\n"
	probe := newDeliveryProbe(redeliverPrompt + "\n\n\n")
	require.False(t, stagedInComposer(pane, paneCursorState{Row: 2, Visible: true}, probe, stagedEvidence{tail: true}, 1))
}

// TestHiddenCursorStagedDraftGetsRemedy is the remedy on Claude's geometry: the
// composer hides the cursor (cursor_flag=0, measured in claude_trust.go), so
// the tail must end a row in the LAST ❯ block instead.
func TestHiddenCursorStagedDraftGetsRemedy(t *testing.T) {
	m := &stagedDraftPane{swallowedEnters: 1, glyph: claudeComposerGlyph, boxed: true, hiddenCursor: true, render: literalRender}
	status, pastes, enters := sendStaged(t, m, redeliverPrompt)
	require.Equal(t, PromptDelivered, status)
	require.Equal(t, 1, pastes)
	require.Equal(t, 2, enters)
}

// TestHiddenCursorMultiParagraphDraftGetsRemedy: a blank row INSIDE the draft
// must not end the ❯ block before the tail is reached.
func TestHiddenCursorMultiParagraphDraftGetsRemedy(t *testing.T) {
	m := &stagedDraftPane{
		swallowedEnters: 1, glyph: claudeComposerGlyph, boxed: true, hiddenCursor: true,
		render: func(p string) []string { return strings.Split(p, "\n") },
	}
	status, _, enters := sendStaged(t, m, "first paragraph of the task\n\n"+redeliverPrompt)
	require.Equal(t, PromptDelivered, status)
	require.Equal(t, 2, enters)
}

// TestHiddenCursorDispatchedDraftGetsNoRemedy: after a submit the echo keeps a
// ❯ row in the transcript, but a fresh empty ❯ composer repaints below it.
func TestHiddenCursorDispatchedDraftGetsNoRemedy(t *testing.T) {
	m := &stagedDraftPane{glyph: claudeComposerGlyph, boxed: true, hiddenCursor: true, render: literalRender}
	status, pastes, enters := sendStaged(t, m, redeliverPrompt)
	require.Equal(t, PromptDelivered, status)
	require.Equal(t, 1, pastes)
	require.Equal(t, 1, enters)
}

// TestRemedyWithUnobservableOutcomeReportsUnverified: the remedy Enter was
// sent and the settle snapshot cannot be read. The prompt was last seen
// unsubmitted, so delivered is not available.
func TestRemedyWithUnobservableOutcomeReportsUnverified(t *testing.T) {
	m := &stagedDraftPane{swallowedEnters: 1, failSnapshotsAfter: 1, render: literalRender}
	status, pastes, enters := sendStaged(t, m, redeliverPrompt)
	require.Equal(t, PromptSentUnverified, status)
	require.Equal(t, 1, pastes)
	require.Equal(t, 2, enters)
}

// TestInputBetweenCheckAndRemedyReportsUnverified covers the check-then-act
// gap: input lands after the grace snapshot and before the remedy Enter. It
// cannot be prevented, but the remedy's own boundary frame shows it, so the
// outcome is reported sent-unverified rather than delivered.
func TestInputBetweenCheckAndRemedyReportsUnverified(t *testing.T) {
	m := &stagedDraftPane{
		swallowedEnters: 1,
		render:          literalRender,
		afterSnapshot: func(m *stagedDraftPane, n int) {
			if n == 1 {
				m.composer = append(m.composer, "typed in the gap")
			}
		},
	}
	status, _, enters := sendStaged(t, m, redeliverPrompt)
	require.Equal(t, 2, enters)
	require.Equal(t, PromptSentUnverified, status)
}

// TestUserPasteIntoStrandedChipWithholdsRemedy: our chip was stranded, and the
// user pasted a draft of their own into the same composer before the check.
// Our bound chip is still on the cursor row, but the pane changed since our
// Enter, so the composer no longer holds only what that Enter was sent to
// submit: no remedy, and the report is sent-unverified.
func TestUserPasteIntoStrandedChipWithholdsRemedy(t *testing.T) {
	m := &stagedDraftPane{swallowedEnters: 1, render: codexChipRender}
	mock := m.exec()
	base := mock.OutputFunc
	mock.OutputFunc = func(c *exec.Cmd) ([]byte, error) {
		out, err := base(c)
		if isDeliveryBoundaryCommand(c) {
			m.mu.Lock()
			m.composer[len(m.composer)-1] = "[Pasted text #2 +12 lines]"
			m.mu.Unlock()
		}
		return out, err
	}
	defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()
	session := newTmuxSession("af_proj", ProgramCodex, NewMockPtyFactory(t), mock)
	status, err := session.SendKeysCommandObserved(redeliverPrompt)
	require.NoError(t, err)
	_, enters := m.counts()
	require.Equal(t, 1, enters, "a composer holding the user's paste must never receive our Enter")
	require.Equal(t, PromptSentUnverified, status)
}

// wrapRender draws a literal paste the way an 80-column pane does: the text
// hard-wraps at the pane width, so the tail's last visual row can be any
// length — including shorter than minDistinctiveFragment.
func wrapRender(width int) func(string) []string {
	return func(payload string) []string {
		r := []rune(payload)
		first := width - 2 // the "› " glyph shares the first row
		var rows []string
		for len(r) > 0 {
			n := first
			if len(rows) > 0 {
				n = width
			}
			if n > len(r) {
				n = len(r)
			}
			rows = append(rows, string(r[:n]))
			r = r[n:]
		}
		return rows
	}
}

// TestWrappedTailWithShortLastRowGetsRemedy is the #4530 play-test failure
// (s2): a 2.9 KB literal draft in an 80-column pane wrapped so its last visual
// row held only "R_4530". Row-by-row matching found no tail there, the remedy
// stood down, and the stranded prompt was reported delivered. The tail has to
// be read across the wrapped rows that end at the anchor.
func TestWrappedTailWithShortLastRowGetsRemedy(t *testing.T) {
	prompt := "PLAYTEST-4530 " + strings.Repeat("Keep the change focused and run the package tests. ", 15) + "FINAL_TAIL_MARKER_4530"
	rows := wrapRender(80)(prompt)
	require.Less(t, len([]rune(rows[len(rows)-1])), minDistinctiveFragment,
		"fixture must reproduce the short last row that defeated the row match")
	m := &stagedDraftPane{swallowedEnters: 1, render: wrapRender(80)}
	status, pastes, enters := sendStaged(t, m, prompt)
	require.Equal(t, 2, enters, "a stranded wrapped draft gets its remedy Enter")
	require.Equal(t, 1, pastes)
	require.Equal(t, PromptDelivered, status)
}

// TestHiddenCursorWrappedTailWithShortLastRowGetsRemedy: the same wrap on
// Claude's hidden-cursor geometry.
func TestHiddenCursorWrappedTailWithShortLastRowGetsRemedy(t *testing.T) {
	prompt := "PLAYTEST-4530 " + strings.Repeat("Keep the change focused and run the package tests. ", 15) + "FINAL_TAIL_MARKER_4530"
	m := &stagedDraftPane{swallowedEnters: 1, glyph: claudeComposerGlyph, boxed: true, hiddenCursor: true, render: wrapRender(80)}
	status, _, enters := sendStaged(t, m, prompt)
	require.Equal(t, 2, enters)
	require.Equal(t, PromptDelivered, status)
}

// TestDispatchedWrappedDraftGetsNoRemedyEnter: reading the tail across wrapped
// rows must not reach a submitted draft's wrapped echo. The fresh composer row
// under it ends the joined text with the glyph, not with the payload's tail.
func TestDispatchedWrappedDraftGetsNoRemedyEnter(t *testing.T) {
	prompt := "PLAYTEST-4530 " + strings.Repeat("Keep the change focused and run the package tests. ", 15) + "FINAL_TAIL_MARKER_4530"
	for name, m := range map[string]*stagedDraftPane{
		"visible cursor": {render: wrapRender(80)},
		"hidden cursor":  {glyph: claudeComposerGlyph, boxed: true, hiddenCursor: true, render: wrapRender(80)},
	} {
		t.Run(name, func(t *testing.T) {
			status, _, enters := sendStaged(t, m, prompt)
			require.Equal(t, 1, enters, "a submitted wrapped draft must never get a second Enter")
			require.Equal(t, PromptDelivered, status)
		})
	}
}
