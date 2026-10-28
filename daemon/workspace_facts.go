package daemon

import (
	"errors"

	"github.com/sachiniyer/agent-factory/session"
)

// WorkspaceFactsRequest asks for the measured contents of an off-box session's
// workspace, for its kill confirmation (#5571 W8.2).
type WorkspaceFactsRequest struct {
	Title  string `json:"title"`
	RepoID string `json:"repo_id"`
	// ID is the session's stable id; when set it is the primary lookup key.
	ID string `json:"id"`
}

// WorkspaceFactsResponse carries the facts, or Measured=false with a Reason when
// the workspace could not be measured (unsupported agent-server, host
// unreachable). Unmeasured is not an RPC error: the caller must show it as "could
// not verify", never as a clean workspace.
type WorkspaceFactsResponse struct {
	Measured bool                   `json:"measured"`
	Reason   string                 `json:"reason,omitempty"`
	Facts    session.WorkspaceFacts `json:"facts"`
}

// workspaceFactsTarget picks the lookup namespace. An explicit stable ID is
// resolved as an ID alone: agentServerForStream would read it as a title once a
// repo scope is present, so a session whose title differs from its ID would never
// be found, and a stale ID must refuse rather than fall back to a title that now
// names a different session. Without an ID the repo-scoped title is authoritative.
func workspaceFactsTarget(req WorkspaceFactsRequest) streamTarget {
	if req.ID != "" {
		return streamTarget{stableID: req.ID, idOnly: true}
	}
	return authoritativeStreamTarget(req.Title, req.RepoID)
}

// WorkspaceFacts measures a session's workspace through its agent-server.
func (s *controlServer) WorkspaceFacts(req WorkspaceFactsRequest, resp *WorkspaceFactsResponse) error {
	if err := s.requireManagerReady(); err != nil {
		return err
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	_, instance, err := s.manager.agentServerForStreamTarget(workspaceFactsTarget(req))
	if err != nil {
		return err
	}
	facts, err := instance.WorkspaceFacts()
	if err != nil {
		if errors.Is(err, session.ErrWorkspaceFactsUnsupported) {
			resp.Reason = err.Error()
		} else {
			resp.Reason = "could not reach the workspace: " + err.Error()
		}
		return nil
	}
	resp.Measured = true
	resp.Facts = facts
	return nil
}
