package daemon

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

// The project's default credential account, applied where every surface passes
// through (#3386).
//
// It lives in the DAEMON, on the create, for the same reason defaultProgramFor
// does: TUI, web, CLI and task deliveries all reach CreateSession, so a default
// applied here is honoured by all of them with no surface re-implementing the
// precedence — and a surface that does render it (the TUI's ctrl+o field, the
// web's Account select) reads the same answer from ListAccounts rather than
// computing its own.
//
// The precedence is agentaccount.Resolve's, unchanged and now with a production
// caller: an explicit --account (or a picker choice) beats the project default,
// which beats the global one, which leaves the session on the ambient identity.

// applyDefaultAccount fills in a create's Account from config when the request
// named none, and REFUSES the create when the configured default cannot be
// honoured.
//
// Refusing is the whole point. A default that silently fell back would start the
// session on whatever identity the agent finds ambiently while the project's
// config — and every listing — said otherwise, which is the #2983
// silently-wrong-identity outcome arriving through configuration instead of
// through the environment.
//
// It runs BEFORE reserveCreate, so a refusal costs no worktree, no branch and no
// tmux session.
func applyDefaultAccount(cfg *config.Config, req *CreateSessionRequest) error {
	if strings.TrimSpace(req.Account) != "" {
		// An explicit account wins and is already validated by the surface that
		// took it (api/sessions.go for the CLI, the pickers for the UIs) and, in
		// every case, by the launch boundary itself.
		return nil
	}
	if req.allowReserved {
		// The daemon's own root-agent ensure loop, and deliberately out of scope.
		//
		// A root agent is an always-ensured singleton whose command comes from
		// `[root_agent]` config rather than from an agent enum — a full command
		// string, with `--dangerously-skip-permissions` ensured. Nothing has
		// established that the account boundary can PROVE that shape of launch, and
		// the boundary fails closed: pulling roots into account scoping as a side
		// effect of a per-project preference could leave a project's guaranteed
		// session refusing to start on every ensure tick, for a key the user set
		// about their ordinary creates. #3386 is about sessions a person makes.
		//
		// Scoping a root deliberately belongs in `[root_agent]`, beside the program
		// it already names, where it would be a choice rather than a consequence.
		return nil
	}
	// The LABEL's agent, which is what an account name is validated against
	// everywhere else (api/sessions.go, app/account_picker.go). A program_overrides
	// entry that points the label at another agent is refused by
	// session.refuseUnsupportedAccountAgent with a message naming both — and, since
	// #3386, naming this config key too when the account came from here.
	agent := sessionenv.AgentForCommand(req.Program)
	if agent == "" {
		return nil
	}
	project, global := config.DefaultAccountLayersFor(cfg, req.RepoPath, agent)
	name := agentaccount.Resolve(req.Account, project.Name, global.Name)
	if name == "" {
		return nil
	}
	selection := project
	if selection.Name != name {
		selection = global
	}
	home, err := config.GetConfigDir()
	if err != nil {
		return fmt.Errorf("%s selects account %q, but af cannot resolve its agent-factory home to check it: %w",
			selection.Source(), name, err)
	}
	if err := config.CheckDefaultAccount(home, req.RepoPath, selection); err != nil {
		return err
	}
	req.Account = name
	req.AccountSource = defaultAccountProvenance(selection, req.RepoPath)
	return nil
}

// defaultAccountProvenance is the sentence a later refusal appends so a user who
// never typed --account is told where the account came from and how to remove it.
func defaultAccountProvenance(selection config.DefaultAccountSelection, repoPath string) string {
	return fmt.Sprintf("this session's account was not requested — it comes from %s; clear it with `%s`",
		selection.Source(), selection.ClearHint(repoPath))
}

// defaultAccountsFor reports, per agent on the account roster, the account a
// create with no explicit account would resolve to for repoPath. It is the
// catalog half of applyDefaultAccount and calls the same resolver, so a picker
// cannot preselect an account the create would not have applied.
//
// Unvalidated on purpose: this answers "what is configured", and a listing that
// silently dropped an unregistered default would hide exactly the misconfiguration
// the create is about to refuse. The entry that comes back is what the create
// will use, right or wrong.
func defaultAccountsFor(cfg *config.Config, repoPath string, agents []string) map[string]string {
	// ONE resolution for every agent, not one per agent: this runs on every open of
	// a form nobody has submitted yet, and resolving a repository's config costs git
	// probes.
	effective := config.ResolvedDefaultAccountsFor(cfg, repoPath)
	defaults := map[string]string{}
	for _, agent := range agents {
		if name := effective[agent]; name != "" {
			defaults[agent] = name
		}
	}
	return defaults
}
