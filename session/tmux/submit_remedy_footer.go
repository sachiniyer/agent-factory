package tmux

import (
	"fmt"
	"strings"
	"unicode"
)

// stillChipTailAudit reports why the rows below a composer region break the
// still-chip proof, or "" when the frame bounds the block for the proof.
// A ruled or boxed composer — Claude's "❯", Devin's "❭", or the │-boxed ">" —
// closes at the agent's OWN edge, and only BLANK rows may sit between the
// region's cut and that edge; a draft row inside the composer renders
// there, while rows below the edge are chrome by geometry because no draft
// can draw outside the composer. Codex's "›" lane has no closing edge, so
// every content row below its region must parse Codex's closed footer
// token grammar — a known model token, a whole-segment path token, known
// literals and meters, with no free-text segment (#7850 R2/N1). A draft
// that opened with blank lines, or a second draft line after the absorbed
// Enter's newline, renders the same shape as a footer — the remedy Enter
// would submit it with our prompt.
func stillChipTailAudit(rows []string, end int, anchorRow string) string {
	boxed := strings.HasPrefix(anchorRow, "│ ")
	switch anchorGlyph(anchorRow) {
	case claudeComposerGlyph, "❭":
		boxed = false
	case ">":
	default:
		// Codex's › lane: no closing edge. A column-0 "─" rule in a ruled
		// Codex lane still bounds the block; every other content row must
		// parse the closed footer grammar.
		for j := end; j < len(rows); j++ {
			raw := rows[j]
			switch {
			case blankComposerRow(raw):
			case composerRuleRow(raw):
			case codexComposerFooterRow(raw):
			default:
				return fmt.Sprintf("the composer holds content %q below a blank row that is not provably the agent's own footer", normalizeDelivery(raw))
			}
		}
		return ""
	}
	// A ruled or boxed composer: blanks until the agent's own closing edge;
	// everything below the edge is chrome by geometry.
	for j := end; j < len(rows); j++ {
		raw := rows[j]
		switch {
		case blankComposerRow(raw):
		case stillChipClosingEdge(raw, boxed):
			return ""
		default:
			return fmt.Sprintf("the composer holds content %q inside the composer that is not provably blank", normalizeDelivery(raw))
		}
	}
	return ""
}

// stillChipClosingEdge reports whether raw is the anchor agent's own closing
// edge, drawn at the pane's left column: the "─" rule the ruled lanes draw,
// or the ╰───╯ bottom edge of a │-boxed composer. An indented row of
// box-drawing glyphs — a draft typed in "─"s — is content, not an edge
// (#7850 R3).
func stillChipClosingEdge(raw string, boxed bool) bool {
	if raw == "" || unicode.IsSpace([]rune(raw)[0]) {
		return false
	}
	if boxed {
		return composerBoxBottomRow(raw)
	}
	return composerRuleRow(raw)
}

// composerBoxBottomRow reports whether raw is a ╰───╯ bottom edge — the ╰
// corner plus only "─" through ╯, the shape Claude's composer box closes
// with.
func composerBoxBottomRow(raw string) bool {
	rs := []rune(raw)
	if len(rs) < 2 || rs[0] != '╰' || rs[len(rs)-1] != '╯' {
		return false
	}
	for _, r := range rs[1 : len(rs)-1] {
		if r != '─' {
			return false
		}
	}
	return true
}

// codexComposerFooterRow reports whether raw is a row of Codex's composer
// footer against its whole-row grammar: the model-and-path status line
// ("GPT-6.1 Terra ultra · ~/sahara/… · ⚠ 1 warning · f2 to view", the shape
// codexStatusLineModel owns), or a hints line whose every " · "-separated
// segment is one of the status or shortcut phrases Codex itself draws there
// ("← for agents · ? for shortcuts ⚠ 1 warning · f2 to view"). A draft line
// that merely CONTAINS one of the phrases is composer content (#7850 R2).
func codexComposerFooterRow(raw string) bool {
	line := strings.TrimSpace(raw)
	if line == "" {
		return false
	}
	parts := strings.Split(line, " · ")
	if len(parts) >= 2 && codexStatusModel(parts[0]) && codexStatusPath(parts[1]) {
		for _, seg := range parts[2:] {
			if !codexFooterSegment(seg) {
				return false
			}
		}
		return true
	}
	for _, seg := range parts {
		if !codexFooterSegment(seg) {
			return false
		}
	}
	return true
}

