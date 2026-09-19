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
// it would claim and compare it against this — IsReservedTitle byte-exact,
// ReservedTitleCollision case-folded, so the admission gate stays a strict
// superset of the identity a record can hold.
var reservedTmuxName = tmux.SanitizedNameForRepo(RootSessionTitle, "")

// IsReservedTitle reports whether a session title IS the root agent's. Two
// claims count, and they are deliberately a subset of what admission refuses:
//
//   - a title that derives the reserved tmux session name byte-for-byte.
//     toTmuxName DELETES whitespace, so "ro ot" mints the identical af_root
//     the root itself holds (#3732). Two records cannot own one tmux session:
//     markers, generation cohorts and scope prefixes already conflate that
//     record with the root, so reading it as the reserved session is the only
//     coherent answer.
//   - a title the pre-#4396 spelling rule caught — trim-and-fold on the title
//     itself ("Root", " ROOT "). Admission has refused those long enough that
//     a record holding one can only predate the gate.
//
// What is NOT counted is the rest of the admission superset: a whitespace-
// interleaved CASE VARIANT like "Ro ot" derives a case-DISTINCT tmux name
// (af_Root — tmux session names are case-sensitive), so it was admissible
// before #4396 and can sit in storage as an ordinary session beside the root.
// Folding case on the derived name would reclassify that record as a root it
// is not — unarchivable (daemon/archive.go), refused handoff and limit-resume,
// and arming rootKilledAt on kill against a repo whose real root it is not.
//
// It is asked of records that already exist — whether to pin the row to the
// top of the sidebar, to arm the re-create grace window. Whether to skip
// Lost-restore is the narrower IsReservedTitleSpelling question.
// The admission question (ReservedTitleCollision) is the wider one: a create
// may not take even a lookalike of the reserved name, while a record that
// predates admission keeps the identity its tmux name actually claims.
//
// IsReservedTitle answers the LOCAL-tmux question: whether the title claims
// the reserved name in the local tmux namespace. That is the right form for
// admission-time callers and for a record bound to the local backend; a record
// whose persisted backend is known must go through IsReservedRecordTitle.
func IsReservedTitle(title string) bool {
	return IsReservedRecordTitle(title, config.BackendLocal)
}

// IsReservedRecordTitle is IsReservedTitle asked of a persisted record whose
// backend type is known (InstanceData.BackendType / Instance.BackendType). The
// derived-tmux-name clause only applies to a record that claims a name in the
// LOCAL tmux namespace ("" or "local"): a separately provisioned backend
// (docker/ssh/sandbox/remote) has no local tmux name for af_root to collide
// with, so a pre-#3732 remote record titled "ro ot" keeps ordinary identity —
// archivable, recoverable, and unable to arm rootKilledAt — while the spelling
// clause still marks a remote "root" as the reserved session it is.
func IsReservedRecordTitle(title, backendType string) bool {
	if IsReservedTitleSpelling(title) {
		return true
	}
	if backendType == "" || backendType == config.BackendLocal {
		return tmux.SanitizedNameForRepo(title, "") == reservedTmuxName
	}
	return false
}

// IsReservedTitleSpelling reports whether a title is SPELLED as the reserved
// one — trimmed and case-folded ("root", " ROOT ") — without the derived-name
// clause. It is IsReservedRecordTitle's spelling half, and the whole of the
// pre-#4396 IsReservedTitle.
//
// Ordinary Lost/Dead recovery withholds itself from exactly these records and
// no more (#4407 review). The withholding hands recovery to the root ensure
// loop, but that loop looks up only the exact "root" key, and its re-create is
// refused while any record already claims the root's tmux name. A local
// derived-name record ("ro ot") is therefore one the loop can neither find nor
// replace: withholding ordinary recovery from it too would leave a recoverable
// worktree with kill as its only exit.
func IsReservedTitleSpelling(title string) bool {
	return strings.EqualFold(strings.TrimSpace(title), RootSessionTitle)
}

// ReservedTitleCollision returns the reserved title a candidate would claim,
// or "" when the candidate claims nothing reserved. It is the ADMISSION
// question — "may a create claim this title?" — asked of a title that does not
// exist yet. It is deliberately a superset of IsReservedTitle: the case-fold
// on the derived name also refuses a case variant like "Ro ot" whose tmux name
// (af_Root) is genuinely distinct, because a new session admitted under a
// lookalike spelling is the confusion the reserve exists to prevent — while an
// existing record under that title keeps the identity its tmux name actually
// claims. The string result names the reserved title in refusals.
func ReservedTitleCollision(title string) string {
	if strings.EqualFold(tmux.SanitizedNameForRepo(title, ""), reservedTmuxName) {
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
