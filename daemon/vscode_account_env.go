package daemon

import (
	"fmt"
	"os"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// This file owns the ENVIRONMENT the daemon-spawned editor is exec'd with:
// which of the daemon's own variables it may keep, and — for a session that
// selected an account — which credential root it is pinned to and which
// identity-bearing names are subtracted.
//
// WHAT THE BOUNDARY DOES AND DOES NOT COVER (#3870). Everything af LAUNCHES
// inside the editor inherits the account: the editor process, its extension
// host, every process an extension spawns, and every integrated terminal, all
// from one environ fixed at exec. What af cannot prove is what an INTERACTIVE
// SHELL does after that. An integrated terminal is bash (or the operator's
// shell), reading /etc/bash.bashrc and ~/.bashrc, and one line of `export
// CLAUDE_CONFIG_DIR=…` in either file puts the ambient identity back — measured,
// not assumed. Removing the variable cannot close that door either: an agent's
// ambient credential root is a DEFAULT PATH under $HOME, so a startup file need
// only unset the variable to reach it.
//
// af's own panes avoid this because af chooses their command: the exec shim
// hands the agent to `/bin/sh -c`, which reads no startup file, and an
// account-scoped shell tab is pinned to `bash --noprofile --norc -i`
// (sessionenv.AccountShellCommand). The editor's terminal profile is not af's to
// choose in the same way — the profile lives in code-server's user-data
// directory, which af deliberately SHARES across every session (vscodeArgs), and
// which is rewritable from inside the editor being scoped. A default written
// there is a default, not a boundary.
//
// So the honest answer stays "af cannot prove the integrated-terminal boundary",
// and admitAccountSwap keeps refusing a session with a VS Code tab. Scoping the
// editor is still the right default and still removes the failure mode that
// needs no startup file at all: an extension, task, or terminal that simply
// shells out and authenticates as whoever the daemon is.

// vscodeAccountScope is the credential boundary one editor runs under.
type vscodeAccountScope struct {
	// account is the selected account NAME, empty for the ambient identity. It is
	// compared on reuse; see vscodeServer.account.
	account string
	// environ builds the exact environment the editor is exec'd with.
	//
	// A FUNCTION rather than a slice because resolving an account reads the
	// filesystem (the af home, the account directory and its ancestors), and the
	// reuse path — every proxy request to an already-running editor — has no need
	// of it. It is called only where an editor is actually about to be exec'd, so
	// a failure to resolve becomes a recorded spawn failure under the respawn
	// cooldown rather than an error on a hot path.
	environ func() ([]string, error)
}

// ambientVSCodeScope is the unscoped boundary: the daemon's own environment,
// minus the names below. It is what a session with no selected account gets,
// which is every session's behaviour before #3051 and still the default.
func ambientVSCodeScope() vscodeAccountScope {
	return vscodeAccountScope{environ: func() ([]string, error) { return vscodeChildEnv(), nil }}
}

// environment is what the spawn path calls, and it makes the ZERO VALUE mean
// "ambient" rather than "panic". A scope that names an account but carries no
// builder is the one combination that must not resolve: it would hand the editor
// the daemon's identity under a session that reports the account, which is
// precisely the outcome this file exists to prevent, so it refuses instead.
func (s vscodeAccountScope) environment() ([]string, error) {
	if s.environ != nil {
		return s.environ()
	}
	if strings.TrimSpace(s.account) != "" {
		return nil, fmt.Errorf(
			"the VS Code editor scope names account %q but carries no environment to launch it with", s.account)
	}
	return vscodeChildEnv(), nil
}

// vscodeAccountScopeForInstance gives the daemon-owned editor the same selected
// credential boundary as the tmux panes whose integrated terminals it hosts.
//
// The agent NAMESPACE is derived from the session's configured program, matching
// refreshSessionEnvironment and every other surface that resolves this session's
// account. It deliberately does not use CurrentAgentName, which prefers the
// RUNNING tmux command: an account belongs to an agent's registry, this
// session's account was validated against its configured program when it was
// created (resolveAccountForProvision), and a session resolved by
// program_overrides into a different agent would send the lookup into a registry
// that never held this account — turning a working editor into a refusal.
func vscodeAccountScopeForInstance(instance *session.Instance) vscodeAccountScope {
	account, _ := instance.AccountSelection()
	account = strings.TrimSpace(account)
	if account == "" {
		return ambientVSCodeScope()
	}
	agent := sessionenv.AgentForCommand(instance.AgentProgram())
	return vscodeAccountScope{
		account: account,
		environ: func() ([]string, error) {
			environ, err := vscodeAccountEnvironment(agent, account)
			if err != nil {
				// Both wrapped: the sentinel is what the pane renderer matches on, and
				// the cause is what tells the operator which command to run. The tab
				// route is per-session, so the session names itself.
				return nil, fmt.Errorf("%w %q: %w", errVSCodeAccountScope, account, err)
			}
			return environ, nil
		},
	}
}

// vscodeAccountEnvironment injects the account's credential root and subtracts
// every other identity-bearing name — the same two halves ApplyAccount performs
// for the agent pane, through the environment-only door a sibling pane uses.
//
// The COMMAND is empty, and that is a statement of fact rather than a skipped
// check. ValidateAccountEnvironmentCommand exists because a sibling pane's
// program reaches the kernel through `/bin/sh -c`, where a command-local
// assignment (`CLAUDE_CONFIG_DIR=… claude`) runs AFTER the boundary is installed
// and outranks it. The editor has no such command line: it is exec'd directly
// with an argv af built (vscodeArgs), from a binary path, with no shell in
// between. Handing that filesystem path to a shell parser would not check
// anything the editor can actually do.
//
// What ApplyAccountEnvironment still contributes beyond the injection and the
// subtraction is stripAccountShellStartupEnvironment: BASH_ENV, ENV, ZDOTDIR and
// friends are removed, so the daemon's own environment cannot INJECT startup
// code into an integrated terminal. That closes the half of the terminal problem
// af controls. The other half — a startup file already on disk — is the one the
// file comment above says af cannot prove.
func vscodeAccountEnvironment(agent, account string) ([]string, error) {
	home, err := config.GetConfigDir()
	if err != nil {
		return nil, fmt.Errorf("locate registered accounts: %w", err)
	}
	scope, err := agentaccount.Selected(home, agent, account, "")
	if err != nil {
		return nil, err
	}
	// Selected answers a zero Account for an EMPTY name, which the caller has
	// already excluded, so a zero directory here would mean an unscoped editor
	// under a session that reports an account. Refuse instead.
	if strings.TrimSpace(scope.Dir) == "" {
		return nil, fmt.Errorf("account %q for %s resolved to no credential directory", account, agent)
	}
	return sessionenv.ApplyAccountEnvironment(vscodeChildEnv(), "", scope)
}

// vscodeChildEnv builds the editor's environment from the daemon's, with the tmux
// ancestry markers, VSCODE_IPC_HOOK_CLI and the login-pane browser suppressors
// REMOVED.
//
// This is load-bearing, not hygiene. The daemon inherits its environment from
// whatever autostarted it — often a TUI running inside an af_ tmux pane — so it
// can be carrying AF_SESSION/AF_HOME, and every child it spawns inherits them
// too. /proc/<pid>/environ is fixed at exec and can never be shed, so a
// code-server stamped with a session marker is attributed to that session
// forever: once that session dies, `af doctor --fix` matches the marker plus the
// home and KILLS the editor as a leaked process (doctor/checks.go). Scrubbing the
// markers keeps a daemon-owned editor out of that attribution entirely.
//
// (The tmux teardown reaper is a separate mechanism and never sees this child at
// all: it captures only a tmux pane's descendants and its pane-SID members, and a
// daemon child is neither — the daemon is its own session leader via Setsid.)
//
// VSCODE_IPC_HOOK_CLI is scrubbed for the same reason and it is just as
// load-bearing. code-server's shouldOpenInExistingInstance checks it
// UNCONDITIONALLY, before it starts any server, and when it is set the CLI hands
// the folder to that existing editor over the IPC socket and EXITS — --bind-addr
// is never honored. So a daemon started from any VS Code / code-server integrated
// terminal inherits the var, and then every editor it ever spawns dies during
// startup (the pane shows a broken-editor notice despite a perfectly good
// install) while the worktree pops open in the USER's own window instead. The var
// is fixed in the daemon's environ at exec, so this is sticky for the daemon's
// whole life — and af's own VS Code tab has an integrated terminal that sets it,
// which makes `af` run from inside an af VS Code tab poison the daemon.
//
// NO_BROWSER and BROWSER are scrubbed for a third reason, and the one this file
// is least likely to be read for. They belong to af's ACCOUNT LOGIN flow, which
// sets them to route a sign-in through its device-code shape instead of a
// browser callback (#3854): gemini reads NO_BROWSER, and claude reads BROWSER and
// spawns it with the URL, so BROWSER=true is the no-op that suppresses the
// launch. An editor is not a login pane — it is a place a human opens an
// integrated terminal and runs an agent — and one that inherited them would
// silently change how that agent behaves, or try to exec a program literally
// named "true" as the operator's browser. A tmux pane is already protected from
// this by sessionenv's subtractive, default-deny allowlist, which passes neither
// name unless the operator lists it in session_env_passthrough. The editor's
// environment never goes through that allowlist — it is the daemon's own
// environ minus this list — so for the editor the same property has to be
// written down rather than inherited.
//
// Only what breaks the spawn is scrubbed. The git-askpass family
// (VSCODE_GIT_ASKPASS_*, VSCODE_GIT_IPC_HANDLE, GIT_ASKPASS) also inherits stale
// handles, but code-server overwrites those for its own terminals, so removing
// them buys nothing; the shell-integration markers (VSCODE_INJECTION, VSCODE_PID,
// TERM_PROGRAM, …) the editor resets itself. Blanket-scrubbing VSCODE_* would
// trade a filter you can audit against upstream for one that merely looks tidy.
var vscodeScrubbedEnv = []string{
	tmux.EnvMarkerSession,
	tmux.EnvMarkerHome,
	vscodeOwnerNonceEnv,
	"VSCODE_IPC_HOOK_CLI",
	"NO_BROWSER",
	"BROWSER",
}

func vscodeChildEnv() []string {
	src := os.Environ()
	out := make([]string, 0, len(src))
	for _, kv := range src {
		if hasAnyEnvPrefix(kv, vscodeScrubbedEnv) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// hasAnyEnvPrefix reports whether the KEY=VALUE entry kv names any of keys.
func hasAnyEnvPrefix(kv string, keys []string) bool {
	for _, k := range keys {
		if strings.HasPrefix(kv, k+"=") {
			return true
		}
	}
	return false
}