// codexStatusModel reports whether the first " · "-segment of a Codex status
// line is a model token — a known model shape head ("GPT-6-Astra",
// "gpt-5.6-sol", "o3") followed only by known tier words and capitalized
// codename words ("ultra", "Terra", "max"). A draft's "1. see my notes" or
// "v2 ok go" carries no model token (#7850 N1) — failing closed on an odd
// model name is fine.
func codexStatusModel(seg string) bool {
	f := strings.Fields(strings.TrimSpace(seg))
	if len(f) == 0 || len(f) > 3 {
		return false
	}
	if !codexModelToken(f[0]) {
		return false
	}
	for _, w := range f[1:] {
		if !codexModelTier(w) && !codexModelCodename(w) {
			return false
		}
	}
	return true
}

// codexModelToken reports whether w is a known Codex model token shape —
// "GPT-…", "gpt-…", "o<digit>…", or "codex-…".
func codexModelToken(w string) bool {
	for _, p := range []string{"GPT", "gpt", "codex"} {
		if strings.HasPrefix(w, p) {
			return true
		}
	}
	return len(w) >= 2 && w[0] == 'o' && w[1] >= '0' && w[1] <= '9'
}

// codexModelTier reports whether w is a reasoning-effort or tier word the
// model segment carries.
func codexModelTier(w string) bool {
	switch w {
	case "ultra", "max", "mini", "high", "medium", "low", "fast", "standard", "pro", "plus":
		return true
	}
	return false
}

// codexModelCodename reports whether w is a capitalized codename word the
// model segment carries — "Terra", "Astra", "Sol".
func codexModelCodename(w string) bool {
	if len(w) < 2 || len(w) > 12 {
		return false
	}
	for i, r := range w {
		if i == 0 {
			if r < 'A' || r > 'Z' {
				return false
			}
		} else if r < 'a' || r > 'z' {
			return false
		}
	}
	return true
}

// codexStatusPath reports whether a status segment is Codex's current-dir —
// a whole segment that is one path token: "~", "~/…", or "/…". A segment
// with spaces is draft text, not a path (#7850 N1).
func codexStatusPath(seg string) bool {
	seg = strings.TrimSpace(seg)
	if len(strings.Fields(seg)) != 1 {
		return false
	}
	return seg == "~" || strings.HasPrefix(seg, "~/") || strings.HasPrefix(seg, "/")
}

// codexFooterSegment reports whether one " · "-separated segment of a Codex
// footer row is a status or shortcut phrase Codex draws there —
// whole-segment, or its parameterized shapes ("⚠ N warning(s)", "NN% context
// left", "Main [branch]"). A warning count may trail "? for shortcuts"
// inside one segment.
func codexFooterSegment(seg string) bool {
	seg = strings.TrimSpace(seg)
	switch seg {
	case "← for agents", "? for shortcuts", "f2 to view",
		"esc to interrupt", "Pursuing goal", "for agents":
		return true
	}
	if rest, ok := strings.CutPrefix(seg, "? for shortcuts "); ok {
		seg = strings.TrimSpace(rest) // a warning count can trail the hint
	}
	if rest, ok := strings.CutPrefix(seg, "⚠ "); ok {
		f := strings.Fields(rest)
		return len(f) == 2 && digitsOnly(f[0]) && strings.HasPrefix(f[1], "warning")
	}
	if strings.HasPrefix(seg, "Main [") && strings.HasSuffix(seg, "]") {
		// the branch name is one token — "Main [default]", never a clause
		inner := seg[len("Main [") : len(seg)-1]
		return inner != "" && len(strings.Fields(inner)) == 1
	}
	if strings.HasSuffix(seg, " context left") {
		f := strings.Fields(seg)
		return len(f) == 3 && f[1] == "context" && f[2] == "left" &&
			strings.HasSuffix(f[0], "%") && digitsOnly(strings.TrimSuffix(f[0], "%"))
	}
	return false
}
