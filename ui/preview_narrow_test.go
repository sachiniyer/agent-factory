package ui

import (
	"strings"
	"testing"

	xansi "github.com/charmbracelet/x/ansi"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/ui/layout"
)

// previewBodyLines returns the pane body rows of a rendered TabbedWindow: the
// lines between the frame's top border + header row and its bottom border, with
// the vertical frame characters and ANSI stripped. It is the "what the user sees
// in the preview body" projection the #1958 assertions check.
func previewBodyLines(rendered string) []string {
	all := strings.Split(rendered, "\n")
	if len(all) < 4 {
		return nil
	}
	// Row 0 is the top border, row 1 is the header, the last row is the bottom
	// border. Everything in between is the body.
	body := all[2 : len(all)-1]
	out := make([]string, 0, len(body))
	for _, line := range body {
		stripped := xansi.Strip(line)
		// Drop the frame border columns (│ … │) and surrounding whitespace so the
		// test sees only the pane content.
		stripped = strings.Trim(stripped, "│ ")
		out = append(out, stripped)
	}
	return out
}

func previewBodyHasContent(rendered string) bool {
	for _, line := range previewBodyLines(rendered) {
		if strings.TrimSpace(line) != "" {
			return true
		}
	}
	return false
}

// TestPreviewBodyNonEmptyWhenCaptureTallerThanPane is the #1958 regression: a
// non-agent tab's transient preview renders the daemon capture of that tab's
// tmux session. capture-pane returns the session's FULL screen height — the
// visible content (a shell prompt at the top) followed by blank padding rows it
// does NOT strip. When that captured session is TALLER than the preview pane —
// the common case, since a non-streamed tab's window keeps whatever taller size
// a prior stream pinned it to — the keep-newest truncation in TabPane.String()
// kept the trailing blank rows and dropped the real content off the top, so the
// body rendered EMPTY while the header stayed correct.
//
// The reporter measured this against terminal WIDTH (blank at 80x24 / 90x24,
// content at 100x30), but the driver is HEIGHT: only the 100x30 case gave the
// pane enough inner rows to keep the top content. The pane's inner height at a
// 24-row terminal is below the 25-row capture; at 30 rows it clears it. The body
// must show the content (top-aligned, possibly truncated) at every size, never
// go blank.
func TestPreviewBodyNonEmptyWhenCaptureTallerThanPane(t *testing.T) {
	log.Initialize(false)
	defer log.Close()

	// A shell-tab capture: the prompt on the first row, then blank padding rows
	// out to the (taller-than-the-pane) session height — exactly what
	// `tmux capture-pane -p` returns for an idle shell whose window is 25 rows.
	const marker = "PROMPT_TOP_MARKER$"
	capture := marker + strings.Repeat("\n", 24)

	cases := []struct {
		name string
		w, h int
	}{
		// The two sizes the reporter saw blank on current code (terminal height 24
		// → pane inner height ~21, below the 25-row capture).
		{"80x24", 80, 24},
		{"90x24", 90, 24},
		// The control: a 30-row terminal gives the pane enough rows to keep the top.
		{"100x30", 100, 30},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inst := startedWindowInstance(t, "alpha")
			w := newTestTabbedWindow()
			setWindowInstance(w, inst)
			w.SetRect(layout.Rect{W: tc.w, H: tc.h})
			// Enter the transient preview binding for the shell tab (index 1) so the
			// header renders "PREVIEW …" exactly as the reported repro does.
			w.SetPreview(inst, 1, "alpha · shell")
			// Publish the shell capture as the pane's content, as the daemon-backed
			// capture would on a refresh.
			w.tab.content = tabContentState{fallback: false, text: capture}

			rendered := w.View()

			require := func(cond bool, msg string) {
				t.Helper()
				if !cond {
					t.Errorf("%s\n--- rendered %dx%d ---\n%s", msg, tc.w, tc.h, rendered)
				}
			}
			// The header must always be correct — the bug never touched it.
			require(strings.Contains(rendered, "PREVIEW"), "preview header must render")
			require(previewBodyHasContent(rendered),
				"preview body must not be empty when the capture is taller than the pane")
			require(strings.Contains(xansi.Strip(rendered), marker),
				"the captured content (the prompt) must be visible in the body")
		})
	}
}
