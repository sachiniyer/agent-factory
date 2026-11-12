package tmux

import (
	"fmt"
	"slices"
	"strings"

	"github.com/sachiniyer/agent-factory/log"
)

// Claude Code 2.1.322 reworded its single-server MCP approval dialog (#7163).
// Captured with af's own capture flags in a fresh worktree whose .mcp.json
// names one server (session/tmux/testdata/claude_2_1_322_mcp_server_*.ansi):
//
//	────────────────────────────────────────
//	  New MCP server found in this project: probe
//
//	  MCP servers may execute code or access system resources. All tool calls
//	  require approval. Learn more in the MCP documentation.
//
//	    Use this MCP server
//	    Use this and all future MCP servers in this project
//	  ❯ Continue without using this MCP server
//
//	  Enter to confirm · Esc to cancel
//
// None of the older wording's anchors survive — no "Do you trust this new MCP
// server?" question, no Yes/No rows — so claudeMCPTrustFooterIsLast no longer
// matches it, and af answered nothing.
//
// The contract af keeps for this dialog is unchanged: it accepts the option
// Claude Code PRESELECTED and gains no trust authority of its own. On this
// wording that preselection is the decline row, so af confirms only that row,
// by label, and refuses — sending nothing — whenever any other row owns the
// cursor. Both "Use" rows grant an MCP server the right to run code; af must
// never be the thing that picked them.
const (
	claudeMCPServerPreamble     = "New MCP server found in this project:"
	claudeMCPServerUseLabel     = "Use this MCP server"
	claudeMCPServerUseAllLabel  = "Use this and all future MCP servers in this project"
	claudeMCPServerDeclineLabel = "Continue without using this MCP server"
)

// claudeMCPServerOptionLabels are the rows the dialog's picker paints, matched
// EXACTLY after claudeTrustRowOf has stripped the cursor and any ordinal: a
// prefix test would accept "Use this MCP server, please" from a composer draft.
var claudeMCPServerOptionLabels = []string{
	claudeMCPServerUseLabel,
	claudeMCPServerUseAllLabel,
	claudeMCPServerDeclineLabel,
}

// claudeMCPServerVerdict is what one capture says about the dialog.
type claudeMCPServerVerdict int

const (
	// claudeMCPServerNone: the pane is not showing the dialog.
	claudeMCPServerNone claudeMCPServerVerdict = iota
	// claudeMCPServerPartial: the pane shows a proper prefix of the dialog —
	// Claude Code is still painting it. Hold: send nothing, and do not report
	// the pane clear.
	claudeMCPServerPartial
	// claudeMCPServerComplete: the whole dialog, footer last. selected names the
	// row that owns the cursor, or is empty when no single row does.
	claudeMCPServerComplete
)

// claudeMCPServerDialog is a parsed capture of the dialog.
type claudeMCPServerDialog struct {
	verdict  claudeMCPServerVerdict
	selected string
}

// claudeMCPServerDialogOf decides whether the 2.1.322 MCP-server dialog owns
// the pane. Like claudeLegacyTrustDialogOf it decides from ONE property, because
// it runs against arbitrary agent output and Enter typed into a working
// composer cannot be taken back:
//
// The dialog owns the pane only when its preamble, painted as a whole row of its
// own, opens the pane's trailing region, and every row from the preamble to the
// bottom of the pane is one the dialog itself paints, in the dialog's order:
//
//	preamble      a row starting claudeMCPServerPreamble (the server name follows)
//	body rows     plain text: no ❯, no frame rule, no footer
//	option block  one unbroken run of the three option rows, each exactly once
//	footer        "Enter to confirm…", then nothing but blank rows and frame chrome
//
// The footer being the last content is the structural guard every Claude branch
// relies on (#4254, #4735): a working agent paints its composer — framed by
// rules, led by ❯ — below its output, so a dialog quoted in a transcript always
// has something painted after it, and a live one does not.
//
// A region that is a proper prefix of the grammar — preamble and body with no
// options yet, some options, or all three with no footer yet — is partial.
// Anything else below the preamble, at any point, means this is not the dialog.
func claudeMCPServerDialogOf(content string) claudeMCPServerDialog {
	lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
	rows := make([]claudeTrustRow, len(lines))
	preamble := -1
	for i, line := range lines {
		rows[i] = claudeTrustRowOf(line)
		if !rows[i].selected && strings.HasPrefix(rows[i].label, claudeMCPServerPreamble) {
			preamble = i
		}
	}
	if preamble < 0 {
		return claudeMCPServerDialog{}
	}

	const (
		inBody = iota
		inOptions
		afterOptions
		afterFooter
	)
	phase := inBody
	seen := make([]bool, len(claudeMCPServerOptionLabels))
	cursors, selected := 0, ""
	for i := preamble + 1; i < len(rows); i++ {
		row, rule := rows[i], claudeTrustFrameRule(lines[i])
		option := slices.Index(claudeMCPServerOptionLabels, row.label)
		switch phase {
		case inBody:
			switch {
			case rule || claudeTrustIsFooter(row.label):
				return claudeMCPServerDialog{}
			case row.blank:
				continue
			case option >= 0:
				phase = inOptions
			case row.selected:
				// A ❯ row that is not an option is a composer, not this picker.
				return claudeMCPServerDialog{}
			default:
				continue
			}
		case inOptions:
			switch {
			case row.blank:
				phase = afterOptions
				continue
			case claudeTrustIsFooter(row.label):
				phase = afterFooter
				continue
			case option < 0:
				return claudeMCPServerDialog{}
			}
		case afterOptions:
			switch {
			case row.blank:
			case claudeTrustIsFooter(row.label):
				phase = afterFooter
			default:
				return claudeMCPServerDialog{}
			}
			continue
		case afterFooter:
			// Frame chrome may close a boxed variant; content may not follow.
			if !row.blank && !rule {
				return claudeMCPServerDialog{}
			}
			continue
		}
		// An option row.
		if seen[option] {
			return claudeMCPServerDialog{}
		}
		seen[option] = true
		if row.selected {
			cursors++
			selected = row.label
		}
	}

	if phase != afterFooter {
		return claudeMCPServerDialog{verdict: claudeMCPServerPartial}
	}
	for _, ok := range seen {
		if !ok {
			// A footer under an incomplete option block is not a shape this
			// dialog paints in any order — Claude Code paints the footer last.
			return claudeMCPServerDialog{}
		}
	}
	if cursors != 1 {
		selected = ""
	}
	return claudeMCPServerDialog{verdict: claudeMCPServerComplete, selected: selected}
}

