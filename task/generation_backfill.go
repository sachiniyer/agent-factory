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
