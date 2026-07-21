package tree

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/session"
)

// Tab-kind glyphs (#1813). Where render.go's status glyphs say how a session is
// doing, these say what a tab *is*, so a tab bar reads at a glance without
// parsing names. Shell and Process deliberately share `›`: a process tab is a
// terminal that happens to run one command, and the thing that distinguishes
// them — the command name — is already the label text beside the glyph.
//
// This block is the canonical definition; the web client copies the values
// verbatim, the mirror of the convention in web/src/status.ts (which copies
// render.go's status glyphs). Keep the two in sync.
//
// AgentTabGlyph is intentionally the same `◆` as render.go's limitIcon, which
// marks a session blocked at a usage-limit wall. The two never meet, and the
// renderer — not this comment — is what keeps them apart, in two ways at once:
//
//   - Different COLUMNS. limitIcon is a status glyph, rendered into the row's
//     right-hand column (render.go's `join`, placed after a Place(width-3)); a
//     tab glyph prefixes a tab label at the far left. At any usable width they
//     sit most of the row apart.
//   - Different ROWS. A status glyph is on an *instance* row, a tab glyph on a
//     *tab* child, and an instance's branch line plus a blank spacer separate
//     the two even when the instance is expanded.
//
// So an expanded limit-blocked instance reads (width 80):
//
//	▸  [limit] resets 2:30pm alpha                                        ◆
//	   ⎇-dev/alpha
//
//	  ├ 1 ◆ Agent
//
// Note what the left-hand `[limit] resets 2:30pm` is and isn't: it is a TEXT
// title prefix (render.go's limitBadgePrefix), which exists so the limit state
// survives low contrast and color-blindness — not a `◆ [limit] title` string.
// Nothing renders the diamond adjacent to it. The rule to preserve if this
// layout ever changes: right column = how a session is DOING, left glyph = what
// a tab IS.
const (
	AgentTabGlyph   = "◆"
	ShellTabGlyph   = "›"
	ProcessTabGlyph = "›"
	WebTabGlyph     = "◱"
)

// TabGlyph returns the glyph for a tab kind. Unknown kinds fall back to
// ProcessTabGlyph, matching labelForTab's default arm: an unrecognized kind is
// some command running in the worktree, which is what `›` already says.
//
// TabKindVSCode (#1817) shares WebTabGlyph deliberately, on the same rule that
// has Shell and Process share `›`: the glyph names what a tab IS, and a VS Code
// tab is an embedded browser surface with no PTY — a web pane whose page happens
// to be an editor. What separates the two is the label text beside the glyph
// ("VS Code" vs the target's name), exactly as the command name separates a
// process tab from a shell. Falling through to the default arm would have called
// it a terminal, which is the one thing it is not.
func TabGlyph(kind session.TabKind) string {
	switch kind {
	case session.TabKindAgent:
		return AgentTabGlyph
	case session.TabKindShell:
		return ShellTabGlyph
	case session.TabKindWeb, session.TabKindVSCode:
		return WebTabGlyph
	default:
		return ProcessTabGlyph
	}
}

// placeholderTabLabels is the bar shown before an instance is selected or
// before its tabs have materialized (mid-start/Loading). It is a single slot:
// the agent tab is the only tab every instance is guaranteed to have —
// terminal tabs exist on demand only (#1100) — so promising a second slot
// before the real tab list exists would manufacture a phantom jump/attach
// target in every consumer of TabLabels.
var placeholderTabLabels = []string{AgentTabGlyph + " Agent"}

// TabLabels returns the display labels for an instance's tab slots — the
// single source of truth shared by the TabbedWindow's header, the sidebar
// tree (#1024 PR 3), and the 1-9 jump keys, so slot numbering can never
// disagree between them. Each label is a kind glyph plus text: agent tabs
// render as "◆ Agent", shell tabs as "› Terminal"; any Process tab renders
// under its own name ("› btop"). See labelForTab.
//
// Once an instance's tabs have materialized the labels mirror the real tab
// list exactly, one slot per tab — local and remote alike. Since #1100 a
// fresh local instance starts with only its agent tab, so a fresh instance
// shows a single slot until `t` adds a terminal; padding to a two-slot
// minimum (the pre-#1100 behavior) would advertise a dead "Terminal" slot.
// A nil instance or one with no tabs yet yields the single-slot placeholder;
// the result is never empty.
func TabLabels(instance *session.Instance) []string {
	if instance != nil {
		if tabs := instance.GetTabs(); len(tabs) > 0 {
			labels := make([]string, len(tabs))
			for i, tab := range tabs {
				labels[i] = labelForTab(tab)
			}
			return labels
		}
	}
	return append([]string(nil), placeholderTabLabels...)
}

// TabLabelAt returns the presentation label for a real tab and prefixes its
// 1-based jump slot when another tab has the same label. The tree already
// renders that slot beside every child row; pane-facing surfaces need it only
// when labels collide, so a numbered jump between two default Terminal tabs
// produces a visibly different identity without adding noise to ordinary
// single-Agent/single-Terminal panes (#2150).
//
// It reports false when the instance's real tab list cannot answer: no tabs have
// materialized yet, or idx is out of range. Unlike TabLabels it never falls back
// to the placeholder slot — a caller that names a pane TO THE USER must be able
// to tell "this is the Agent tab" from "no tab list exists yet", because the
// placeholder's "Agent" is a guess and naming the wrong pane is worse than not
// naming one (#1997). Best-effort renderers may apply their existing placeholder
// fallback after checking the bool.
func TabLabelAt(instance *session.Instance, idx int) (string, bool) {
	if instance == nil {
		return "", false
	}
	tabs := instance.GetTabs()
	if idx < 0 || idx >= len(tabs) {
		return "", false
	}
	label := labelForTab(tabs[idx])
	for candidateIdx, candidate := range tabs {
		if candidateIdx != idx && labelForTab(candidate) == label {
			return fmt.Sprintf("%d %s", idx+1, label), true
		}
	}
	return label, true
}

// labelForTab is the kind glyph, a space, then the tab's text. Every consumer
// treats a label as an opaque display string — the tab bar, the tree rows, and
// the preview/selection hints all render it whole, and the 1-9 jump keys index
// the slice without reading it — so the glyph is baked into the label rather
// than exposed for each consumer to compose. Composing separately would hand
// three call sites the chance to disagree, which is the thing TabLabels exists
// to prevent. TabGlyph stays exported for anyone who needs the glyph alone.
func labelForTab(tab *session.Tab) string {
	return TabGlyph(tab.Kind) + " " + textForTab(tab)
}

// textForTab delegates to session.TabLabel, the single definition of what a user
// SEES for a tab's text. It lives beside the Tab type because the label is
// presentation-only and deliberately differs from the name for agent/shell tabs
// (#1986); keeping "what a user reads" next to "what a user types" is what lets a
// "no tab named …" error surface the real name when the two differ, rather than
// asserting a visible tab is absent (#1984). The label is never resolved against
// (session.TabMatches keys on Name alone), so it is free to be the pretty string.
func textForTab(tab *session.Tab) string {
	return session.TabLabel(tab)
}