// declineClaudeMCPServer answers a complete 2.1.322 MCP-server dialog by
// confirming Claude Code's preselected decline row — and only that row. It
// always reports true: the dialog was positively identified, so every outcome
// leaves the caller owing another check before it may type into the pane
// (#3302).
//
// It looks, then looks again before the irreversible key: Enter is sent only
// after a FRESH capture still shows the complete dialog with the cursor on the
// decline row. A dialog that closed, or a cursor that moved, between the two
// captures gets no Enter — the same look-then-act shape selectClaudeFolderTrust
// keeps.
func (t *TmuxSession) declineClaudeMCPServer(dialog claudeMCPServerDialog) bool {
	if dialog.selected != claudeMCPServerDeclineLabel {
		t.refuseClaudeMCPServer(dialog.selected)
		return true
	}
	content, err := t.CapturePaneContent()
	if err != nil {
		log.InfoLog.Printf("session %q: could not re-read the %s dialog before confirming %q: %v",
			t.sanitizedName, claudeMCPServerDialogName, claudeMCPServerDeclineLabel, err)
		return true
	}
	confirmed := claudeMCPServerDialogOf(content)
	if confirmed.verdict != claudeMCPServerComplete || confirmed.selected != claudeMCPServerDeclineLabel {
		log.InfoLog.Printf("session %q: the %s dialog changed before af confirmed it (now %q selected); not confirming this frame",
			t.sanitizedName, claudeMCPServerDialogName, confirmed.selected)
		return true
	}
	if err := t.tapPromptKeys("Enter"); err != nil {
		log.ErrorLog.Printf("could not confirm %q on the %s dialog for session %q: %v",
			claudeMCPServerDeclineLabel, claudeMCPServerDialogName, t.sanitizedName, err)
		return true
	}
	t.noteDialogKeystroke(claudeMCPServerDialogName, claudeMCPServerDeclineLabel, "Enter")
	t.claudeTrust.refusalLogged = false
	return true
}

// refuseClaudeMCPServer reports, once per dialog rather than once per poll, a
// dialog whose cursor is not on the decline row. af will not move it there: the
// contract is to accept Claude Code's own preselection, and a preselection that
// is anything but the decline row is not one af may confirm on the user's
// behalf. Refusing surfaces as the create's trust-prompt budget expiring — loud
// and recoverable — where Enter would have enabled an MCP server.
func (t *TmuxSession) refuseClaudeMCPServer(selected string) {
	if t.claudeTrust.refusalLogged {
		return
	}
	t.claudeTrust.refusalLogged = true
	reason := fmt.Sprintf("%q is selected", selected)
	if selected == "" {
		reason = fmt.Sprintf("no single row carries the %q cursor", claudeTrustSelectionGlyph)
	}
	log.ErrorLog.Printf(
		"refusing to answer the %s dialog for session %q: %s, and af only ever confirms %q, the option Claude Code preselects; answer it in the session",
		claudeMCPServerDialogName, t.sanitizedName, reason, claudeMCPServerDeclineLabel)
}
