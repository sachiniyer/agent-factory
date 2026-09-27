package tmux

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// statusRowQuotingPrompt stages the #4885 play-test scenario 6 shape on the
// #4884 sliding pane: a status row that flips idle -> busy when the paste
// arrives and then stays put, and a prompt that begins with the busy row's
// text, so the row carries the prompt's render witness below the composer.
func statusRowQuotingPrompt(t *testing.T) (*slidingClaudePane, string) {
	t.Helper()
	const busyRow = "  LANESMONITOR-HEARTBEAT-STATUS state=busy"
	prompt := strings.TrimSpace(busyRow) + " is what the status row says; " + heartbeatPrompt
	require.Contains(t, normalizeDelivery(busyRow), newDeliveryProbe(prompt).renderWitness,
		"fixture: the prompt's render witness must appear in the changed status row")

	pane := newAlignedPane(t, prompt)
	pane.footer = "  LANESMONITOR-HEARTBEAT-STATUS state=idle"
	pane.footerAfterPaste = busyRow
	return pane, prompt
}

// TestPromptQuotingChangedStatusRowIsReportedDelivered is #4934 part 2. The
// paste lands whole and submits once, but the changed status row wins the
// newest-render inference, so master reads the whole composer render as cut
// short and, after the pre-retry check correctly withholds the retry, reports
// sent-unverified for a prompt the agent received. A prompt that arrived
// exactly once must end as delivered.
func TestPromptQuotingChangedStatusRowIsReportedDelivered(t *testing.T) {
	defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()
	errors := captureErrorLog(t)

	pane, prompt := statusRowQuotingPrompt(t)
	pane.onPaste = func(_ int, payload string) []string { return wrapClaude(payload) }
	submitted := 0
	pane.onEnter = func(p *slidingClaudePane, _ int) {
		submitted++
		p.submitComposerAs(strings.Join(p.composer, " "), "", "✻ Crunching… (esc to interrupt)")
	}
	probe := newDeliveryProbe(prompt)
	idle := normalizeDelivery(pane.frame())
	require.Equal(t, 2, strings.Count(idle, probe.completion),
		"fixture: the idle frame holds the older copy's tail row and the newer copy")

	session := newTmuxSession("af_proj", ProgramClaude, NewMockPtyFactory(t), pane.exec())
	status, err := session.SendKeysCommandObserved(prompt)
	require.NoError(t, err)

	pastes, _ := pane.counts()
	require.Equal(t, 1, submitted, "the agent must receive the prompt exactly once")
	require.Equal(t, 1, pastes, "a prompt that submitted must never be pasted again")
	require.Equal(t, PromptDelivered, status,
		"the whole prompt newly rendered in the composer: that is a landed paste, whatever the status row says")
	require.NotContains(t, errors.String(), "prompt delivery observed absent")
}

// TestStrandUnderChangedStatusRowIsRedeliveredOnce is the counterpart that
// keeps #3293: the same status row and prompt, but the first paste strands
// truncated and its Enter is absorbed. The only whole copies on screen are the
// older transcript echoes, one of which scrolls off as the strand grows the
// composer, so no fresh whole copy exists and the strand must still read as
// absent and be redelivered exactly once.
func TestStrandUnderChangedStatusRowIsRedeliveredOnce(t *testing.T) {
	defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()

	pane, prompt := statusRowQuotingPrompt(t)
	pane.onPaste = func(n int, payload string) []string {
		if n == 1 {
			return wrapClaude(payload)[:4]
		}
		return wrapClaude(payload)
	}
	submitted := 0
	pane.onEnter = func(p *slidingClaudePane, n int) {
		if n == 1 {
			p.composer = append(p.composer, "  ") // absorbed as a newline
			return
		}
		submitted++
		p.submitComposerAs(strings.Join(p.composer, " "), "", "✻ Crunching… (esc to interrupt)")
	}
	session := newTmuxSession("af_proj", ProgramClaude, NewMockPtyFactory(t), pane.exec())

	status, err := session.SendKeysCommandObserved(prompt)
	require.NoError(t, err)

	pastes, enters := pane.counts()
	require.Equal(t, 2, pastes, "a proven-absent strand must be redelivered exactly once")
	require.Equal(t, 2, enters)
	require.Equal(t, 1, submitted, "the agent must receive the prompt exactly once")
	require.Equal(t, PromptDelivered, status)
}

