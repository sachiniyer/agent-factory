package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
)

var sessionEnvExecutable = os.Executable

// SetEnvPassthrough replaces the exact-name extension to this session's
// default-deny environment. Call it before Start or Restore; sibling tabs copy
// the normalized list from their agent session.
func (t *TmuxSession) SetEnvPassthrough(names []string) error {
	normalized, err := sessionenv.NormalizeExtraNames(names)
	if err != nil {
		return err
	}
	t.programMu.Lock()
	t.envPassthrough = normalized
	t.programMu.Unlock()
	return nil
}

// SetAccount selects the credential account this session's agent runs as. An
// empty name leaves the session on the ambient identity, exactly as before this
// feature existed.
func (t *TmuxSession) SetAccount(name string) {
	t.programMu.Lock()
	defer t.programMu.Unlock()
	t.account = name
	t.accountEnvironmentOnly = false
	if name == "" {
		t.accountAgent = ""
		return
	}
	t.accountAgent = sessionenv.AgentForCommand(t.program)
}

// SetAccountForAgent selects name in the namespace the caller validated. The
// namespace is explicit because program is override-resolved and may later be
// rewritten; deriving it again would let the same string silently select a
// different agent's account.
func (t *TmuxSession) SetAccountForAgent(agent, name string) {
	t.programMu.Lock()
	defer t.programMu.Unlock()
	t.account = name
	t.accountEnvironmentOnly = false
	if name == "" {
		t.accountAgent = ""
		return
	}
	t.accountAgent = agent
}

// SetAccountEnvironmentForAgent scopes a shell/process sibling to the selected
// account while keeping its own command shape. This is distinct from claiming
// the sibling command is the agent executable.
func (t *TmuxSession) SetAccountEnvironmentForAgent(agent, name string) {
	t.programMu.Lock()
	defer t.programMu.Unlock()
	t.account = name
	t.accountAgent = agent
	t.accountEnvironmentOnly = name != ""
}

// SetAccountShellEnvironmentForAgent scopes an af-created interactive shell.
// Named-account shells use the startup-file-free form the boundary recognizes.
func (t *TmuxSession) SetAccountShellEnvironmentForAgent(agent, name string) error {
	t.programMu.Lock()
	defer t.programMu.Unlock()
	if name != "" {
		program, err := sessionenv.AccountShellCommand(t.program)
		if err != nil {
			return err
		}
		t.program = program
	}
	t.account = name
	t.accountAgent = agent
	t.accountEnvironmentOnly = name != ""
	return nil
}

// SetLaunchProgram sets the pane command AND af's executable/argument proof
// under ONE programMu hold (#3083 review, #3108).
//
// Paired because a reader between two independently-locked setters sees a torn
// pair — the old command with the new declaration, or the reverse — and the
// account boundary requires the command to be the agent plus EXACTLY the declared
// words, so either half of that tear refuses the launch and the pane exits
// immediately. The call site pairing them is not enough: it makes the two writes
// adjacent, not atomic.
func (t *TmuxSession) SetLaunchProgram(program string, proof sessionenv.AccountLaunchProof) {
	t.programMu.Lock()
	defer t.programMu.Unlock()
	t.program = program
	t.generatedArgs = append([]string(nil), proof.GeneratedArgs...)
	t.trustedExecutable = proof.TrustedExecutable
}

// launchSnapshot reads every field a launch needs in ONE hold, for the same
// reason SetLaunchProgram writes them in one: Start used to read program through
// its own lock and the declaration through another, so a rewrite landing between
// them paired an old command with a new declaration.
func (t *TmuxSession) launchSnapshot() (program string, proof sessionenv.AccountLaunchProof, extras []string, account, accountAgent string, accountEnvironmentOnly bool) {
	t.programMu.RLock()
	defer t.programMu.RUnlock()
	return t.program, sessionenv.AccountLaunchProof{
			TrustedExecutable: t.trustedExecutable,
			GeneratedArgs:     append([]string(nil), t.generatedArgs...),
		},
		append([]string(nil), t.envPassthrough...),
		t.account,
		t.accountAgent,
		t.accountEnvironmentOnly
}

