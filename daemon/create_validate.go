package daemon

import (
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/preflight"
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
	local, err := createNeedsDaemonHostPrereqs(req, repo.WorkspacePath())
	if err != nil {
		return err
	}
	if local {
		// Relative overrides resolve where the launch will run: the session's
		// repo/workspace, not this daemon's cwd (#5779).
		return preflight.LocalSessionPrereqsAt(&cfg.Config, program, repo.WorkspacePath())
	}
	return nil
}
