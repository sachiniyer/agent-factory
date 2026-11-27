package tmux

import (
	"fmt"
	"slices"
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
// line is a model segment: a model token from one of the enumerated families
// below, followed only by tier words from Codex's effort enum or the
// fixtures' enumerated codename words ("GPT-6.1 Terra ultra",
// "gpt-5.6-sol max", "o3", "gpt-5.1-codex-max xhigh"). It is a closed
// vocabulary: a draft's "gpt-5 Ship It", "GPT Deploy Prod" or "v2 ok go"
// matches no family, and "1. see my notes" never did (#7850 N1, #7912) —
// failing closed on an odd model name is fine.
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

// codexModelToken reports whether w is a model token from one of the
// enumerated Codex model families — a fixed head, a numeric version, and
// only that family's own suffixes, never a bare prefix:
//
//   - "gpt-<ver>": the gpt-5 line — "gpt-5", "gpt-5-codex", "gpt-5.1",
//     "gpt-5.1-codex", "gpt-5.1-codex-mini", "gpt-5.1-codex-max" and
//     "gpt-5.2-codex" (codex-rs's real model ids), plus the fixtures' own
//     draws "gpt-5.6-sol" and "gpt-5.6-luna";
//   - "GPT-<ver>": the fixtures' capitalized draws — "GPT-6.1",
//     "GPT-6-Astra";
//   - "o<digit>": the o-series — "o3", "o4-mini";
//   - the codex-mini line — "codex-mini", "codex-mini-latest".
//
// A draft's "gpt-please-force-push-release", "codex-then-delete-…", "GPTs"
// or bare "GPT" shares a family's prefix and matches none (#7912).
func codexModelToken(w string) bool {
	if rest, ok := strings.CutPrefix(w, "gpt-"); ok {
		return codexModelVersionTail(rest, codexGptLowerSuffixes)
	}
	if rest, ok := strings.CutPrefix(w, "GPT-"); ok {
		return codexModelVersionTail(rest, codexModelCodenames)
	}
	if w == "codex-mini" || w == "codex-mini-latest" {
		return true
	}
	if len(w) >= 2 && w[0] == 'o' && w[1] >= '0' && w[1] <= '9' {
		return w[2:] == "" || w[2:] == "-mini"
	}
	return false
}

// codexGptLowerSuffixes are the gpt- family's own suffix words after the
// version: the "-codex" line's "codex", "mini" and "max" (codex-rs's real
// gpt-5-codex, gpt-5.1-codex-mini and gpt-5.1-codex-max ids), and the
// fixtures' codename draws "sol" and "luna" (gpt-5.6-sol, gpt-5.6-luna).
var codexGptLowerSuffixes = []string{"codex", "mini", "max", "sol", "luna"}

// codexModelCodenames are the capitalized codename words the fixtures' model
// segments carry — as their own word ("GPT-6.1 Terra ultra") or inside the
// model token ("GPT-6-Astra"). Both are attested by this repo's Codex footer
// captures; a draft's "Ship" or "Deploy" is not a codename (#7912).
var codexModelCodenames = []string{"Terra", "Astra"}

// codexModelVersionTail reports whether rest is a model family's versioned
// tail: a numeric version ("5", "5.1", "6.1") followed only by "-"-joined
// words from the family's own enumerated suffixes.
func codexModelVersionTail(rest string, suffixes []string) bool {
	for i, p := range strings.Split(rest, "-") {
		if i == 0 {
			if !codexModelVersion(p) {
				return false
			}
			continue
		}
		if !slices.Contains(suffixes, p) {
			return false
		}
	}
	return true
}

// codexModelVersion is a family's numeric version: digits with at most one
// minor point — "5", "5.1", "6.1".
func codexModelVersion(s string) bool {
	parts := strings.Split(s, ".")
	if len(parts) > 2 {
		return false
	}
	for _, p := range parts {
		if !digitsOnly(p) {
			return false
		}
	}
	return true
}

// codexModelTier reports whether w is a tier word from Codex's effort enum —
// the ReasoningEffort set codex-rs draws after the model token
// (codex-rs/protocol/src/openai_models.rs): none, minimal, low, medium,
// high, xhigh, max, ultra, persistent. The enum's open Custom(_) arm stays
// closed — an effort word af does not know fails closed.
func codexModelTier(w string) bool {
	switch w {
	case "none", "minimal", "low", "medium", "high", "xhigh",
		"max", "ultra", "persistent":
		return true
	}
	return false
}

// codexModelCodename reports whether w is one of the enumerated codename
// words the model segment carries — codexModelCodenames — never just a
// capitalized word.
func codexModelCodename(w string) bool {
	return slices.Contains(codexModelCodenames, w)
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
		return len(f) == 2 && digitsOnly(f[0]) && (f[1] == "warning" || f[1] == "warnings")
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
