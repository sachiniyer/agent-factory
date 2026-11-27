package tmux

import (
	"fmt"
	"strings"
	"unicode"
)

// stillChipTailAudit reports why the rows below a composer region break the
// still-chip proof, or "" when the frame bounds the block for the proof:
// EVERY row from the region's cut to the pane end must be blank, a border
// drawn at the pane's left column, or a row of the anchor agent's own footer
// matched against that agent's whole-row grammar (#7850 R2). A draft that
// opened with blank lines, or a second draft line after the absorbed Enter's
// newline, renders the same shape as a footer — the remedy Enter would
// submit it with our prompt, so a footer-shaped row never excuses the rows
// below it, and a composer whose glyph names no agent borrows no other
// agent's grammar.
func stillChipTailAudit(rows []string, end int, anchorRow string) string {
	for j := end; j < len(rows); j++ {
		raw := rows[j]
		switch {
		case blankComposerRow(raw):
		case stillChipBoundaryEdge(raw, anchorRow):
		case agentComposerFooterRow(raw, anchorRow):
		default:
			return fmt.Sprintf("the composer holds content %q below a blank row that is not provably the agent's own footer", normalizeDelivery(raw))
		}
	}
	return ""
}

// stillChipBoundaryEdge reports whether raw ends the composer at the anchor
// agent's OWN geometry: the column-0 "─" rule the ruled lanes draw, or the
// ╰───╯ bottom edge of Claude's │-boxed composer (#7850 R3). An indented row
// of box-drawing glyphs — a draft typed in "─"s — is content, not an edge;
// a border that belongs to no agent's composer borrows no boundary.
func stillChipBoundaryEdge(raw, anchorRow string) bool {
	if raw == "" || unicode.IsSpace([]rune(raw)[0]) {
		return false
	}
	if anchorGlyph(anchorRow) == ">" {
		return strings.HasPrefix(anchorRow, "│ ") && composerBoxBottomRow(raw)
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

// agentComposerFooterRow reports whether raw is provably a row of the anchor
// agent's own footer, matched against that agent's whole-row grammar. The
// anchor's glyph names the agent; the │-boxed ">" is Claude's box composer,
// and every other ">" — like every glyph this table does not key — borrows
// no other agent's grammar (#7850 R2).
func agentComposerFooterRow(raw, anchorRow string) bool {
	switch anchorGlyph(anchorRow) {
	case "›":
		return codexComposerFooterRow(raw)
	case claudeComposerGlyph:
		return claudeComposerFooterRow(raw)
	case "❭":
		return devinComposerFooterRow(raw)
	case ">":
		return strings.HasPrefix(anchorRow, "│ ") && claudeComposerFooterRow(raw)
	}
	return false
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
// line is a model-with-effort token. The model's first word always carries a
// version digit ("GPT-6-Astra", "gpt-5.6-sol", "o3") — a draft's "see my
// notes" does not (#7850 B6).
func codexStatusModel(seg string) bool {
	seg = strings.TrimSpace(seg)
	if seg == "" || strings.ContainsAny(seg, "›❯❭>»") {
		return false
	}
	return strings.ContainsAny(strings.Fields(seg)[0], "0123456789")
}

// codexStatusPath reports whether a status segment is Codex's current-dir —
// "~", "~/…", or "/…" — the shape codexStatusLineModel requires of the same
// segment.
func codexStatusPath(seg string) bool {
	seg = strings.TrimSpace(seg)
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
		return true
	}
	if strings.HasSuffix(seg, " context left") {
		f := strings.Fields(seg)
		return len(f) == 3 && f[1] == "context" && f[2] == "left" &&
			strings.HasSuffix(f[0], "%") && digitsOnly(strings.TrimSuffix(f[0], "%"))
	}
	return false
}

// claudeComposerFooterRow reports whether raw is a row of Claude's composer
// footer against its whole-row grammar: every " · "-separated segment must
// be composed entirely of the phrases Claude draws there — the vim-mode
// flags, the permission mode, the agent and shell counts, the shortcut
// hints. A draft line that merely CONTAINS a phrase ("esc to interrupt if
// this is slow") is composer content (#7850 R2).
func claudeComposerFooterRow(raw string) bool {
	line := strings.TrimSpace(raw)
	if line == "" {
		return false
	}
	for _, seg := range strings.Split(line, " · ") {
		if !claudeFooterSegment(seg) {
			return false
		}
	}
	return true
}

// claudeFooterPhrases are the fixed phrases a Claude footer segment is built
// from, peeled one at a time off the segment's front.
var claudeFooterPhrases = []string{
	"-- INSERT --", "-- NORMAL --", "-- VISUAL --", "-- REPLACE --",
	"(shift+tab to cycle)", "shift+tab to cycle",
	"? for shortcuts", "esc to interrupt",
}

// claudeFooterSegment reports whether one " · "-separated segment of a
// Claude footer row is composed entirely of Claude's own footer phrases. The
// mode flag ("⏵⏵ bypass permissions on") carries free wording between "⏵⏵"
// and its trailing " on"; the agent and shell counts are whole segments.
func claudeFooterSegment(seg string) bool {
	rest := strings.TrimSpace(seg)
	for rest != "" {
		switch {
		case strings.HasPrefix(rest, "⏵⏵ "):
			body := strings.TrimPrefix(rest, "⏵⏵ ")
			i := strings.Index(body, " on")
			if i <= 0 {
				return false
			}
			rest = strings.TrimSpace(body[i+len(" on"):])
		case strings.HasPrefix(rest, "← "):
			// the agent count is always the segment's tail
			tail := strings.TrimPrefix(rest, "← ")
			return tail == "for agents" || claudeCountWord(tail, "agent")
		default:
			matched := false
			for _, p := range claudeFooterPhrases {
				if r, ok := strings.CutPrefix(rest, p); ok {
					rest = strings.TrimSpace(r)
					matched = true
					break
				}
			}
			if !matched {
				return claudeCountWord(rest, "shells") || claudeContextLeft(rest)
			}
		}
	}
	return true
}

// claudeCountWord reports whether s is a count plus a unit — "1 agent",
// "2 agents", "2 shells" — the trailing counts Claude's footer carries.
func claudeCountWord(s, unit string) bool {
	f := strings.Fields(s)
	return len(f) == 2 && digitsOnly(f[0]) && strings.HasPrefix(f[1], unit)
}

// claudeContextLeft reports whether s is the context meter — "87% context
// left".
func claudeContextLeft(s string) bool {
	f := strings.Fields(s)
	return len(f) == 3 && f[1] == "context" && f[2] == "left" &&
		strings.HasSuffix(f[0], "%") && digitsOnly(strings.TrimSuffix(f[0], "%"))
}

// devinComposerFooterRow reports whether raw is a row of Devin's composer
// footer against its whole-row grammar: the model and mode segments plus the
// shells/edit hint, as "SWE-2 Max · bypass · 2 shells ↓ ctrl+o to expand
// edits" draws them.
func devinComposerFooterRow(raw string) bool {
	line := strings.TrimSpace(raw)
	if line == "" {
		return false
	}
	for _, seg := range strings.Split(line, " · ") {
		if !devinFooterSegment(seg) {
			return false
		}
	}
	return true
}

// devinFooterSegment reports whether one " · "-separated segment of Devin's
// footer row is the model token ("SWE-2", the first word always carries a
// version digit), a mode word, or the shells/edit hint tail.
func devinFooterSegment(seg string) bool {
	seg = strings.TrimSpace(seg)
	if seg == "" {
		return false
	}
	if strings.HasSuffix(seg, "ctrl+o to expand edits") {
		head := strings.TrimSpace(strings.TrimSuffix(seg, "ctrl+o to expand edits"))
		if head == "" || head == "↓" {
			return true
		}
		f := strings.Fields(head)
		return len(f) == 3 && digitsOnly(f[0]) && f[1] == "shells" && f[2] == "↓"
	}
	f := strings.Fields(seg)
	if strings.ContainsAny(f[0], "0123456789") {
		return true
	}
	if len(f) == 1 {
		switch f[0] {
		case "bypass", "plan", "act", "build", "auto", "agent":
			return true
		}
	}
	return false
}
