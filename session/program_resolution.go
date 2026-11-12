package session

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/log"
)

// ProjectConfigUnknownError is a launch-class resolve that could not learn the
// project's configuration: every attempt's repository or checkout-identity
// probe went unanswered (config.ResolveWentUnanswered) — most often a deadline
// a heavily loaded machine could not meet (#7077).
//
// It is an error rather than a fallback on purpose. "Unknown" is not "absent":
// the project may set default_program, program_overrides or
// session_env_passthrough, and resolving the global config in its place starts
// the wrong agent or environment with one warning line as the only evidence.
// A launch that cannot learn the project's answer therefore refuses, and says
// so in words a user can act on rather than as a bare context error.
type ProjectConfigUnknownError struct {
	Path     string
	Attempts int
	Err      error
}

func (e *ProjectConfigUnknownError) Error() string {
	if errors.Is(e.Err, context.DeadlineExceeded) {
		return fmt.Sprintf("couldn't read the project config for %s in time (%s) — the machine may be under heavy load, or its filesystem may not be responding; "+
			"af won't fall back to the global config, which could start the wrong agent, so retry once it recovers: %v",
			e.Path, e.attempts(), e.Err)
	}
	return fmt.Sprintf("couldn't read the project config for %s (%s): git did not answer — "+
		"af won't fall back to the global config, which could start the wrong agent, so retry, and check that git runs on this machine: %v",
		e.Path, e.attempts(), e.Err)
}

func (e *ProjectConfigUnknownError) attempts() string {
	if e.Attempts == 1 {
		return "1 attempt"
	}
	return fmt.Sprintf("%d attempts", e.Attempts)
}

func (e *ProjectConfigUnknownError) Unwrap() error { return e.Err }

func isProjectConfigUnknown(err error) bool {
	var unknown *ProjectConfigUnknownError
	return errors.As(err, &unknown)
}

// IsProjectConfigUnknown reports whether err carries a *ProjectConfigUnknownError:
// the project's config could not be read, which is never the same as having none.
func IsProjectConfigUnknown(err error) bool { return isProjectConfigUnknown(err) }

// ResolveProjectConfigForLaunch is resolveProjectConfig for callers outside
// this package that decide what a launch runs — the daemon's default program
// and default account for a create (#7077 review). It records the in-repo load
// observation like any runtime resolve and spends a launch's retry budget.
// (nil, nil) means path has no project config, and the global config applies.
func ResolveProjectConfigForLaunch(path string) (*config.ResolvedConfig, error) {
	return resolveProjectConfig(path, runtimeProjectConfig())
}

// CheckProjectConfigForLaunch is ResolveProjectConfigForLaunch without the
// durable load observation: a check a create runs before it commits to
// launching — the default account it would scope to — is a read and must not
// create durable state (#4430), but it decides what the launch runs, so it
// keeps a launch's retry budget (#7077 review).
func CheckProjectConfigForLaunch(path string) (*config.ResolvedConfig, error) {
	src := inspectionProjectConfig()
	src.attempts = 0
	return resolveProjectConfig(path, src)
}

// InspectProjectConfig is ResolveProjectConfigForLaunch for read-only callers:
// no durable load observation, and one attempt — a picker must not stall on a
// launch's patience. Its unknown is the caller's to show, not to substitute.
func InspectProjectConfig(path string) (*config.ResolvedConfig, error) {
	return resolveProjectConfig(path, inspectionProjectConfig())
}

// launchConfigResolveAttempts is how many times a launch-class resolve asks for
// the project's config before refusing. Each attempt is already bounded by the
// config layer's own probe budget (registeredProjectScanTimeout, 1s) — the
// budget #7077's load-61 box missed once — so three attempts give a launch
// roughly three times that patience while a wedged probe still fails it in a
// few seconds rather than never. Only unanswered attempts are repeated; an
// answer, including "this config is invalid", is final on the first try.
//
// Package vars so tests can drive the loop without sleeping.
var (
	launchConfigResolveAttempts = 3
	launchConfigRetryPause      = 250 * time.Millisecond
)

