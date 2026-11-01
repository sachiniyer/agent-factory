package tree

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui/theme"
)

func attentionInstance(t *testing.T, status session.Status, reason string) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "feature", Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)
	inst.Branch = "af-feature"
	inst.SetStatusForTest(status)
	_, err = inst.SetAttention(reason, time.Now())
	require.NoError(t, err)
	require.NotNil(t, inst.Attention())
	return inst
}

// TestInstanceRendererAttentionGlyphAndReason pins #5461's TUI half: a live row
// carrying an attention request shows the static bang in the generated
// attention role and the reason leading its detail line — the web's
// blinking-dot + "Needs attention: <reason>" pair, translated.
func TestInstanceRendererAttentionGlyphAndReason(t *testing.T) {
	inst := attentionInstance(t, session.Ready, "which file should I edit?")

	r := NewInstanceRenderer()
	r.SetWidth(120)
	out := ansiEscape.ReplaceAllString(r.Render(inst, 1, true, false, false), "")
	assert.Contains(t, out, attentionGlyph,
		"a live attention row must carry the static bang glyph")
	assert.NotContains(t, out, strings.TrimSpace(readyIcon),
		"the bang replaces the ready dot until the request clears")
	assert.Contains(t, out, "needs attention: which file should I edit? · af-feature",
		"the reason leads the detail line and the branch trails it")
	assert.NotRegexp(t, brailleFrame, out,
		"the marker is static — no animated frame may appear (#1766 discipline)")
}

// TestInstanceRendererAttentionColor pins the glyph to the generated attention
// role — the same token the web's dot draws — not the dead/lost ambers that
// would read the request as breakage.
func TestInstanceRendererAttentionColor(t *testing.T) {
	assert.Equal(t, theme.Roles().Attention, attentionStyle.GetForeground(),
		"the bang must draw in theme.Roles().Attention")
}

// TestInstanceRendererAttentionReasonEmpty covers a bare `attention --self`:
// with no reason the detail still names the state rather than rendering a
// dangling colon.
func TestInstanceRendererAttentionReasonEmpty(t *testing.T) {
	inst := attentionInstance(t, session.Running, "")

	r := NewInstanceRenderer()
	r.SetWidth(120)
	out := ansiEscape.ReplaceAllString(r.Render(inst, 1, true, false, false), "")
	assert.Contains(t, out, "needs attention")
	assert.NotContains(t, out, "needs attention:")
}

// TestInstanceRendererAttentionCollapsedRowKeepsGlyphOnly mirrors the web's
// information hierarchy: every row carries the marker, but the reason waits
// for the row's detail to show — on a collapsed, unselected row the request
// stays one line, the same place the web keeps it to a tooltip.
func TestInstanceRendererAttentionCollapsedRowKeepsGlyphOnly(t *testing.T) {
	inst := attentionInstance(t, session.Ready, "which file should I edit?")

	r := NewInstanceRenderer()
	r.SetWidth(120)
	out := ansiEscape.ReplaceAllString(r.Render(inst, 1, false, false, false), "")
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	assert.Len(t, lines, 1, "a collapsed attention row stays one line")
	assert.Contains(t, lines[0], attentionGlyph,
		"the bang still marks the collapsed row")
	assert.NotContains(t, out, "which file",
		"the reason waits for the row's detail")
}

// TestInstanceRendererAttentionClearsOnTerminalRows pins the daemon's clear
// rule reaching the rail: a liveness edge to a non-live state drops the
// request (session.noteAttentionLivenessLocked), so the ◆ limit glyph — not
// the bang — renders.
func TestInstanceRendererAttentionClearsOnTerminalRows(t *testing.T) {
	inst := attentionInstance(t, session.Ready, "stale question")
	inst.SetLimitReached(time.Time{})
	require.Nil(t, inst.Attention(),
		"the liveness edge must drop a request on a non-live row")

	r := NewInstanceRenderer()
	r.SetWidth(120)
	out := ansiEscape.ReplaceAllString(r.Render(inst, 1, true, false, false), "")
	assert.NotContains(t, out, attentionGlyph)
	assert.NotContains(t, out, "needs attention")
	assert.Contains(t, out, strings.TrimSpace(limitIcon),
		"the limit glyph renders instead")
}

