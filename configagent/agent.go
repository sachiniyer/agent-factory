package configagent

import (
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/preflight"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The config agent (phase 2): the user's own default agent, spawned as an
// ordinary session and pre-briefed with the config manifest, that walks them
// through configuration and applies each choice with `af config set`.
//
// It runs as a BARE tmux session owned by the daemon, at AF home, with no
// session.Instance behind it — so it creates no worktree, no branch, and no row
// in the session list.
//
// It is not an Instance because it cannot be: an Instance IS a row. It is
// persisted to instances.json, Snapshot() builds the session list by iterating
// the same map the attach route resolves against, and it is restorable and
// killable from the sidebar. "Reachable by the WS attach route" and "appears in
// the roster" are literally the same bit, so there is no arrangement where the
// config agent is an attachable Instance and not a row.
//
// The takeover therefore does not use the WS route at all: the TUI hands the
// terminal to `tmux attach-session` through tea.ExecProcess, which is bubbletea's
// own primitive for running an interactive child (an editor, a shell) inside a
// Program. The daemon still owns the session's whole life — it spawns it, waits
// for readiness, dismisses the trust prompt, delivers the briefing, and reaps it
// — because a TUI-spawned process is one the daemon cannot clean up if the TUI
// dies, and this repo has a history of orphaned tmux (#1093, #1104).
//
// Running at AF home also fixes what the previous in-place seam could not: the
// agent's working directory is now the directory whose config it is editing, not
// the user's live working tree.
//
// The hotkey (phase 3) and the first-run trigger (phase 4) are the callers.

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
	// RepoPath is the repo whose resolved config picks the agent — the user's
	// active project. It does NOT become the agent's working directory: the
	// config agent runs at AF home, so it never sits in the user's tree. It need
	// not be a git repo; an unresolvable path falls back to the global config.
	RepoPath string
}

// spawnViaDaemon is the daemon round trip, as a package var so tests can observe
// the request without a daemon. It mirrors api/sessions.go's
// `createSessionViaDaemon = daemon.CreateSession` — the established idiom for a
// stubbable daemon call — and routes over the unix control socket, whose
// callDaemon already carries the daemon warm-up retry.
var spawnViaDaemon = daemon.SpawnConfigAgent

// reapViaDaemon tears the config-agent session down once the caller is done with
// it. A package var for the same reason.
var reapViaDaemon = daemon.ReapConfigAgent

// Reap tears down a config-agent session by name. The caller invokes it when the
// user leaves the takeover; the daemon reaps on its own shutdown regardless, so a
// missed call leaks nothing beyond the daemon's life.
func Reap(sessionName string) error {
	if sessionName == "" {
		return nil
	}
	return reapViaDaemon(sessionName)
}

// resolveConfigForRepo is config.ResolveConfig behind a var, so a test can drive
// the missing-binary path without materializing a repo. Production always uses
// the real resolver.
var resolveConfigForRepo = config.ResolveConfig

// Spawn starts the config agent and returns the tmux session name AND the
// absolute socket path to attach to.
//
//  1. Resolve the repo's config — this picks up an in-repo default_program /
//     program_overrides, so the agent launched is the one the user actually gets
//     in this repo, exactly like `af sessions create` with no -p.
//  2. Preflight the program. A missing binary returns ProgramUnavailableError and
//     spawns NOTHING — never a spawn that hangs to a readiness timeout.
//  3. Ask the daemon to run it in a bare tmux session at AF home. The daemon owns
//     the rest: readiness, trust-prompt dismissal, briefing delivery, and reaping.
//
// The socket path rides back alongside the name so the caller can attach with
// `tmux -S <socket>` (#2019); it may be empty, in which case the attach falls
// back to the default socket.
//
// No Instance is created anywhere in this path, which is what keeps the config
// agent out of instances.json and out of the session list.
//
// Approval behavior is not a field on this request: the agent runs its command
// verbatim. Note what that does NOT buy — on a default install
// program_overrides.claude already carries --dangerously-skip-permissions
// (config_types.go), so a default claude user's config agent runs with
// permissions skipped. What actually bounds this agent is the scope fence in the
// briefing: an instruction, not a sandbox. That is the honest posture, and it is
// why the fence is worded as forcefully as it is.
func Spawn(opts Options) (string, string, error) {
	// The briefing describes the GLOBAL config, because that is what `af config
	// set`/`get` read and write — briefing the agent on a repo-resolved view
	// would show it values it cannot write. The PROGRAM comes from the resolved
	// config just below, which is a different question: which agent this user
	// gets in this repo.
	globalCfg, err := config.LoadConfig()
	if err != nil {
		return "", "", fmt.Errorf("cannot read the global config to brief the config agent: %w", err)
	}
	configPath, pathErr := config.GlobalConfigPath()
	if pathErr != nil {
		// Only the path LABEL in the briefing is lost; the briefing still renders
		// and every `af config` command still resolves the file itself.
		configPath = ""
	}

	// An unresolvable repo (no path given, or not a repo) is not fatal: the
	// config agent edits the GLOBAL config, so the global default_program is a
	// correct answer. It only costs an in-repo default_program override.
	agentCfg := globalCfg
	if opts.RepoPath != "" {
		if resolved, rerr := resolveConfigForRepo(opts.RepoPath); rerr == nil {
			agentCfg = &resolved.Config
		} else {
			log.WarningLog.Printf("config agent: could not resolve the config for %s (%v); using the global config", opts.RepoPath, rerr)
		}
	}
	agent := agentCfg.DefaultProgram
	command := config.ResolveProgram(agentCfg, agent)

	// Check the binary before spending a daemon round trip, a tmux session and a
	// readiness wait on a program that cannot start. Checked against the resolved
	// command BEFORE the trust flag is appended below, so an unavailable-program
	// error names the command the user configured, not one with an af flag on it.
	if _, perr := preflight.CheckProgram(agentCfg, agent); perr != nil {
		return "", "", &ProgramUnavailableError{
			Agent:   agent,
			Command: command,
			Err:     preflight.ProgramError(agent, command, perr),
		}
	}

	// Suppress devin's first-run workspace-trust modal the same way an ordinary
	// session does. This path never runs injectSystemPrompt — a config agent edits
	// config, has no worktree, and must not inherit skill/plugin injection — but
	// without the trust flag a devin config agent launches bare, hangs on the modal
	// for the full readiness timeout, and is reaped (#2435). Scoped to the trust
	// flag only, via the shared helper both launch paths call; a non-devin command
	// is returned unchanged.
	command = tmux.EnsureDevinWorkspaceTrustSuppressed(command)

	name, socketPath, err := spawnViaDaemon(daemon.SpawnConfigAgentRequest{
		Program: command,
		Prompt:  BuildBriefing(opts.Mode, globalCfg, configPath),
	})
	if err != nil {
		return "", "", fmt.Errorf("failed to start the config agent: %w", err)
	}
	return name, socketPath, nil
}
