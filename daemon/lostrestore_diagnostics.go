package daemon

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// The vanished-worktree diagnostic family for the lost-restore loop.
//
// logVanishedWorktreeOnce emits the structured WORKTREE_MISSING_DETECTED line
// once per distinct missing path per episode — the single evidence record a
// postmortem gets for a worktree that disappeared while a restore was trying
// to put it back. The helpers below it pick apart the
// WorktreeUnavailableError shape, dedupe per state, and render enum/bool
// fields for the structured line.

func (m *Manager) logVanishedWorktreeOnce(repoID string, st *lostRestoreState, inst *session.Instance, restoreErr error, teardownInFlight bool) {
	worktreePath, ok := missingWorktreePath(restoreErr)
	if !ok {
		return
	}

	m.mu.Lock()
	alreadyLogged := st.vanishedWorktreeWasLogged(worktreePath)
	if !alreadyLogged {
		if st.vanishedWorktreesLogged == nil {
			st.vanishedWorktreesLogged = make(map[string]struct{})
		}
		st.vanishedWorktreesLogged[worktreePath] = struct{}{}
	}
	m.mu.Unlock()
	if alreadyLogged {
		return
	}

	gw, _ := inst.GetGitWorktree()
	diag := sessiongit.MissingWorktreeDiagnosis{
		WorktreePath: worktreePath,
		ParentPath:   filepath.Dir(worktreePath),
	}
	if gw != nil {
		diag = gw.DiagnoseMissingWorktree()
	}
	branch := inst.GetBranch()
	if branch == "" {
		branch = diag.BranchName
	}
	liveness := inst.GetLiveness()
	op := inst.GetInFlightOp()
	userKilled := inst.UserKilled()
	teardownIntent := userKilled || teardownInFlight || op == session.OpKilling || op == session.OpArchiving
	classification := classifyMissingWorktree(diag.WorktreeRegistrationKnown, diag.WorktreeRegistered, teardownIntent)

	m.err().Printf(
		"WORKTREE_MISSING_DETECTED classification=%q title=%q instance_id=%q repo_id=%q repo_path=%q worktree_path=%q branch=%q liveness=%q status=%q started=%t user_killed=%t kill_in_flight=%t in_flight_op=%d external_worktree=%t branch_created_by_us=%t observed_at=%q parent_path=%q parent_exists=%t parent_stat_error=%q repo_exists=%t repo_stat_error=%q git_worktree_registered=%q git_worktree_list_error=%q branch_exists=%q branch_probe_error=%q recover_error=%q",
		classification,
		inst.Title,
		inst.ID,
		repoID,
		diag.RepoPath,
		worktreePath,
		branch,
		livenessLabel(liveness),
		statusLabel(inst.GetStatus()),
		inst.Started(),
		userKilled,
		teardownInFlight,
		op,
		diag.ExternalWorktree,
		diag.BranchCreatedByUs,
		time.Now().UTC().Format(time.RFC3339Nano),
		diag.ParentPath,
		diag.ParentExists,
		diag.ParentStatError,
		diag.RepoExists,
		diag.RepoStatError,
		triBool(diag.WorktreeRegistrationKnown, diag.WorktreeRegistered),
		diag.WorktreeListError,
		triBool(diag.BranchKnown, diag.BranchExists),
		diag.BranchError,
		restoreErr.Error(),
	)
}

func missingWorktreePath(err error) (string, bool) {
	var wtErr *session.WorktreeUnavailableError
	if !errors.As(err, &wtErr) || !errors.Is(err, os.ErrNotExist) {
		return "", false
	}
	if wtErr.WorktreePath == "" {
		return "<empty>", true
	}
	return wtErr.WorktreePath, true
}

func (st *lostRestoreState) vanishedWorktreeWasLogged(path string) bool {
	if st == nil || st.vanishedWorktreesLogged == nil {
		return false
	}
	_, ok := st.vanishedWorktreesLogged[path]
	return ok
}

func classifyMissingWorktree(registrationKnown, registered, teardownIntent bool) string {
	if teardownIntent {
		return "expected_teardown"
	}
	if registrationKnown && registered {
		return "unexpected_external_removal"
	}
	if registrationKnown {
		return "missing_unregistered_live_worktree"
	}
	return "missing_worktree_registration_unknown"
}

func triBool(known, value bool) string {
	if !known {
		return "unknown"
	}
	return strconv.FormatBool(value)
}

func livenessLabel(lv session.Liveness) string {
	switch lv {
	case session.LivenessUnset:
		return "LivenessUnset"
	case session.LiveRunning:
		return "LiveRunning"
	case session.LiveReady:
		return "LiveReady"
	case session.LiveLost:
		return "LiveLost"
	case session.LiveDead:
		return "LiveDead"
	case session.LiveArchived:
		return "LiveArchived"
	case session.LiveLimitReached:
		return "LiveLimitReached"
	default:
		return "Liveness(" + strconv.Itoa(int(lv)) + ")"
	}
}

func statusLabel(s session.Status) string {
	switch s {
	case session.Running:
		return "Running"
	case session.Ready:
		return "Ready"
	case session.Loading:
		return "Loading"
	case session.Deleting:
		return "Deleting"
	case session.Dead:
		return "Dead"
	case session.Lost:
		return "Lost"
	case session.Archived:
		return "Archived"
	default:
		return "Status(" + strconv.Itoa(int(s)) + ")"
	}
}
