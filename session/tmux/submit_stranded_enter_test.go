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

// strandedEnterPaneModel is a composer state machine that can answer the
// post-Enter cursor query, which redeliverPaneModel cannot — the #4200 remedy
// keys its staged-draft detection off the row the pane cursor sits on. The
// pane is transcript echo lines above a three-row composer frame; the cursor
// rides the composer row, so a draft still staged there is provably
// unsubmitted input while the same text in a transcript echo row is output.
//
// swallowedEnters is the count of leading Enters the composer absorbs as a
// literal newline — the #4200 defect. Once they are exhausted an Enter
// dispatches: the staged draft leaves the composer row and lands in the
// transcript as an echo line, which is how the real composers draw a
// submitted prompt.
type strandedEnterPaneModel struct {
	mu              sync.Mutex
	loads           int
	pastes          int
	enters          int
	snapshots       int
	composer        string
	lastLoaded      string
	transcript      []string
	swallowedEnters int
	renderForPaste  func(n int, payload string) string
	// glyph is the composer prompt glyph drawn on the input row; ">" by
	// default, "❯" for the Claude-shaped hidden-cursor fixtures.
	glyph string
	// cursorVisible is the pane's cursor_flag. Claude Code's ordinary composer
	// hides it (measured in claude_trust.go), which is exactly the case the
	// structural staged check exists for.
	cursorVisible *bool
	// failSnapshotsAfter fails snapshot commands beyond this count — the
	// post-remedy capture failure that must not let a seen-staged draft keep
	// its earlier verdict.
	failSnapshotsAfter int
}

func (m *strandedEnterPaneModel) glyphOr(def string) string {
	if m.glyph != "" {
		return m.glyph
	}
	return def
}

func (m *strandedEnterPaneModel) cursorFlagLocked() int {
	if m.cursorVisible != nil && !*m.cursorVisible {
		return 0
	}
	return 1
}

func (m *strandedEnterPaneModel) cursorRowLocked() int {
	// Transcript lines, then the composer top border, then the input row.
	return len(m.transcript) + 1
}

func (m *strandedEnterPaneModel) paneLocked() string {
	g := m.glyphOr(">")
	rows := append([]string(nil), m.transcript...)
	rows = append(rows,
		"╭─ composer ─╮",
		"│ "+g+" "+m.composer+" │",
		"╰────────────╯")
	return strings.Join(rows, "\n")
}

func (m *strandedEnterPaneModel) exec() cmd_test.MockCmdExec {
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
				m.loads++
				m.lastLoaded = stdin
			case strings.Contains(joined, "send-keys") && hasArg(c.Args, "C-u"):
				m.composer = ""
			case strings.Contains(joined, "paste-buffer"):
				m.pastes++
				m.composer = m.renderForPaste(m.pastes, m.lastLoaded)
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			joined := strings.Join(c.Args, " ")
			m.mu.Lock()
			defer m.mu.Unlock()
			// The boundary command embeds display-message between send-keys and
			// capture-pane, so it must be recognized before the cursor query.
			if isDeliveryBoundaryCommand(c) {
				m.enters++
				if m.enters > m.swallowedEnters && m.composer != "" {
					m.transcript = append(m.transcript, m.glyphOr(">")+" "+m.composer)
					m.composer = ""
				}
				return []byte(deliveryBoundarySentinel + "\n" + m.paneLocked()), nil
			}
			// The staged-draft snapshot is one command list — display-message
			// then capture-pane — so its answer is the cursor line followed by
			// the whole grid.
			if strings.Contains(joined, "display-message") && strings.Contains(joined, "capture-pane") {
				m.snapshots++
				if m.failSnapshotsAfter > 0 && m.snapshots > m.failSnapshotsAfter {
					return nil, exec.ErrNotFound
				}
				return []byte(fmt.Sprintf("%d 0 %d\n%s", m.cursorRowLocked(), m.cursorFlagLocked(), m.paneLocked())), nil
			}
			if strings.Contains(joined, "display-message") {
				return []byte(fmt.Sprintf("%d 0 %d", m.cursorRowLocked(), m.cursorFlagLocked())), nil
			}
			return []byte(m.paneLocked()), nil
		},
	}
}

