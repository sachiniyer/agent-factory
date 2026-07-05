package daemon

import (
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// Snapshot RPC types and the delivery-failure alarm projection (#960 PR 3,
// #1238). Extracted from control.go so that file stays under its length ceiling
// (#1145). The client entry points (Snapshot / SnapshotNoSpawn / SnapshotWithAlarms)
// and the controlServer.Snapshot handler live in control.go alongside the other
// RPCs; this file owns the wire shapes and the alarm assembly.

// SnapshotRequest asks the daemon for the authoritative session list of a repo
// (#960 PR 3). RepoID scopes the read like the other sessions verbs (empty =
// all repos). It is the read side of the single-writer model: the daemon's
// in-memory instance map is the source of truth, so the TUI mirrors this
// projection instead of re-reading instances.json off disk.
type SnapshotRequest struct {
	RepoID string `json:"repo_id"`
}

type SnapshotResponse struct {
	Instances []session.InstanceData `json:"instances"`
	// DeliveryAlarms projects persistent watch-task delivery failures into the
	// authoritative snapshot the TUI mirrors (#1238). When a watch task's events
	// have been failing to reach their target session for longer than the alarm
	// threshold — the 2026-07-05 silent event-pipeline outage — the daemon lists
	// them here so the TUI can raise a banner instead of the failure being
	// visible only in the daemon log. Empty (omitted) in the healthy steady
	// state, and additive/gob- and JSON-compatible with older peers that ignore
	// the field.
	DeliveryAlarms []DeliveryAlarm `json:"delivery_alarms,omitempty"`
}

// DeliveryAlarm is one watch task whose event delivery to TargetSession has
// been failing persistently (past watcherDeliveryAlarmThreshold). It carries
// what the TUI banner needs — the target, how many events are stuck, when the
// failures began, and the last error — so an operator sees a dead delivery
// pipeline within the threshold window instead of discovering it by accident
// (#1238 fix c).
type DeliveryAlarm struct {
	TaskID        string `json:"task_id"`
	TaskName      string `json:"task_name"`
	TargetSession string `json:"target_session"`
	// Pending is the number of undelivered events queued behind the failure.
	Pending int `json:"pending"`
	// Consecutive is the count of back-to-back failed delivery attempts.
	Consecutive int `json:"consecutive"`
	// Since is when the current run of failures began (first consecutive
	// failure); the TUI renders "since HH:MM".
	Since time.Time `json:"since"`
	// LastError is the most recent delivery error, for the banner detail.
	LastError string `json:"last_error,omitempty"`
}

// SnapshotWithAlarms is Snapshot plus the persistent delivery-failure alarms
// carried on the same authoritative response (#1238). The TUI reconcile path
// uses it so the session list and the alarm projection arrive from one RPC —
// the alarm is a field on the snapshot, not a side channel. Plain Snapshot
// (which drops the alarms) stays the read path for CLI/API callers that only
// need the session list.
func SnapshotWithAlarms(req SnapshotRequest) (SnapshotResponse, error) {
	var resp SnapshotResponse
	if err := callDaemon("Snapshot", req, &resp); err != nil {
		return SnapshotResponse{}, err
	}
	return resp, nil
}

// deliveryAlarms assembles the persistent delivery-failure alarms for a repo
// (empty = all repos) from the watcher supervisor, evaluated against the
// current time. Nil when there is no supervisor or nothing is failing.
func (s *controlServer) deliveryAlarms(repoID string) []DeliveryAlarm {
	if s.watchers == nil {
		return nil
	}
	return s.watchers.deliveryAlarms(repoID, time.Now())
}
