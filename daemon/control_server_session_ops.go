package daemon

import (
	"context"

	"github.com/sachiniyer/agent-factory/session"
)

func (s *controlServer) archiveSession(ctx context.Context, req *ArchiveSessionRequest, resp *ArchiveSessionResponse) error {
	return s.archiveSessionGuarded(ctx, req, resp, nil)
}

// archiveSessionGuarded is archiveSession with an optional guard evaluated inside
// the archive fence (see sessionTeardownGuard).
func (s *controlServer) archiveSessionGuarded(ctx context.Context, req *ArchiveSessionRequest, resp *ArchiveSessionResponse, guard sessionTeardownGuard) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	// The `af sessions archive --self` caller this issue is about (#5182):
	// register the kernel-verified requester before its own pane tree is
	// reaped, and unregister when the reply is on the wire.
	defer s.trackTeardownRequester(ctx, req)()
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	// ArchiveSession resolves the target (id-first, erroring on a stale/missing id),
	// commits it, and publishes the full projection inside its operation lock. That
	// keeps lifecycle event order identical to operation order (#2680).
	archivedPath, archived, err := s.manager.archiveSessionGuarded(*req, guard)
	if !resp.record(err) {
		return err
	}
	resp.OK = true
	resp.ArchivedPath = archivedPath
	resp.ArchivedWorkspace = archived.ArchivedWorkspace
	return nil
}

func (s *controlServer) SendPrompt(req SendPromptRequest, resp *SendPromptResponse) error {
	return s.sendPrompt(req, resp, nil)
}

// sendPrompt is SendPrompt with an optional send guard evaluated under the
// target's op lock on the instance the send reaches (see sendPromptGuarded).
func (s *controlServer) sendPrompt(req SendPromptRequest, resp *SendPromptResponse, guard func(*session.Instance, string) error) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	status, err := s.manager.sendPromptGuarded(req, guard)
	if err != nil {
		return err
	}
	resp.OK = true
	resp.Status = status
	return nil
}
