package tmux

import (
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
)

// redeliverPrompt is long enough (81 normalized runes) for newDeliveryProbe to
// reserve both witnesses: the render witness is carved from WITNESS_PREFIX_MARKER
// and the completion tail ends in COMPLETION_TAIL_MARKER_LANDS, so a pane can
// render one without the other.
const redeliverPrompt = "WITNESS_PREFIX_MARKER inspect the build logs and then report COMPLETION_TAIL_MARKER_LANDS"

// redeliverStrandRender is the truncated frame of a genuine #1982 strand: the
// prompt's prefix rendered, its completion tail absent — the only evidence that
// authorizes observed-absent.
const redeliverStrandRender = "WITNESS_PREFIX_MARKER inspect the build logs and"

// redeliverPaneModel is a minimal composer state machine behind MockCmdExec.
// C-u empties the composer (the property that makes redelivery safe: the
// pre-paste clear removes a stranded draft, #2070), and each paste renders
// whatever renderForPaste scripts for that attempt and payload. Enter is
// deliberately absorbed — the composer keeps its content — which is exactly the
// #1982 end state the redelivery exists for. A non-empty boundaryPane overrides
// the frame the Enter boundary command captures, so a test can model a paste
// that visibly drained only in the gap between the last observation poll and
// Enter.
type redeliverPaneModel struct {
	mu           sync.Mutex
	loads        int
	pastes       int
	enters       int
	composer     string
	lastLoaded   string
	pasteOrder   []string
	captureFails bool
	boundaryPane string
	// backdrop is transcript content above the composer. A paste's render
	// burst scrolls it off (the model clears it on paste-buffer), so a test can
	// stage baseline-visible content that is gone by the time the pane is
	// observed again — the #1146 identical-tail-in-scrollback shape.
	backdrop       string
	renderForPaste func(n int, payload string) string
}

func (m *redeliverPaneModel) pane() string {
	frame := "╭─ composer ─╮\n│ > " + m.composer + " │\n╰────────────╯"
	if m.backdrop != "" {
		return m.backdrop + "\n" + frame
	}
	return frame
}

func (m *redeliverPaneModel) exec() cmd_test.MockCmdExec {
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
				m.pasteOrder = append(m.pasteOrder, m.lastLoaded)
				m.composer = m.renderForPaste(m.pastes, m.lastLoaded)
				m.backdrop = ""
			}
			return nil
		},
		OutputFunc: func(c *exec.Cmd) ([]byte, error) {
			m.mu.Lock()
			defer m.mu.Unlock()
			if isDeliveryBoundaryCommand(c) {
				m.enters++
				frame := m.pane()
				if m.boundaryPane != "" {
					frame = m.boundaryPane
				}
				return []byte(deliveryBoundarySentinel + "\n" + frame), nil
			}
			if m.captureFails {
				return nil, exec.ErrNotFound
			}
			return []byte(m.pane()), nil
		},
	}
}

func (m *redeliverPaneModel) counts() (loads, pastes, enters int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loads, m.pastes, m.enters
}

// TestObservedAbsentPromptIsRedeliveredOnceAndConfirms is #3293's happy path.
// The first attempt strands (the pane renders the prompt's prefix but never its
// completion tail — observed absent, the genuine #1982 signal), and the single
// automatic redelivery goes through the full clear-observe path: the clear
// removes the strand, the fresh paste lands whole, and the caller hears
// delivered — the outcome a human retry used to produce by hand.
func TestObservedAbsentPromptIsRedeliveredOnceAndConfirms(t *testing.T) {
	defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()
	errors := captureErrorLog(t)

	model := &redeliverPaneModel{renderForPaste: func(n int, _ string) string {
		if n == 1 {
			return redeliverStrandRender
		}
		return redeliverPrompt
	}}
	session := newTmuxSession("af_proj", ProgramClaude, NewMockPtyFactory(t), model.exec())

	status, err := session.SendKeysCommandObserved(redeliverPrompt)
	require.NoError(t, err)
	require.Equal(t, PromptDelivered, status,
		"a clean redelivery after an observed-absent strand must report delivered, not the first attempt's failure")

	loads, pastes, enters := model.counts()
	require.Equal(t, 2, pastes, "observed-absent must trigger exactly one automatic redelivery")
	require.Equal(t, 2, loads, "the redelivery must be a full submit (own load), not a bare re-paste of stale state")
	require.Equal(t, 2, enters, "each attempt must submit through its own Enter")
	require.Equal(t, 1, strings.Count(errors.String(), "prompt delivery observed absent"),
		"only the stranded first attempt should log the observed-absent ERROR")
}

