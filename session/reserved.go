package session

import "strings"

// RootSessionTitle is the reserved title of the always-ensured root agent
// (#1106): an in-place session the daemon creates at the repo root for repos
// opted in via the root_agents config key, and re-creates when it dies.
const RootSessionTitle = "root"

// IsReservedTitle reports whether a session title is reserved for the
// daemon-managed root agent and therefore unavailable to normal session
// creation (TUI, CLI, API, task runs). Matching is case-insensitive on the
// trimmed title so "Root"/" ROOT " cannot masquerade as a distinct session
// next to the reserved one.
func IsReservedTitle(title string) bool {
	return strings.EqualFold(strings.TrimSpace(title), RootSessionTitle)
}
