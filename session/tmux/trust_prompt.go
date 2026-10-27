package tmux

import (
	"regexp"
	"strings"
)

// CodexTrustPromptPresent reports whether content shows Codex's directory-trust
// modal introduced by 0.144.6:
//
//	Do you trust the contents of this directory?
//	› 1. Yes, continue
//	  2. No, quit
//	Press enter to continue
//
// This modal is on the config-agent delivery path before the real composer.
// Its selected option uses the SAME `›` glyph as Codex's composer, so the old
// readiness check treated it as ready, pasted the briefing into the modal, and
// used the trailing Enter to select Yes. Every tmux command succeeded while
// Codex recorded no user turn, leaving an empty composer by attach time (#2220).
//
// The match is anchored on the question, both option labels and the affordance.
// CheckAndHandleTrustPrompt also runs on live-session polls, so a prose mention
// of one phrase must never inject Enter into a working agent.
var ansiCSISequence = regexp.MustCompile(`\x1b\[[0-?]*[ -/]*[@-~]`)

func CodexTrustPromptPresent(content string) bool {
	content = ansiCSISequence.ReplaceAllString(strings.ReplaceAll(content, "\r\n", "\n"), "")
	question, selectedYes, noOption, affordance, last := -1, -1, -1, -1, -1
	for idx, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" {
			continue
		}
		last = idx
		switch {
		case strings.HasPrefix(line, "Do you trust the contents of this directory?"):
			question = idx
		case line == "› 1. Yes, continue":
			selectedYes = idx
		case line == "2. No, quit":
			noOption = idx
		case line == "Press enter to continue":
			affordance = idx
		}
	}
	return question >= 0 && question < selectedYes && selectedYes < noOption &&
		noOption < affordance && affordance == last
}

// DocTrustPromptPresent reports whether content ends at the documentation-link
// confirmation prompt shared by aider/gemini:
//
//	<reverse-video>https://aider.chat/docs/...
//	<question text> (Y)es/(N)o/(D)on't ask again [Yes]:
//
// aider's InputOutput.confirm_ask accepts caller-controlled question text, then
// appends the confirmation suffix above. InputOutput.offer_url separately renders
// its URL subject in reverse video before asking. Match that renderer-owned SGR
// prefix immediately before the final prompt line. Do not key daemon input on the
// overridable prose. In particular, this source file contains the old prose and
// affordance together; a strings.Contains matcher lets repo-controlled text inject
// D+Enter (#2638).
//
// The reverse-video URL subject and final-line requirements both matter. The
// affordance appears on unrelated aider confirmations, while an ordinary URL can
// appear immediately before one. Without the renderer prefix those two unrelated
// lines compose a false positive. A no-color or otherwise ambiguous capture is not
// proof of a prompt and stays false.
//
// This is deliberately conservative because the two errors are asymmetric. A miss
// costs one manual keypress, and the real dialog remains visible for the next poll
// to retry. A false positive types D+Enter into a live agent that asked nothing; it
// can answer a different question or modify the agent's input, and the daemon's
// continuous poll repeats it while the text remains visible (#1952).
//
// This is the single copy, shared with task's readiness check (task/runner.go
// isReadyContent) so the dismissal and the readiness signal can never drift apart
// again — the drift between this and the Claude branch beside it is exactly how
// #1952 happened. It lives here because task already imports session/tmux; the
// reverse edge would be an import cycle.
func DocTrustPromptPresent(content string) bool {
	const confirmPrefix = "(Y)es/(N)o/(D)on't ask again [Yes]:"
	content = strings.ReplaceAll(content, "\r\n", "\n")
	lines := strings.Split(content, "\n")
	last := len(lines) - 1
	for last >= 0 && strings.TrimSpace(ansiCSISequence.ReplaceAllString(lines[last], "")) == "" {
		last--
	}
	if last < 1 || !strings.HasSuffix(
		strings.TrimSpace(ansiCSISequence.ReplaceAllString(lines[last], "")), confirmPrefix,
	) {
		return false
	}
	return reverseVideoURLSubject(lines[last-1])
}

