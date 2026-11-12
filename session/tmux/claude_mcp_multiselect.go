package tmux

import (
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/sachiniyer/agent-factory/log"
)

// When a project's .mcp.json names two or more servers, Claude Code 2.1.322 does
// not show the single-server dialog claudeMCPServerDialogOf reads. It shows a
// multi-select picker instead (#7166), captured in
// session/tmux/testdata/claude_2_1_322_mcp_multi_server_dialog*.ansi:
//
//	 3 new MCP servers found in this project
//	 Select any you wish to enable.
//
//	 MCP servers may execute code or access system resources. All tool calls
//	 require approval. Learn more in the MCP documentation.
//
//	 ❯ [✔] probe
//	   [✔] probe-two
//	   [✔] probe-three
//	      Enable selected
//	Space to select · Esc to reject all
//
// Every server is PRE-CHECKED, so "accepting the preselection" — af's contract
// for the single-server dialog — would mean Enable selected, which enables every
// server. af never enables an MCP server for the user, so it does not accept
// this preselection. It answers with Esc, the picker's own "reject all", which
// is the outcome the single-server dialog preselects ("Continue without using
// this MCP server"). Measured on 2.1.322 with two and three servers: Esc closes
// the picker, the composer follows, and no server is enabled. The user can
// enable any of them later with Claude Code's /mcp.
//
// af sends Esc and nothing else: never Space (it toggles a server) and never
// Enter (it toggles the cursor row, or enables the checked servers from the
// last row). It sends Esc at most once per pane process. A second Esc that
// lands after the picker has closed reaches the composer, where it is Claude
// Code's interrupt and rewind key.
const (
	claudeMCPServersSubtitle    = "Select any you wish to enable."
	claudeMCPServersSubmitLabel = "Enable selected"
	claudeMCPServersFooter      = "Space to select · Esc to reject all"
	// claudeMCPServersChoice is what af chose, in the words the keystroke
	// record and the death diagnostic carry, so a start that fails or comes up
	// without the servers says where to turn them on.
	claudeMCPServersChoice = "reject all (enable any later with /mcp)"
)

// claudeMCPServersHeader is the picker's first row: the server count, then the
// fixed wording. A count below two is the single-server dialog's job.
var claudeMCPServersHeader = regexp.MustCompile(`^([2-9]|[1-9][0-9]+) new MCP servers found in this project$`)

// claudeMCPServersVerdict is what one capture says about the picker.
type claudeMCPServersVerdict int

const (
	claudeMCPServersNone claudeMCPServersVerdict = iota
	// claudeMCPServersPartial: a proper prefix of the picker, still painting.
	claudeMCPServersPartial
	// claudeMCPServersComplete: the whole picker, its footer the last content.
	claudeMCPServersComplete
)

// claudeMCPServerCheckboxRow reports whether label is one server row: a
// one-rune checkbox ("[✔]" or "[ ]") then the server's name.
func claudeMCPServerCheckboxRow(label string) bool {
	rest, ok := strings.CutPrefix(label, "[")
	if !ok {
		return false
	}
	_, size := utf8.DecodeRuneInString(rest)
	if size == 0 || !strings.HasPrefix(rest[size:], "] ") {
		return false
	}
	return strings.TrimSpace(rest[size+len("] "):]) != ""
}

