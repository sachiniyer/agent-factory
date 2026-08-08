package session

import (
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
)

// resolveProgramForInstance returns the actual tmux command for an instance.
// Resolution chain: agent enum -> cfg.ProgramOverrides[agent] (if set) -> bare
// agent name. Repo-resolved config wins when the path belongs to a repository;
// otherwise the global config applies. A nil cfg preserves legacy free-form
// Program values verbatim.
func resolveProgramForInstance(i *Instance) string {
	i.mu.RLock()
	agent := i.Program
	alreadyResolved := agent != "" && agent == i.preResolvedProgram
	i.mu.RUnlock()
	if alreadyResolved {
		return agent
	}
	return resolveProgramForAgent(i, agent)
}

type launchProgramResolution struct {
	command   string
	trustBase bool
}

// resolveLaunchProgramForInstance resolves a create/restore command and the
// provenance needed by the account boundary in one config snapshot. Only a
// command that exactly matches the same resolution's built-in detected override
// can receive executable provenance; any differing global, repo, or personal
// override remains operator/repository-authored and undeclared (#3108).
func resolveLaunchProgramForInstance(i *Instance) launchProgramResolution {
	i.mu.RLock()
	agent := i.Program
	alreadyResolved := agent != "" && agent == i.preResolvedProgram
	i.mu.RUnlock()
	if alreadyResolved {
		return launchProgramResolution{command: agent}
	}

	resolved := resolveResolvedConfigForInstance(i)
	command := agent
	if resolved != nil {
		command = config.ResolveProgram(&resolved.Config, agent)
	}
	return launchProgramResolution{
		command:   command,
		trustBase: builtInProgramOverride(resolved, agent, command),
	}
}

func resolveResolvedConfigForInstance(i *Instance) *config.ResolvedConfig {
	if repo, err := config.RepoFromPath(i.Path); err == nil {
		if resolved, rerr := config.ResolveConfig(repo.Root); rerr == nil {
			return resolved
		} else {
			log.WarningLog.Printf("failed to resolve repo config when resolving program for %q: %v", i.Title, rerr)
		}
	}
	resolved, err := config.ResolveGlobalConfig()
	if err != nil {
		log.WarningLog.Printf("failed to load config when resolving program for %q: %v", i.Title, err)
		return nil
	}
	return resolved
}

func builtInProgramOverride(resolved *config.ResolvedConfig, agent, command string) bool {
	if resolved == nil || agent == "" || config.ResolveProgram(&resolved.Config, agent) != command {
		return false
	}
	value, ok := resolved.ResolvedValuePath("program_overrides." + agent)
	if !ok {
		return false
	}
	// First-run materialization writes DefaultConfig to config.toml, so the
	// effective leaf is reported as global even though its exact value came from
	// af's detected built-in. Compare against the built-in candidate from the same
	// resolution instead of trusting the winner's location. A user/repo value is
	// admitted only when it is byte-for-byte the command af independently detected
	// for this process; a different path or argument remains undeclared.
	for _, candidate := range value.Candidates {
		if candidate.Layer != config.SourceBuiltIn.String() || !candidate.Present {
			continue
		}
		builtIn, stringValue := candidate.Value.(string)
		return stringValue && builtIn == command
	}
	return false
}

// resolveProgramForAgent is the target-explicit form used by handoff preflight.
// It resolves configuration before Instance.Program is rewritten, so an invalid
// incoming override can be rejected while the outgoing process is still alive.
func resolveProgramForAgent(i *Instance, agent string) string {
	cfg := resolveConfigForInstance(i)
	// Read the enum through the accessor, not the bare field: a handoff (#2013)
	// rewrites Program in place while the instance is live and shared, so this
	// is a genuinely concurrent read now. Every other reader of the field
	// (ToInstanceData, ReconcileTabsFromData) already holds the instance lock.
	return config.ResolveProgram(cfg, agent)
}

func resolveConfigForInstance(i *Instance) *config.Config {
	var cfg *config.Config
	if repo, err := config.RepoFromPath(i.Path); err == nil {
		if resolved, rerr := config.ResolveConfig(repo.Root); rerr == nil {
			cfg = &resolved.Config
		} else {
			log.WarningLog.Printf("failed to resolve repo config when resolving program for %q: %v", i.Title, rerr)
		}
	}
	if cfg == nil {
		loaded, err := config.LoadConfig()
		if err != nil {
			log.WarningLog.Printf("failed to load config when resolving program for %q: %v", i.Title, err)
			loaded = nil
		}
		cfg = loaded
	}
	return cfg
}
