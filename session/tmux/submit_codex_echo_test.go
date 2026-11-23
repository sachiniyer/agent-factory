package tmux

import (
	"os/exec"
	"strconv"
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
// the agent's "› "-marked echoes among them — the live composer's rows, the
// rule Codex draws beneath it, and the model/shortcut footer rows.
func codexLanePane(above []string, composer ...string) string {
	rows := append([]string{}, above...)
	rows = append(rows, composer...)
	rows = append(rows,
		placeholderTestRule,
		"  GPT-6-Astra ultra · ~/sahara/sahara-clean · Pursuing goal · Main [default]",
		"  ← for agents · ? for shortcuts ⚠ 1 warning · f2 to view")
	return strings.Join(rows, "\n") + "\n"
}

// boundaryShapeForTest reduces a boundary fixture to its composer shape the
// way submit does: the anchor, mark and block start from the visible grid,
// the echo baseline from the scrollback-inclusive capture. A fixture pane
// models the whole transcript, so its own above-block rows ARE the
// baseline; scrollback passes extra rows that sat only in history at the
// boundary — the copies a tall composer pushed off the pane top (Codex
// #7802 R2).
func boundaryShapeForTest(boundary string, scrollback ...string) boundaryComposerShape {
	rows := paneGridRows(boundary)
	anchor := stagedLastComposerAnchor(rows)
	upper := rows
	if anchor >= 0 {
		upper = rows[:anchor]
	}
	return composerShapeAtBoundary(boundary, append(scrollback, upper...), true)
}

func codexBoundLiteral(t *testing.T, boundary string) (deliveryProbe, stagedEvidence, boundaryComposerShape) {
	t.Helper()
	probe := newDeliveryProbe(codexEchoPrompt)
	bound := stagedEvidence{tail: true}
	shape := boundaryShapeForTest(boundary)
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
	shape := boundaryShapeForTest(boundary)
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
		// The echo row is the composer block's earliest anchor, so the bound
		// text below it keeps the literal-tail reading.
		"earlier identical echo, no new echo": {
			boundary:   codexLanePane([]string{"› " + codexEchoPrompt}, "» "+codexEchoPrompt),
			grace:      codexLanePane([]string{"› " + codexEchoPrompt, "✻ Ran tool"}, "» Ask Codex to do anything"),
			wantReason: "still in the live composer",
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
		// composer: the separator must not fake or hide anything. The flat
		// echo holds the literal-tail reading the same way.
		"status row between a flat echo and the composer": {
			boundary:   codexLanePane([]string{"› " + codexEchoPrompt}, "» "+codexEchoPrompt),
			grace:      codexLanePane([]string{"› " + codexEchoPrompt, "• Working · 4m · esc to interrupt"}, "» Ask Codex to do anything"),
			wantReason: "still in the live composer",
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

// TestGlyphLeadingDraftLineKeepsStagedReading is #7802 R1: the bound draft's
// own lines can lead with a composer glyph — "Explain this:\n» quote" puts a
// "»"-leading continuation row inside a "›" (or "❯"/"❭") composer. The wider
// staged anchor lands on that row, INSIDE the draft, and the draft's head
// sits above it. On the reviewed head the literal-tail check searched only
// below the last glyph row, missed the head, found no echo growth, and read
// the still-staged draft as delivered. Bound evidence at or below the
// composer block's first row — the earliest of the narrow and wide anchors —
// keeps the staged reading.
func TestGlyphLeadingDraftLineKeepsStagedReading(t *testing.T) {
	const prompt = "Explain this:\n» quote"
	for _, glyph := range []string{"›", "❯", "❭"} {
		t.Run("composer "+glyph, func(t *testing.T) {
			composer := []string{glyph + " Explain this:", "  » quote"}
			boundary := codexLanePane([]string{"✻ Ran tool"}, composer...)
			probe := newDeliveryProbe(prompt)
			bound := stagedEvidence{tail: true}
			shape := boundaryShapeForTest(boundary)
			require.True(t, boundStillStaged(boundary, probe, bound, shape),
				"fixture: the draft with a glyph-leading line sits in the composer at the boundary")
			// The Enter was absorbed: the grace frame is the same pane.
			reason, consumed := stagedReading(boundary, probe, bound, shape)
			require.False(t, consumed, "a draft's own glyph-leading line is not an echo")
			require.Contains(t, reason, "still in the live composer")
			require.True(t, boundStillStaged(boundary, probe, bound, shape))
		})
	}
}

// TestRevealedScrollbackEchoCannotFakeConsumption is #7802 R2: an identical
// echo the tall composer pushed off the pane top is already counted by the
// scrollback-inclusive baseline, so its REAPPEARING when the composer
// shrinks — the dropped-prompt frame — cannot read as a new echo. On the
// reviewed head the visible-only baseline counted zero and the revealed copy
// read as growth: a dropped prompt claimed consumed.
func TestRevealedScrollbackEchoCannotFakeConsumption(t *testing.T) {
	boundary := codexLanePane([]string{"⠋ Working · 3m"}, "» "+codexEchoPrompt)
	probe, bound, _ := codexBoundLiteral(t, boundary)

	// The baseline the boundary capture actually held: the prompt's earlier
	// echo sat in scrollback above the visible frame — a tall composer had
	// pushed it off the pane top.
	shape := boundaryShapeForTest(boundary, "› "+codexEchoPrompt)

	// Dropped: the composer repainted idle and the scrollback echo is
	// revealed. Count 1 against baseline 1 — never grew, so not evidence.
	revealed := codexLanePane([]string{"⠋ Working · 3m", "› " + codexEchoPrompt}, "» Ask Codex to do anything")
	reason, consumed := stagedReading(revealed, probe, bound, shape)
	require.False(t, consumed, "a revealed scrollback echo is not a new echo")
	require.NotEmpty(t, reason, "the dropped-prompt frame stays staged")

	// The same baseline must not overblock: a NEW echo really does grow the
	// count past it and reads consumed.
	submitted := codexLanePane([]string{
		"⠋ Working · 3m",
		"› " + codexEchoPrompt,
		"› " + codexEchoPrompt,
	}, "» Ask Codex to do anything")
	reason, consumed = stagedReading(submitted, probe, bound, shape)
	require.Empty(t, reason)
	require.True(t, consumed, "a second copy — the new echo — beats the scrollback baseline")
}

// TestEchoBaselineWithoutScrollbackReachStaysStaged is R2's fallback: a
// boundary whose scrollback reach is unproven AND whose transcript left no
// mark can tell a new echo from neither revealed history nor a post-boundary
// position, so the consumed reading never fires — the frame stays staged
// rather than guess.
func TestEchoBaselineWithoutScrollbackReachStaysStaged(t *testing.T) {
	boundary := codexLanePane([]string{}, "» "+codexEchoPrompt)
	probe, bound, _ := codexBoundLiteral(t, boundary)
	shape := composerShapeAtBoundary(boundary, nil, false)
	grace := codexLanePane([]string{"› " + codexEchoPrompt}, "» Ask Codex to do anything")
	reason, consumed := stagedReading(grace, probe, bound, shape)
	require.False(t, consumed, "no baseline and no mark: a grown-looking count cannot be trusted")
	require.NotEmpty(t, reason, "the frame stays staged")
}

// TestBoundaryMarkProvesNewEchoBelowIt is the second proof newEchoCopy
// admits — the one that works on the alternate screen, where tmux keeps no
// history and the baseline can never count. The mark is the transcript's own
// tail line at Enter, so an echo BELOW it is post-boundary by position:
// revealed scrollback rows can only reappear above the boundary's line.
func TestBoundaryMarkProvesNewEchoBelowIt(t *testing.T) {
	// The boundary's transcript tail must clear the mark's 16-rune floor —
	// anything shorter yields no mark at all.
	const status = "⠋ Working · 3m 12s · esc to interrupt"
	boundary := codexLanePane([]string{status}, "» "+codexEchoPrompt)
	probe, bound, _ := codexBoundLiteral(t, boundary)
	shape := composerShapeAtBoundary(boundary, nil, false) // no history reach at all
	grace := codexLanePane([]string{status, "› " + codexEchoPrompt}, "» Ask Codex to do anything")
	reason, consumed := stagedReading(grace, probe, bound, shape)
	require.Empty(t, reason)
	require.True(t, consumed,
		"an echo below the boundary's own tail line is post-boundary — no baseline needed")
}

// TestMarkFromDraftHeadStaysStaged is the third review's R1 frame: a draft
// whose own head leads with a glyph makes the last glyph row land inside
// the draft, and a mark taken from that head is draft text — a resending
// watch's identical echo then "proves" a dropped prompt. The mark must come
// only from transcript rows above the composer block's head.
func TestMarkFromDraftHeadStaysStaged(t *testing.T) {
	const head = "sweep every running lane for a stalled composer"
	const cont = "› the daemon supervises sessions itself"
	probe := newDeliveryProbe(head + "\n" + cont)
	bound := stagedEvidence{tail: true}
	boundary := codexLanePane([]string{"› " + head, "  " + cont}, "» "+head, "  "+cont)
	shape := composerShapeAtBoundary(boundary, nil, false) // no history reach
	require.NotContains(t, shape.mark, "»",
		"the draft's own head row must never become the boundary mark")
	// The dropped draft: the composer repaints its resting face and the old
	// echo — including its continuation — is all that remains above it.
	grace := codexLanePane([]string{"› " + head, "  " + cont}, "» Ask Codex to do anything")
	reason, consumed := stagedReading(grace, probe, bound, shape)
	require.False(t, consumed,
		"the mark came from the draft's own head — it proves nothing about the echo")
	require.NotEmpty(t, reason, "the dropped frame stays staged")
}

// TestMarkInsideIdenticalEchoStaysStaged pins the mark path's conservative
// miss: when the boundary's transcript tail IS the same echo — a resending
// watch left it there — every new copy wears the mark too, so the last
// occurrence hides the genuinely new one. The frame stays staged rather
// than guess which copy is new.
func TestMarkInsideIdenticalEchoStaysStaged(t *testing.T) {
	boundary := codexLanePane([]string{"› " + codexEchoPrompt}, "» "+codexEchoPrompt)
	probe, bound, _ := codexBoundLiteral(t, boundary)
	shape := composerShapeAtBoundary(boundary, nil, false)
	grace := codexLanePane([]string{
		"› " + codexEchoPrompt,
		"› " + codexEchoPrompt,
	}, "» Ask Codex to do anything")
	reason, consumed := stagedReading(grace, probe, bound, shape)
	require.False(t, consumed,
		"the mark lands inside an identical echo — the new copy is unprovable")
	require.NotEmpty(t, reason, "the ambiguous frame stays staged")
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
	m := &stagedDraftPane{
		glyph: "»", echoGlyph: "›", placeholder: "Ask Codex to do anything", render: literalRender,
		// A transcript tail gives the boundary a mark to prove the new echo
		// positionally — the honest mock's -S capture is post-Enter, so the
		// baseline already holds this delivery's echo and the count can
		// never grow past it (#7802 R2).
		transcript: []string{"› tidy the sidebar before the next sweep"},
	}
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

// TestGlyphLeadingLineStrandStillWarnsAndWithholds drives #7802 R1 end to
// end: the draft's own second line leads with "»", the wider anchor lands
// INSIDE the draft, and the absorbed Enter leaves the whole draft staged.
// The reviewed head read that pane delivered; it must warn sent-unverified.
func TestGlyphLeadingLineStrandStillWarnsAndWithholds(t *testing.T) {
	for _, glyph := range []string{"›", "❯", "❭"} {
		t.Run("composer "+glyph, func(t *testing.T) {
			warnings := captureWarningLog(t)
			m := &stagedDraftPane{
				glyph:           glyph,
				swallowedEnters: 1,
				render:          func(payload string) []string { return strings.Split(payload, "\n") },
			}
			status, pastes, enters := sendStaged(t, m, "Explain this:\n» quote")
			require.Equal(t, PromptSentUnverified, status,
				"a draft whose own line leads with %s must never read as delivered", "»")
			require.Equal(t, 1, pastes)
			require.Equal(t, 1, enters, "a staged draft authorizes no remedy Enter")
			require.Contains(t, warnings.String(), "withholding the remedy Enter")
			require.Contains(t, warnings.String(), "still in the live composer")
		})
	}
}

// TestConsumedEchoDemotesAbsentInsteadOfRedelivering pins property 4: polls
// classified the delivery absent, the boundary bound the whole draft, and the
// echo then proves it was consumed. That reading is evidence AGAINST absence
// — the outcome demotes to sent-unverified, and the #3293 redelivery can
// never fire because of it: one paste, one Enter. The log assertions are the
// witness (#7802 R3): without the demotion the absent outcome logs its ERROR
// and the veto logs its warning, and paste/Enter counts alone cannot tell
// the two paths apart.
func TestConsumedEchoDemotesAbsentInsteadOfRedelivering(t *testing.T) {
	errors := captureErrorLog(t)
	warnings := captureWarningLog(t)
	m := &stagedDraftPane{
		glyph: "»", echoGlyph: "›", pollPartial: true, placeholder: "Ask Codex to do anything", render: literalRender,
		// The transcript tail marks the boundary so the new echo proves
		// positionally even though the post-Enter -S baseline already holds it.
		transcript: []string{"› tidy the sidebar before the next sweep"},
	}
	status, pastes, enters := sendStaged(t, m, codexEchoPrompt)
	require.Equal(t, PromptSentUnverified, status)
	require.Equal(t, 1, pastes, "a proven echo must never reach the redelivery path")
	require.Equal(t, 1, enters)
	require.NotContains(t, warnings.String(), "withholding the remedy Enter")
	require.NotContains(t, errors.String(), "prompt delivery observed absent",
		"the consumed reading must demote the absent outcome before the ERROR logs")
	require.NotContains(t, warnings.String(), "withholding redelivery",
		"the demoted outcome must never reach the redelivery veto")
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

// TestAltScreenDroppedPromptStaysStaged is #7802 R1's whole-path regression:
// on the alternate screen tmux keeps NO history, so the -S capture returns
// the visible grid with ok=true — a baseline that only looks scrollback-
// inclusive. A tall draft pushes an earlier "› " echo off the pane top; the
// dropped draft repaints the resting placeholder and the old echo reappears
// — a visible count of 0 growing to 1. alternate_on=1 keeps the baseline
// out of the proof, the boundary's off-screen transcript left no mark, and
// the frame stays staged. The composer is "›" because the positional bind
// needs a narrow glyph row — the consumed-reading defect it exercises is
// the same one an Ultra pane hits.
func TestAltScreenDroppedPromptStaysStaged(t *testing.T) {
	warnings := captureWarningLog(t)
	m := &stagedDraftPane{
		glyph: "›", echoGlyph: "›", placeholder: "Ask Codex to do anything",
		alternateOn: true, dropDraftOnEnter: true, height: 8,
		transcript: []string{"› " + codexEchoPrompt},
		render: func(string) []string {
			return []string{codexEchoPrompt, "draft 2", "draft 3", "draft 4",
				"draft 5", "draft 6", "draft 7", "draft 8"}
		},
	}
	status, pastes, enters := sendStaged(t, m, codexEchoPrompt)
	require.Equal(t, PromptSentUnverified, status,
		"a revealed alt-screen echo is not a new echo — the baseline never reached history")
	require.Equal(t, 1, pastes)
	require.Equal(t, 1, enters, "no consumed reading authorizes no remedy Enter")
	require.Contains(t, warnings.String(), "withholding the remedy Enter")
}

// TestBaselineHoldingOwnEchoOnlyWithholdsConsumed is #7802 R2's pin: the
// baseline capture runs AFTER the Enter, and the mock's echo lands inside
// the boundary command — so the -S baseline legitimately holds this
// delivery's own echo, exactly like a fast agent in production. With no
// transcript tail to mark the boundary, the count can never grow past the
// baseline and the consumed reading is withheld — safe, never a claim.
func TestBaselineHoldingOwnEchoOnlyWithholdsConsumed(t *testing.T) {
	warnings := captureWarningLog(t)
	m := &stagedDraftPane{glyph: "»", echoGlyph: "›", placeholder: "Ask Codex to do anything", render: literalRender}
	status, pastes, enters := sendStaged(t, m, codexEchoPrompt)
	require.Equal(t, PromptSentUnverified, status,
		"a baseline already holding this delivery's echo proves nothing new")
	require.Equal(t, 1, pastes)
	require.Equal(t, 1, enters)
	require.Contains(t, warnings.String(), "withholding the remedy Enter")
}

// TestAltAnswerUncertainStaysStaged is R2's remainder: any #{alternate_on}
// answer but the literal "0" — tmux says something unparseable, or nothing —
// leaves the baseline unproven, and this dropped-prompt frame stays staged.
func TestAltAnswerUncertainStaysStaged(t *testing.T) {
	for _, answer := range []string{"maybe", ""} {
		t.Run("answer "+strconv.Quote(answer), func(t *testing.T) {
			warnings := captureWarningLog(t)
			alt := answer
			m := &stagedDraftPane{
				glyph: "›", echoGlyph: "›", placeholder: "Ask Codex to do anything",
				altAnswer: &alt, dropDraftOnEnter: true, height: 8,
				transcript: []string{"› " + codexEchoPrompt},
				render: func(string) []string {
					return []string{codexEchoPrompt, "draft 2", "draft 3", "draft 4",
						"draft 5", "draft 6", "draft 7", "draft 8"}
				},
			}
			status, _, enters := sendStaged(t, m, codexEchoPrompt)
			require.Equal(t, PromptSentUnverified, status,
				"an alternate_on answer of %q is not the literal \"0\" — no baseline", answer)
			require.Equal(t, 1, enters)
			require.Contains(t, warnings.String(), "withholding the remedy Enter")
		})
	}
}
