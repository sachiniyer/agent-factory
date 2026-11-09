package tmux

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// #7023: production logged "still shows this prompt's bound evidence staged in
// the composer after Enter … withholding the remedy Enter and reporting
// sent-unverified (#4200)" about 400 times a week. In 252 of the 537 logged
// warnings, the grace frame's live composer row was Devin's own empty-input
// placeholder ("❭ Guide Devin while it works" mid-turn, 226 of them), with
// only the rule and the footer below it. So the draft had left the composer.
// capture-pane -p carries no attributes, so the dim placeholder read as typed
// content, and the repainted row as a composer holding foreign text. Claude
// draws the same kind of hint when a message queues behind a running turn
// ("❯ Press up to edit queued messages", root's own composer in the
// 2026-07-21 production log).
//
// The pane shapes below are those production rows: the composer and footer
// rows come from the logged pane tails, and the transcript rows above them
// come from task/devin_ready_test.go and task/claude_working_test.go.

const placeholderTestPrompt = "Review PR 7031 and answer every open Codex thread with RESOLVED or ACCEPTED before you push again"

var placeholderTestRule = strings.Repeat("─", 80)

// devinLanePane is a Devin lane as the warning's pane tail shows it: the
// transcript, the composer between two rules, and the footer.
func devinLanePane(above []string, composer ...string) string {
	rows := append([]string{}, above...)
	rows = append(rows, placeholderTestRule)
	rows = append(rows, composer...)
	rows = append(rows, placeholderTestRule, "SWE-2 Max · bypass · 2 shells ↓ ctrl+o to expand edits")
	return strings.Join(rows, "\n") + "\n"
}

// claudeRootPane is root's shape: vim INSERT footer one blank row below the
// composer box.
func claudeRootPane(above []string, composer ...string) string {
	rows := append([]string{}, above...)
	rows = append(rows, placeholderTestRule)
	rows = append(rows, composer...)
	rows = append(rows, placeholderTestRule, "",
		"  -- INSERT -- ⏵⏵ bypass permissions on · 2 shells · ← 1 agent")
	return strings.Join(rows, "\n") + "\n"
}

// placeholderDraft is the prompt as each agent wraps it in its composer.
func placeholderDraft(glyph string) []string {
	return []string{
		glyph + " Review PR 7031 and answer every open Codex thread",
		"  with RESOLVED or ACCEPTED before you push again",
	}
}

type placeholderShape struct {
	name     string
	boundary string // the Enter boundary: the draft still in the composer
	grace    string // the grace frame: the draft gone, the composer repainted
	// repaint builds the grace frame with a different composer row, for the
	// controls.
	repaint func(composerRow string) string
}

func placeholderShapes() []placeholderShape {
	devinWorking := []string{"⡎⢱ Thinking · 41s (esc twice to interrupt) · (2.1kc)", ""}
	devinAfter := func(row string) string {
		return devinLanePane(append(append(append([]string{}, devinWorking...),
			placeholderDraft("❭")...), ""), row)
	}
	claudeWorking := []string{"✻ Orbiting… (3m 25s · ↓ 18.8k tokens)", ""}
	claudeAfter := func(row string) string {
		// The queued message sits above the composer box; its exact layout is not
		// load-bearing, only that it is outside the live composer.
		queued := []string{"✻ Orbiting… (3m 26s · ↓ 18.9k tokens)", "",
			"  Review PR 7031 and answer every open Codex thread",
			"  with RESOLVED or ACCEPTED before you push again", ""}
		return claudeRootPane(queued, row)
	}
	return []placeholderShape{
		{
			name:     "devin mid-turn",
			boundary: devinLanePane(devinWorking, placeholderDraft("❭")...),
			grace:    devinAfter("❭ Guide Devin while it works"),
			repaint:  devinAfter,
		},
		{
			name:     "claude message queued behind a running turn",
			boundary: claudeRootPane(claudeWorking, placeholderDraft("❯")...),
			grace:    claudeAfter("❯ Press up to edit queued messages"),
			repaint:  claudeAfter,
		},
	}
}

func boundLiteral(t *testing.T, boundary string) (deliveryProbe, stagedEvidence, int) {
	t.Helper()
	probe := newDeliveryProbe(placeholderTestPrompt)
	bound := stagedEvidence{tail: true}
	anchor := lastComposerGlyphRow(paneGridRows(boundary))
	require.GreaterOrEqual(t, anchor, 0, "fixture: the boundary has a composer glyph row")
	require.True(t, boundStillStaged(boundary, probe, bound, anchor),
		"fixture: the draft sitting in the composer at the Enter boundary reads as staged")
	return probe, bound, anchor
}

// TestAgentPlaceholderAfterEnterIsNotStillStaged: the draft left the composer
// and the agent repainted its empty input with its own hint. That is the bare
// composer a submit leaves, not a draft still staged.
func TestAgentPlaceholderAfterEnterIsNotStillStaged(t *testing.T) {
	for _, shape := range placeholderShapes() {
		t.Run(shape.name, func(t *testing.T) {
			probe, bound, anchor := boundLiteral(t, shape.boundary)
			require.Greater(t, lastComposerGlyphRow(paneGridRows(shape.grace)), anchor,
				"fixture: the repainted composer sits below the boundary's")
			require.False(t, boundStillStaged(shape.grace, probe, bound, anchor),
				"the agent's own empty-input placeholder is an empty composer: the draft left it")
		})
	}
}