// TestFreshWholeRenderCountsOnlyProvenScrollOff pins the conservative side of
// the scroll adjustment: baseline copies are subtracted only when the frame's
// top is placed in the baseline by a run at least a payload long. A top that
// cannot be placed falls back to the plain count, so an old copy that did not
// scroll off can never read as fresh.
func TestFreshWholeRenderCountsOnlyProvenScrollOff(t *testing.T) {
	const (
		payload    = "the prompt the agent was sent, long enough to be distinctive"
		transcript = "an earlier reply line that fills the transcript above the prompt. "
		reply      = "a reply to that prompt, running on for longer than the prompt did. "
		footer     = "FOOTER"
	)
	probe := newDeliveryProbe(payload).withBaseline(transcript + payload + reply + footer)

	// Scrolled, with only a working line drawn: the old copy is still on screen
	// and nothing rendered a new one.
	working := normalizeDelivery(transcript[20:] + payload + reply + "✻ Working " + footer)
	require.Equal(t, 1, strings.Count(working, normalizeDelivery(payload)))
	require.False(t, probe.freshWholeRender(working))

	// The same frame under a header that did not exist at baseline: the top
	// cannot be placed, so no baseline copy may be discounted.
	redrawn := normalizeDelivery("CLOCK 12:01 " + payload + reply + "✻ Working " + footer)
	require.Zero(t, probe.scrolledOff(redrawn))
	require.False(t, probe.freshWholeRender(redrawn))

	// The old copy's opening scrolled off and a new copy rendered in the
	// composer: fresh, though the count is flat.
	slid := normalizeDelivery(payload[10:] + reply + payload + footer)
	require.Equal(t, 1, strings.Count(slid, normalizeDelivery(payload)))
	require.True(t, probe.freshWholeRender(slid))

	// The same slide with the new copy cut short is a strand, not a render.
	strand := normalizeDelivery(payload[10:] + reply + payload[:30] + footer)
	require.False(t, probe.freshWholeRender(strand))
}

// TestShortPayloadInUnrelatedRowIsNotAFreshRender is the Codex review
// fail-first on #4943. The payload is short and common ("ok"), an older copy of
// it sits on the top row, and the paste renders only a collapsed placeholder:
// the composer's growth scrolls the old copy off while a status row that
// changed at paste time happens to read "ok". Nothing in the pane rendered the
// paste, so the observation must not read as landed. A fresh whole copy is
// render evidence only for a payload distinctive enough to carry a render
// witness; short text anywhere on screen proves nothing.
func TestShortPayloadInUnrelatedRowIsNotAFreshRender(t *testing.T) {
	defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()

	const prompt = "ok"
	pane := &slidingClaudePane{
		height:           8,
		transcript:       []string{"ok", "● the previous turn finished", "  with nothing further to add"},
		footer:           "  state: idle",
		footerAfterPaste: "  ok",
	}
	pane.onPaste = func(int, string) []string {
		return []string{"❯ [Pasted text #1 +0 lines]", "  "}
	}
	pane.onEnter = func(*slidingClaudePane, int) {}
	baseline := normalizeDelivery(pane.frame())
	require.Equal(t, 1, strings.Count(baseline, prompt), "fixture: one old copy, on the top row")
	pane.composer = pane.onPaste(0, prompt)
	pane.footer = pane.footerAfterPaste
	require.Equal(t, 1, strings.Count(normalizeDelivery(pane.frame()), prompt),
		"fixture: the old copy scrolls off as the status row draws one, so the count is flat")
	pane.composer, pane.footer = nil, "  state: idle"

	session := newTmuxSession("af_proj", ProgramClaude, NewMockPtyFactory(t), pane.exec())
	status, err := session.SendKeysCommandObserved(prompt)
	require.NoError(t, err)

	pastes, _ := pane.counts()
	require.Equal(t, 1, pastes)
	require.NotEqual(t, PromptDelivered, status,
		"a status row that happens to read the short payload is not a render of the paste")
}