// TestPromptStrandedTwiceReportsNotDeliveredAfterExactlyTwoAttempts bounds the
// retry. A pane that strands twice is a persistent condition: after the one
// redelivery the path must report not-delivered and stop — two attempts, never
// three.
func TestPromptStrandedTwiceReportsNotDeliveredAfterExactlyTwoAttempts(t *testing.T) {
	defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()
	errors := captureErrorLog(t)

	model := &redeliverPaneModel{renderForPaste: func(int, string) string {
		return redeliverStrandRender
	}}
	session := newTmuxSession("af_proj", ProgramClaude, NewMockPtyFactory(t), model.exec())

	status, err := session.SendKeysCommandObserved(redeliverPrompt)
	require.NoError(t, err)
	require.Equal(t, PromptNotDelivered, status,
		"a pane that strands both attempts must still report the honest terminal negative")

	_, pastes, _ := model.counts()
	require.Equal(t, 2, pastes,
		"exactly two attempts: the automatic redelivery must happen once and never loop")
	require.Equal(t, 2, strings.Count(errors.String(), "prompt delivery observed absent"),
		"both stranded attempts must stay loud at ERROR")
}

// TestSentUnverifiedIsNotRedelivered pins the #3293 boundary at the mock level.
// Sent-unverified (#3170) means tmux accepted the paste and Enter while the pane
// rendered no payload-specific proof either way — the prompt may have SUBMITTED,
// so a redelivery would hand the agent the same instruction twice. Exactly one
// paste, ever.
func TestSentUnverifiedIsNotRedelivered(t *testing.T) {
	defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()

	model := &redeliverPaneModel{renderForPaste: func(int, string) string {
		return "[Pasted text #1 +10 lines]"
	}}
	session := newTmuxSession("af_proj", ProgramClaude, NewMockPtyFactory(t), model.exec())

	status, err := session.SendKeysCommandObserved(redeliverPrompt)
	require.NoError(t, err)
	require.Equal(t, PromptSentUnverified, status)

	_, pastes, _ := model.counts()
	require.Equal(t, 1, pastes,
		"sent-unverified may already have submitted; redelivering it risks a double prompt and must never happen")
}

// TestCouldNotConfirmIsNotRedelivered pins the other half of the boundary:
// when observation itself failed, nothing proves the prompt absent, so the
// paste may have landed and submitted. Same double-prompt risk, same rule —
// exactly one attempt.
func TestCouldNotConfirmIsNotRedelivered(t *testing.T) {
	defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()

	model := &redeliverPaneModel{captureFails: true, renderForPaste: func(int, string) string {
		return redeliverPrompt
	}}
	session := newTmuxSession("af_proj", ProgramClaude, NewMockPtyFactory(t), model.exec())

	status, err := session.SendKeysCommandObserved(redeliverPrompt)
	require.NoError(t, err)
	require.Equal(t, PromptCouldNotConfirm, status)

	_, pastes, _ := model.counts()
	require.Equal(t, 1, pastes,
		"an unobservable pane cannot authorize a redelivery: the first paste may have delivered fine")
}

