package task

import (
	"regexp"
	"strings"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// agentStoppedTailLines bounds how much of the pane's bottom counts as the
// CURRENT frame. codex's failure notices sit directly above its composer, so a
// small window finds them while a notice that scrolled up after the session
// recovered falls outside it.
const agentStoppedTailLines = 8

// codexStoppedNotice matches codex's terminal connection-failure notices:
//
//	■ Server connection could not be restored
//	• Reconnect failed — check the endpoint, then relaunch (187h 37m 44s)
//	■ Automatic reconnect could not restore this session. …
//
// Every alternative is anchored to the start of a line after codex's bullet
// glyph, so transcript prose that merely quotes the words does not match.
var codexStoppedNotice = regexp.MustCompile(
	`^\s*[\x{25A0}\x{2022}]\s*(?:Reconnect failed\b|Server connection could not be restored|Automatic reconnect could not restore)`)

// AgentStoppedContent reports whether the captured pane shows POSITIVE evidence
// that the agent is NOT working and will not resume on its own: the agent itself
// saying it gave up, not an inference from the pane being quiet.
//
// It is the mirror image of IsWorkingContent. The liveness poll infers working
// from pane churn, and a dead screen can churn forever — codex's "Reconnect
// failed" line carries an elapsed timer that repaints every second, so a session
// whose backend died days ago read Running indefinitely (#6055). Churn alone must
// never keep a session Running when the frame says otherwise.
//
// Only the bottom of the pane is read, and a visible in-flight indicator ("esc to
// interrupt") overrides the notice: a codex that recovered and is working again
// has the stale notice in scrollback above, never as its live frame. Agents
// without a known stop screen return false and keep the churn inference.
func AgentStoppedContent(content, agent string) bool {
	if agent != tmux.ProgramCodex {
		return false
	}
	plain := paneAnsiEscape.ReplaceAllString(strings.ReplaceAll(content, "\r\n", "\n"), "")
	var tail []string
	for _, line := range strings.Split(plain, "\n") {
		if strings.TrimSpace(line) != "" {
			tail = append(tail, line)
		}
	}
	if len(tail) > agentStoppedTailLines {
		tail = tail[len(tail)-agentStoppedTailLines:]
	}
	stopped := false
	for _, line := range tail {
		if escToInterruptHint.MatchString(line) {
			return false
		}
		if codexStoppedNotice.MatchString(line) {
			stopped = true
		}
	}
	return stopped
}