func (m *strandedEnterPaneModel) counts() (pastes, enters int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pastes, m.enters
}

// TestSwallowedEnterGetsOneRemedyAndReportsDelivered is the #4200 repair at
// the mechanism level. The paste lands whole (delivered-quality evidence) but
// the composer absorbs Enter into the still-rendering paste, leaving the full
// prompt staged at the cursor. The remedy sends exactly ONE more Enter — the
// submit the first keystroke never became — the draft dispatches, and the
// caller hears delivered because the payload itself was observed staged and
// then gone.
func TestSwallowedEnterGetsOneRemedyAndReportsDelivered(t *testing.T) {
	defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()

	model := &strandedEnterPaneModel{
		swallowedEnters: 1,
		renderForPaste: func(int, string) string {
			return redeliverPrompt
		},
	}
	session := newTmuxSession("af_proj", ProgramClaude, NewMockPtyFactory(t), model.exec())

	status, err := session.SendKeysCommandObserved(redeliverPrompt)
	require.NoError(t, err)
	require.Equal(t, PromptDelivered, status,
		"the remedy Enter dispatched the prompt the caller saw land — delivered is the honest verdict")

	pastes, enters := model.counts()
	require.Equal(t, 1, pastes, "the remedy must never re-paste — one paste, ever")
	require.Equal(t, 2, enters, "a swallowed Enter gets exactly one remedy Enter, not a retry loop")
}

// TestDraftSurvivingRemedyEnterReportsUnverified is the #4200 failure
// direction: a draft that stays staged after BOTH Enters can no longer claim
// delivered — the paste reached the pane but the prompt never provably reached
// the agent. The verdict must downgrade to sent-unverified rather than round
// up, and the attempt stops at two Enters and one paste.
func TestDraftSurvivingRemedyEnterReportsUnverified(t *testing.T) {
	defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()

	model := &strandedEnterPaneModel{
		swallowedEnters: 2,
		renderForPaste: func(int, string) string {
			return redeliverPrompt
		},
	}
	session := newTmuxSession("af_proj", ProgramClaude, NewMockPtyFactory(t), model.exec())

	status, err := session.SendKeysCommandObserved(redeliverPrompt)
	require.NoError(t, err)
	require.Equal(t, PromptSentUnverified, status,
		"a draft still staged after the remedy Enter is unconfirmed, not delivered — the wedge #4200 exists to surface")

	pastes, enters := model.counts()
	require.Equal(t, 1, pastes)
	require.Equal(t, 2, enters, "the remedy is one Enter — a persistent wedge must not loop keystrokes")
}

// TestStagedPasteChipRemedyStaysUnverified covers the collapsed-paste shape:
// the composer rendered "[Pasted text #1 +10 lines]" instead of the payload,
// so no prompt-specific evidence ever existed. The remedy still dispatches
// the staged chip — it is provably held input — but a placeholder never proved
// WHICH text it held, so dispatching it can only upgrade the outcome to
// sent-unverified, never delivered.
func TestStagedPasteChipRemedyStaysUnverified(t *testing.T) {
	defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()

	model := &strandedEnterPaneModel{
		swallowedEnters: 1,
		renderForPaste: func(int, string) string {
			return "[Pasted text #1 +10 lines]"
		},
	}
	session := newTmuxSession("af_proj", ProgramClaude, NewMockPtyFactory(t), model.exec())

	status, err := session.SendKeysCommandObserved(redeliverPrompt)
	require.NoError(t, err)
	require.Equal(t, PromptSentUnverified, status,
		"an observed chip dispatch is an unverified payload — the honest status stays sent-unverified")

	pastes, enters := model.counts()
	require.Equal(t, 1, pastes)
	require.Equal(t, 2, enters)
}

