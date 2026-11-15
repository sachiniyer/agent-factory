package tmux

import "strings"

// claudeMCPDialogScrolledOff reports the old single-server MCP trust modal in the
// one shape a too-short pane reaches and no other parser claims: its preamble
// ("New MCP server found") and question ("Do you trust this new MCP server?")
// have scrolled off the top, leaving only the option rows and the "Enter to
// confirm" footer on screen. The folder-trust twin hit the same class of frame
// and was fixed to hold in claudeFolderTrustHeadless / claudeFolderTrustFooterOnly
// (#7280, #7285); the old-MCP branch was the launch gate that still failed open
// on it, and a false return here is the create path's permission to type the
// user's prompt and press Enter — on the modal's preselected "1. Yes" (#3579
// class, auto-trust of an MCP server that can execute code).
//
// The reported frame
//
//	❯ 1. Yes
//	  2. No
//	Enter to confirm
//
// is pixel-identical to a caller-controlled newline above an unrelated yes/no
// picker, and the preselected option of either is entered by a bare Enter, so
// the two want opposite keys. af cannot tell them apart, so it fails closed:
// reports the pane not clear and sends no key. The cost is the create's trust
// budget expiring loudly and recoverably; the alternative is silent auto-trust,
// the exact failure this gate exists to prevent. Mirrors the trade the
// folder-trust fix (a32e070c) accepted.
//
// The hold fires when, and only when:
//
//   - the modal's "Enter to confirm" footer is the LAST non-blank content in
//     the pane — a working agent paints its composer below its output, so a
//     quoted modal with the composer beneath it never reaches this shape;
//   - the two non-blank rows immediately above the footer (skipping the blank
//     chrome the Claude picker layout places between options and footer, and
//     between the question and the options) reduce, after claudeTrustRowOf
//     strips the selection glyph and numeric ordinals, to exactly the modal's
//     own "yes" and "no" option labels — exact (not suffix) match is what keeps
//     "Always yes" and an unrelated picker from matching; and
//   - the modal's preamble appears NOWHERE on screen. The preamble is a fixed
//     string only the modal itself renders, never attacker-controlled prose.
//     A frame with the preamble present is the full modal, which
//     claudeMCPTrustFooterIsLast reads and fires Enter for; it never reaches
//     this branch. A frame with the options and footer in place and the
//     preamble absent is exactly the ambiguous one af must hold on.
//
// The question may or may not still be visible: Claude renders the preamble and
// question on one row (and they scroll off together) or on separate consecutive
// rows (so the preamble alone scrolls off). Either way the frame ends in the
// modal's own options and footer with the preamble absent, and a bare Enter
// would auto-trust the preselected "1. Yes", so af holds.
func claudeMCPDialogScrolledOff(screen visibleText) bool {
	affordance := strings.ToLower(claudeTrustAffordancePrefix)
	lines := screen.lines()
	rows := make([]claudeTrustRow, len(lines))
	for i, line := range lines {
		rows[i] = claudeTrustRowOf(line)
	}

	// The footer must be the last non-blank content. A quoted modal with the
	// composer below it fails here, so it does not reach the option check.
	lastIdx := -1
	for i := len(rows) - 1; i >= 0; i-- {
		if !rows[i].blank {
			lastIdx = i
			break
		}
	}
	if lastIdx < 0 {
		return false
	}
	if !strings.HasPrefix(strings.ToLower(rows[lastIdx].label), affordance) {
		return false
	}

	// The two non-blank rows immediately above the footer, skipping blank chrome,
	// must be EXACTLY the modal's own "yes" and "no" option labels. Walking up
	// from the footer and stopping at two collects the option rows whether the
	// question (or banner rows) sit in the same block above them or in a
	// separate block across a blank — all three are the scrolled-off shape, and
	// none should be reported clear. An unrelated picker's rows do not reduce to
	// exactly "yes"/"no", so the first mismatch returns false.
	collected := 0
	yesFound, noFound := false, false
	for i := lastIdx - 1; i >= 0 && collected < 2; i-- {
		if rows[i].blank {
			continue
		}
		switch strings.ToLower(rows[i].label) {
		case claudeMCPOptionLabels[0]:
			yesFound = true
		case claudeMCPOptionLabels[1]:
			noFound = true
		default:
			return false
		}
		collected++
	}
	if collected != 2 || !yesFound || !noFound {
		return false
	}

	// The modal's preamble must appear nowhere on screen. Its presence is the
	// full modal — handled by claudeMCPTrustFooterIsLast, which fires Enter;
	// its absence with the options and footer in place is exactly the
	// ambiguous scrolled-off frame af must fail closed on.
	for i := 0; i <= lastIdx; i++ {
		if strings.Contains(strings.ToLower(rows[i].label), claudeMCPDialogPreamble) {
			return false
		}
	}
	return true
}
