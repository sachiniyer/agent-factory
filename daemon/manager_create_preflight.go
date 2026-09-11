package daemon

import "errors"

// taskCreatePreReservationError classifies every task-originated refusal before
// reserveCreate's reservation commit as not attempted. Complementary defers in
// CreateSession and reserveCreate cover the interval from operation entry to the
// exact commit rather than asking each return site to remember it, so a new
// pre-flight exit cannot silently leak the watch event's rate slot.
//
// TaskOrigin is daemon-only provenance independent of retained identity or
// concurrency ownership. Legacy targeted rows can have neither TaskID nor
// TaskRepoID, so retain those older identity shapes as compatibility evidence
// for in-process callers constructed before TaskOrigin was added. Ordinary
// client creates carry none of the three and retain their plain errors.
//
// A concurrency refusal also precedes the title reservation, but it has its own
// sentinel and watcher branch: that branch parks the event and releases the
// event-rate slot. Preserve that classification instead of obscuring it behind
// the general pre-flight marker.
func taskCreatePreReservationError(req CreateSessionRequest, err error) error {
	if err == nil || errors.Is(err, errAtConcurrencyLimit) {
		return err
	}
	if req.TaskOrigin || req.TaskID != "" || req.TaskRepoID != "" {
		return notAttempted(err)
	}
	return err
}
