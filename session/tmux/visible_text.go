package tmux

import (
	"regexp"
	"strings"
)

// visibleText is one pane capture as a person looking at the pane reads it:
// every escape sequence `capture-pane -e` emits removed, CRLF line ends folded
// to LF. The Claude launch-gate clauses read this, never the raw capture
// (#7271).
//
// Claude Code styles its dialogs word by word and links words with OSC 8, so
// the raw bytes of a fully painted dialog carry escapes between the words of
// every phrase a clause matches on, and a clause reading them reports the
// dialog absent: task.DismissTrustPrompt then types the user's prompt into the
// modal (#7265 closed that for SGR). The clauses used to strip for themselves,
// some per row and some per capture, all with ansiCSISequence, which leaves OSC
// 8 in place. Making the visible text its own type means a capture is stripped
// once, where it becomes text, and a clause that would read the raw bytes does
// not compile.
type visibleText string

// terminalEscapeSequence matches every escape sequence `capture-pane -e`
// emits. capture-pane serializes the cell grid rather than replaying what the
// program wrote (measured on tmux 3.4: cursor movement comes back as plain
// spaces), so the only escapes in a capture are cell attributes — SGR, a CSI,
// matched with ansiCSISequence's grammar — and hyperlinks — OSC 8, terminated
// by ST (ESC \) or by BEL. An OSC never spans a newline here, so a stray ESC ]
// cannot swallow the rows below it.
var terminalEscapeSequence = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]|\x1b\][^\x07\x1b\n]*(?:\x07|\x1b\\)`)

// visibleTextOf strips one capture. It is idempotent: text that is already
// visible comes back unchanged, without a regexp pass.
func visibleTextOf(capture string) visibleText {
	capture = strings.ReplaceAll(capture, "\r\n", "\n")
	if strings.IndexByte(capture, '\x1b') < 0 {
		return visibleText(capture)
	}
	return visibleText(terminalEscapeSequence.ReplaceAllString(capture, ""))
}

// lines is the text split into the pane's rows.
func (v visibleText) lines() []string {
	return strings.Split(string(v), "\n")
}
