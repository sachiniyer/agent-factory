package tmux

import (
	"fmt"
	"slices"
	"strings"
)

// stillChipTailAudit reports why the rows below a composer region break the
// still-chip proof, or "" when the frame bounds the block for the proof.
// A ruled or boxed composer — Claude's "❯", Devin's "❭", or the │-boxed ">" —
// closes only at the agent's OWN edge: the row byte-identical to the
// composer's top edge, with only BLANK rows between the region's cut and
// that edge, and chrome below it because no draft can draw outside the
// composer. Codex's "›" lane has no closing edge, so every content row
// below its region must parse Codex's closed footer token grammar — a
// known model token, a whole-segment path token, known literals and
// meters, with no free-text segment (#7850 R2/N1). A draft that opened
// with blank lines, or a second draft line after the absorbed Enter's
// newline, renders the same shape as a footer — the remedy Enter would
// submit it with our prompt.
func stillChipTailAudit(rows []string, anchor, end int) string {
	switch glyph := anchorGlyph(rows[anchor]); glyph {
	case claudeComposerGlyph, "❭", ">":
		// A ruled or boxed composer: blanks until the agent's own closing
		// edge — the LAST row byte-identical to it, with only blank rows
		// between it and the region. A row the user typed, or a wrapped
		// continuation, that happens to equal the edge cannot close the
		// composer early and excuse a draft beneath it: the real edge still
		// sits below it in the frame (#7913 R4). Everything below the edge
		// is chrome by geometry.
		edge := composerClosingEdge(rows, anchor, glyph)
		last := -1
		for j := end; j < len(rows); j++ {
			if stillChipClosingEdge(rows[j], edge) {
				last = j
			}
		}
		for j := end; j < len(rows); j++ {
			raw := rows[j]
			if j == last {
				return ""
			}
			if !blankComposerRow(raw) {
				return fmt.Sprintf("the composer holds content %q inside the composer that is not provably blank", normalizeDelivery(raw))
			}
		}
		return ""
	default:
		// Codex's › lane: no closing edge, so every row below the region is
		// audited — blank, Codex's own column-0 "─" rule row, or the closed
		// footer grammar. When a rule also bounds the composer's top, a "─"
		// row below counts only when it is that rule byte-for-byte — a
		// different width is a draft row, not Codex's edge (#7912). With no
		// top rule (the transcript gap or the pane's own top bounds the
		// span) any column-0 "─" row is the one rule Codex's ruled lane
		// draws below its composer.
		topRule := ""
		if anchor > 0 && composerRuleRow(rows[anchor-1]) {
			topRule = rows[anchor-1]
		}
		for j := end; j < len(rows); j++ {
			raw := rows[j]
			switch {
			case blankComposerRow(raw):
			case composerRuleRow(raw) && (topRule == "" || raw == topRule):
			case codexComposerFooterRow(raw):
			default:
				return fmt.Sprintf("the composer holds content %q below a blank row that is not provably the agent's own footer", normalizeDelivery(raw))
			}
		}
		return ""
	}
}

// composerClosingEdge is the row that proves THIS composer's closing edge —
// byte-identical to the edge the composer drew above the anchor: for a ruled
// lane (Claude's ❯, Devin's ❭) the same "─" rule stillChipFirstRow required
// directly above it; for a │-boxed ">" the ╰───╯ twin of the ╭───╮ top edge,
// its exact "─" span with the bottom corners. "" when the frame cannot show
// the composer's own edge, in which case nothing below the cut can close it.
// Any other column-0 row of box-drawing glyphs — a single "─", a rule of a
// different width, a mistyped box bottom — is content, not the edge: under
// the column-0 wrap model the cursor proof accepts, a wrapped draft's
// continuation could draw one (#7912).
func composerClosingEdge(rows []string, anchor int, glyph string) string {
	if glyph == ">" {
		for r := anchor - 1; r >= 0; r-- {
			if composerBoxTopRow(rows[r]) {
				return composerBoxBottomTwin(rows[r])
			}
			if !blankComposerRow(rows[r]) {
				return ""
			}
		}
		return ""
	}
	if anchor > 0 && composerRuleRow(rows[anchor-1]) {
		return rows[anchor-1]
	}
	return ""
}

