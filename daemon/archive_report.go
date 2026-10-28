package daemon

import (
	"errors"
	"fmt"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// archiveCommitWarning combines every nonfatal condition the caller must see
// after the Archived record is durable. The skipped-file report is not a log:
// this committed outcome reaches CLI, TUI, HTTP, and the control socket.
func archiveCommitWarning(instance *session.Instance, hookErr error) error {
	return archiveCommittedWarning(instance, hookErr)
}

func archiveCommittedWarning(instance *session.Instance, hookErr error, extra ...error) error {
	warnings := append([]error(nil), extra...)
	if report := instance.GetArchiveReport(); !report.Empty() {
		warnings = append(warnings, errors.New(report.Warning("archive")))
	}
	if hookErr != nil {
		warnings = append(warnings, fmt.Errorf("archive committed, but on-archive hook failed: %w", hookErr))
	}
	if len(warnings) == 0 {
		return nil
	}
	return &mutationCommittedError{err: errors.Join(warnings...)}
}

// failedArchiveWithHook composes what a FAILED archive returns: its primary
// cause, plus a clause naming a failed on-archive hook when there was one —
// logged on the way past.
//
// The hook runs at the teardown chokepoint, BEFORE the worktree moves, so every
// archive failure below it is reachable with a broken cleanup command behind it.
// The primary cause stays primary — it is what the caller acts on — but it does
// not subsume the hook's (#2763): a cleanup command that may already have mutated
// external resources is otherwise invisible, and every retry fails the same way
// and reports the same single cause.
//
// The log is the other half, not decoration. An archive driven by an `af tasks`
// run has no one reading a return value, so the daemon log is the only diagnosis
// that survives. Composing both here is what stops an exit on these paths from
// getting one and forgetting the other — which is exactly how the two
// rollback-success returns came to drop the hook entirely (#3452).
//
// Committed outcomes do not come through here: archiveCommittedWarning folds the
// hook failure into the warning those paths already carry.
func failedArchiveWithHook(title string, cause, hookErr error) error {
	if hookErr == nil {
		return cause
	}
	log.WarningLog.Printf("archive of session %q failed and its on-archive hook also failed: %v", title, hookErr)
	return fmt.Errorf("%w (its on-archive hook also failed: %v)", cause, hookErr)
}

// restoredArchiveResult surfaces a report persisted weeks earlier without
// turning the completed restore into a retryable failure. MutationOutcome is
// the existing three-valued wire channel for "committed, with a warning".
func restoredArchiveResult(instance *session.Instance, worktreePath string, extra ...error) (string, error) {
	warnings := append([]error(nil), extra...)
	report := instance.GetArchiveReport()
	if !report.Empty() {
		warnings = append(warnings, errors.New(report.Warning("restore")))
	}
	if len(warnings) == 0 {
		return worktreePath, nil
	}
	return worktreePath, &mutationCommittedError{err: errors.Join(warnings...)}
}

// failedRestoredArchiveResult reports a restore whose worktree relocate already
// landed but whose follow-up — the durable record, or the agent re-spawn —
// failed. It is reached only after RestoreArchivedWorktreeWithClaim succeeded,
// so the worktree has already moved off the archive path: a second archived
// restore would fail relocate's source-exists guard, and the Lost loop — not a
// restore retry — owns recovery. The relocated path and committed marker are
// therefore returned for BOTH complete and incomplete archives, so a transport
// cannot read a landed move as failed-nothing-committed (#3235) — the same shape
// keepUnrollableArchiveCommitted gives the archive-side durable mutation. The
// skipped-file report joins the warning for incomplete archives; a complete
// archive carries only the failure, still committed because the relocate landed.
func failedRestoredArchiveResult(instance *session.Instance, worktreePath string, failure error) (string, error) {
	return restoredArchiveResult(instance, worktreePath, failure)
}

// failedArchiveError keeps a failed archive retryable while still surfacing any
// files already omitted from the published copy. Registration repair can fail
// after the archive copier retained the complete source and recorded its report;
// dropping that report from this error would let automatic Lost recovery make
// the incomplete copy live without the requesting client learning why.
func failedArchiveError(instance *session.Instance, failure error) error {
	report := instance.GetArchiveReport()
	if report.Empty() {
		return failure
	}
	return errors.Join(failure, errors.New(report.Warning("archive")))
}

func failedArchiveResult(instance *session.Instance, failure error) (string, session.InstanceData, error) {
	return "", session.InstanceData{}, failedArchiveError(instance, failure)
}

// keepIncompleteArchiveCommitted is the no-data-loss alternative to archive's
// ordinary rollback. The published copy omits files whose only complete copy is
// in a retained tree, so restoring it home would start a Lost session without
// those bytes. Keep and publish Archived, preserve the report, and return the
// committed marker that prevents every caller from retrying the landed move.
//
// The committed claim must BE durable before it is made, the same rule
// keepUnrollableArchiveCommitted enforces below (#3448). A committed marker is
// read by DeleteProject as success-with-warning: it records the message as a
// warning and goes on to DEREGISTER the project. If the Archived row never
// reached disk, that deregisters on top of a stale PRE-ARCHIVE live row — a
// restart reloads it, cannot reconstruct the worktree at the old path, and skips
// the instance, leaving the bytes orphaned under the archive with no project and
// no session pointing at them. So the durable write is attempted here and only
// its SUCCESS claims committed; a failure returns the plain, retryable error
// callers refuse on, naming the archive location for manual recovery.
//
// One write, not two branches. The caller that arrives here BECAUSE its durable
// write already failed used to take a best-effort persistInstance whose error was
// discarded, and claimed committed regardless — the identical undurable claim,
// reached by the more likely road. Both callers now retry the same write through
// the same gate.
func (m *Manager) keepIncompleteArchiveCommitted(
	repoID, archivedPath string,
	instance *session.Instance,
	hookErr error,
	cause error,
) (string, session.InstanceData, error) {
	if persistErr := archivePersist(m, repoID, instance); persistErr != nil {
		// A FAILED archive exit, so the on-archive hook's outcome composes in here
		// too (#3460) — this path carries a hookErr that the committed return below
		// would have surfaced, and dropping it on the way out is the same
		// swallowed-outcome bug pointed at a different exit.
		return "", session.InstanceData{}, failedArchiveWithHook(instance.Title, errors.Join(cause, fmt.Errorf(
			"its incomplete archive was kept at %s because rolling it back would omit retained files, but the committed archive could not be written durably, so it is not claimed committed and needs manual recovery: %w",
			archivedPath, persistErr)), hookErr)
	}
	archived := instance.ToInstanceData()
	m.publishEvent(agentproto.EventSessionArchived, archived)
	committedErr := archiveCommittedWarning(instance, hookErr, fmt.Errorf(
		"archive was kept committed because rolling its incomplete copy back would omit retained files; %w", cause,
	))
	m.warn().Printf("%v", committedErr)
	return archivedPath, archived, committedErr
}

// keepUnrollableArchiveCommitted keeps the committed archive when rolling the
// worktree back home itself failed AND the bytes provably stayed at the
// archived location. In that case callers get the archived location, the
// resolved projection, and the same committed marker
// keepIncompleteArchiveCommitted returns — a plain error and an empty location
// told every transport failed-nothing-committed about an archive that IS
// committed and kept (#3235).
//
// A rollback error is NOT proof the archive stayed put (#3335 review): the
// move home can land the bytes and then fail registration repair, which
// commits the pre-archive location while still returning an error — and a
// cut-off move leaves the directory identity unresolved. Claiming
// committed-at-archivedPath there would point every client at a vacated path,
// so those cases keep the prior plain double-failure shape, whose message
// names both locations for manual recovery.
//
// The committed claim must also BE durable before it is made (#3335 review):
// an undurable Archived row means a restart reloads the pre-archive live row,
// and a committed marker would let DeleteProject deregister the project on top
// of it. So the durable write is retried here, and only its success claims
// committed; a second failure keeps the plain shape callers refuse-and-retry on.
func (m *Manager) keepUnrollableArchiveCommitted(
	repoID, archivedPath string,
	instance *session.Instance,
	hookErr error,
	cause error,
) (string, session.InstanceData, error) {
	if _, _, unresolved := instance.GetWorktreeRelocationCandidates(); unresolved ||
		instance.GetWorktreePath() != archivedPath {
		m.persistInstance(repoID, instance)
		// Plain shape, so no committed warning carries the hook failure here: this
		// return owes the operator the same truth the committed one below does.
		return "", session.InstanceData{}, failedArchiveWithHook(instance.Title, cause, hookErr)
	}
	if persistErr := archivePersist(m, repoID, instance); persistErr != nil {
		// The committed claim itself could not be made durable: a restart reloads
		// the pre-archive live row while the bytes sit under archivedPath. A
		// committed marker here would also let DeleteProject convert this to a
		// warning and deregister the project on top of that stale row (#3335
		// review) — before this helper existed, the plain error is what stopped
		// that. Keep the plain double-failure shape until the claim can land.
		return "", session.InstanceData{}, failedArchiveWithHook(instance.Title, errors.Join(cause,
			fmt.Errorf("the committed archive also could not be written durably: %w", persistErr)), hookErr)
	}
	archived := instance.ToInstanceData()
	m.publishEvent(agentproto.EventSessionArchived, archived)
	committedErr := archiveCommittedWarning(instance, hookErr, cause)
	m.warn().Printf("%v", committedErr)
	return archivedPath, archived, committedErr
}

func worktreeRecoveryLocation(instance *session.Instance) string {
	if primary, alternate, ok := instance.GetWorktreeRelocationCandidates(); ok {
		if alternate != "" {
			return fmt.Sprintf("either %s or %s (identity unresolved)", primary, alternate)
		}
		return fmt.Sprintf("%s (identity unresolved)", primary)
	}
	return instance.GetWorktreePath()
}
