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
//
// The verdict for these frames stays sent-unverified: a "› " echo row above
// an idle composer is exactly what a dropped prompt also shows (#4200), and
// pane text has never proven a positive consumption — four review rounds each
// found a frame that faked it (#7802). Every test below is the staged
// direction: what must keep the sent-unverified verdict and its honest
// reason.

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

// boundaryBlockForTest reduces a boundary fixture to the composer block the
// way the submit remedy does: deliveryBoundaryBlock over the visible grid,
// deriving the wide composer anchor and the block's first row from where the
// bound render provably sat.
func boundaryBlockForTest(boundary string, probe deliveryProbe, bound stagedEvidence) boundaryBlock {
	return deliveryBoundaryBlock(boundary, probe, bound)
}

func codexBoundLiteral(t *testing.T, boundary string) (deliveryProbe, stagedEvidence, boundaryBlock) {
	return boundDraftFixture(t, boundary, codexEchoPrompt)
}

// boundDraftFixture builds the literal-tail bound evidence and the composer
// block for a boundary fixture holding prompt's draft in the live composer.
func boundDraftFixture(t *testing.T, boundary, prompt string) (deliveryProbe, stagedEvidence, boundaryBlock) {
	t.Helper()
	probe := newDeliveryProbe(prompt)
	bound := stagedEvidence{tail: true}
	block := boundaryBlockForTest(boundary, probe, bound)
	require.GreaterOrEqual(t, block.anchor, 0, "fixture: the boundary has a composer glyph row")
	require.True(t, boundStillStaged(boundary, probe, bound, block),
		"fixture: the draft sitting in the composer at the Enter boundary reads as staged")
	return probe, bound, block
}

// TestCodexAmbiguousEchoFramesStayStaged: every one of these must stay staged
// or unverified, because none carries evidence the draft left the live
// composer:
//
//   - A dropped prompt shows the same idle composer as a submitted one whose
//     echo the frame cannot attribute (#4200).
//   - A watcher that resends identical text leaves an earlier echo — the
//     bound text at or below the composer block's first row still reads
//     staged.
//   - An echo above a composer that still holds the draft is a staged draft
//     with its echo.
//   - A status/rule row between the echo and the composer is Codex's own
//     layout; it must neither hide the composer nor the bound text.
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
		// resending watch leaves. The echo row is the composer block's
		// earliest anchor, so the bound text below it keeps the literal-tail
		// reading.
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
			probe, bound, block := codexBoundLiteral(t, frames.boundary)
			reason := stagedReading(frames.grace, probe, bound, block)
			require.Contains(t, reason, frames.wantReason)
			require.True(t, boundStillStaged(frames.grace, probe, bound, block))
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
	probe, bound, block := codexBoundLiteral(t, boundary)
	grace := codexLanePane([]string{"⠋ Working · 3m"}, "» "+codexEchoPrompt)
	reason := stagedReading(grace, probe, bound, block)
	require.Contains(t, reason, "still in the live composer")
}

// TestGlyphLeadingDraftLineKeepsStagedReading is #7802 R1: the bound draft's
// own lines can lead with a composer glyph — "Explain this:\n» quote" puts a
// "»"-leading continuation row inside a "›" (or "❯"/"❭") composer. The wider
// staged anchor lands on that row, INSIDE the draft, and the draft's head
// sits above it. On the reviewed head the literal-tail check searched only
// below the last glyph row, missed the head, and read the still-staged draft
// as delivered. Bound evidence at or below the composer block's first row —
// the earliest of the narrow and wide anchors — keeps the staged reading.
func TestGlyphLeadingDraftLineKeepsStagedReading(t *testing.T) {
	const prompt = "Explain this:\n» quote"
	for _, glyph := range []string{"›", "❯", "❭"} {
		t.Run("composer "+glyph, func(t *testing.T) {
			composer := []string{glyph + " Explain this:", "  » quote"}
			boundary := codexLanePane([]string{"✻ Ran tool"}, composer...)
			probe, bound, block := boundDraftFixture(t, boundary, prompt)
			// The Enter was absorbed: the grace frame is the same pane.
			reason := stagedReading(boundary, probe, bound, block)
			require.Contains(t, reason, "still in the live composer")
			require.True(t, boundStillStaged(boundary, probe, bound, block))
		})
	}
}

