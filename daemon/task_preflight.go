package daemon

import "errors"

// taskPreflightError classifies every task-originated refusal before a create's
// reservation commit or a delivery attempt as not attempted. Boundary owners
// call it from defers until they delegate to the next owner, so the complete
// cross-layer interval is covered without double-wrapping and without asking
// individual return sites to remember the rule.
//
// TaskOrigin is daemon-only provenance independent of retained identity or
// concurrency ownership. Legacy targeted rows can have neither TaskID nor
// TaskRepoID, so retain those older identity shapes as compatibility evidence
// for in-process callers constructed before TaskOrigin was added. Ordinary
// client requests carry none of the three and retain their plain errors.
//
// A concurrency refusal also precedes the title reservation, but it has its own
// sentinel and watcher branch: that branch parks the event and releases the
// event-rate slot. Preserve that classification instead of obscuring it behind
// the general pre-flight marker.
func taskPreflightError(taskOrigin bool, taskID, taskRepoID string, err error) error {
	if err == nil || errors.Is(err, errAtConcurrencyLimit) {
		return err
	}
	if taskOrigin || taskID != "" || taskRepoID != "" {
		return notAttempted(err)
	}
	return err
}
