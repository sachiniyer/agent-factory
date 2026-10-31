package session

import (
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/log"
)

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
	rc := i.restoreContext() // nil outside a batched restore: resolves exactly as before
	if repo, err := rc.RepoFromPath(i.Path); err == nil {
		if resolved, rerr := rc.ResolveConfigForRepo(repo); rerr == nil {
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
	// resolution instead of trusting the winner's location. The comparison is on
	// parsed argv, not bytes (#5487): af versions disagree on whether the detected
	// path was persisted quoted or bare, and those spellings are the same words.
	// A user/repo value is admitted only when it runs the identical argv af
	// independently detected for this process — a different path, an extra or
	// reordered word, or anything the literal parse cannot prove (an expansion,
	// an assignment prefix, an operator) remains undeclared.
	for _, candidate := range value.Candidates {
		if candidate.Layer != config.SourceBuiltIn.String() || !candidate.Present {
			continue
		}
		builtIn, stringValue := candidate.Value.(string)
		return stringValue && sessionenv.LiteralCommandsEqual(builtIn, command)
	}
	return false
}

// resolveProgramForAgent is the target-explicit form used by handoff preflight.
// It resolves configuration before Instance.Program is rewritten, so an invalid
// incoming override can be rejected while the outgoing process is still alive.
func resolveProgramForAgent(i *Instance, agent string) string {
	// Read the enum through the accessor, not the bare field: a handoff (#2013)
	// rewrites Program in place while the instance is live and shared, so this
	// is a genuinely concurrent read now. Every other reader of the field
	// (ToInstanceData, ReconcileTabsFromData) already holds the instance lock.
	return resolveProgramForPath(i.Path, agent)
}

// resolveProgramForPath is the session-free half of resolveProgramForAgent: the
// same repo-then-global config chain over an explicit path. The daemon's
// account-list response resolves handoff targets through it for callers that
// hold no Instance — a resolved command means the same thing whichever caller
// asked (#4430 review).
func resolveProgramForPath(path, agent string) string {
	return config.ResolveProgram(resolveConfigForPath(path), agent)
}

// ResolveProgramForPath is the exported form of resolveProgramForPath for
// out-of-package callers that must apply configured overrides before doing
// their own resolution — a directly-invoked `af agent-server` resolves the
// configured command's executable on its host, and the key-level override
// lookup must happen first or the absolute path can no longer match
// program_overrides entries (#5578 review).
func ResolveProgramForPath(path, agent string) string {
	return resolveProgramForPath(path, agent)
}

func resolveConfigForInstance(i *Instance) *config.Config {
	rc := i.restoreContext()
	return resolveConfigForPathVia(i.Path, rc.RepoFromPath, rc.ResolveConfigForRepo)
}

func resolveConfigForPath(path string) *config.Config {
	return resolveConfigForPathResolving(path, config.ResolveConfigForRepo)
}

// resolveConfigForPathInspection is resolveConfigForPath's read-only twin:
// the same repo-then-global precedence answered without the durable in-repo
// load observation ResolveConfigForRepo records. List and picker endpoints
// use it so a read cannot claim the runtime-load log and inrepo-config-hash
// marker the operation that actually consumes the config owes (#4430 review
// round 2).
func resolveConfigForPathInspection(path string) *config.Config {
	return resolveConfigForPathResolving(path, config.ResolveConfigForRepoInspection)
}

func resolveConfigForPathResolving(path string, resolveRepo func(*config.RepoContext) (*config.ResolvedConfig, error)) *config.Config {
	return resolveConfigForPathVia(path, config.RepoFromPath, resolveRepo)
}

// resolveConfigForPathVia is resolveConfigForPathResolving with the repo lookup
// injected, so a batched restore can answer both from its RestoreContext.
func resolveConfigForPathVia(
	path string,
	repoFromPath func(string) (*config.RepoContext, error),
	resolveRepo func(*config.RepoContext) (*config.ResolvedConfig, error),
) *config.Config {
	var cfg *config.Config
	if repo, err := repoFromPath(path); err == nil {
		if resolved, rerr := resolveRepo(repo); rerr == nil {
			cfg = &resolved.Config
		} else {
			log.WarningLog.Printf("failed to resolve repo config when resolving program for path %q: %v", path, rerr)
		}
	}
	if cfg == nil {
		loaded, err := config.LoadConfig()
		if err != nil {
			log.WarningLog.Printf("failed to load config when resolving program for path %q: %v", path, err)
			loaded = nil
		}
		cfg = loaded
	}
	return cfg
}