// TestLowEntropyPayloadInUnrelatedRowIsNotAFreshRender is the second-round
// Codex fail-first on #4943. The payload is long enough to carry a render
// witness but is only hyphens, text any divider can draw. Its old copy scrolls
// off as a collapsed placeholder grows the composer, and a status row that
// changes at paste time draws the same hyphens, so every count stays flat.
// The paste never rendered, so it must not read as landed.
func TestLowEntropyPayloadInUnrelatedRowIsNotAFreshRender(t *testing.T) {
	defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()

	prompt := strings.Repeat("-", 40)
	require.NotEmpty(t, newDeliveryProbe(prompt).renderWitness, "fixture: the payload carries a witness")
	pane := &slidingClaudePane{
		height: 10,
		transcript: []string{
			"❯ " + prompt,
			"● the previous turn finished and wrote its reply",
			"  across a few rows of ordinary transcript text",
			"  so the top of the frame can be placed again",
			"  in the baseline after the composer scrolls it",
		},
		footer:           "  state: idle",
		footerAfterPaste: "  " + prompt,
	}
	pane.onPaste = func(int, string) []string { return []string{"❯ [Pasted text #1 +0 lines]", "  "} }
	pane.onEnter = func(*slidingClaudePane, int) {}
	probe := newDeliveryProbe(prompt).withBaseline(pane.frame())
	pane.composer, pane.footer = pane.onPaste(0, prompt), pane.footerAfterPaste
	after := normalizeDelivery(pane.frame())
	require.Equal(t, 1, strings.Count(after, normalizeDelivery(prompt)), "fixture: old copy gone, status row copy in")
	require.Equal(t, probe.renderWitnessBaseline, strings.Count(after, probe.renderWitness), "fixture: witness count flat")
	pane.composer, pane.footer = nil, "  state: idle"

	session := newTmuxSession("af_proj", ProgramClaude, NewMockPtyFactory(t), pane.exec())
	status, err := session.SendKeysCommandObserved(prompt)
	require.NoError(t, err)
	require.NotEqual(t, PromptDelivered, status,
		"a divider that happens to read the payload is not a render of the paste")
}

// TestFreshRenderIsFoundWhenLittleTranscriptSurvives is the second-round Codex
// fail-first on #4943. The scroll carries the old copy wholly off and leaves
// only a few short transcript rows above the new composer, far fewer bytes than
// the prompt. Those rows still place the frame's top in the baseline, and the
// prompt that arrived once must end as delivered.
func TestFreshRenderIsFoundWhenLittleTranscriptSurvives(t *testing.T) {
	defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()

	pane, prompt := statusRowQuotingPrompt(t)
	old := wrapClaude(prompt)
	short := []string{"● ok, checked the lanes again", "  nothing new since the last", "  heartbeat went through"}
	pane.transcript = append(append([]string{}, old...), short...)
	pane.height = len(pane.transcript) + 5
	pane.onPaste = func(_ int, payload string) []string { return append(wrapClaude(payload), "  ") }
	submitted := 0
	pane.onEnter = func(p *slidingClaudePane, _ int) {
		submitted++
		p.submitComposerAs(strings.Join(p.composer, " "), "", "✻ Crunching… (esc to interrupt)")
	}
	kept := len(normalizeDelivery(strings.Join(short, "")))
	require.Less(t, kept, len(normalizeDelivery(prompt)), "fixture: fewer surviving bytes than the prompt")

	session := newTmuxSession("af_proj", ProgramClaude, NewMockPtyFactory(t), pane.exec())
	status, err := session.SendKeysCommandObserved(prompt)
	require.NoError(t, err)

	pastes, _ := pane.counts()
	require.Equal(t, 1, submitted, "the agent must receive the prompt exactly once")
	require.Equal(t, 1, pastes)
	require.Equal(t, PromptDelivered, status)
}

// TestFreshRenderIgnoresPayloadInPersistentChrome is the third second-round
// Codex fail-first on #4943. A persistent row under the status row shows the
// whole prompt and never changes. It is chrome, so the frame side of the count
// leaves it out, and the baseline side has to leave it out too, or the old copy
// it stands in for hides the fresh composer render.
func TestFreshRenderIgnoresPayloadInPersistentChrome(t *testing.T) {
	defer withPasteDeliveryTiming(30*time.Millisecond, time.Millisecond)()

	pane, prompt := statusRowQuotingPrompt(t)
	persistent := "  last sent: " + prompt
	pane.footer += "\n" + persistent
	pane.footerAfterPaste += "\n" + persistent
	pane.onPaste = func(_ int, payload string) []string { return wrapClaude(payload) }
	submitted := 0
	pane.onEnter = func(p *slidingClaudePane, _ int) {
		submitted++
		p.submitComposerAs(strings.Join(p.composer, " "), "", "✻ Crunching… (esc to interrupt)")
	}

	session := newTmuxSession("af_proj", ProgramClaude, NewMockPtyFactory(t), pane.exec())
	status, err := session.SendKeysCommandObserved(prompt)
	require.NoError(t, err)

	pastes, _ := pane.counts()
	require.Equal(t, 1, submitted, "the agent must receive the prompt exactly once")
	require.Equal(t, 1, pastes)
	require.Equal(t, PromptDelivered, status)
}