// claudeMCPServersPickerOf decides whether the multi-select picker owns the
// pane, from the same one property every Claude parser here uses:
//
// The header, painted as a whole row of its own, opens the pane's trailing
// region, and every row from it to the bottom of the pane is one the picker
// paints, in the picker's order:
//
//	header        "<N> new MCP servers found in this project", N ≥ 2
//	body rows     plain text: no ❯, no frame rule, no checkbox, no footer
//	server rows   one unbroken run of "[✔] name" / "[ ] name" rows
//	submit row    "Enable selected", directly below the last server row
//	footer        "Space to select · Esc to reject all", then only blank rows
//
// A working agent paints its composer — framed by rules, led by ❯ — below its
// output, so the picker quoted in a transcript has something painted after its
// footer and never matches (#4254, #4735). A proper prefix of the grammar is
// partial.
func claudeMCPServersPickerOf(content string) claudeMCPServersVerdict {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	rows := make([]claudeTrustRow, len(lines))
	header := -1
	var cursorRows []int
	for i, line := range lines {
		rows[i] = claudeTrustRowOf(line)
		if rows[i].selected {
			cursorRows = append(cursorRows, i)
		}
		if !rows[i].selected && claudeMCPServersHeader.MatchString(rows[i].label) {
			header = i
		}
	}
	// The pane's ❯ rows: at most one, and none above the header. The live
	// picker is the only thing on screen and paints exactly one; a composer
	// anywhere paints another — the rule claudeMCPServerDialogOf and
	// parseClaudeFolderTrustDialog keep (#7167 review). An Esc that reached a
	// working composer would interrupt the agent.
	if header < 0 || len(cursorRows) > 1 || (len(cursorRows) == 1 && cursorRows[0] < header) {
		return claudeMCPServersNone
	}

	const (
		inBody = iota
		inServers
		// unpainted: blank rows straight after the server rows. The picker
		// paints its submit row with no gap, so these can only be the bottom
		// of a picker still being painted — anything painted below them means
		// this is not the picker.
		unpainted
		afterSubmit
		afterFooter
	)
	phase := inBody
	for i := header + 1; i < len(rows); i++ {
		row, rule := rows[i], claudeTrustFrameRule(lines[i])
		footer := row.label == claudeMCPServersFooter
		switch phase {
		case inBody:
			switch {
			case rule || footer || row.label == claudeMCPServersSubmitLabel:
				return claudeMCPServersNone
			case row.blank:
			case claudeMCPServerCheckboxRow(row.label):
				phase = inServers
			case row.selected:
				// A ❯ row that is not a server row is a composer.
				return claudeMCPServersNone
			}
		case inServers:
			switch {
			case claudeMCPServerCheckboxRow(row.label):
			case row.label == claudeMCPServersSubmitLabel:
				phase = afterSubmit
			case row.blank && !rule:
				phase = unpainted
			default:
				return claudeMCPServersNone
			}
		case unpainted:
			if !row.blank || rule {
				return claudeMCPServersNone
			}
		case afterSubmit:
			switch {
			case row.blank && !rule:
			case footer:
				phase = afterFooter
			default:
				return claudeMCPServersNone
			}
		case afterFooter:
			if !row.blank || rule {
				return claudeMCPServersNone
			}
		}
	}
	if phase == afterFooter {
		if len(cursorRows) != 1 {
			// A picker always has its cursor on one row.
			return claudeMCPServersNone
		}
		return claudeMCPServersComplete
	}
	return claudeMCPServersPartial
}

// rejectClaudeMCPServers answers a complete multi-select picker with Esc, once,
// and then re-reads the pane. It always reports true: the picker was
// positively identified, so the caller owes another check before it may type
// into the pane (#3302).
//
// The Esc waits out claudeTrustSettleDelay first. Claude Code drops keys sent in
// the instant after it paints a picker, and af will not send a second Esc, so
// a dropped first one would leave the picker on screen for good.
func (t *TmuxSession) rejectClaudeMCPServers() bool {
	state := &t.claudeTrust
	if state.mcpServersRejectSent {
		// af already sent its one Esc to this pane process and the picker is
		// on screen again (or never left). Another key is not af's to send.
		t.refuseClaudeMCPServers("af already sent Esc once and the picker is on screen again")
		return true
	}
	// The picker's own clock, not the shared firstSeen: the folder-trust
	// dialog af answered a moment ago may have started that one, and this
	// picker may have been painted only now.
	now := claudeTrustNow()
	if state.mcpServersFirstSeen.IsZero() {
		state.mcpServersFirstSeen = now
		return true
	}
	if now.Sub(state.mcpServersFirstSeen) < claudeTrustSettleDelay {
		return true
	}
	state.mcpServersRejectSent = true // before the send: an errored send-keys may still have landed
	if err := t.tapPromptKeys("Escape"); err != nil {
		log.ErrorLog.Printf("could not send Esc to the %s picker for session %q: %v",
			claudeMCPServersDialogName, t.sanitizedName, err)
		return true
	}
	t.noteDialogKeystroke(claudeMCPServersDialogName, claudeMCPServersChoice, "Escape")

	// Re-read: the picker must leave the screen. A repaint lags send-keys, so
	// look for the bounded repaint window before calling it stuck.
	deadline := claudeTrustNow().Add(claudeTrustRepaintWindow)
	for {
		content, err := t.CapturePaneContent()
		if err == nil && claudeMCPServersPickerOf(content) == claudeMCPServersNone {
			log.InfoLog.Printf("session %q: rejected every server on the %s picker with Esc; enable any of them later with /mcp",
				t.sanitizedName, claudeMCPServersDialogName)
			return true
		}
		if !claudeTrustNow().Before(deadline) {
			t.refuseClaudeMCPServers("the picker is still on screen after af sent Esc")
			return true
		}
		claudeTrustSleep(claudeTrustRepaintPoll)
	}
}

// refuseClaudeMCPServers reports, once per dialog rather than once per poll, a
// picker af will not send another key to. Refusing surfaces as the create's
// trust-prompt budget expiring — loud and recoverable — where a second Esc
// could reach the composer, and Space or Enter could enable a server.
func (t *TmuxSession) refuseClaudeMCPServers(reason string) {
	if t.claudeTrust.refusalLogged {
		return
	}
	t.claudeTrust.refusalLogged = true
	log.ErrorLog.Printf(
		"refusing to answer the %s picker for session %q: %s; af sends at most one Esc (reject all) and never Space or Enter. Answer it in the session; enable servers later with /mcp",
		claudeMCPServersDialogName, t.sanitizedName, reason)
}
