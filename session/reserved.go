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

// reservedTmuxName is the tmux session name the reserved title claims in the
// repo-free namespace (the repo prefix cancels on both sides of the
// comparison, so a caller with no repo path handy asks the identical
// question). Both reserved-title predicates normalize a candidate to the name
// it would claim and compare it against this — one normalization, so the
// identity question and the admission question can no longer diverge the way
// they did before #4396.
var reservedTmuxName = tmux.SanitizedNameForRepo(RootSessionTitle, "")

// IsReservedTitle reports whether a session title IS the root agent's. What is
// reserved is the DERIVED tmux session name, not the spelling (#3732):
// toTmuxName DELETES whitespace, so "ro ot" claims the identical tmux session
// name as "root" — and the tmux name, not the title, is what markers,
// generation cohorts and scope prefixes key on. The comparison folds case on
// the derived name, so " Root ", "ROOT" and "Ro ot" cannot masquerade as a
// distinct session beside the reserved one.
//
// It is asked of records that already exist — whether to pin the row to the
// top of the sidebar, to skip Lost-restore, to arm the re-create grace window
// — and it is equally the admission question (see ReservedTitleCollision).
// The two share this normalization on purpose: a session af admits under a
// title it calls the root is exactly the incoherence the admission rule exists
// to prevent, so a create may not take a title the daemon would project as the
// root agent.
//
// The widened identity is deliberate for the shapes it adds over the old
// trim-and-fold rule. A record titled "ro ot" can only predate the #3732
// admission rule — the create gate has refused it since — and its tmux name
// already collides with the root's, so every tmux-keyed mechanism treated it
// as the same session anyway. Reading it as the reserved session is the
// coherent answer for that record.
func IsReservedTitle(title string) bool {
	return strings.EqualFold(tmux.SanitizedNameForRepo(title, ""), reservedTmuxName)
}

// ReservedTitleCollision returns the reserved title a candidate would claim,
// or "" when the candidate claims nothing reserved. It is the ADMISSION
// question — "may a create claim this title?" — asked of a title that does not
// exist yet, and since #4396 it is the same question IsReservedTitle asks of a
// record that does. The string result names the reserved title in refusals.
func ReservedTitleCollision(title string) string {
	if IsReservedTitle(title) {
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
// The refusal names BOTH titles and says why they are one name: "ro ot" and
// "root" look nothing alike on a sidebar row, so a refusal that only said
// "reserved" would read as a bug.
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
	return fmt.Errorf("session title %q is reserved for the daemon-managed root agent: af reserves the tmux session name a title derives, not its spelling — tmux session names drop whitespace and the comparison folds case — so %q claims the reserved name %q; %s",
		title, title, reserved, remedy)
}
