package daemon

import (
	"strings"

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
	// Trim the account so a whitespace-only value is treated as "no account"
	// here too, rather than reaching agentaccount.Selected — which trims
	// internally and silently returns a zero Account for whitespace, skipping
	// the validation a non-empty value must pass.
	if account := strings.TrimSpace(req.Account); account != "" {
		home, err := config.GetConfigDir()
		if err != nil {
			return err
		}
		if _, err := agentaccount.Selected(home, sessionenv.AgentForCommand(program), account); err != nil {
			return err
		}
	}
	// The host pick is a record-independent refusal knowable from cfg.Hosts —
	// exactly the class #5744 moved ahead of the reservation. reserveCreate can
	// rename an archived session holding the requested title (relocating its
	// worktree and rewriting its durable record) to free the name, and an
	// unlisted host fails only later inside sshRuntime.Provision, after that
	// rename has run. The local CLI already gives this refusal client-side
	// (localCreateInputs); the daemon owns the same list here, so this is the
	// one check the hook's doc comment promises. An empty host is "no pick"
	// and is not an error, matching localCreateInputs and ValidateHostPick's
	// own contract. A whitespace-only value is treated as absent for the same
	// reason the account is trimmed above.
	if host := strings.TrimSpace(req.Host); host != "" {
		if err := config.ValidateHostPick(cfg.Hosts, req.Host); err != nil {
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
