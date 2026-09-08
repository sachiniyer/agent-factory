package session

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// RootSessionTitle is the reserved title of the always-ensured root agent
// (#1106): an in-place session the daemon creates at the repo root for repos
// opted in via the [root_agent] project profile, and re-creates when it dies.
const RootSessionTitle = "root"

// IsReservedTitle reports whether a session title IS the root agent's — the
// identity question, asked of records that already exist: whether to pin the row
// to the top of the sidebar, to skip Lost-restore, to arm the re-create grace
// window. Matching is case-insensitive on the trimmed title so "Root"/" ROOT "
// cannot masquerade as a distinct session next to the reserved one.
//
// It is deliberately NOT the admission question — see ReservedTitleCollision.
// Widening this predicate would hand an ordinary session that merely derives the
// root's runtime name the root agent's whole lifecycle (kill grace, limit
// resume, archive refusal, IsRoot in every client), which is not what it is.
func IsReservedTitle(title string) bool {
	return strings.EqualFold(strings.TrimSpace(title), RootSessionTitle)
}

// ReservedTitleCollision returns the reserved title a candidate would take the
// name of, or "" when the candidate claims nothing reserved. It is the
// ADMISSION question — "may a create claim this title?" — which is strictly
// wider than IsReservedTitle, because a title can claim the reserved session's
// runtime name without being spelled like it (#3732).
//
// Two rules, and both are load-bearing:
//
//   - The spelling. "Root"/" ROOT " derive DIFFERENT tmux names (toTmuxName
//     preserves case), so only the fold below catches them — and they must be
//     caught, because IsReservedTitle already treats such a session as the root
//     agent everywhere else.
//   - The derived name. toTmuxName DELETES interior whitespace while the fold
//     above only trims it, so "ro ot" is not the reserved spelling yet derives
//     the identical tmux session name as "root" — and the tmux name, not the
//     title, is what markers, generation cohorts and scope prefixes are keyed
//     on. Two titles that derive one name are two sessions the daemon keys as
//     one.
//
// The comparison is repo-free on purpose: both sides would take the same repo
// prefix, so it cancels, and a caller that has no repo path handy (the API's
// fail-fast pre-check, the TUI naming overlay) asks the identical question.
func ReservedTitleCollision(title string) string {
	if IsReservedTitle(title) {
		return RootSessionTitle
	}
	if tmux.SanitizedNameForRepo(title, "") == tmux.SanitizedNameForRepo(RootSessionTitle, "") {
		return RootSessionTitle
	}
	return ""
}

// ReservedTitleRefusal returns the error a create must fail with when the title
// claims a reserved name, or nil when the title is free. Every creation path
// lands on the daemon's authoritative gate, but the API pre-check refuses the
// same titles a round trip earlier; both call this so the wording cannot drift
// apart the way two copies of the message already had.
//
// The derived-name refusal names BOTH titles and says why they are one name:
// "ro ot" and "root" look nothing alike on a sidebar row, so a refusal that
// only said "reserved" would read as a bug.
func ReservedTitleRefusal(title string) error {
	return ReservedTitleRefusalFor(title, "")
}

// ReservedTitleRefusalFor is ReservedTitleRefusal with the resolved repository
// workspace path. A known path is shell-quoted in both remedy commands so they
// target that repository regardless of the caller's cwd. An empty path retains
// the instruction to run the commands from the intended repository.
func ReservedTitleRefusalFor(title, repoPath string) error {
	reserved := ReservedTitleCollision(title)
	if reserved == "" {
		return nil
	}
	pathArg, context := ".", "from this repo "
	if repoPath != "" {
		pathArg, context = config.ShellQuotePath(repoPath), ""
	}
	remedy := fmt.Sprintf("pick another name (on the daemon host, with AF_DAEMON_URL unset and without --daemon-url, %srun "+
		"`af projects add %s`, then enable its personal [root_agent] profile with "+
		"`af config set --project %s root_agent '{\"enabled\":true}'`; restart the daemon to apply)", context, pathArg, pathArg)
	if IsReservedTitle(title) {
		return fmt.Errorf("session title %q is reserved for the daemon-managed root agent; %s", title, remedy)
	}
	return fmt.Errorf("session title %q is reserved for the daemon-managed root agent: tmux session names drop whitespace, so %q and the reserved title %q are the same session to tmux; %s",
		title, title, reserved, remedy)
}
