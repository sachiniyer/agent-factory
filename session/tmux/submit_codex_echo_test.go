package tmux

import (
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// #7801's Codex shapes, grounded in codex-rs at the version the warnings log
// (rust-v0.171.0-alpha):
//
//   - The live composer prompts with "›" — or "»" on the Ultra tier
//     (bottom_pane/chat_composer.rs; bottom_pane/effort_ignition.rs
//     EffortTier::prompt_glyph) — and repaints an idle composer as
//     "Ask Codex to do anything", idle and busy alike.
//   - A submitted user prompt echoes in the transcript with a "› " marker at
//     EVERY tier (history_cell/messages.rs), and a queued input sits in the
//     "• Queued follow-up inputs" / "  ↳ …" preview above the composer
//     (bottom_pane/chat_composer.rs + pending_input_preview.rs).
//   - Below the composer Codex draws a rule and the model/shortcut footer
//     rows — the pane tails the production log prints beside these warnings.
//
// Under the narrower composer-glyph set an Ultra pane's "›"-marked echo row
// won the live-composer anchor and the echo's own text read as "this prompt's
// bound text is still in the live composer" — the literal-tail reading behind
// ~150 of #7801's warnings. The wider anchor (stagedLastComposerAnchor) is
// provably still the live composer: echoes, queued previews, and footers can
// only ever sort AROUND the last "»"/"›"-leading row.

const codexEchoPrompt = "Sweep every running lane for a stalled composer and file the report under today's run log."

// codexLanePane builds a sahara lane's real bottom pane: the transcript rows —
// the agent's "› "-marked echoes among them — the live composer row, the rule
// Codex draws beneath it, and the model/shortcut footer rows.
func codexLanePane(above []string, composer string) string {
	rows := append([]string{}, above...)
	rows = append(rows, composer)
	rows = append(rows,
		placeholderTestRule,
		"  GPT-6-Astra ultra · ~/sahara/sahara-clean · Pursuing goal · Main [default]",
		"  ← for agents · ? for shortcuts ⚠ 1 warning · f2 to view")
	return strings.Join(rows, "\n") + "\n"
}

func codexBoundLiteral(t *testing.T, boundary string) (deliveryProbe, stagedEvidence, boundaryComposerShape) {
	t.Helper()
	probe := newDeliveryProbe(codexEchoPrompt)
	bound := stagedEvidence{tail: true}
	shape := composerShapeAtBoundary(boundary)
	require.True(t, boundStillStaged(boundary, probe, bound, shape),
		"fixture: the draft sitting in the composer at the Enter boundary reads as staged")
	return probe, bound, shape
}

// TestCodexEchoAboveLiveComposerReadsConsumed: this delivery's prompt echoed
// in the transcript as a NEW copy — more than the boundary frame already held
// — while the live composer itself reads empty. That is positive evidence the
// prompt was consumed, not a draft still staged. On master every one of these
// reads as staged: the "›" echo row wins the narrower anchor (literal-tail)
// or the repainted idle placeholder reads as foreign content.
func TestCodexEchoAboveLiveComposerReadsConsumed(t *testing.T) {
	for name, frames := range map[string]struct{ boundary, grace string }{
		// Ultra tier: the "»" composer repaints its idle placeholder under the
		// "›"-marked echo. Master reads literal-tail through the echo row.
		"ultra tier idle composer": {
			boundary: codexLanePane([]string{"› tidy the sidebar before the next sweep"}, "» "+codexEchoPrompt),
			grace: codexLanePane([]string{
				"› tidy the sidebar before the next sweep",
				"› " + codexEchoPrompt,
			}, "» Ask Codex to do anything"),
		},
		// Same delivery, the composer repainted a bare glyph row.
		"ultra tier bare composer": {
			boundary: codexLanePane([]string{"› tidy the sidebar before the next sweep"}, "» "+codexEchoPrompt),
			grace: codexLanePane([]string{
				"› tidy the sidebar before the next sweep",
				"› " + codexEchoPrompt,
			}, "»"),
		},
		// The other tier: a "›" composer under its own-glyph echo. Master reads
		// the repainted idle placeholder as unattributable content.
		"old glyph idle composer": {
			boundary: codexLanePane([]string{"› tidy the sidebar before the next sweep"}, "› "+codexEchoPrompt),
			grace: codexLanePane([]string{
				"› tidy the sidebar before the next sweep",
				"› " + codexEchoPrompt,
			}, "› Ask Codex to do anything"),
		},
		// Codex's queued-input preview holds the prompt above an idle "»"
		// composer (chat_composer.rs's pending_input_preview: "• Queued
		// follow-up inputs" then "  ↳ …" rows). Queued is consumed — the
		// composer let the draft go. Master reads literal-tail through the
		// earlier echo row.
		"queued input preview": {
			boundary: codexLanePane([]string{"› tidy the sidebar before the next sweep"}, "» "+codexEchoPrompt),
			grace: codexLanePane([]string{
				"› tidy the sidebar before the next sweep",
				"• Queued follow-up inputs",
				"  ↳ " + codexEchoPrompt,
				"    ⇧+← edit last queued message",
			}, "» Ask Codex to do anything"),
		},
	} {
		t.Run(name, func(t *testing.T) {
			probe, bound, shape := codexBoundLiteral(t, frames.boundary)
			reason, consumed := stagedReading(frames.grace, probe, bound, shape)
			require.Empty(t, reason, "a NEW echo of this delivery above an empty live composer is consumed, not staged")
			require.True(t, consumed, "the reading must carry its positive evidence")
			require.False(t, boundStillStaged(frames.grace, probe, bound, shape))
		})
	}
}

// TestCodexChipEchoAboveUltraComposerReadsConsumed: a bound paste chip echoes
// the same way — Codex draws the submitted message's chip in its "›" echo
// just as it draws the pending one in the composer. The new copy above an
// empty live composer is consumed evidence. Master reads literal-chip through
// the echo row.
func TestCodexChipEchoAboveUltraComposerReadsConsumed(t *testing.T) {
	chip := "[Pasted Content 4207 chars]"
	bound := stagedEvidence{chip: true, chipText: normalizeDelivery(chip)}
	boundary := codexLanePane([]string{"› tidy the sidebar before the next sweep"}, "» "+chip)
	shape := composerShapeAtBoundary(boundary)
	require.True(t, boundStillStaged(boundary, deliveryProbe{}, bound, shape),
		"fixture: the bound chip sits in the composer at the Enter boundary")
	grace := codexLanePane([]string{
		"› tidy the sidebar before the next sweep",
		"› " + chip,
	}, "» Ask Codex to do anything")
	reason, consumed := stagedReading(grace, deliveryProbe{}, bound, shape)
	require.Empty(t, reason)
	require.True(t, consumed)
}

// TestCodexAmbiguousEchoFramesStayStaged is the conservative half — every one
// of these must stay staged or unverified, because none carries positive
// evidence THIS delivery was consumed:
//
//   - A dropped prompt shows the same idle composer as a submitted one whose
//     echo the frame cannot prove (#4200).
//   - A watcher that resends identical text leaves an earlier echo whose
//     count never grows — presence is not evidence.
//   - An echo above a composer that still holds the draft is a staged draft
//     with its echo, never a consumed one.
//   - A status/rule row between the echo and the composer is Codex's own
//     layout; it must neither fake growth nor hide the composer.
func TestCodexAmbiguousEchoFramesStayStaged(t *testing.T) {
	for name, frames := range map[string]struct {
		boundary, grace, wantReason string
	}{
		// A dropped prompt: the composer repainted idle and moved down a row,
		// with no echo anywhere. Master's narrower anchor finds no glyph row
		// at all and reports the still-visible draft as delivered.
		"dropped prompt: idle composer, no new echo": {
			boundary:   codexLanePane([]string{"⠋ Working · 3m"}, "» "+codexEchoPrompt),
			grace:      codexLanePane([]string{"⠋ Working · 3m", "✻ Ran tool", "── output ──"}, "» Ask Codex to do anything"),
			wantReason: "idle placeholder",
		},
		// An earlier echo of the SAME prompt above the idle composer — what a
		// resending watch leaves. The count never grew, so it is not evidence.
		// Master reads literal-tail through the echo row.
		"earlier identical echo, no new echo": {
			boundary:   codexLanePane([]string{"› " + codexEchoPrompt}, "» "+codexEchoPrompt),
			grace:      codexLanePane([]string{"› " + codexEchoPrompt, "✻ Ran tool"}, "» Ask Codex to do anything"),
			wantReason: "idle placeholder",
		},
		// The draft still in the live composer, with this delivery's echo above
		// it — a wedge where the agent echoed and kept the draft. Literal-tail
		// stays the winning reading on master and after.
		"draft still in the live composer under its echo": {
			boundary:   codexLanePane(nil, "» "+codexEchoPrompt),
			grace:      codexLanePane([]string{"› " + codexEchoPrompt}, "» "+codexEchoPrompt),
			wantReason: "still in the live composer",
		},
		// Codex's own status row drawn between a flat echo and the idle
		// composer: the separator must not fake or hide anything.
		"status row between a flat echo and the composer": {
			boundary:   codexLanePane([]string{"› " + codexEchoPrompt}, "» "+codexEchoPrompt),
			grace:      codexLanePane([]string{"› " + codexEchoPrompt, "• Working · 4m · esc to interrupt"}, "» Ask Codex to do anything"),
			wantReason: "idle placeholder",
		},
	} {
		t.Run(name, func(t *testing.T) {
			probe, bound, shape := codexBoundLiteral(t, frames.boundary)
			reason, consumed := stagedReading(frames.grace, probe, bound, shape)
			require.False(t, consumed, "an ambiguous frame must never read as consumed")
			require.Contains(t, reason, frames.wantReason)
			require.True(t, boundStillStaged(frames.grace, probe, bound, shape))
		})
	}
}

// TestUltraComposerStillStagedWithoutAnyNarrowGlyphRow is the under-warning
// half of the same bug: a pane whose draft is still in the "»" composer and
// whose transcript carries NO "›"/"❯"/"❭"/">" row gave master's narrower
// anchor nothing to land on — the still-staged draft read as delivered. The
// wider anchor sees it.
func TestUltraComposerStillStagedWithoutAnyNarrowGlyphRow(t *testing.T) {
	boundary := codexLanePane([]string{"⠋ Working · 3m"}, "» "+codexEchoPrompt)
	probe, bound, shape := codexBoundLiteral(t, boundary)
	grace := codexLanePane([]string{"⠋ Working · 3m"}, "» "+codexEchoPrompt)
	reason, consumed := stagedReading(grace, probe, bound, shape)
	require.False(t, consumed)
	require.Contains(t, reason, "still in the live composer")
}

// TestDevinIdlePlaceholderRepaintNamesTheIdleReading is #7801's residual 2:
// Devin's "Starting session…" frame holds only the idle placeholder — no echo
// of this prompt above the live composer — so the staged reading is CORRECT
// (a dropped prompt shows the same frame); the warning just has to say what
// it saw. Master names it "repainted holding content".
func TestDevinIdlePlaceholderRepaintNamesTheIdleReading(t *testing.T) {
	boundary := devinLanePane([]string{"one"}, placeholderDraft("❭")...)
	probe, bound, shape := boundLiteral(t, boundary)
	grace := devinLanePane([]string{"one", "SWE-2 Max · bypass Starting session…"},
		"❭ Ask Devin to build features, fix bugs, or work on your code")
	reason, consumed := stagedReading(grace, probe, bound, shape)
	require.False(t, consumed, "the idle placeholder alone is not evidence the prompt was consumed")
	require.Contains(t, reason, "idle placeholder")
	require.True(t, boundStillStaged(grace, probe, bound, shape))
}

// TestUltraCodexSubmittedDraftNeedsNoWarningOrRemedyEnter drives the whole
// delivery path end to end: a literal paste lands in the "»" composer, Enter
// submits it, the agent echoes it "› " and repaints "» Ask Codex to do
// anything". Consumed — delivered, one Enter, no warning. Master reads
// literal-tail through the echo row and reports sent-unverified.
func TestUltraCodexSubmittedDraftNeedsNoWarningOrRemedyEnter(t *testing.T) {
	warnings := captureWarningLog(t)
	m := &stagedDraftPane{glyph: "»", echoGlyph: "›", placeholder: "Ask Codex to do anything", render: literalRender}
	status, pastes, enters := sendStaged(t, m, codexEchoPrompt)
	require.Equal(t, PromptDelivered, status)
	require.Equal(t, 1, pastes)
	require.Equal(t, 1, enters, "a consumed delivery never earns a remedy Enter")
	require.NotContains(t, warnings.String(), "withholding the remedy Enter")
}

// TestUltraCodexLiteralStrandStillWarnsAndWithholds: a draft whose Enter was
// absorbed stays in the "»" composer — literal-tail, one Enter total, the
// warning logged. Master finds no narrower glyph row at all and reports the
// still-staged draft delivered — the under-warning this also fixes.
func TestUltraCodexLiteralStrandStillWarnsAndWithholds(t *testing.T) {
	warnings := captureWarningLog(t)
	m := &stagedDraftPane{glyph: "»", echoGlyph: "›", swallowedEnters: 1, render: literalRender}
	status, _, enters := sendStaged(t, m, codexEchoPrompt)
	require.Equal(t, PromptSentUnverified, status)
	require.Equal(t, 1, enters, "the literal tail still authorizes no remedy Enter")
	require.Contains(t, warnings.String(), "withholding the remedy Enter")
	require.Contains(t, warnings.String(), "still in the live composer")
}

// TestConsumedEchoDemotesAbsentInsteadOfRedelivering pins property 4: polls
// classified the delivery absent, the boundary bound the whole draft, and the
// echo then proves it was consumed. That reading is evidence AGAINST absence
// — the outcome demotes to sent-unverified, and the #3293 redelivery can
// never fire because of it: one paste, one Enter.
func TestConsumedEchoDemotesAbsentInsteadOfRedelivering(t *testing.T) {
	warnings := captureWarningLog(t)
	m := &stagedDraftPane{glyph: "»", echoGlyph: "›", pollPartial: true, placeholder: "Ask Codex to do anything", render: literalRender}
	status, pastes, enters := sendStaged(t, m, codexEchoPrompt)
	require.Equal(t, PromptSentUnverified, status)
	require.Equal(t, 1, pastes, "a proven echo must never reach the redelivery path")
	require.Equal(t, 1, enters)
	require.NotContains(t, warnings.String(), "withholding the remedy Enter")
}

// TestDevinStartingSessionWarnsNamingIdlePlaceholder drives residual 2 end to
// end: Devin's boot frame replaces the draft with the idle placeholder over a
// "Starting session…" footer and shows no echo — the same frame a dropped
// prompt leaves. The verdict stays sent-unverified, one Enter, and the
// warning says what it saw. Master warns too, but names it "repainted holding
// content".
func TestDevinStartingSessionWarnsNamingIdlePlaceholder(t *testing.T) {
	warnings := captureWarningLog(t)
	m := &stagedDraftPane{glyph: "❭", placeholder: "Ask Devin to build features, fix bugs, or work on your code", render: literalRender}
	mock := m.exec()
	base := mock.OutputFunc
	mock.OutputFunc = func(c *exec.Cmd) ([]byte, error) {
		out, err := base(c)
		if isDeliveryBoundaryCommand(c) {
			m.mu.Lock()
			m.transcript = []string{"SWE-2 Max · bypass Starting session…"}
			m.composer = nil
			m.mu.Unlock()
		}
		return out, err
	}
	defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()
	session := newTmuxSession("af_proj", ProgramCodex, NewMockPtyFactory(t), mock)
	status, err := session.SendKeysCommandObserved(redeliverPrompt)
	require.NoError(t, err)
	_, enters := m.counts()
	require.Equal(t, 1, enters, "an idle placeholder authorizes no remedy Enter")
	require.Equal(t, PromptSentUnverified, status, "no positive evidence: the warning is correct")
	logged := warnings.String()
	require.Contains(t, logged, "withholding the remedy Enter")
	require.Contains(t, logged, "idle placeholder", "the warning names the reading it saw")
}