// TestNewlineLeadingDraftComposerStaysStaged is Codex thread 4666233827's
// staged-direction half: a draft that opens with a newline leaves a bare
// glyph first row while its text sits below it. The composer block's first
// row is the bare glyph, and the bound text below it keeps the staged
// reading — a first-row-only emptiness check would miss the draft entirely.
func TestNewlineLeadingDraftComposerStaysStaged(t *testing.T) {
	boundary := codexLanePane([]string{"› " + codexEchoPrompt}, "› ", "  "+codexEchoPrompt)
	probe, bound, block := codexBoundLiteral(t, boundary)
	grace := codexLanePane([]string{
		"› " + codexEchoPrompt,
		"› " + codexEchoPrompt,
	}, "› ", "  "+codexEchoPrompt)
	reason := stagedReading(grace, probe, bound, block)
	require.NotEmpty(t, reason,
		"a bare glyph first row with the draft still below it keeps the staged verdict")
	require.Contains(t, reason, "still in the live composer")
}

// TestDevinIdlePlaceholderRepaintNamesTheIdleReading is #7801's residual 2:
// Devin's "Starting session…" frame holds only the idle placeholder — no echo
// of this prompt above the live composer — so the staged reading is CORRECT
// (a dropped prompt shows the same frame); the warning just has to say what
// it saw. Master names it "repainted holding content".
func TestDevinIdlePlaceholderRepaintNamesTheIdleReading(t *testing.T) {
	boundary := devinLanePane([]string{"one"}, placeholderDraft("❭")...)
	probe, bound, block := boundLiteral(t, boundary)
	grace := devinLanePane([]string{"one", "SWE-2 Max · bypass Starting session…"},
		"❭ Ask Devin to build features, fix bugs, or work on your code")
	reason := stagedReading(grace, probe, bound, block)
	require.Contains(t, reason, "idle placeholder")
	require.True(t, boundStillStaged(grace, probe, bound, block))
}

