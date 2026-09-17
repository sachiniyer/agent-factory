package task

import "strings"

// backfilledGenerationPrefix marks a generation the store minted for a row
// written before generation_id existed (#4224). Nothing else mints this form:
// add and restore call generateTaskGenerationID, which never carries it, and
// both discard any generation a client supplied. The marker is part of the
// stored value, so it survives a crash between the backfill write and any
// follow-up that depends on it.
const backfilledGenerationPrefix = "legacy-"

func generateBackfilledTaskGenerationID() (string, error) {
	generationID, err := generateTaskGenerationID()
	if err != nil {
		return "", err
	}
	return backfilledGenerationPrefix + generationID, nil
}

// IsBackfilledGeneration reports whether generationID was minted by the upgrade
// backfill rather than by an add. A row with such a generation is the same row
// that existed before generations did, so state that row owned under the empty
// generation can belong to it: its legacy watch-event queue and the concurrency
// slots its in-flight pre-upgrade runs hold.
//
// The marker proves nothing about pre-upgrade sessions. The empty generation
// cannot tell this row's sessions apart from those of a removed pre-field row
// that used the same ID, so on_complete still keeps every session stamped with
// the empty generation.
func IsBackfilledGeneration(generationID string) bool {
	return strings.HasPrefix(generationID, backfilledGenerationPrefix)
}

// GenerationStillNames reports whether expected, a generation an in-flight
// operation read from a task row, still names the row that now stores stored.
// It is exact equality with one addition. An operation that read a pre-field
// row before the backfill holds the empty generation, and the backfill gave
// that same row its generation without replacing it, so the operation still
// names the row (#4224). Without this, a delivery that read a hand-edited row
// just before a load backfilled it was refused as though the task had been
// replaced. A replacement added under the same ID mints an unmarked
// generation, which the empty generation never names.
//
// The session-run writers use it too, and stay safe for sessions stamped
// before the upgrade: BeginTaskRun is called only for a run this daemon just
// admitted, and the outcome writers also require the row to name the run's
// stable session ID, which no binary from before the field ever recorded. The
// lifecycle and the legacy outcome claim compare exactly, because there an
// empty generation can be a removed pre-field row's session.
func GenerationStillNames(stored, expected string) bool {
	return stored == expected || (expected == "" && IsBackfilledGeneration(stored))
}

// EnsureTaskGeneration gives one row the backfilled generation the stable load
// would give it, and reports whether it wrote. Run admission calls it for a row
// no stable load has read yet, typically one added to tasks.json by hand since
// the last reload. A run admitted under the empty generation is kept whatever
// the row declares for on_complete (#4224 review). A row that already has a
// generation is left alone and applied is false.
func EnsureTaskGeneration(taskID string) (Task, bool, error) {
	var mintErr error
	updated, applied, err := mutateTaskStatus(taskID, func(t *Task) bool {
		if t.GenerationID != "" {
			return false
		}
		generationID, err := generateBackfilledTaskGenerationID()
		if err != nil {
			mintErr = err
			return false
		}
		t.GenerationID = generationID
		appendAudit(t, ActorDaemonUpgrade, AuditUpdated, []string{"generation_id"}, nowFn())
		return true
	})
	if err == nil && mintErr != nil {
		return Task{}, false, mintErr
	}
	return updated, applied, err
}
