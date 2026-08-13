package daemon

import (
	"context"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// Snapshot RPC types, the delivery-failure alarm projection, and the Snapshot
// handler itself (#960 PR 3, #1238). Extracted from control.go so that file stays
// under its length ceiling (#1145).
//
// The HANDLER moved here from control_server.go in #3056, when the owner
// constraint pushed that file back over the limit — so this file now owns the
// wire shapes, the alarm assembly, and the read that produces both. That is the
// right grouping anyway: the constraint below decides what goes into these types,
// and splitting the two across files is how they drift.
//
// The TUI's SnapshotWithAlarms read moved onto the HTTP apiclient in #1592 Phase
// 2 PR3 (apiclient.Client.SnapshotWithAlarms), so the net/rpc SnapshotWithAlarms
// client wrapper that used to live here is gone — the TUI was its only caller.
// The sessions SnapshotNoSpawn read moved to apiclient in Phase 2 PR2; task
// reads (ListTasksNoSpawn) stay on net/rpc.

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
	// PendingUnknown marks a queue whose on-disk state could not be loaded
	// (#3242): Pending is 0 because the backlog could not be enumerated, not
	// because it is empty. The banner says "unknown" instead of "0 pending" —
	// a fabricated zero here would tell the operator nothing is stuck while an
	// unreadable file holds the backlog.
	PendingUnknown bool `json:"pending_unknown,omitempty"`
	// Consecutive is the count of back-to-back failed delivery attempts.
	Consecutive int `json:"consecutive"`
	// Since is when the current run of failures began (first consecutive
	// failure); the TUI renders "since HH:MM".
	Since time.Time `json:"since"`
	// LastError is the most recent delivery error, for the banner detail.
	LastError string `json:"last_error,omitempty"`
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

// It keeps the two-argument shape net/rpc requires by reflection, and passes a
// bare context — so this path is never narrowed to a sandbox owner.
//
// That is correct rather than a bypass, and worth stating because it does not
// look it: net/rpc is registered on the UNIX SOCKET only (0600, chmod'd at
// creation), which is trusted transport and carries the operator's authority by
// construction. A sandbox never reaches it — its callback dials the HTTP listener,
// where servePosture binds the owner and the route below narrows. If net/rpc is
// ever exposed on a network listener, this wrapper becomes the hole, and the fix
// is to thread a real context rather than to widen the constraint.
func (s *controlServer) Snapshot(req SnapshotRequest, resp *SnapshotResponse) error {
	return s.snapshot(context.Background(), req, resp)
}

// snapshot is Snapshot with the request context, so it can see whether the caller
// is a SANDBOX and narrow the answer to that sandbox's own session (#3056).
//
// This is the first route admitted to the sandbox callback scope, and it carries
// the constraint that admission requires. Snapshot is a deliberate choice rather
// than a convenient one: it is the enumeration that made every other route
// aimable — #3012 denied it for exactly that reason — so if the owner constraint
// is worth anything, it has to hold here. An agent reading its OWN session's state
// is also the most obviously legitimate thing a remote agent does.
//
// THREE narrowings. The first is the obvious one; the other two are each a
// finding, because "show only the caller's own row" is not the same as "show only
// what the caller may see":
//
//   - Instances is filtered to the owner's own id. Not to its repo, and not to a
//     title match: the id is the stable identity (#1195), and titles are unique
//     only per repo, so matching on one would let a same-titled session in another
//     repo through.
//   - Each surviving row is PROJECTED (sandboxSafeInstanceData). The owner's own
//     row still carries the daemon host's layout — Path is the absolute repo root
//     on the host — so narrowing alone handed back the operator layout that the
//     path-oracle routes were withdrawn from this scope for (#3065 review).
//   - DeliveryAlarms is emptied outright. An alarm names its target by TITLE, so
//     filtering it would mean mapping the owner's id to a title and comparing —
//     an identity conversion in the one place that must not get identity wrong.
//     A sandbox has no use for watch-task delivery diagnostics anyway, so the
//     honest answer is none rather than a mapping that could be off by one repo.
//
// The operator's own callers (unix socket, operator token) are untouched: they
// carry no sandbox owner, so neither narrowing applies.
func (s *controlServer) snapshot(ctx context.Context, req SnapshotRequest, resp *SnapshotResponse) error {
	if err := s.requireManagerReady(); err != nil {
		return err
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	instances := s.manager.Snapshot(req.RepoID)
	alarms := s.deliveryAlarms(req.RepoID)
	if owner, isSandbox := sandboxOwner(ctx); isSandbox {
		instances = onlyOwnedBySandbox(instances, owner)
		alarms = nil
	}
	resp.Instances = instances
	resp.DeliveryAlarms = alarms
	return nil
}

// onlyOwnedBySandbox keeps just the caller's own session.
//
// Returns an EMPTY slice rather than nil when nothing matches, and never returns
// the input unfiltered: an owner whose session has been archived or killed sees
// nothing, which is correct, and is also the direction a bug here should fail in.
func onlyOwnedBySandbox(instances []session.InstanceData, owner string) []session.InstanceData {
	owned := make([]session.InstanceData, 0, 1)
	for _, d := range instances {
		if d.ID != "" && d.ID == owner {
			owned = append(owned, sandboxSafeInstanceData(d))
		}
	}
	return owned
}

// sandboxSafeInstanceData projects one session row down to what a sandbox may
// learn about ITSELF (#3065 review).
//
// Narrowing to the owner's row is not sufficient, which is the finding: the row
// itself carries the DAEMON HOST's layout. InstanceData.Path is the absolute
// repository root on the host, Worktree carries host paths, TmuxName names a host
// tmux session — none of which exist inside the sandbox, and all of which are
// exactly the operator layout that ListDirectory and the repo-path routes were
// withdrawn from the scope for. A credential correctly restricted to its own row
// still read them.
//
// Built by CONSTRUCTION, not by blanking. Copying d and clearing a denylist would
// leak every field added to InstanceData afterwards, silently, at the moment
// someone else adds one — and this struct has thirty-odd fields and grows. Listing
// what may go out means a new field is withheld until somebody decides otherwise,
// which is the same default-deny the route scope uses. A test walks the projection
// reflectively so a newly populated field has to be an explicit decision.
//
// What is included is the agent's own operational state: who it is, what it is
// running, what state it is in. What is excluded is anything describing the HOST,
// anything about other sessions, and the conversation/prompt payloads a sandbox
// already has locally and does not need served back.
func sandboxSafeInstanceData(d session.InstanceData) session.InstanceData {
	return session.InstanceData{
		ID:                       d.ID,
		TaskID:                   d.TaskID,
		Title:                    d.Title,
		Branch:                   d.Branch,
		Status:                   d.Status,
		Liveness:                 d.Liveness,
		InFlightOp:               d.InFlightOp,
		LifecycleAction:          d.LifecycleAction,
		CurrentAgent:             d.CurrentAgent,
		Program:                  d.Program,
		BackendType:              d.BackendType,
		TaskRunActive:            d.TaskRunActive,
		LimitResetAt:             d.LimitResetAt,
		IdleReason:               d.IdleReason,
		LastPromptAttemptAt:      d.LastPromptAttemptAt,
		LastPromptDeliveryStatus: d.LastPromptDeliveryStatus,
		LastPaneChurnAt:          d.LastPaneChurnAt,
		CreatedAt:                d.CreatedAt,
		UpdatedAt:                d.UpdatedAt,
	}
}
