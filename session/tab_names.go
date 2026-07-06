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

// uniqueTabName returns base, or base with the lowest free "-N" suffix (N>=2),
// such that the result is not already a tab name in tabs. Tab names are unique
// per instance so each tab's derived tmux session name is collision-free. This
// is the shared collision handling for both shell tabs (AddShellTab) and
// CLI-spawned process tabs (AddProcessTab).
func uniqueTabName(tabs []*Tab, base string) string {
	used := make(map[string]bool, len(tabs))
	for _, t := range tabs {
		used[t.Name] = true
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
