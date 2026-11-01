package apiclient

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"syscall"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// Snapshot returns the daemon's authoritative session list over the HTTP API,
// the exact `[]session.InstanceData` the net/rpc Snapshot RPC returns. It POSTs
// to /v1/Snapshot and unwraps resp.Instances from the shared envelope, so its
// result is byte-identical to daemon.SnapshotNoSpawn's on a running daemon. Like
// the RPC read it is scoped by req.RepoID (empty = all repos).
func (c *Client) Snapshot(req daemon.SnapshotRequest) ([]session.InstanceData, []daemon.SkippedRepo, error) {
	return c.SnapshotCtx(context.Background(), req)
}

// SnapshotCtx is Snapshot bounded by ctx.
//
// The LOCAL unix socket deliberately carries no overall request timeout (see
// Client.requestTimeout), because most local RPCs return from an in-memory read
// and a create legitimately runs long. That leaves one gap: a daemon that
// ACCEPTS the connection and then never answers blocks the caller forever — the
// 250ms dial timeout has already been satisfied by then.
//
// A read-only diagnostic must not hang on that. `af doctor`'s orphaned-pin
// collector (#3560) bounds this read and treats the deadline as "the session
// inventory could not be read", which reports UNKNOWN and refuses every removal
// — the same answer an unreachable daemon produces.
//
// The second return carries repos the daemon dropped at startup due to a
// corrupted instances.json, so the caller can refuse or caveat instead of
// silently serving a partial list (#603 closed over the wire). Empty when no
// repo was skipped; absent (also empty) on an older daemon that predates the
// field.
func (c *Client) SnapshotCtx(ctx context.Context, req daemon.SnapshotRequest) ([]session.InstanceData, []daemon.SkippedRepo, error) {
	var resp daemon.SnapshotResponse
	if err := c.callCtx(ctx, "Snapshot", req, &resp); err != nil {
		return nil, nil, err
	}
	return resp.Instances, resp.SkippedRepos, nil
}

// SnapshotNoSpawn is a drop-in for daemon.SnapshotNoSpawn over the HTTP API: it
// returns the daemon's authoritative session snapshot WITHOUT ever starting a
// daemon, and collapses every failure — no socket, a refused dial, a warming
// daemon's starting-error envelope (#829) — into daemon.ErrDaemonUnavailable so
// the read-only CLI read path (api/sessions.go) falls back to disk instead of
// spawning a daemon or failing. This is the ONE behavioral contract the LOCAL
// caller depends on, and it matches the net/rpc twin exactly: live daemon → live
// snapshot, otherwise → disk fallback.
//
// For a REMOTE target (#1592 Phase 3 PR4) the contract inverts: there is no local
// disk to fall back to (the sessions live on the remote machine), and a wrong or
// missing token MUST surface as an auth error rather than be masked by a
// same-machine disk read. So a remote failure returns the real error verbatim;
// the caller (api/sessions.go) suppresses its disk fallback when IsRemoteTarget().
//
// The second return carries the daemon's startup-time skipped repos (#603
// closed over the wire); see SnapshotCtx.
func SnapshotNoSpawn(req daemon.SnapshotRequest) ([]session.InstanceData, []daemon.SkippedRepo, error) {
	c, err := NewTargeted()
	if err != nil {
		if IsRemoteTarget() {
			return nil, nil, err
		}
		return nil, nil, daemon.ErrDaemonUnavailable
	}
	instances, skipped, err := c.Snapshot(req)
	if err != nil {
		if IsRemoteTarget() {
			return nil, nil, err
		}
		return nil, nil, classifyLocalSnapshotFailure(err)
	}
	return instances, skipped, nil
}

// classifyLocalSnapshotFailure separates "no daemon exists" from "a daemon did
// not answer" (#6503). Only positive evidence of absence — a missing socket or a
// refused connect — is ErrDaemonUnavailable, the cue to read disk. Everything
// else (the warm-up envelope, a dial timeout under load, an undecodable reply, a
// daemon-side error) came from or toward a live daemon, so it is
// ErrDaemonNotAnswering and the caller decides whether disk can stand in.
func classifyLocalSnapshotFailure(err error) error {
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		return daemon.ErrDaemonUnavailable
	}
	return fmt.Errorf("%w: %v", daemon.ErrDaemonNotAnswering, err)
}

// CallerNoSpawn returns the session a sandbox credential is bound to together
// with that session's (narrowed) row (#5571 W3.2). ok is false when the daemon
// reports no bound caller — an operator credential, an older daemon, or no
// daemon at all — so the caller falls back to resolving by tmux name. It never
// starts a daemon.
func CallerNoSpawn() (row session.InstanceData, ok bool, err error) {
	c, err := NewTargeted()
	if err != nil {
		return session.InstanceData{}, false, err
	}
	var resp daemon.SnapshotResponse
	if err := c.callCtx(context.Background(), "Snapshot", daemon.SnapshotRequest{}, &resp); err != nil {
		return session.InstanceData{}, false, err
	}
	if resp.Caller == "" {
		return session.InstanceData{}, false, nil
	}
	for _, d := range resp.Instances {
		if d.ID == resp.Caller {
			return d, true, nil
		}
	}
	return session.InstanceData{}, false, nil
}
