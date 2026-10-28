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

// WorkspaceFacts measures a session's workspace through its agent-server.
func (s *controlServer) WorkspaceFacts(req WorkspaceFactsRequest, resp *WorkspaceFactsResponse) error {
	if err := s.requireManagerReady(); err != nil {
		return err
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	key := req.ID
	if key == "" {
		key = req.Title
	}
	_, instance, err := s.manager.agentServerForStream(key, req.RepoID)
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
