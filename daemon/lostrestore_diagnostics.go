package daemon

import (
	"errors"
	stdlog "log"
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

func attemptNoun(attempts int) string {
	if attempts == 1 {
		return "attempt"
	}
	return "attempts"
}

// clearLostRestoreWaitDurable retires an environmental wait AND makes the
// retirement durable when the wait was actually armed — a checkpoint write,
// not a settlement, the same class the arm's own write uses: the clear is
// advisory and the next state-changing write will carry it, while an owed
// settlement would retry the write every poll tick during the very resource
// incident this code exists to tolerate (#5295 review). Use it wherever the
// clear is not already swept into a settlement write on the same path — the
// give-up and heal arms write the row themselves right after clearing. The
// caller must not hold m.mu (the write takes it through the repo start lock).
func (m *Manager) clearLostRestoreWaitDurable(repoID string, inst *session.Instance) {
	if !inst.ClearLostRestoreWait() {
		return
	}
	if err := m.persistAndPublishInstanceErr(repoID, inst); err != nil {
		m.warn().Printf("lost session %q: the ended environmental wait is cleared in memory but its stale copy is not yet durable: %v", inst.Title, err)
	}
}

// resetPreserveBudget clears the push-failure episode counter for the session
// identified by repoID and inst after a successful preserve. This mirrors what
// the automatic probeAnsweredDead path does in-line, so that the manual restore
// path also resets the budget when its own preserveSandboxBeforeReap lands — a
// successful push ends the episode regardless of which path ran it.
func (m *Manager) resetPreserveBudget(repoID string, inst *session.Instance) {
	stateKey := stableSessionKey(repoID, inst)
	m.mu.Lock()
	st := m.lostRestoreStates[stateKey]
	if st != nil {
		st.preserveFailureAttempts = 0
		st.remoteUnknownAttempts = 0
		st.environmentalFailures = 0
		st.environmentalDeadlineFailures = 0
		st.nextAttempt = time.Time{}
	}
	m.mu.Unlock()
	m.clearLostRestoreWaitDurable(repoID, inst)
}

// resetRecoverBudget clears the Recover-flap episode counter for the session
// identified by repoID and inst. Call this after the old sandbox is provably
// retired (i.e. after a successful force-replace): the old sandbox is gone, so
// any prior Recover failures are stale and the new sandbox earns a fresh budget.
// Do NOT call this on a plain successful preserve push — that does not replace
// the sandbox, and zeroing consecutiveFailures there would erase a legitimate
// Recover-flap count from a running episode. The symmetric probeAlive paths that
// settle the session (RestoreLostSessions) instead delete the whole
// lostRestoreStates entry, so they do not use this helper either.
//
// An entry is always created (or reset in place) so that a daemon restart before
// the operator uses --force-reap does not leave lostRestoreStates empty: if the
// entry were absent, recordLostRestoreFailure would seed consecutiveFailures from
// the persisted terminal failure, causing the first new-sandbox failure to be
// counted as attempt maxAttempts+1 and triggering immediate give-up. The zeroed
// entry ensures the new sandbox starts from attempt 1.
// Any stale awaitingConfirm from the predecessor episode is also cleared: the
// old sandbox is gone, so its pending confirmation is irrelevant, and leaving it
// set would cause the next automatic poll to double-charge a failure.
func (m *Manager) resetRecoverBudget(repoID string, inst *session.Instance) {
	stateKey := stableSessionKey(repoID, inst)
	m.mu.Lock()
	st := m.lostRestoreStates[stateKey]
	if st == nil {
		st = &lostRestoreState{}
		m.lostRestoreStates[stateKey] = st
	}
	st.consecutiveFailures = 0
	st.environmentalFailures = 0
	st.environmentalDeadlineFailures = 0
	st.totalAttempts = 0
	st.awaitingConfirm = false
	// Clear per-episode diagnostic dedupe flags so that the replacement sandbox's
	// failures log at their natural first occurrence rather than being suppressed
	// by a flag left from the predecessor episode. armRestoreConfirmation
	// deliberately retains these across the ConfirmLive edge for the confirmation
	// window, so they are not cleared there — only at the episode boundary here.
	st.remoteUnknownLogged = false
	st.preserveFailureLogged = false
	m.mu.Unlock()
	// A force-replaced sandbox owns none of the predecessor's wait — the row
	// must not keep claiming the environment is to blame once the episode is
	// deliberately reset.
	m.clearLostRestoreWaitDurable(repoID, inst)
}

// observationsSinceSpawnLocked reports how many ANSWERS polls have gotten out of
// this session's runtime since its last respawn. Caller holds m.mu.
func (m *Manager) observationsSinceSpawnLocked(repoID string, inst *session.Instance, st *lostRestoreState) uint64 {
	observed := m.aliveObservations[stableSessionKey(repoID, inst)]
	if observed < st.observedAtSpawn {
		// The counter only moves forward, but a state rebuilt against a different
		// runtime must never read as confirmed through unsigned arithmetic.
		return 0
	}
	return observed - st.observedAtSpawn
}

// confirmedAliveSinceSpawnLocked reports whether the replacement runtime has
// answered enough polls to count as restored — the definition of "confirmed alive"
// (#1917 round 6), sustained rather than single-shot (#3412). Caller holds m.mu.
func (m *Manager) confirmedAliveSinceSpawnLocked(repoID string, inst *session.Instance, st *lostRestoreState) bool {
	return m.observationsSinceSpawnLocked(repoID, inst, st) >= lostRestoreConfirmObservations
}

// info is the restoring Manager's INFO logger. This helper is package-level and
// its message ("restored after") is asserted on, so leaving it reaching for the
// global would let those assertions read a sink any Manager writes into — the
// #3795 trap, one level down (#3797).
func logLostRestoreSuccess(info *stdlog.Logger, title, repoID string, attempts int) {
	info.Printf("restore of lost session %q (repo %s): restored after %d %s", title, repoID, attempts, attemptNoun(attempts))
}

func lostRestoreBackoff(attempt int) time.Duration {
	backoff := lostRestoreBackoffMax
	// Guard the shift: past ~16 doublings the exponential form has no meaning
	// and would overflow.
	if shift := attempt - 1; shift >= 0 && shift < 16 {
		if b := lostRestoreBackoffBase << shift; b < backoff {
			backoff = b
		}
	}
	return backoff
}
