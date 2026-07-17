package session

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// uniqueShellName returns a shell-tab display name not already used by any tab
// in tabs: "shell", then "shell-2", "shell-3", ...
func uniqueShellName(tabs []*Tab) string {
	return uniqueTabName(tabs, shellTabName)
}

// uniqueTabName returns a name based on base that is free within tabs. See
// uniqueTabNameExcluding; this is the create-side spelling, where no existing
// tab is being re-named and so none is excluded.
func uniqueTabName(tabs []*Tab, base string) string {
	return uniqueTabNameExcluding(tabs, base, nil)
}

// uniqueTabNameExcluding returns base, or base with the lowest free "-N" suffix
// (N>=2), such that the result collides with neither an existing tab's display
// name nor an existing tab's live tmux token (see tabTmuxToken). exclude, when
// non-nil, is skipped entirely — it is the tab being renamed, which must be
// allowed to keep its own name (a no-op rename) and to reclaim its own tmux
// token, neither of which any other tab can have taken.
//
// Reserving the tmux token as well as the name upholds this invariant: no tab's
// NAME may equal another tab's live tmux token. That is what makes the tmux name
// derived for any name handed out here (agent session + "__" + name, see
// tab_spawn.go) guaranteed free. Before rename existed the two were always equal
// and reserving the name alone was sufficient; a rename decouples them (it
// deliberately does NOT rename the live tmux session — there is no tmux
// rename-session primitive here, and the TUI keys live panes by tmux name), so
// without the token reservation this sequence collides:
//
//	shell tab: Name="shell", tmux "af_x_T__shell" -> rename to "editor"
//	-> Name="editor", tmux still "af_x_T__shell" -> `t` finds "shell" free
//	-> new tab derives tmux "af_x_T__shell" -> collides with the live session.
//
// This is the shared collision handling for shell tabs (AddShellTab), CLI-spawned
// process tabs (AddProcessTab), web tabs (AddWebTab), and renames (RenameTab).
func uniqueTabNameExcluding(tabs []*Tab, base string, exclude *Tab) string {
	agentPrefix := agentTmuxPrefix(tabs)
	used := make(map[string]bool, 2*len(tabs))
	for _, t := range tabs {
		if t == exclude {
			continue
		}
		used[t.Name] = true
		if token := tabTmuxToken(agentPrefix, t); token != "" {
			used[token] = true
		}
	}
	if !used[base] {
		return base
	}
	for n := 2; ; n++ {
		candidate := fmt.Sprintf("%s-%d", base, n)
		if !used[candidate] {
			return candidate
		}
	}
}

// agentTmuxPrefix returns the tmux session-name prefix every non-agent tab's
// session extends — the agent tab's sanitized name plus the "__" separator
// (tab_spawn.go derives each sibling as agentSanitized + "__" + name). Empty when
// there is no agent session yet (an unstarted instance, or an empty tab list): a
// sibling can't exist without one, so there is nothing to reserve. Tabs[0] is the
// agent tab by invariant.
func agentTmuxPrefix(tabs []*Tab) string {
	if len(tabs) == 0 || tabs[0].tmux == nil {
		return ""
	}
	return tabs[0].tmux.SanitizedName() + tmuxTabSeparator
}

// tabTmuxToken returns the EXACT name token a tab's live tmux session was derived
// from at spawn: its session name with the agent prefix stripped. Because a
// session is always "<agent sanitized>__<name>" by construction (tab_spawn.go),
// stripping the prefix recovers the original spawn-time name whole — even when
// that name itself contains "__" (a sanitized name may, since sanitizeTabName
// keeps '_'), e.g. "logs__api" derives session "…__logs__api" whose token is
// "logs__api", NOT the "api" a split-on-last-"__" would wrongly yield and leave
// free for a colliding tab-create. It is this spawn-time token, not the tab's
// current display name (which a rename decouples from it), that a later
// tab-create would collide with.
//
// Empty when there is nothing to reserve: a web/vscode tab (no PTY), an unstarted
// tab, the agent tab, or an instance with no agent session (agentPrefix == "").
// The HasPrefix guard is belt-and-suspenders — by construction every sibling
// carries the prefix — and declines to reserve a token it can't derive honestly
// rather than reserving a wrong one.
func tabTmuxToken(agentPrefix string, t *Tab) string {
	if t == nil || t.Kind == TabKindAgent || t.tmux == nil || agentPrefix == "" {
		return ""
	}
	name := t.tmux.SanitizedName()
	if !strings.HasPrefix(name, agentPrefix) {
		return ""
	}
	return strings.TrimPrefix(name, agentPrefix)
}

// tabNameUnsafe matches any run of characters that must not appear in a tab's
// derived tmux session name (the agent session name + "__" + the tab name).
// tmux silently rewrites '.', ':', '#', '$' in session names, so a name
// containing them would not round-trip on restore; whitespace and path
// separators are likewise collapsed. Anything outside [A-Za-z0-9_-] becomes a
// single '-'.
var tabNameUnsafe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// sanitizeTabName converts a requested or derived tab name into a token safe to
// embed in a tmux session name and stable across a save/restore round-trip.
// Returns "" when nothing usable remains so callers can fall back to a default.
func sanitizeTabName(name string) string {
	return strings.Trim(tabNameUnsafe.ReplaceAllString(name, "-"), "-")
}

// processTabBaseName picks the base display name for a new Process tab: the
// sanitized requestedName when the caller passed --name, otherwise the sanitized
// basename of the command's first word ("/usr/bin/btop -t" -> "btop"). Falls back
// to "process" when neither yields a usable token.
func processTabBaseName(requestedName, command string) string {
	if base := sanitizeTabName(requestedName); base != "" {
		return base
	}
	if fields := strings.Fields(command); len(fields) > 0 {
		if base := sanitizeTabName(filepath.Base(fields[0])); base != "" {
			return base
		}
	}
	return "process"
}
