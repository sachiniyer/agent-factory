package configagent

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/preflight"
	"github.com/sachiniyer/agent-factory/session"
)

// The config agent (phase 2): the user's own default agent, spawned as an
// ordinary session and pre-briefed with the config manifest, that walks them
// through configuration and applies each choice with `af config set`.
//
// It rides the IN-PLACE (`af sessions create --here`) seam rather than getting a
// session kind of its own. That is the whole reason this package is small: the
// daemon already plumbs InPlace end to end (CreateSessionRequest.InPlace →
// session.NewInstance → git.NewGitWorktreeInPlace), the daemon's own root-agent
// loop already uses it in-process, and an in-place worktree creates no branch
// and no worktree — Setup() and Cleanup() are no-ops for it, so a config session
// cannot remove the user's tree or branch even when killed.
//
// The trade-off that buys is real and is NOT hidden: the agent's working
// directory IS the user's live working tree. Nothing enforces that it only
// touches config — the scope fence in BuildBriefing is an instruction, not a
// sandbox. That is the owner's decided trade; the fence is worded accordingly.
//
// The hotkey (phase 3) and the first-run trigger (phase 4) are the callers.
// Until they land, Spawn has no production caller by design.

// configSessionTitleBase is the title new config sessions are derived from. It
// is a TitleBase rather than a Title so the daemon auto-suffixes on collision
// (config, config-2, …) instead of failing when one already exists — pressing
// the phase-3 hotkey twice must not be an error. It is not the reserved "root".
const configSessionTitleBase = "config"

// ProgramUnavailableError reports that the user's configured agent is not
// installed or not runnable, so no config session was created.
//
// It is typed so a caller (the phase-3 hotkey, the phase-4 first-run trigger)
// can render it as a one-line fallback — "af couldn't start <agent>; run
// `af config set …` yourself" — instead of a modal error or, worse, a spawn that
// hangs until a readiness timeout. Spawning a missing binary is exactly how you
// get that hang, so this is checked BEFORE any daemon round trip.
//
// The message is preflight.ProgramError's, verbatim: it already names the agent,
// the resolved command, and the three ways out (install it, pick another agent,
// fix program_overrides). Re-wording it here would be a worse message maintained
// twice.
type ProgramUnavailableError struct {
	// Agent is the agent enum that could not be run (e.g. "claude").
	Agent string
	// Command is what the agent resolved to through program_overrides — the
	// thing that was actually looked for on disk/PATH.
	Command string
	// Err is the rendered, user-facing cause from preflight.ProgramError.
	Err error
}

func (e *ProgramUnavailableError) Error() string { return e.Err.Error() }

// Unwrap exposes the preflight cause to errors.Is/As.
func (e *ProgramUnavailableError) Unwrap() error { return e.Err }

// Options configures a config-agent spawn.
type Options struct {
	// Mode selects the opening behavior: a guided walkthrough (ModeOnboard) or
	// a targeted change (ModeChange).
	Mode Mode
	// RepoPath is the repository the session attaches to in place — the user's
	// cwd. It must be a git repository: the in-place seam resolves the repo root
	// and the current branch from it.
	RepoPath string
}

// createSession is the daemon round trip, as a package var so tests can observe
// the request without a daemon. It mirrors api/sessions.go's
// `createSessionViaDaemon = daemon.CreateSession` — the established idiom for a
// stubbable daemon call — and routes over the unix control socket, the same path
// `af sessions create` uses.
var createSession = daemon.CreateSession

// resolveConfigForRepo is config.ResolveConfig behind a var, so a test can drive
// the missing-binary path without materializing a repo. Production always uses
// the real resolver.
var resolveConfigForRepo = config.ResolveConfig