// projectConfigSource is where a resolve asks — the real config package, or a
// load's RestoreContext answering the same two questions — and how many
// unanswered attempts it makes before reporting unknown (zero: the launch
// default).
type projectConfigSource struct {
	repoFromPath func(string) (*config.RepoContext, error)
	resolveRepo  func(*config.RepoContext) (*config.ResolvedConfig, error)
	attempts     int
}

func runtimeProjectConfig() projectConfigSource {
	return projectConfigSource{repoFromPath: config.RepoFromPath, resolveRepo: config.ResolveConfigForRepo}
}

func inspectionProjectConfig() projectConfigSource {
	return projectConfigSource{repoFromPath: config.RepoFromPath, resolveRepo: config.ResolveConfigForRepoInspection, attempts: 1}
}

func instanceProjectConfig(i *Instance) projectConfigSource {
	rc := i.restoreContext() // nil outside a load: resolves exactly as before
	src := projectConfigSource{repoFromPath: rc.RepoFromPath, resolveRepo: rc.ResolveConfigForRepo}
	if rc != nil {
		src.attempts = rc.attempts
	}
	return src
}

// resolveProjectConfig is the one place a path's project config is asked for,
// and the one place "absent" and "unknown" are told apart (#7077). It answers:
//
//   - (resolved, nil): the project's config.
//   - (nil, nil): there is no project config to read. RepoFromPath answered —
//     the path is not a repository, or git refused it with a real exit — so
//     callers resolve the global config exactly as they always have.
//   - (nil, err) with an answered err: the repository's config was read and
//     refused (invalid TOML or value). The caller keeps its own policy for that.
//   - (nil, *ProjectConfigUnknownError): every attempt went unanswered.
//
// A project-registry read failure never reaches here as an error: the config
// layer degrades it to "no personal layer" itself (#2216), which #6748 kept.
func resolveProjectConfig(path string, src projectConfigSource) (*config.ResolvedConfig, error) {
	attempts := src.attempts
	if attempts < 1 {
		attempts = launchConfigResolveAttempts
	}
	if attempts < 1 {
		attempts = 1
	}
	var unanswered error
	for attempt := 1; attempt <= attempts; attempt++ {
		if attempt > 1 && launchConfigRetryPause > 0 {
			time.Sleep(launchConfigRetryPause)
		}
		repo, err := src.repoFromPath(path)
		if err != nil {
			if !config.ResolveWentUnanswered(err) {
				return nil, nil
			}
			unanswered = err
			continue
		}
		resolved, err := src.resolveRepo(repo)
		if err == nil {
			return resolved, nil
		}
		if !config.ResolveWentUnanswered(err) {
			return nil, err
		}
		unanswered = err
	}
	return nil, &ProjectConfigUnknownError{Path: path, Attempts: attempts, Err: unanswered}
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
//
// It fails with a *ProjectConfigUnknownError rather than resolve the global
// config when the project's cannot be read (#7077).
func resolveLaunchProgramForInstance(i *Instance) (launchProgramResolution, error) {
	return resolveLaunchProgramForInstanceReporting(i, log.WarningLog.Printf)
}

// resolveLaunchProgramForInstanceReporting is resolveLaunchProgramForInstance
// with the degrade warnings of resolveResolvedConfigForInstance routed through
// warn; nil drops them.
func resolveLaunchProgramForInstanceReporting(i *Instance, warn func(string, ...any)) (launchProgramResolution, error) {
	i.mu.RLock()
	agent := i.Program
	alreadyResolved := agent != "" && agent == i.preResolvedProgram
	i.mu.RUnlock()
	if alreadyResolved {
		return launchProgramResolution{command: agent}, nil
	}

	resolved, err := resolveResolvedConfigForInstanceReporting(i, warn)
	if err != nil {
		return launchProgramResolution{}, err
	}
	command := agent
	if resolved != nil {
		command = config.ResolveProgram(&resolved.Config, agent)
	}
	return launchProgramResolution{
		command:   command,
		trustBase: builtInProgramOverride(resolved, agent, command),
	}, nil
}

// resolveResolvedConfigForInstance is the launch-class resolve with provenance:
// the project's config, the global one when the session has no project config,
// or a *ProjectConfigUnknownError when the project's could not be read — never
// the global config standing in for an unknown project (#7077). A config the
// repository answered as invalid keeps the legacy warn-and-use-global degrade.
func resolveResolvedConfigForInstance(i *Instance) (*config.ResolvedConfig, error) {
	return resolveResolvedConfigForInstanceReporting(i, log.WarningLog.Printf)
}

// resolveResolvedConfigForInstanceReporting is resolveResolvedConfigForInstance
// with its degrade warnings routed through warn; nil drops them.
func resolveResolvedConfigForInstanceReporting(i *Instance, warn func(string, ...any)) (*config.ResolvedConfig, error) {
	if warn == nil {
		warn = func(string, ...any) {}
	}
	resolved, err := resolveProjectConfig(i.Path, instanceProjectConfig(i))
	if err != nil {
		if isProjectConfigUnknown(err) {
			return nil, err
		}
		warn("failed to resolve repo config when resolving program for %q, using the global config: %v", i.Title, err)
	}
	if resolved != nil {
		return resolved, nil
	}
	global, err := config.ResolveGlobalConfig()
	if err != nil {
		warn("failed to load config when resolving program for %q: %v", i.Title, err)
		return nil, nil
	}
	return global, nil
}

func builtInProgramOverride(resolved *config.ResolvedConfig, agent, command string) bool {
	if resolved == nil || agent == "" || config.ResolveProgram(&resolved.Config, agent) != command {
		return false
	}
	return builtInOverrideCandidateMatches(resolved, agent, command)
}

// builtInOverrideCandidateMatches answers whether command is the argv af's own
// built-in detection produced for agent, without requiring it to be today's
// effective override. builtInProgramOverride's effective-resolution gate (Gate 1)
// is correct for a fresh resolution — the command under proof is whatever the
// current config resolves to — but a committed account swap retry's frozen
// command is a matter of durable record: program_overrides may have been edited
// between the durable commit and the restart's recovery, and the retry must
// still re-prove the commit's own frozen base. The provenance question for a
// frozen record is only "is this argv the one af detected", which the built-in
// candidate comparison alone answers; the effective-resolution gate would refuse
// a frozen built-in flagged base whenever the override moved, wedging the retry
// forever (#6820 remainder — the same permanent-wedge class 9c8a7c1b fixed for
// the matching-config case).
func builtInOverrideCandidateMatches(resolved *config.ResolvedConfig, agent, command string) bool {
	if resolved == nil || agent == "" {
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

// resolveLaunchProgramForAgent is resolveLaunchProgramForInstance for an
// explicit target agent — the command a cross-agent swap launches and its
// built-in provenance — failing, like every launch-class resolve, when the
// project's config cannot be read (#7077).
func resolveLaunchProgramForAgent(i *Instance, agent string) (launchProgramResolution, error) {
	resolved, err := resolveResolvedConfigForInstance(i)
	if err != nil {
		return launchProgramResolution{}, err
	}
	command, err := resolveProgramForAgent(i, agent)
	if err != nil {
		return launchProgramResolution{}, err
	}
	return launchProgramResolution{command: command, trustBase: builtInProgramOverride(resolved, agent, command)}, nil
}

// resolveProgramForAgent is the target-explicit form used by handoff preflight.
// It resolves configuration before Instance.Program is rewritten, so an invalid
// incoming override can be rejected while the outgoing process is still alive.
func resolveProgramForAgent(i *Instance, agent string) (string, error) {
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
func resolveProgramForPath(path, agent string) (string, error) {
	cfg, err := resolveConfigForPath(path)
	if err != nil {
		return "", err
	}
	return config.ResolveProgram(cfg, agent), nil
}

// ResolveProgramForPath is the exported form of resolveProgramForPath for
// out-of-package callers that must apply configured overrides before doing
// their own resolution — a directly-invoked `af agent-server` resolves the
// configured command's executable on its host, and the key-level override
// lookup must happen first or the absolute path can no longer match
// program_overrides entries (#5578 review). Like every launch-class resolve it
// fails with a *ProjectConfigUnknownError rather than answer from the global
// config when the project's cannot be read (#7077).
func ResolveProgramForPath(path, agent string) (string, error) {
	return resolveProgramForPath(path, agent)
}

func resolveConfigForInstance(i *Instance) (*config.Config, error) {
	return resolveConfigForPathVia(i.Path, instanceProjectConfig(i))
}

func resolveConfigForPath(path string) (*config.Config, error) {
	return resolveConfigForPathVia(path, runtimeProjectConfig())
}

// resolveConfigForPathInspection is resolveConfigForPath's read-only twin:
// the same repo-then-global precedence answered without the durable in-repo
// load observation ResolveConfigForRepo records. List and picker endpoints
// use it so a read cannot claim the runtime-load log and inrepo-config-hash
// marker the operation that actually consumes the config owes (#4430 review
// round 2).
//
// It is the one resolver that still answers from the global config when the
// project's cannot be read in time — a picker must render something — and it
// asks once rather than spend a launch's retry budget. The substitution is
// named in the log rather than reported as a bare resolve failure, and nothing
// it answers can launch anything: every launch re-resolves through the strict
// chain and refuses on the same unknown (#7077).
func resolveConfigForPathInspection(path string) *config.Config {
	resolved, err := resolveProjectConfig(path, inspectionProjectConfig())
	if err != nil {
		var unknown *ProjectConfigUnknownError
		if errors.As(err, &unknown) {
			log.WarningLog.Printf("couldn't read the project config for %q in time (%v); this read-only lookup is answering from the global config instead, "+
				"and any launch will re-read the project config rather than use this answer", path, unknown.Err)
		} else {
			log.WarningLog.Printf("failed to resolve repo config for %q, answering this read-only lookup from the global config: %v", path, err)
		}
	}
	if resolved != nil {
		return &resolved.Config
	}
	return loadGlobalConfigFor(path)
}

// resolveConfigForPathVia is the launch-class repo-then-global chain over an
// injected source, so a batched restore can answer it from its RestoreContext.
// It never resolves the global config in place of a project config it could
// not read (#7077): that case is a *ProjectConfigUnknownError. A path with no
// project config resolves the global config as it always has, and a config the
// repository answered as invalid keeps its legacy warn-and-use-global degrade.
func resolveConfigForPathVia(path string, src projectConfigSource) (*config.Config, error) {
	resolved, err := resolveProjectConfig(path, src)
	if err != nil {
		if isProjectConfigUnknown(err) {
			return nil, err
		}
		log.WarningLog.Printf("failed to resolve repo config when resolving program for path %q, using the global config: %v", path, err)
	}
	if resolved != nil {
		return &resolved.Config, nil
	}
	return loadGlobalConfigFor(path), nil
}

// loadGlobalConfigFor is the global half of the chain. A global config that
// cannot load answers nil, which config.ResolveProgram reads as "no overrides":
// the label resolves to itself, as it always has.
func loadGlobalConfigFor(path string) *config.Config {
	loaded, err := config.LoadConfig()
	if err != nil {
		log.WarningLog.Printf("failed to load config when resolving program for path %q: %v", path, err)
		return nil
	}
	return loaded
}