// TestObservedAbsentWithDrainedBoundaryFrameIsNotRedelivered pins the retry's
// post-Enter authorization. The observation deadline saw only the prompt's
// prefix (observed absent), but by the time Enter was enqueued the paste had
// visibly drained: the boundary frame — captured in the same tmux command queue
// as Enter — shows the completion tail. That Enter may well have submitted the
// full prompt, so the redelivery must be withheld: one paste, and the honest
// not-delivered report stands on its authorizing pre-Enter frame.
//
// The styled case matters separately: the boundary capture preserves ANSI
// escapes (capture-pane -e), so a colorized composer interleaves styling
// through the very tail being matched. The veto must see through that, or the
// one frame it exists to inspect goes blind exactly on real agent composers.
func TestObservedAbsentWithDrainedBoundaryFrameIsNotRedelivered(t *testing.T) {
	drained := redeliverPrompt
	styledTail := strings.Replace(redeliverPrompt, "COMPLETION_TAIL", "COMPLETION\x1b[32m_\x1b[0mTAIL", 1)
	require.NotEqual(t, drained, styledTail, "the styled fixture must actually interleave escapes through the tail")

	tests := []struct {
		name  string
		frame string
	}{
		{name: "plain drained tail", frame: drained},
		{name: "ansi-styled drained tail", frame: styledTail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()

			model := &redeliverPaneModel{
				boundaryPane: "╭─ composer ─╮\n│ > " + tt.frame + " │\n╰────────────╯",
				renderForPaste: func(int, string) string {
					return redeliverStrandRender
				},
			}
			session := newTmuxSession("af_proj", ProgramClaude, NewMockPtyFactory(t), model.exec())

			status, err := session.SendKeysCommandObserved(redeliverPrompt)
			require.NoError(t, err)
			require.Equal(t, PromptNotDelivered, status,
				"the pre-Enter frame authorized observed-absent and remains the reported evidence")

			_, pastes, _ := model.counts()
			require.Equal(t, 1, pastes,
				"a boundary frame showing the completion tail means the paste drained by Enter time — the Enter may have submitted it, so redelivery must be withheld")
		})
	}
}

// TestScrolledOffBaselineTailCannotAuthorizeRedelivery pins the veto's anchor
// to the ABSENT observation frame rather than the pre-paste baseline. An
// identical completion tail sits in the transcript at baseline (the #1146 limit
// resume redelivers the same prompt, so this is a designed-for case), the
// paste's render burst scrolls it off, and the newly drained tail then REPLACES
// it in the boundary frame: the total against the baseline never grows, but
// against the absent frame — where the tail was proven missing — it does. The
// retry must be withheld: that boundary tail is this paste, visibly drained,
// and Enter may have submitted it.
func TestScrolledOffBaselineTailCannotAuthorizeRedelivery(t *testing.T) {
	defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()

	model := &redeliverPaneModel{
		backdrop:     "previous turn echoed: report COMPLETION_TAIL_MARKER_LANDS",
		boundaryPane: "╭─ composer ─╮\n│ > " + redeliverPrompt + " │\n╰────────────╯",
		renderForPaste: func(int, string) string {
			return redeliverStrandRender
		},
	}
	session := newTmuxSession("af_proj", ProgramClaude, NewMockPtyFactory(t), model.exec())

	status, err := session.SendKeysCommandObserved(redeliverPrompt)
	require.NoError(t, err)
	require.Equal(t, PromptNotDelivered, status)

	_, pastes, _ := model.counts()
	require.Equal(t, 1, pastes,
		"a boundary tail that replaced a scrolled-off baseline copy is still this paste, visibly drained — redelivery must be withheld")
}

// TestStyledPayloadDeliveryConfirmsInsteadOfRetrying pins the probe side of
// the ANSI asymmetry. A prompt can itself carry escape bytes (colorized logs
// submitted through the API); the pane interprets them and renders clean text,
// so witnesses derived from the raw bytes would never match any capture — a
// fully drained paste would read as absent and, with #3293, be redelivered
// even though its Enter may already have submitted it. The probe must strip
// ANSI from the payload: the delivery confirms, in one paste.
func TestStyledPayloadDeliveryConfirmsInsteadOfRetrying(t *testing.T) {
	defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()

	styled := strings.Replace(redeliverPrompt, "COMPLETION_TAIL_MARKER_LANDS",
		"\x1b[31mCOMPLETION_TAIL_MARKER_LANDS\x1b[0m", 1)
	model := &redeliverPaneModel{renderForPaste: func(int, string) string {
		// The pane processed the SGR sequences; only clean text renders.
		return redeliverPrompt
	}}
	session := newTmuxSession("af_proj", ProgramClaude, NewMockPtyFactory(t), model.exec())

	status, err := session.SendKeysCommandObserved(styled)
	require.NoError(t, err)
	require.Equal(t, PromptDelivered, status,
		"a styled payload whose clean text rendered whole must confirm, not read as absent")

	_, pastes, _ := model.counts()
	require.Equal(t, 1, pastes,
		"a confirmed styled delivery must never be redelivered")
}