// Spawn starts the config agent against repoPath and returns the created
// session. The flow, and why each step is where it is:
//
//  1. Resolve the repo's config — this picks up an in-repo default_program /
//     program_overrides, so the agent we launch is the one the user actually gets
//     in this repo, exactly like `af sessions create` with no -p.
//  2. Preflight the program. A missing binary returns ProgramUnavailableError and
//     creates NOTHING — never a spawn that hangs to a readiness timeout.
//  3. Create the session in place, with the briefing as its Prompt. The daemon
//     does the rest: NewGitWorktreeInPlace (no branch, no worktree), tmux spawn,
//     WaitForReady, trust-prompt dismissal, then SendPrompt.
//
// AutoYes is hard-wired false and must stay that way, for ONE reason: this is an
// interactive walkthrough, and auto-yes would answer the user's own questions for
// them.
//
// It is NOT a permission control, and it would be dishonest to imply otherwise.
// AutoYes=false does keep resolveProgramForInstance from appending claude's
// `--permission-mode bypassPermissions` — but on a default install that buys
// nothing, because DefaultConfig() already seeds
// program_overrides.claude = "<detected claude> --dangerously-skip-permissions"
// (config_types.go), a strictly broader flag that AutoYes has no say over. So a
// default claude user's config agent runs with permissions skipped either way.
//
// What actually bounds this agent is the scope fence in the briefing — an
// instruction, not a sandbox. That is the honest posture, and it is why the fence
// is worded as forcefully as it is.
func Spawn(opts Options) (*session.InstanceData, error) {
	if opts.RepoPath == "" {
		return nil, fmt.Errorf("config agent needs a repository to run in: no repo path given")
	}

	// The briefing describes the GLOBAL config, because that is what `af config
	// set`/`get` read and write — briefing the agent on a repo-resolved view
	// would show it values it cannot write. The PROGRAM comes from the resolved
	// config just below, which is a different question: which agent this user
	// gets in this repo.
	globalCfg, err := config.LoadConfig()
	if err != nil {
		return nil, fmt.Errorf("cannot read the global config to brief the config agent: %w", err)
	}
	configPath, pathErr := config.GlobalConfigPath()
	if pathErr != nil {
		// Only the path LABEL in the briefing is lost; the briefing still
		// renders and every `af config` command still resolves the file itself.
		configPath = ""
	}

	resolved, err := resolveConfigForRepo(opts.RepoPath)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve the config for %s: %w", opts.RepoPath, err)
	}
	agent := resolved.DefaultProgram

	// Check the binary before spending a daemon round trip, a tmux session and a
	// readiness wait on a program that cannot start.
	if _, perr := preflight.CheckProgram(&resolved.Config, agent); perr != nil {
		command := config.ResolveProgram(&resolved.Config, agent)
		return nil, &ProgramUnavailableError{
			Agent:   agent,
			Command: command,
			Err:     preflight.ProgramError(agent, command, perr),
		}
	}

	data, err := createSession(daemon.CreateSessionRequest{
		TitleBase: configSessionTitleBase,
		RepoPath:  opts.RepoPath,
		Program:   agent,
		Prompt:    BuildBriefing(opts.Mode, globalCfg, configPath),
		AutoYes:   false,
		InPlace:   true,
		// Pin the LOCAL runtime explicitly. This is the feature's premise, not a
		// default worth inheriting: the config agent exists to inspect THE USER'S
		// machine and fix THEIR config, so it has to run on the machine whose
		// config it is reading.
		//
		// An empty Backend does NOT mean local — it means "daemon, you decide",
		// and the daemon decides from the repo's config
		// (session/instance_factory.go resolveBackendKind falls through to
		// resolveRepoConfig). So in a repo that declares `backend = "docker"` /
		// `ssh` / `hook`, an unpinned config agent spawns on the REMOTE and then
		// faithfully inspects the wrong machine, reporting on an environment the
		// user does not have. Nothing about that failure looks like a failure,
		// which is what makes it worse than not starting at all.
		//
		// A remote config session has no meaningful semantics today. If one ever
		// does, that should be a deliberate change here, not something a repo's
		// checked-in config can turn on by accident.
		Backend: config.BackendLocal,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to start the config agent: %w", err)
	}
	return data, nil
}
