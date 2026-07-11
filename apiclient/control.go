package apiclient

import (
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// This file grows the HTTP client past the read-only Snapshot path (PR2) onto
// the full control + task surface the TUI drives (#1592 Phase 2 PR3). Each
// method is the HTTP twin of the identically-named net/rpc client wrapper the
// daemon package used to expose: same request/response structs, same return
// shape, so a caller swaps `daemon.X(req)` for `client.X(req)` with no other
// change and gets byte-identical results (the envelope guarantees parity). The
// TUI is the first full consumer — it now routes create/kill/archive/restore/
// tab/PR-info/task/poll-pause/limit-resume through here instead of net/rpc, so
// the gob control client stays only for CLI/internal callers.

// CreateSession asks the daemon to create, start, and persist a session.
func (c *Client) CreateSession(req daemon.CreateSessionRequest) (*session.InstanceData, error) {
	var resp daemon.CreateSessionResponse
	if err := c.call("CreateSession", req, &resp); err != nil {
		return nil, err
	}
	return &resp.Instance, nil
}

// KillSession asks the daemon to kill a session and remove it from storage.
func (c *Client) KillSession(req daemon.KillSessionRequest) error {
	return c.call("KillSession", req, &daemon.KillSessionResponse{})
}

// ArchiveSession asks the daemon to archive a session (#1028) and returns the
// relocated worktree's new path.
func (c *Client) ArchiveSession(req daemon.ArchiveSessionRequest) (string, error) {
	var resp daemon.ArchiveSessionResponse
	if err := c.call("ArchiveSession", req, &resp); err != nil {
		return "", err
	}
	return resp.ArchivedPath, nil
}

// RestoreSession asks the daemon to restore an archived, Lost, or Dead session.
func (c *Client) RestoreSession(req daemon.RestoreSessionRequest) (string, error) {
	var resp daemon.RestoreSessionResponse
	if err := c.call("RestoreSession", req, &resp); err != nil {
		return "", err
	}
	return resp.WorktreePath, nil
}

// ResumeFromLimit asks the daemon to resume a usage-limit-blocked session
// (#1146) — the TUI's `c` key. It rides an internal (non-cataloged) route: a
// client-facing session verb the `af api` catalog does not advertise.
func (c *Client) ResumeFromLimit(req daemon.ResumeFromLimitRequest) error {
	return c.call("ResumeFromLimit", req, &daemon.ResumeFromLimitResponse{})
}

// CreateTab asks the daemon to spawn, persist, and report a new process/shell
// tab on an existing session, returning the resolved (collision-suffixed) name.
func (c *Client) CreateTab(req daemon.CreateTabRequest) (string, error) {
	var resp daemon.CreateTabResponse
	if err := c.call("CreateTab", req, &resp); err != nil {
		return "", err
	}
	return resp.Name, nil
}

// CloseTab asks the daemon to close a non-agent tab and returns the resolved
// name of the tab that was closed.
func (c *Client) CloseTab(req daemon.CloseTabRequest) (string, error) {
	var resp daemon.CloseTabResponse
	if err := c.call("CloseTab", req, &resp); err != nil {
		return "", err
	}
	return resp.Name, nil
}

// SetPRInfo asks the daemon to record (or clear) a session's GitHub PR info.
func (c *Client) SetPRInfo(req daemon.SetPRInfoRequest) error {
	return c.call("SetPRInfo", req, &daemon.SetPRInfoResponse{})
}

// ImportRemoteHookSessions asks the daemon to reconcile remote sessions
// reported by list_cmd into persisted storage.
func (c *Client) ImportRemoteHookSessions(req daemon.ImportRemoteHookSessionsRequest) ([]session.InstanceData, error) {
	var resp daemon.ImportRemoteHookSessionsResponse
	if err := c.call("ImportRemoteHookSessions", req, &resp); err != nil {
		return nil, err
	}
	return resp.Instances, nil
}

// PauseStatusPoll asks the daemon to pause its capture-pane liveness poll for
// one attached session (#1160). Best-effort attach coordination; it rides an
// internal (non-cataloged) route because it is daemon infra, not a public verb.
func (c *Client) PauseStatusPoll(req daemon.PauseStatusPollRequest) error {
	return c.call("PauseStatusPoll", req, &daemon.PauseStatusPollResponse{})
}

// ResumeStatusPoll asks the daemon to resume polling a session on a clean
// detach (#1160). Internal (non-cataloged) route, like PauseStatusPoll.
func (c *Client) ResumeStatusPoll(req daemon.ResumeStatusPollRequest) error {
	return c.call("ResumeStatusPoll", req, &daemon.ResumeStatusPollResponse{})
}

// AddTask asks the daemon to append a task and re-arm its schedule set.
func (c *Client) AddTask(t task.Task) error {
	return c.call("AddTask", daemon.AddTaskRequest{Task: t}, &daemon.AddTaskResponse{})
}

// UpdateTask asks the daemon to persist an edited task and re-arm its schedule.
func (c *Client) UpdateTask(t task.Task) error {
	return c.call("UpdateTask", daemon.UpdateTaskRequest{Task: t}, &daemon.UpdateTaskResponse{})
}

// RemoveTask asks the daemon to delete a task and re-arm its schedule.
func (c *Client) RemoveTask(id string) error {
	return c.call("RemoveTask", daemon.RemoveTaskRequest{ID: id}, &daemon.RemoveTaskResponse{})
}

// TriggerTask asks the daemon to fire a task now through the shared RunTask
// firing path (the same entrypoint the scheduler uses).
func (c *Client) TriggerTask(id string) error {
	return c.call("TriggerTask", daemon.TriggerTaskRequest{ID: id}, &daemon.TriggerTaskResponse{})
}

// SnapshotWithAlarms is Snapshot plus the persistent delivery-failure alarms
// carried on the same authoritative response (#1238). It is the TUI's read
// path: the session list and the alarm projection arrive from one call, so the
// alarm is a field on the snapshot, not a side channel. Plain Snapshot (which
// drops the alarms) stays the read path for CLI/API callers that only need the
// session list.
func (c *Client) SnapshotWithAlarms(req daemon.SnapshotRequest) (daemon.SnapshotResponse, error) {
	var resp daemon.SnapshotResponse
	if err := c.call("Snapshot", req, &resp); err != nil {
		return daemon.SnapshotResponse{}, err
	}
	return resp, nil
}
