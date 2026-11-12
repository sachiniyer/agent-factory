package tmux

import (
	"regexp"
	"strings"
)

// Gemini CLI 0.51.0's composer is a half-block frame, with no "╰" for af's #714
// glyph check to find (#7168):
//
//	▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄▄
//	 >   Type your message or @path/to/file
//	▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀▀
//	 workspace (/directory)   branch   sandbox   /model
//	 /home/dev/af-trees/wt    main     no sandbox Auto
//
// A composer on screen does not prove the pane is ready. Gemini opens its
// folder-trust dialog in an effect that runs after its first render, so in an
// untrusted folder the composer's first paint is replaced by the dialog 80–156
// ms later (measured: #7161's runs and #7162's). A prompt typed in that window
// goes into the dialog, whose picker commits an option on a digit.
//
// What proves it is a later render. React flushes a commit's mount effects
// before it starts any later render, so a frame that shows something Gemini
// paints only after its first render already reflects the dialog decision: if
// the folder were untrusted, the dialog would be on screen instead of the
// composer. Measured on every capture taken (11 runs, trusted and untrusted),
// the first composer frame carries neither anchor below, and no anchored
// composer ever preceded the dialog.
//
// The two anchors:
//   - the banner: the "Gemini CLI v<version>" row Gemini paints above the
//     composer (hidden by ui.hideBanner);
//   - the footer's branch column: the column header "branch", in the first row
//     below the composer, which appears once Gemini has read the worktree's
//     branch (hidden by ui.hideFooter).
//
// With both hidden there is nothing in one frame to read. task's readiness wait
// then falls back to seeing the composer on two polls far enough apart.
var geminiBannerVersion = regexp.MustCompile(`Gemini CLI v[0-9]+\.[0-9]+\.[0-9]+`)

// GeminiComposerFrame reports whether content shows Gemini's composer, and
// whether the same frame carries one of the later-render anchors described
// above. It does not consult the folder-trust dialog: readiness decides that
// first (a frame the dialog owns has no composer to find).
func GeminiComposerFrame(content string) (composer, anchored bool) {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	top := -1
	for i := range lines {
		if geminiComposerOpensAt(lines, i) {
			top = i
		}
	}
	if top < 0 {
		return false, false
	}
	plain := func(j int) string {
		return strings.TrimSpace(ansiCSISequence.ReplaceAllString(strings.TrimSuffix(lines[j], "\r"), ""))
	}

	// The banner counts only when it belongs to THIS composer's render: no
	// earlier composer may sit between them. A banner left on screen by a
	// process that ran in the pane before is followed by that process's own
	// composer, so it never anchors a fresh first paint.
	for j := top - 1; j >= 0; j-- {
		if geminiComposerOpensAt(lines, j) {
			break
		}
		if geminiBannerVersion.MatchString(plain(j)) {
			return true, true
		}
	}

	// The footer's header row is the first content below the composer's
	// bottom edge.
	bottom := top + 2
	for bottom < len(lines) && strings.Trim(plain(bottom), "▀") != "" {
		bottom++
	}
	for j := bottom + 1; j < len(lines); j++ {
		row := plain(j)
		if row == "" {
			continue
		}
		for _, field := range strings.Fields(row) {
			if field == "branch" {
				return true, true
			}
		}
		break
	}
	return true, false
}