// composerBoxBottomTwin maps a ╭───╮ top edge to the ╰───╯ bottom edge the
// box draws beneath its composer — the same "─" span with the bottom
// corners.
func composerBoxBottomTwin(top string) string {
	rs := []rune(top)
	if len(rs) < 2 {
		return ""
	}
	return string(append(append([]rune{'╰'}, rs[1:len(rs)-1]...), '╯'))
}

// stillChipClosingEdge reports whether raw is the composer's own closing
// edge — byte-identical to the expected edge row composerClosingEdge derived
// from the top edge above the anchor. A column-0 all-"─" row of any other
// width is content, not the composer's own (#7912).
func stillChipClosingEdge(raw, edge string) bool {
	return edge != "" && raw == edge
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
// below, followed only by tier words from Codex's effort enum, the catalog's
// enumerated codename words, and ServiceTier's service-tier words — the way
// codex-rs draws "<display_name> <effort> [<service_tier>]" ("GPT-6.1 Terra
// ultra", "GPT-6 Sol ultra", "gpt-5.6-sol max", "o3", "gpt-5.1-codex-max
// xhigh", "gpt-5.4 high fast"). It is a closed vocabulary: a draft's
// "gpt-5 Ship It", "GPT Deploy Prod" or "v2 ok go" matches no family, and
// "1. see my notes" never did (#7850 N1, #7912, #7913 R2) — failing closed
// on an odd model name is fine.
func codexStatusModel(seg string) bool {
	f := strings.Fields(strings.TrimSpace(seg))
	if len(f) == 0 || len(f) > 3 {
		return false
	}
	if !codexModelToken(f[0]) {
		return false
	}
	for _, w := range f[1:] {
		if !codexModelTier(w) && !codexModelCodename(w) && !codexServiceTier(w) {
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
		// The GPT- line carries at most one codename inside the token
		// ("GPT-6-Astra", "GPT-6-Sol"); a second "-word" is a chain no
		// real id draws — "GPT-5-Terra-Astra" (#7913 R5).
		return len(strings.Split(rest, "-")) <= 2 &&
			codexModelVersionTail(rest, codexModelCodenames)
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

// codexModelCodenames are the codename words the model segment carries — the
// display names in codex-rs's model catalog: Sol, Luna, Terra, Astra and
// Mini (codex-rs/core/src/amazon_bedrock/catalog.rs;
// codex-rs/startup_prompts.rs maps OpenAI's default gpt-5.4 to "GPT-6 Sol").
// Each can be its own segment word ("GPT-6.1 Terra ultra", "GPT-6 Sol
// ultra") or sit inside a GPT- model token ("GPT-6-Astra", "GPT-5.4-Mini").
// A draft's "Ship" or "Deploy" is not a codename (#7912, #7913 R2).
var codexModelCodenames = []string{"Sol", "Luna", "Terra", "Astra", "Mini"}

// codexModelVersionTail reports whether rest is a model family's versioned
// tail: a numeric version ("5", "5.1", "6.1") followed only by "-"-joined
// words from the family's own enumerated suffixes, each appearing at most
// once and in the id's order — "gpt-5.1-codex-mini" is a real id while
// "gpt-5-codex-codex-mini" repeats and "gpt-5-mini-codex" is out of order
// (#7913 R5).
func codexModelVersionTail(rest string, suffixes []string) bool {
	prev := -1
	for i, p := range strings.Split(rest, "-") {
		if i == 0 {
			if !codexModelVersion(p) {
				return false
			}
			continue
		}
		idx := slices.Index(suffixes, p)
		if idx < 0 || idx <= prev {
			return false
		}
		prev = idx
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

// codexServiceTier reports whether w is a service-tier word the model
// segment carries after the effort word — codex-rs's ServiceTier is Fast |
// Flex, drawn lowercase in the footer when fast mode is on ("gpt-5.4 xhigh
// fast", "gpt-5.4 high fast"; codex-rs/protocol/src/config_types.rs).
func codexServiceTier(w string) bool {
	return w == "fast" || w == "flex"
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