// TestUltraCodexSubmittedDraftStaysUnverified is #7801's residual warning,
// honestly named: a literal paste lands in the "»" composer, Enter submits
// it, the agent echoes it "› " and repaints "» Ask Codex to do anything".
// The echo row is the composer block's earliest anchor, so the bound text
// keeps the literal-tail staged reading — sent-unverified, one Enter, and
// the warning names what it saw rather than a bare "still staged".
func TestUltraCodexSubmittedDraftStaysUnverified(t *testing.T) {
	warnings := captureWarningLog(t)
	m := &stagedDraftPane{
		glyph: "»", echoGlyph: "›", placeholder: "Ask Codex to do anything", render: literalRender,
		transcript: []string{"› tidy the sidebar before the next sweep"},
	}
	status, pastes, enters := sendStaged(t, m, codexEchoPrompt)
	require.Equal(t, PromptSentUnverified, status,
		"an unattributable echo keeps the staged verdict — pane text cannot prove consumption")
	require.Equal(t, 1, pastes)
	require.Equal(t, 1, enters, "a staged reading never earns a remedy Enter")
	require.Contains(t, warnings.String(), "withholding the remedy Enter")
	require.Contains(t, warnings.String(), "still in the live composer")
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

// composerDraftRows renders prompt's lines the way a composer does: the first
// line under the prompt glyph, each continuation line indented — including a
// line that is itself a lone glyph, which normalizes to a composer-glyph row
// inside the draft (#7809).
func composerDraftRows(glyph, prompt string) []string {
	lines := strings.Split(prompt, "\n")
	rows := []string{glyph + " " + lines[0]}
	for _, line := range lines[1:] {
		rows = append(rows, "  "+line)
	}
	return rows
}

// TestLoneGlyphDraftLineKeepsStagedReading is #7809: the bound draft's own
// continuation line can lead with a NARROW-set glyph — a lone "›" or ">" — so
// the pane's last composer-glyph row lands INSIDE the draft and the head sits
// above the search region unread. Master's block start read that still-staged
// draft as delivered. The composer block's first row is where the boundary
// frame proves the draft's render begins — found by content, never by glyph
// shape — so the head keeps the staged reading in every composer the narrow
// set covers, in the Ultra "»" composer, and whether the glyph line ends the
// draft or a blank row precedes it.
func TestLoneGlyphDraftLineKeepsStagedReading(t *testing.T) {
	for name, tc := range map[string]struct {
		glyph  string
		prompt string
	}{
		"lone › line in › composer":             {"›", "check the reported stall\n›"},
		"lone › line in ❯ composer":             {"❯", "check the reported stall\n›"},
		"lone › line in ❭ composer":             {"❭", "check the reported stall\n›"},
		"lone > line in › composer":             {"›", "check the reported stall\n>"},
		"lone > line in ❯ composer":             {"❯", "check the reported stall\n>"},
		"lone > line in ❭ composer":             {"❭", "check the reported stall\n>"},
		"blank row before the glyph line":       {"›", "check the stall\n\n›"},
		"draft whose last line is a lone glyph": {"›", "check the stall\nand its neighbor\n›"},
		"lone › line in the Ultra » composer":   {"»", "check the reported stall\n›"},
	} {
		t.Run(name, func(t *testing.T) {
			boundary := codexLanePane([]string{"✻ Ran tool"}, composerDraftRows(tc.glyph, tc.prompt)...)
			probe, bound, block := boundDraftFixture(t, boundary, tc.prompt)
			// The Enter was absorbed: the grace frame is the same pane.
			reason := stagedReading(boundary, probe, bound, block)
			require.Contains(t, reason, "still in the live composer")
			require.True(t, boundStillStaged(boundary, probe, bound, block))
		})
	}
}

// TestSubmittedDraftRepaintedBelowReadsDelivered is the control: a draft that
// really submitted leaves the composer repainted bare BELOW the boundary's
// anchor row, its echo in the transcript above. The block's first row moved
// down with the anchor, so the echo stays transcript and the staged reading
// keeps master's verdict: not staged.
func TestSubmittedDraftRepaintedBelowReadsDelivered(t *testing.T) {
	const prompt = "check the reported stall\n›"
	boundary := codexLanePane([]string{"✻ Ran tool"}, composerDraftRows("›", prompt)...)
	probe, bound, block := boundDraftFixture(t, boundary, prompt)
	grace := codexLanePane([]string{
		"✻ Ran tool",
		"› check the reported stall ›",
		"",
	}, "›")
	require.Equal(t, "", stagedReading(grace, probe, bound, block),
		"a draft repainted bare below the boundary row reads as it does on master")
	require.False(t, boundStillStaged(grace, probe, bound, block))
}

// TestLoneGlyphLineStrandStillWarnsAndWithholds drives #7809 end to end: the
// draft's own continuation line is a lone narrow-set glyph, the pane's last
// composer-glyph row lands INSIDE the draft, and the absorbed Enter leaves
// the whole draft staged. Master read that pane delivered; it must warn
// sent-unverified and authorize no remedy Enter.
func TestLoneGlyphLineStrandStillWarnsAndWithholds(t *testing.T) {
	splitRender := func(payload string) []string { return strings.Split(payload, "\n") }
	for name, tc := range map[string]struct {
		glyph  string
		prompt string
	}{
		"lone › line in › composer":             {"›", "check the reported stall\n›"},
		"lone › line in ❯ composer":             {"❯", "check the reported stall\n›"},
		"lone › line in ❭ composer":             {"❭", "check the reported stall\n›"},
		"lone > line in › composer":             {"›", "check the reported stall\n>"},
		"lone > line in ❯ composer":             {"❯", "check the reported stall\n>"},
		"lone > line in ❭ composer":             {"❭", "check the reported stall\n>"},
		"blank row before the glyph line":       {"›", "check the stall\n\n›"},
		"draft whose last line is a lone glyph": {"›", "check the stall\nand its neighbor\n›"},
		"lone › line in the Ultra » composer":   {"»", "check the reported stall\n›"},
	} {
		t.Run(name, func(t *testing.T) {
			warnings := captureWarningLog(t)
			m := &stagedDraftPane{glyph: tc.glyph, swallowedEnters: 1, render: splitRender}
			status, pastes, enters := sendStaged(t, m, tc.prompt)
			require.Equal(t, PromptSentUnverified, status,
				"a draft whose own line is a lone glyph must never read as delivered")
			require.Equal(t, 1, pastes)
			require.Equal(t, 1, enters, "a staged draft authorizes no remedy Enter")
			require.Contains(t, warnings.String(), "withholding the remedy Enter")
			require.Contains(t, warnings.String(), "still in the live composer")
		})
	}
}
