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
