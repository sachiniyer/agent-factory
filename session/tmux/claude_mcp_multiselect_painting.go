package tmux

import (
	"strings"
	"unicode/utf8"
)

// claudeMCPServersPaintingOf reports the 2.1.322 multi-server picker (#7166)
// while Claude Code paints it, read from its header down whatever is above the
// header (#7504).
//
// Claude Code paints the picker top-down. claudeMCPServersPickerOf holds a
// frame of that paint, but it reads clear in three shapes:
//
//   - A composer that an earlier Claude left above the picker. A relaunch in
//     the same pane leaves it there. Its ❯ row is above the header, and the
//     grammar declines the whole pane for any ❯ above the header, so that its
//     one Esc never reaches a composer. That rule stays. But the composer
//     explains only its own rows (#7388). It decides nothing about the
//     picker's rows below the header, which are still being painted.
//   - A row the paint has only begun: "❯ [✔]" before the server's name,
//     "Enable" or "Space to select ·". Each is none of the grammar's whole
//     rows.
//   - A header the pane wraps ("3 new MCP servers found in this" over
//     "project", the 40-column capture), or a server name it wraps (the
//     long-name capture). The grammar reads the header only as one row, and
//     each server only as one checkbox row.
//
// In each shape, the create typed its prompt and pressed Enter into a picker
// that has every server pre-checked. That enables every server once the picker
// finishes painting.
//
// It is the last clause CheckAndHandleTrustPrompt asks before it reports a
// Claude pane clear. It only holds, and sends no key. Every clause that
// answers a dialog has declined the frame before it runs, so it turns a clear
// pane into a hold and changes nothing else.
//
// It holds when all of these are true:
//
//   - a row shows the header as a row of its own, or as much of it as a wrap
//     leaves on its first row ("<N> new MCP servers", N ≥ 2);
//   - from that row down, the pane shows at most one ❯, the picker's cursor;
//   - every row below the header, down to the pane's last content (frame
//     rules counted as content), is a row of the picker's grammar, in the
//     grammar's order (claudeMCPServersRowAfter). Below a server row, a row of
//     plain text also counts: the wrapped rest of its name;
//   - the last content row is a whole row of the grammar, or the start of
//     the row the picker paints next (claudeMCPServersRowStart). A frame
//     rule is neither;
//   - the paint has not reached the whole footer. A whole picker is for the
//     readers before this one: the grammar answers it, and the unreadable
//     hold holds it.
//
// Nothing above the header is asked. A working agent paints its composer,
// framed by rules and led by ❯, below its output. So a picker it quotes has a
// rule or a second ❯ below the header, and stays clear.
func claudeMCPServersPaintingOf(screen visibleText) bool {
	lines := screen.lines()
	rows := make([]claudeTrustRow, len(lines))
	header, last := -1, -1
	for i, line := range lines {
		rows[i] = claudeTrustRowOf(line)
		if !rows[i].selected && claudeMCPServersHeaderStart.MatchString(rows[i].label) {
			header = i
		}
		if strings.TrimSpace(line) != "" {
			last = i
		}
	}
	if header < 0 {
		return false
	}
	cursors := 0
	for _, row := range rows[header:] {
		if row.selected {
			cursors++
		}
	}
	if cursors > 1 {
		return false
	}
	phase := claudeMCPServersInBody
	for i := header + 1; i <= last; i++ {
		rule := claudeTrustFrameRule(lines[i])
		next, ok := claudeMCPServersRowAfter(phase, rows[i], rule)
		switch {
		case ok:
			phase = next
		case phase == claudeMCPServersInServers && !rule && !rows[i].selected && rows[i].label != claudeMCPServersFooter:
			// The wrapped rest of a long server name: a row of its own,
			// below the server row it continues.
		case i == last && !rule:
			// A frame rule is never a row begun: claudeTrustRowOf reads it as
			// an empty label, which would pass for a checkbox not yet painted.
			// A composer's closing rule under a draft that quotes the picker
			// ends that way (#7507 review).
			return claudeMCPServersRowStart(phase, rows[i])
		default:
			return false
		}
	}
	return phase != claudeMCPServersAfterFooter
}

// claudeMCPServersRowStart reports whether row is the start of the row the
// picker paints next in phase: a server row as far as its checkbox, or a proper
// start of the "Enable selected" row or of the footer.
func claudeMCPServersRowStart(phase claudeMCPServersPhase, row claudeTrustRow) bool {
	switch phase {
	case claudeMCPServersInBody:
		// The first server row carries the cursor. A row of plain text is a
		// body row already.
		return row.selected && claudeMCPServerCheckboxPainted(row.label)
	case claudeMCPServersInServers:
		return claudeMCPServerCheckboxPainted(row.label) || claudeProperStart(row.label, claudeMCPServersSubmitLabel)
	case claudeMCPServersAfterSubmit:
		return claudeProperStart(row.label, claudeMCPServersFooter)
	}
	return false
}

// claudeMCPServerCheckboxPainted reports a server row painted no further than
// its checkbox. Under any scroll arrow, label is "", "[", "[✔" or "[✔]"; a
// checkbox is one rune between brackets.
func claudeMCPServerCheckboxPainted(label string) bool {
	if r, size := utf8.DecodeRuneInString(label); size > 0 && strings.ContainsRune(claudeMCPServersScrollArrows, r) {
		label = strings.TrimSpace(label[size:])
	}
	if label == "" {
		return true
	}
	rest, ok := strings.CutPrefix(label, "[")
	if !ok {
		return false
	}
	_, size := utf8.DecodeRuneInString(rest)
	return size == len(rest) || rest[size:] == "]"
}

// claudeProperStart reports whether label is a non-empty start of whole that is
// shorter than whole.
func claudeProperStart(label, whole string) bool {
	return label != "" && len(label) < len(whole) && strings.HasPrefix(whole, label)
}