// It SNAPSHOTS rather than taking the program as a parameter: the caller used to
// read program through its own lock and this read the declaration through
// another, so a rewrite landing between them wrapped an old command with a new
// declaration (#3083 review).
func (t *TmuxSession) launchEnvironment() (string, []string, []string, error) {
	program, proof, extra, account, accountAgent, accountEnvironmentOnly := t.launchSnapshot()
	agent := sessionenv.AgentForCommand(program)
	executable, err := sessionEnvExecutable()
	if err != nil {
		return "", nil, nil, err
	}
	var wrapped string
	if account != "" {
		if agent != accountAgent && !accountEnvironmentOnly {
			resolved := agent
			if resolved == "" {
				resolved = "an unrecognized command"
			}
			return "", nil, nil, fmt.Errorf(
				"account %q was selected for %s, but the launch program resolves to %s; refusing rather than "+
					"looking up the same account name in another agent's namespace",
				account, accountAgent, resolved)
		}
		// tmux < 3.2 REFUSES rather than falling back. A fallback would launch on
		// the ambient account while every visible signal reported the selected one,
		// spending someone else's quota (#3051).
		if !newSessionEnvSupportedForAccounts() {
			return "", nil, nil, fmt.Errorf(
				"account %q cannot be used on this tmux: account-scoped sessions require tmux 3.2 or newer, "+
					"and af refuses rather than starting the session on the ambient account", account)
		}
		if accountEnvironmentOnly {
			wrapped, err = sessionenv.WrapAccountEnvironmentCommand(executable, accountAgent, account, extra, program)
		} else {
			wrapped, err = sessionenv.WrapAccountCommand(executable, accountAgent, account, proof, extra, program)
		}
	} else {
		wrapped, err = sessionenv.WrapCommand(executable, agent, extra, program)
	}
	if err != nil {
		return "", nil, nil, err
	}
	filterAgent := agent
	if accountEnvironmentOnly {
		filterAgent = accountAgent
	}
	source := os.Environ()
	return wrapped,
		sessionenv.FilterForCommand(source, filterAgent, program, extra),
		sessionenv.ImportNamesForCommand(source, filterAgent, program, extra), nil
}

// importClientEnvironmentArgs makes an existing tmux server copy the approved
// client variables into only the new session. update-environment is temporarily
// expanded and restored in the same tmux command queue, so values stay in
// cmd.Env and never enter argv or the server's persistent global environment.
func (t *TmuxSession) importClientEnvironmentArgs(newSessionArgs, names []string) ([]string, error) {
	ctx, cancel := tmuxTimeoutContext()
	previousRaw, err := t.outputTmuxBounded(ctx, "show-options", "-gv", "update-environment")
	timedOut := ctx.Err() != nil
	cancel()
	if err != nil {
		if timedOut {
			return nil, fmt.Errorf("%w: read existing tmux update-environment option", ErrTmuxTimeout)
		}
		if tmuxServerAbsent(err) {
			// No server is the ordinary first-session case. A fresh server snapshots
			// the already-filtered client environment, so no import override is needed.
			return newSessionArgs, nil
		}
		return nil, fmt.Errorf("read existing tmux update-environment option: %w", err)
	}

	previous := strings.Fields(string(previousRaw))
	combinedSet := make(map[string]struct{}, len(previous)+len(names))
	for _, name := range previous {
		combinedSet[name] = struct{}{}
	}
	for _, name := range names {
		combinedSet[name] = struct{}{}
	}
	combined := make([]string, 0, len(combinedSet))
	for name := range combinedSet {
		combined = append(combined, name)
	}
	sort.Strings(combined)

	args := []string{"set-option", "-g", "update-environment", strings.Join(combined, " "), ";"}
	args = append(args, newSessionArgs...)
	args = append(args, ";", "set-option", "-g", "update-environment", strings.Join(previous, " "))
	return args, nil
}

func tmuxServerAbsent(err error) bool {
	message := err.Error()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		message += " " + string(exitErr.Stderr)
	}
	message = strings.ToLower(message)
	for _, fragment := range []string{
		"no server running",
		"failed to connect to server",
		"error connecting to ",
	} {
		if strings.Contains(message, fragment) {
			return true
		}
	}
	return false
}
