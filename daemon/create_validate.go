package daemon

import (
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/preflight"
	"github.com/sachiniyer/agent-factory/session"
)

// validateCreateFirst runs the host-owned checks the local CLI runs client-side
// (api/sessions.go localCreateInputs), on the host that owns the answers, BEFORE
// the title reservation: reserveCreate can rename an existing archive and
// relocate its worktree to free the title, so a create destined to fail must
// fail first (#5744). Opt-in via ValidateFirst — the remote CLI sets it; clients
// that validated on their own host do not repeat it here.
func validateCreateFirst(req CreateSessionRequest) error {
	repo, err := config.RepoFromPath(req.RepoPath)
	if err != nil {
		return err
	}
	cfg, err := config.ResolveConfigForRepo(repo)
	if err != nil {
		return err
	}
	program := req.Program
	if program == "" {
		program = cfg.DefaultProgram
	}
	if req.Account != "" {
		home, err := config.GetConfigDir()
		if err != nil {
			return err
		}
		if _, err := agentaccount.Selected(home, sessionenv.AgentForCommand(program), req.Account); err != nil {
			return err
		}
	}
	local, err := session.LocalPrereqsRequired(session.InstanceOptions{
		Backend: session.BackendKind(req.Backend),
	}, repo.WorkspacePath())
	if err != nil {
		return err
	}
	if local {
		return preflight.LocalSessionPrereqs(&cfg.Config, program)
	}
	return nil
}
