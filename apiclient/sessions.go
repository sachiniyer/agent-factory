package apiclient

import (
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// Snapshot returns the daemon's authoritative session list over the HTTP API,
// the exact `[]session.InstanceData` the net/rpc Snapshot RPC returns. It POSTs
// to /v1/Snapshot and unwraps resp.Instances from the shared envelope, so its
// result is byte-identical to daemon.SnapshotNoSpawn's on a running daemon. Like
// the RPC read it is scoped by req.RepoID (empty = all repos).
func (c *Client) Snapshot(req daemon.SnapshotRequest) ([]session.InstanceData, error) {
	var resp daemon.SnapshotResponse
	if err := c.call("Snapshot", req, &resp); err != nil {
		return nil, err
	}
	return resp.Instances, nil
}

// SnapshotNoSpawn is a drop-in for daemon.SnapshotNoSpawn over the HTTP API: it
// returns the daemon's authoritative session snapshot WITHOUT ever starting a
// daemon, and collapses every failure — no socket, a refused dial, a warming
// daemon's starting-error envelope (#829) — into daemon.ErrDaemonUnavailable so
// the read-only CLI read path (api/sessions.go) falls back to disk instead of
// spawning a daemon or failing. This is the ONE behavioral contract the caller
// depends on, and it matches the net/rpc twin exactly: live daemon → live
// snapshot, otherwise → disk fallback.
func SnapshotNoSpawn(req daemon.SnapshotRequest) ([]session.InstanceData, error) {
	c, err := New()
	if err != nil {
		return nil, daemon.ErrDaemonUnavailable
	}
	instances, err := c.Snapshot(req)
	if err != nil {
		return nil, daemon.ErrDaemonUnavailable
	}
	return instances, nil
}