// TestRedeliveryHoldsTheInputLockAcrossBothAttempts pins the two attempts as
// ONE submission transaction. If the input lock were released across the
// redelivery wait, a concurrent same-session submission could run inside the
// gap: its unconditional pre-paste clear consumes the first attempt's strand,
// and the retry's clear can then destroy the newer prompt if it also stranded —
// or at minimum submit the older instruction after the newer one. Serialized,
// the paste order must be strand, its replacement, then the second caller.
func TestRedeliveryHoldsTheInputLockAcrossBothAttempts(t *testing.T) {
	defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()
	redeliverAfterAbsentDelay = 400 * time.Millisecond

	const secondPrompt = "SECOND-CALLER-PROMPT-DELIVERS-CLEAN"
	model := &redeliverPaneModel{renderForPaste: func(_ int, payload string) string {
		if payload == secondPrompt {
			return payload
		}
		return redeliverStrandRender
	}}
	session := newTmuxSession("af_proj", ProgramClaude, NewMockPtyFactory(t), model.exec())

	firstDone := make(chan PromptDeliveryStatus, 1)
	go func() {
		status, _ := session.SendKeysCommandObserved(redeliverPrompt)
		firstDone <- status
	}()

	// Launch the second caller once the first attempt's paste has happened, so
	// it arrives during the redelivery wait — the window under test.
	waitFor(t, func() bool {
		_, pastes, _ := model.counts()
		return pastes >= 1
	}, "the first caller's initial paste never happened")
	secondStatus, err := session.SendKeysCommandObserved(secondPrompt)
	require.NoError(t, err)
	require.Equal(t, PromptDelivered, secondStatus)
	require.Equal(t, PromptNotDelivered, <-firstDone,
		"the doubly-stranded first caller still reports the honest terminal negative")

	model.mu.Lock()
	order := append([]string(nil), model.pasteOrder...)
	model.mu.Unlock()
	require.Equal(t, []string{redeliverPrompt, redeliverPrompt, secondPrompt}, order,
		"the redelivery must complete before a concurrent submission may run: strand, replacement, then the second caller")
}

// TestSentUnverifiedRealPaneDeliversExactlyOnce is the boundary proven against a
// real pane, where a wrong retry is observable as the double prompt itself. The
// line-composer runs with echo off, so the pasted text never renders and the
// submit honestly reports sent-unverified (#3170) — while the prompt in fact
// DELIVERED. Any automatic redelivery of that outcome would make the receiver
// record the instruction twice; the receiver's log must hold exactly one copy.
//
// Under parallel-test load a real capture can also overrun the shrunk
// observation budget, in which case the same delivery honestly reports
// could-not-confirm instead. Both are unconfirmed outcomes of a prompt that DID
// submit, and both must stay single-attempt, so the status assertion accepts
// either; the receiver's file is the assertion that catches a wrong retry.
func TestSentUnverifiedRealPaneDeliversExactlyOnce(t *testing.T) {
	defer withPasteDeliveryTiming(300*time.Millisecond, 25*time.Millisecond)()
	ts, out := startLineComposerPane(t, "af_unverified_once")

	const prompt = "UNVERIFIED-PROMPT-STAYS-SINGLE"
	status, err := ts.SendKeysCommandObserved(prompt)
	require.NoError(t, err)
	require.Contains(t, []PromptDeliveryStatus{PromptSentUnverified, PromptCouldNotConfirm}, status,
		"an echo-off pane renders no payload proof, so the honest observation is an unconfirmed outcome (#3170)")

	readReceived(t, out, "["+prompt+"]")
	// Any redelivery would have happened before SendKeysCommandObserved returned;
	// give the receiver a moment to flush a hypothetical second submission, then
	// require exactly one.
	time.Sleep(200 * time.Millisecond)
	b, rerr := os.ReadFile(out)
	require.NoError(t, rerr)
	require.Equal(t, "["+prompt+"]", string(b),
		"a sent-unverified prompt may have submitted; redelivering it doubles the instruction")
}