// TestInstanceRendererAttentionOutranksIdleDetail pins the priority the web's
// operatorKind encodes: the agent's explicit ask tops the ordinary idle
// detail, so a ready row with both shows the request, not the churn report.
func TestInstanceRendererAttentionOutranksIdleDetail(t *testing.T) {
	inst := attentionInstance(t, session.Ready, "which file should I edit?")
	require.True(t, inst.RecordPromptAttempt(session.PromptDelivered, time.Now().Add(-time.Hour)))

	r := NewInstanceRenderer()
	r.SetWidth(160)
	out := ansiEscape.ReplaceAllString(r.Render(inst, 1, true, false, false), "")
	assert.Contains(t, out, "needs attention: which file should I edit? · af-feature")
	assert.NotContains(t, out, "no change after delivery",
		"the request outranks the mechanical idle detail")
}

// TestInstanceRendererAttentionTrailsOutageDetail: on an unreachable row the
// outage leads because it explains why the request may be stale — but the
// reported reason stays on the line rather than hiding until the row re-probes.
func TestInstanceRendererAttentionTrailsOutageDetail(t *testing.T) {
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "feature", Path: t.TempDir(), Program: "test",
	})
	require.NoError(t, err)
	inst.Branch = "af-feature"
	inst.SetBackend(&session.HookBackend{})
	inst.SetStartedForTest(true)
	require.NoError(t, inst.Transition(session.ObserveLiveness(session.LiveReady)))
	inst.SetUnreachable(errors.New("ssh: connection reset"))
	_, err = inst.SetAttention("which file?", time.Now())
	require.NoError(t, err)

	r := NewInstanceRenderer()
	r.SetWidth(160)
	out := ansiEscape.ReplaceAllString(r.Render(inst, 1, true, false, false), "")
	assert.Contains(t, out, "unreachable <1m ago · ssh: connection reset · needs attention: which file? · af-feature")
}

// TestNeedsYouMarksShareOneRendering pins one visual language for the rail's two
// needs-you marks (#6510, #6475): a live attention request draws "!", and an
// archived item session waiting on an answer draws "?" in place of ▧. Both go
// through needsYouMark, so both carry the attention role's foreground escape.
func TestNeedsYouMarksShareOneRendering(t *testing.T) {
	prevProfile := lipgloss.ColorProfile()
	prevDark := lipgloss.HasDarkBackground()
	lipgloss.SetColorProfile(termenv.TrueColor)
	lipgloss.SetHasDarkBackground(true)
	t.Cleanup(func() {
		lipgloss.SetColorProfile(prevProfile)
		lipgloss.SetHasDarkBackground(prevDark)
	})

	waiting, err := session.NewInstance(session.InstanceOptions{Title: "B-7 pick a db", Path: t.TempDir(), Program: "test", BacklogItem: 7})
	require.NoError(t, err)
	waiting.SetStatusForTest(session.Archived)
	waiting.MarkArchivedForAnswer("ask", time.Now())
	live := attentionInstance(t, session.Ready, "which file?")

	r := NewInstanceRenderer()
	r.SetWidth(120)
	waitingRow := r.Render(waiting, 1, false, false, false)
	liveRow := r.Render(live, 2, false, false, false)
	assert.Contains(t, waitingRow, needsYouMark(AwaitingAnswerGlyph), "the waiting row draws ? through needsYouMark")
	assert.Contains(t, liveRow, needsYouMark(attentionGlyph), "the attention row draws ! through needsYouMark")

	fg := termenv.RGBColor(theme.Colors()["attention"].Dark).Sequence(false)
	require.Contains(t, needsYouMark(AwaitingAnswerGlyph), fg, "the mark carries the attention role's foreground")
	plain := ansiEscape.ReplaceAllString(waitingRow, "")
	assert.NotContains(t, plain, strings.TrimSpace(archivedIcon), "? takes ▧'s place")
	assert.NotContains(t, plain, attentionGlyph, "an awaiting-answer row is not an attention request")
}