// TestDispatchedDraftGetsNoRemedyEnter pins the remedy's authorization: it
// fires ONLY on a draft staged at the live cursor. When the first Enter
// submits, the payload's echo may keep the same completion tail visible in
// the transcript — but the cursor row holds the fresh empty composer, so no
// second keystroke may fire. One Enter, one paste.
func TestDispatchedDraftGetsNoRemedyEnter(t *testing.T) {
	defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()

	model := &strandedEnterPaneModel{
		renderForPaste: func(int, string) string {
			return redeliverPrompt
		},
	}
	session := newTmuxSession("af_proj", ProgramClaude, NewMockPtyFactory(t), model.exec())

	status, err := session.SendKeysCommandObserved(redeliverPrompt)
	require.NoError(t, err)
	require.Equal(t, PromptDelivered, status)

	pastes, enters := model.counts()
	require.Equal(t, 1, pastes)
	require.Equal(t, 1, enters,
		"the identical tail in the transcript echo is NOT staged evidence — a dispatched prompt must never get a second Enter")
}

// TestHiddenCursorStagedDraftGetsRemedy is the #4200 remedy on Claude's own
// geometry: its composer hides the terminal cursor (cursor_flag=0, measured in
// claude_trust.go), so the cursor-row anchor never fires for the primary
// supported agent. The structural check replaces it — the payload's tail ends
// the LAST ❯-anchored composer block, which a submitted echo cannot do because
// a fresh ❯ composer row repaints beneath it.
func TestHiddenCursorStagedDraftGetsRemedy(t *testing.T) {
	defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()

	hidden := false
	model := &strandedEnterPaneModel{
		swallowedEnters: 1,
		glyph:           claudeComposerGlyph,
		cursorVisible:   &hidden,
		renderForPaste: func(int, string) string {
			return redeliverPrompt
		},
	}
	session := newTmuxSession("af_proj", ProgramClaude, NewMockPtyFactory(t), model.exec())

	status, err := session.SendKeysCommandObserved(redeliverPrompt)
	require.NoError(t, err)
	require.Equal(t, PromptDelivered, status,
		"the structural check must see Claude's staged draft — a hidden cursor is not license to stay stranded")

	pastes, enters := model.counts()
	require.Equal(t, 1, pastes, "the remedy must never re-paste — one paste, ever")
	require.Equal(t, 2, enters, "the swallowed Enter gets its one remedy Enter even without a visible cursor")
}

// TestHiddenCursorDispatchedDraftGetsNoRemedy is the hidden-cursor false
// positive direction: once Enter submits, the payload's echo keeps a ❯-prefixed
// row in the transcript — but the composer repaints a fresh ❯ row below it, so
// the tail is NOT on the last glyph block and no second Enter may fire.
func TestHiddenCursorDispatchedDraftGetsNoRemedy(t *testing.T) {
	defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()

	hidden := false
	model := &strandedEnterPaneModel{
		glyph:         claudeComposerGlyph,
		cursorVisible: &hidden,
		renderForPaste: func(int, string) string {
			return redeliverPrompt
		},
	}
	session := newTmuxSession("af_proj", ProgramClaude, NewMockPtyFactory(t), model.exec())

	status, err := session.SendKeysCommandObserved(redeliverPrompt)
	require.NoError(t, err)
	require.Equal(t, PromptDelivered, status)

	pastes, enters := model.counts()
	require.Equal(t, 1, pastes)
	require.Equal(t, 1, enters,
		"the transcript echo's ❯ row is above the repainted composer — not staged evidence")
}

// TestRemedyWithUnobservableOutcomeReportsUnverified is the verdict-honesty
// boundary: staged evidence WAS seen and the remedy Enter was sent, but the
// post-remedy snapshot cannot be captured. The earlier landed observation must
// not survive — the prompt was last seen unsubmitted — so the report is
// sent-unverified, never delivered.
func TestRemedyWithUnobservableOutcomeReportsUnverified(t *testing.T) {
	defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()

	model := &strandedEnterPaneModel{
		swallowedEnters:    1,
		failSnapshotsAfter: 1, // the staged check reads; the post-remedy one cannot
		renderForPaste: func(int, string) string {
			return redeliverPrompt
		},
	}
	session := newTmuxSession("af_proj", ProgramClaude, NewMockPtyFactory(t), model.exec())

	status, err := session.SendKeysCommandObserved(redeliverPrompt)
	require.NoError(t, err)
	require.Equal(t, PromptSentUnverified, status,
		"a staged draft whose remedy outcome cannot be re-observed is unconfirmed — not delivered")

	pastes, enters := model.counts()
	require.Equal(t, 1, pastes)
	require.Equal(t, 2, enters)
}
