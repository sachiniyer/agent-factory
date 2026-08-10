package api

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
)

// #3169: a PARTIAL capture must never be indistinguishable from a COMPLETE one.
//
// `af sessions preview` captures the visible screen by default, which for any pane
// an agent has worked in is the composer and footer with the agent's turn scrolled
// just above. The capture was well-formed, plausible, and silent about the omission
// — an operator read one, concluded sessions were wedged, and archived working
// sessions on that reading.
//
// Asserted on the JSON envelope because that is what a scripted consumer sees; the
// human notice goes to stderr and is covered by previewPartialNotice's own tests.
func TestSessionsPreview_MarksAPartialCapture(t *testing.T) {
	setupRepoForCmd(t)
	payload := runPreviewWithSnapshot(t, daemon.PreviewResponse{
		Content: "composer", LinesAbove: 412, LinesAboveKnown: true,
	}, false, false)

	require.Equal(t, true, payload["partial"],
		"a visible-screen capture with scrollback above it must SAY it is partial")
	require.Equal(t, float64(412), payload["lines_above"],
		"and name how much was omitted, so the reader knows whether it matters")
}

// A measured ZERO means the visible screen IS the whole pane, so the capture is
// complete and must not be flagged. Crying partial on every preview would train
// operators to ignore the line, which puts us back where we started.
func TestSessionsPreview_DoesNotMarkACompleteCapture(t *testing.T) {
	setupRepoForCmd(t)
	payload := runPreviewWithSnapshot(t, daemon.PreviewResponse{
		Content: "everything", LinesAbove: 0, LinesAboveKnown: true,
	}, false, false)

	require.NotContains(t, payload, "partial",
		"nothing above the visible screen means the capture is complete")
	require.NotContains(t, payload, "lines_above",
		"a complete capture must return the envelope unchanged — {title, content} — so a consumer "+
			"decoding into map[string]string keeps working; lines_above:0 broke one to say nothing")
	require.Len(t, payload, 2, "exactly title and content")
}

// THE CASE THAT MATTERS MOST, and the one a single-field design gets wrong:
// UNMEASURED is not zero. A remote sandbox does not carry the count over its REST
// preview, so reporting unknown as "0 more lines above" would present a partial
// capture as complete — the bug wearing the fix's clothes.
func TestSessionsPreview_UnmeasuredScrollbackIsNotReportedAsNone(t *testing.T) {
	setupRepoForCmd(t)
	payload := runPreviewWithSnapshot(t, daemon.PreviewResponse{
		Content: "composer", LinesAboveKnown: false,
	}, false, false)

	require.Equal(t, true, payload["partial"],
		"an unmeasured capture is not a complete one; it must still be flagged")
	require.Equal(t, false, payload["lines_above_known"],
		"and must say the count is unknown rather than implying zero")
	require.NotContains(t, payload, "lines_above",
		"a count nobody measured must not appear at all — printing 0 is the fabricated negative")
}

// --full captures everything, so there is nothing above the region and no marker.
func TestSessionsPreview_FullCaptureIsNotMarkedPartial(t *testing.T) {
	setupRepoForCmd(t)
	payload := runPreviewWithSnapshot(t, daemon.PreviewResponse{
		Content: "all of it", LinesAbove: 412, LinesAboveKnown: true,
	}, true, false)

	require.NotContains(t, payload, "partial", "--full is the complete capture by definition")
	require.NotContains(t, payload, "lines_above")
}

// --plain removes the ANSI stripping every scripted consumer was writing by hand.
func TestSessionsPreview_PlainStripsANSI(t *testing.T) {
	setupRepoForCmd(t)
	const colored = "\x1b[31mred\x1b[0m plain \x1b[1mbold\x1b[0m"

	withAnsi := runPreviewWithSnapshot(t, daemon.PreviewResponse{
		Content: colored, LinesAboveKnown: true,
	}, true, false)
	require.Equal(t, colored, withAnsi["content"], "the default keeps escapes for a terminal")

	stripped := runPreviewWithSnapshot(t, daemon.PreviewResponse{
		Content: colored, LinesAboveKnown: true,
	}, true, true)
	require.Equal(t, "red plain bold", stripped["content"],
		"--plain must yield text a parser can read without reimplementing a stripper")
}

// runPreviewWithSnapshot drives the real preview command against a stubbed daemon
// response and returns its parsed JSON envelope.
func runPreviewWithSnapshot(t *testing.T, snapshot daemon.PreviewResponse, full, plain bool) map[string]any {
	t.Helper()
	prevFull, prevPlain := previewFullFlag, previewPlainFlag
	prevTab, prevName, prevID := previewTabFlag, previewTabNameFlag, previewTabIDFlag
	previewFullFlag, previewPlainFlag = full, plain
	previewTabFlag, previewTabNameFlag, previewTabIDFlag = 0, "", ""
	prevPreview := previewSessionViaDaemon
	previewSessionViaDaemon = func(daemon.PreviewRequest) (daemon.PreviewResponse, error) {
		return snapshot, nil
	}
	t.Cleanup(func() {
		previewFullFlag, previewPlainFlag = prevFull, prevPlain
		previewTabFlag, previewTabNameFlag, previewTabIDFlag = prevTab, prevName, prevID
		previewSessionViaDaemon = prevPreview
	})

	out, err := runCmdCaptureStdout(t, sessionsPreviewCmd, []string{"worker"})
	require.NoError(t, err)
	var payload map[string]any
	require.NoError(t, json.Unmarshal(out, &payload), "preview output is not JSON: %q", out)
	return payload
}
