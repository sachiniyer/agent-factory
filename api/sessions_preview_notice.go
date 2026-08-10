package api

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/daemon"
)

// previewPartialNotice renders the one line that makes a partial capture visible
// (#3169).
//
// `af sessions preview` captures the VISIBLE SCREEN by default. For any pane an
// agent has worked in that is the composer and the footer, with the agent's turn
// scrolled just above — so the capture is well-formed, plausible, and omits exactly
// what was wanted, with nothing saying it was partial. An operator read one, decided
// several sessions were wedged, and archived working sessions on that reading.
//
// The property this exists to hold: A PARTIAL CAPTURE MUST NEVER BE
// INDISTINGUISHABLE FROM A COMPLETE ONE. This is the repo's
// failed-read-is-not-an-empty-result class applied to its primary fleet-debugging
// verb, so the fix is to break the silence rather than to enlarge the default —
// a full capture per preview has its own cost on a busy fleet.
//
// Three answers, and keeping them three is the design:
//
//   - full capture, or a measured zero: no notice. Nothing is above the region, so
//     the capture IS complete and saying otherwise would train operators to ignore
//     the line.
//   - measured N > 0: name N and the flag that shows it.
//   - UNMEASURED: say that, and never "0 more lines above". A remote sandbox does
//     not carry the count over its REST preview, so unknown is a real state — and
//     rendering it as zero would report a partial capture as complete, which is the
//     bug itself wearing the fix's clothes.
//
// It goes to STDERR so `af sessions preview ... | jq` keeps working: the JSON
// envelope carries the same facts as fields, and a notice on stdout would corrupt
// every scripted consumer to help the human ones.
func previewPartialNotice(snapshot daemon.PreviewResponse, full bool) string {
	if full {
		return ""
	}
	if !snapshot.LinesAboveKnown {
		return "af: showing the visible screen; this pane's scrollback was not measured — use --full to capture everything"
	}
	if snapshot.LinesAbove <= 0 {
		return ""
	}
	return fmt.Sprintf(
		"af: showing the visible screen; %s above — use --full to capture everything",
		pluralLines(snapshot.LinesAbove))
}

// pluralLines keeps the notice grammatical at 1, since a one-line omission is the
// case most likely to be dismissed as noise.
func pluralLines(n int) string {
	if n == 1 {
		return "1 more line"
	}
	return fmt.Sprintf("%d more lines", n)
}
