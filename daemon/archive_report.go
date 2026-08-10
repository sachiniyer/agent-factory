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

// failedRestoredArchiveResult preserves the ordinary retryable failure for a
// complete archive, but marks an incomplete restore committed: its worktree has
// already moved and the Lost loop, not a second archived restore, owns recovery.
func failedRestoredArchiveResult(instance *session.Instance, worktreePath string, failure error) (string, error) {
	if instance.GetArchiveReport().Empty() {
		return "", failure
	}
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
func (m *Manager) keepIncompleteArchiveCommitted(
	repoID, archivedPath string,
	instance *session.Instance,
	hookErr error,
	needsDurableWrite bool,
	cause error,
) (string, session.InstanceData, error) {
	if needsDurableWrite {
		if persistErr := archivePersist(m, repoID, instance); persistErr != nil {
			cause = fmt.Errorf("%w; the committed archive also could not be written durably: %v", cause, persistErr)
		}
	} else {
		m.persistInstance(repoID, instance)
	}
	archived := instance.ToInstanceData()
	m.publishEvent(agentproto.EventSessionArchived, archived)
	committedErr := archiveCommittedWarning(instance, hookErr, fmt.Errorf(
		"archive was kept committed because rolling its incomplete copy back would omit retained files; %w", cause,
	))
	log.WarningLog.Printf("%v", committedErr)
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