func reverseVideoURLSubject(line string) bool {
	reverse := false
	remaining := line
	for strings.HasPrefix(remaining, "\x1b[") {
		end := strings.IndexByte(remaining, 'm')
		if end < 2 {
			return false
		}
		for _, code := range strings.Split(remaining[2:end], ";") {
			switch code {
			case "", "0", "27":
				reverse = false
			case "7":
				reverse = true
			}
		}
		remaining = remaining[end+1:]
	}
	visible := strings.TrimSpace(ansiCSISequence.ReplaceAllString(remaining, ""))
	return reverse && !strings.ContainsAny(visible, " \t") &&
		(strings.HasPrefix(visible, "https://") || strings.HasPrefix(visible, "http://"))
}

// claudeTrustPromptPresent reports whether the captured pane content is showing
// one of Claude Code's launch-time gates that af auto-dismisses with Enter: the
// folder-trust prompt (either wording) or the "new MCP server" trust prompt.
//
// This runs on the daemon's CONTINUOUS Snapshot poll (session/agentserver_local.go
// CheckAndHandleTrustPrompt), and CapturePaneContent is visible-only
// (capture-pane -p, no -S), so `content` is whatever is on screen right now —
// including ordinary agent output. A bare substring match on natural-language
// prompt text would therefore risk firing a spurious Enter on unrelated output.
//
// Claude Code reworded the folder-trust gate from the interrogative
// "Do you trust the files in this folder?" to a "Quick safety check: Is this a
// project you created or one you trust? ... ❯ 1. Yes, I trust this folder /
// Enter to confirm · Esc to cancel" dialog. af's dismissal missed the new
// wording, so brand-new sessions hung at the prompt and rendered a blank pane.
//
// We match the reworded prompt ANCHORED: the natural-language question must
// co-occur with a dialog-chrome marker only the real modal renders ("Yes, I
// trust this folder" or the "Enter to confirm" affordance), so a stray mention
// of the phrase in scrollback or agent output never triggers a dismissal. The
// old wording ("Do you trust the files in this folder?") is decided by
// claudeLegacyTrustDialogOf: the question must be a whole row that opens the
// pane's trailing region, and every row below it must be one the legacy dialog
// paints, in its order. The phrase alone is the same hazard the MCP branch's
// footer-is-last rule refuses — af's own source contains the legacy phrase
// verbatim, so a visible rendering of this file's contents (a diff, a review
// reply, pasted release notes) carries it too.
//
// The MCP prompt ("New MCP server found. Do you trust this new MCP server? ❯
// 1. Yes ... Enter to confirm") is anchored on its unique question "do you
// trust this new mcp server" AND the modal's "Enter to confirm" footer being
// the last non-blank content in the pane. The phrase alone cannot tell a live
// modal from output that merely quotes it — af's own source contains the
// phrase verbatim — so the footer-is-last rule (the same discipline
// claudeTrustPickerStructure applies to the folder-trust branch,
// claude_trust.go:307-310, and CodexTrustPromptPresent applies as its
// `affordance == last` rule) refuses a quoted phrase whose agent composer or
// further output is painted below it. Requiring the footer at the END is
// strictly stronger than requiring it anywhere in the content: a working
// agent paints its composer beneath its output, so a quoted dialog has
// something after it and a live one does not.
func claudeTrustPromptPresent(content string) bool {
	lower := strings.ToLower(content)

	// Reworded folder-trust dialog — the question co-occurs with the
	// dialog-only option label. Both strings are specific to this modal.
	reworded := strings.Contains(content, "Is this a project you created or one you trust") &&
		strings.Contains(content, "Yes, I trust this folder")

	// MCP trust dialog — anchored on its unique question (case-insensitive)
	// AND the modal's footer being the last content on screen, so a quoted
	// mention of the phrase with the composer painted below it does not fire.
	mcpDialog := strings.Contains(lower, "do you trust this new mcp server") &&
		claudeMCPTrustFooterIsLast(content)

	// Legacy folder-trust dialog — the question as a whole row, opening a
	// trailing region that holds only rows the dialog paints, complete, with
	// the cursor on Yes (claudeLegacyTrustDialogOf). af's own source contains
	// the question verbatim, so the phrase alone must never fire —
	// "a prose mention of one phrase must never inject Enter into a working
	// agent."
	legacy := claudeLegacyTrustDialogOf(content) == claudeLegacyTrustAnswerable

	return reworded || mcpDialog || legacy
}
