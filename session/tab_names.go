package session

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// A tab has TWO names in two separate namespaces, and #1957 is what happens when
// they are collapsed into one:
//
//   - its NAME — the handle a user types and every tab verb resolves against.
//     Unique among the CURRENT names on the roster, and only those.
//   - its TMUX SESSION NAME — "<agent sanitized>__<token>", frozen at spawn
//     because restore rebinds by the persisted TmuxName and the TUI keys live
//     panes by it. Unique among the LIVE tmux sessions of the roster's tabs.
//
// Before rename existed the two could never diverge, so one namespace sufficed.
// #1813 made rename reachable, and the first fix kept a single namespace by
// ALSO reserving each tab's live tmux token as a name: renaming "fresh" to
// "fresh-old" left "fresh" un-takeable, so `tab-create --name fresh` silently
// answered "fresh-2" with nothing on the roster named "fresh" (#1957). The
// reservation was protective — without it the new tab re-derived the renamed
// tab's still-live "…__fresh" session — but it charged the user's namespace for
// a collision in tmux's.
//
// Splitting them pays the same debt honestly: the name is free the moment its
// tab stops using it, and the SPAWN is what walks to "…__fresh-2" to miss the
// live session. Nothing outside this file needs the two to agree — restore and
// the snapshot reconcile bind by the persisted TmuxName, and CreateTab reports
// the spawned tmux name back so the TUI's instant-display attach binds to the
// exact session rather than re-deriving one from the name.

// uniqueShellName returns a shell-tab name not already used by any tab
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
// (N>=2), such that the result collides with no other tab's CURRENT name.
// exclude, when non-nil, is skipped entirely — it is the tab being renamed,
// which must be allowed to keep its own name (a no-op rename), and which no
// other tab can have taken from it.
//
// It reads the live roster and nothing else, so a name stops being taken the
// instant its tab is renamed away from it or closed. Collisions in the tmux
// namespace are not this function's business: uniqueTabTmuxName handles them
// where they arise, at spawn.
//
// This is the shared collision handling for shell tabs (AddShellTab), CLI-spawned
// process tabs (AddProcessTab), web tabs (AddWebTab), and renames (RenameTab).
func uniqueTabNameExcluding(tabs []*Tab, base string, exclude *Tab) string {
	used := make(map[string]bool, len(tabs))
	for _, t := range tabs {
		if t == exclude {
			continue
		}
		used[t.Name] = true
	}
	return firstFreeName(used, base)
}

// uniqueTabTmuxName returns the tmux session name to spawn a new sibling tab
// under: the agent session's sanitized name, the "__" separator, and a token
// starting at base that collides with no live tmux session on the roster.
//
// base is the new tab's already-resolved display NAME, so the two normally
// agree and a session stays greppable by the tab it belongs to. They diverge
// only when a rename has left an older tab's live session holding the token —
// the #1957 sequence — and then it is the spawned session, not the user's
// requested name, that takes the "-2".
//
// agentSanitized is passed in rather than read off tabs[0] because the caller
// (tab_spawn.go) already holds the agent session it is spawning a sibling of,
// and an instance with no agent session cannot spawn a sibling at all.
func uniqueTabTmuxName(tabs []*Tab, agentSanitized, base string) string {
	prefix := agentSanitized + tmuxTabSeparator
	used := make(map[string]bool, len(tabs))
	for _, t := range tabs {
		if token := tabTmuxToken(prefix, t); token != "" {
			used[token] = true
		}
	}
	return prefix + firstFreeName(used, base)
}

// firstFreeName returns base, or base with the lowest "-N" suffix (N>=2) that
// used does not contain. Shared by both namespaces so a name and a tmux token
// suffix identically ("fresh" -> "fresh-2"), which is what keeps the suffixed
// spelling readable wherever it shows up.
func firstFreeName(used map[string]bool, base string) string {
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

// tabTmuxToken returns the EXACT name token a tab's live tmux session was derived
// from at spawn: its session name with the agent prefix stripped. Because a
// session is always "<agent sanitized>__<token>" by construction (tab_spawn.go),
// stripping the prefix recovers the spawn-time token whole — even when that token
// itself contains "__" (a sanitized name may, since sanitizeTabName keeps '_'),
// e.g. "logs__api" derives session "…__logs__api" whose token is "logs__api", NOT
// the "api" a split-on-last-"__" would wrongly yield and leave free for a
// colliding spawn. It is this spawn-time token, not the tab's current name (which
// a rename decouples from it), that a later spawn would collide with.
//
// Empty when there is no live session to reserve against: a web/vscode tab (no
// PTY), an unstarted tab, the agent tab, or an instance with no agent session
// (agentPrefix == ""). The HasPrefix guard is belt-and-suspenders — by
// construction every sibling carries the prefix — and declines to reserve a token
// it can't derive honestly rather than reserving a wrong one.
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
// Keep the tab token inside the same conservative ASCII subset as the agent
// name's positive tmux policy. Anything outside [A-Za-z0-9_-] becomes a single
// '-'.
var tabNameUnsafe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// sanitizeTabName converts a requested or derived tab name into a token safe to
// embed in a tmux session name and stable across a save/restore round-trip.
// Returns "" when nothing usable remains so callers can fall back to a default.
func sanitizeTabName(name string) string {
	return strings.Trim(tabNameUnsafe.ReplaceAllString(name, "-"), "-")
}

// processTabBaseName picks the base name for a new Process tab: the
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