// TestRepaintedComposerHoldingOtherTextStaysStaged keeps #4971: a composer
// repainted with anything that is not exactly the agent's own placeholder,
// under the agent's own glyph, cannot be attributed and still reads as staged.
func TestRepaintedComposerHoldingOtherTextStaysStaged(t *testing.T) {
	for _, shape := range placeholderShapes() {
		glyph := "❭"
		other := "›"
		placeholder := "Guide Devin while it works"
		if strings.HasPrefix(shape.name, "claude") {
			glyph = claudeComposerGlyph
			placeholder = "Press up to edit queued messages"
		}
		for name, row := range map[string]string{
			"user text":                         glyph + " user typed this",
			"placeholder plus typed text":       glyph + " " + placeholder + " and then some",
			"placeholder under another glyph":   other + " " + placeholder,
			"another agent's placeholder":       glyph + " Ask Codex to do anything",
			"placeholder cut short by the user": glyph + " " + placeholder[:len(placeholder)-3],
			// Devin's IDLE hint is deliberately not accepted. It says the input is
			// empty and the agent is not working, which is also what a composer
			// that dropped the prompt looks like (#4200). The mid-turn and queued
			// hints say the agent took input while it works.
			"devin's idle placeholder": "❭ Ask Devin to build features, fix bugs, or work on your code",
		} {
			t.Run(shape.name+"/"+name, func(t *testing.T) {
				probe, bound, anchor := boundLiteral(t, shape.boundary)
				require.True(t, boundStillStaged(shape.repaint(row), probe, bound, anchor),
					"a repainted composer holding %q is not provably empty", row)
			})
		}
	}
}

// TestPlaceholderAboveTheBoundaryAnchorStaysStaged: a composer that moved UP
// is a shape the remedy cannot attribute, placeholder or not. #7023 widens only
// the empty-composer reading below the old anchor.
func TestPlaceholderAboveTheBoundaryAnchorStaysStaged(t *testing.T) {
	boundary := devinLanePane([]string{"one", "two", "three", "four", "five", ""}, placeholderDraft("❭")...)
	probe, bound, anchor := boundLiteral(t, boundary)
	grace := devinLanePane([]string{"one"}, "❭ Guide Devin while it works")
	require.Less(t, lastComposerGlyphRow(paneGridRows(grace)), anchor, "fixture: the composer moved up")
	require.True(t, boundStillStaged(grace, probe, bound, anchor))
}

// TestBusyAgentPlaceholderDeliveryLandsWithoutWarning drives the whole delivery
// path. A literal draft lands, Enter submits it, and the composer comes back
// as the agent's mid-turn placeholder. The delivery is the healthy submit
// TestDispatchedDraftGetsNoRemedyEnter pins for a bare composer: delivered,
// one Enter, and no withheld-remedy warning.
func TestBusyAgentPlaceholderDeliveryLandsWithoutWarning(t *testing.T) {
	warnings := captureWarningLog(t)
	m := &stagedDraftPane{glyph: "❭", placeholder: "Guide Devin while it works", render: literalRender}
	status, pastes, enters := sendStaged(t, m, redeliverPrompt)
	require.Equal(t, 1, pastes)
	require.Equal(t, 1, enters, "a submitted draft gets no remedy Enter")
	require.Equal(t, PromptDelivered, status)
	require.NotContains(t, warnings.String(), "withholding the remedy Enter",
		"a delivery whose draft left the composer must not warn that it is still staged")
}

// TestUnattributableRepaintStillWarnsWithItsReading is the other half: a
// repainted composer holding the user's text stays sent-unverified, gets no
// Enter, and still logs the WARNING with the session, the reading that
// withheld it, and the pane tail. A genuinely unprovable composer stays
// visible.
func TestUnattributableRepaintStillWarnsWithItsReading(t *testing.T) {
	warnings := captureWarningLog(t)
	m := &stagedDraftPane{glyph: "❭", placeholder: "Guide Devin while it works", render: literalRender}
	mock := m.exec()
	base := mock.OutputFunc
	mock.OutputFunc = func(c *exec.Cmd) ([]byte, error) {
		out, err := base(c)
		if isDeliveryBoundaryCommand(c) {
			m.mu.Lock()
			m.composer = []string{"user typed this"}
			m.mu.Unlock()
		}
		return out, err
	}
	defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()
	session := newTmuxSession("af_proj", ProgramCodex, NewMockPtyFactory(t), mock)
	status, err := session.SendKeysCommandObserved(redeliverPrompt)
	require.NoError(t, err)
	_, enters := m.counts()
	require.Equal(t, 1, enters, "the user's text must never receive our Enter")
	require.Equal(t, PromptSentUnverified, status)

	logged := warnings.String()
	require.Equal(t, 1, strings.Count(logged, "withholding the remedy Enter"), "one warning: %s", logged)
	require.Contains(t, logged, session.sanitizedName)
	require.Contains(t, logged, "repainted", "the warning names the reading that withheld the remedy")
	require.Contains(t, logged, "Pane tail: ")
	require.Contains(t, logged, "user typed this", "the pane tail is the frame that decided")
}

// TestLiteralStrandWarningSaysTheTextIsStillInTheComposer: a draft whose
// Enter was absorbed keeps the warning, and the warning now says the bound
// text is still in the live composer. That is the stuck-draft reading, so
// the log tells it apart from an unattributable repaint.
func TestLiteralStrandWarningSaysTheTextIsStillInTheComposer(t *testing.T) {
	warnings := captureWarningLog(t)
	m := &stagedDraftPane{swallowedEnters: 1, render: literalRender}
	status, _, enters := sendStaged(t, m, redeliverPrompt)
	require.Equal(t, PromptSentUnverified, status)
	require.Equal(t, 1, enters)
	require.Contains(t, warnings.String(), "still in the live composer")
}
