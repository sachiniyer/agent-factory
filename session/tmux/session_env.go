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
	wrapped, launchEnv, importNames, _, _, err := t.prepareLaunchEnvironment()
	return wrapped, launchEnv, importNames, err
}

func (t *TmuxSession) prepareLaunchEnvironment() (string, []string, []string, []string, string, error) {
	program, proof, extra, account, accountAgent, accountEnvironmentOnly := t.launchSnapshot()
	agent := sessionenv.AgentForCommand(program)
	executable, err := sessionEnvExecutable()
	if err != nil {
		return "", nil, nil, nil, "", err
	}
	var wrapped string
	if account != "" {
		if agent != accountAgent && !accountEnvironmentOnly {
			resolved := agent
			if resolved == "" {
				resolved = "an unrecognized command"
			}
			return "", nil, nil, nil, "", fmt.Errorf(
				"account %q was selected for %s, but the launch program resolves to %s; refusing rather than "+
					"looking up the same account name in another agent's namespace",
				account, accountAgent, resolved)
		}
		// tmux < 3.2 REFUSES rather than falling back. A fallback would launch on
		// the ambient account while every visible signal reported the selected one,
		// spending someone else's quota (#3051).
		if !newSessionEnvSupportedForAccounts() {
			return "", nil, nil, nil, "", fmt.Errorf(
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
		return "", nil, nil, nil, "", err
	}
	filterAgent := agent
	if accountEnvironmentOnly {
		filterAgent = accountAgent
	}
	source := os.Environ()
	launchEnv := sessionenv.FilterForCommand(source, filterAgent, program, extra)
	importNames := sessionenv.ImportNamesForCommand(source, filterAgent, program, extra)
	var sessionEnv []string
	if account != "" {
		boundaryNames := accountSessionBoundaryNames(accountAgent)
		launchEnv = removeEnvironmentNames(launchEnv, boundaryNames)
		selectedEnv, resolveErr := sessionenv.ResolveAccountEnvironment(accountAgent, account)
		if resolveErr != nil {
			return "", nil, nil, nil, "", resolveErr
		}
		// Selected roots belong to this tmux session, not the client environment:
		// a fresh server copies its first client's environment globally.
		sessionEnv = selectedEnv
		// Keep every removed name in update-environment so an existing server
		// explicitly unsets stale identities and startup hooks before new-session.
		importNames = appendMissingEnvironmentNames(importNames, boundaryNames)
	}
	defaultCommand := ""
	if account != "" {
		// tmux otherwise starts its default shell as a login shell for an empty
		// new-window command. Every scoped session needs an account shim and proven
		// startup-free shell here, including the main agent session and process tabs
		// whose primary command is not itself a shell.
		defaultProgram := program
		if !accountEnvironmentOnly || !sessionenv.IsAccountShellCommand(defaultProgram) {
			defaultProgram, err = sessionenv.AccountShellCommand("/bin/sh")
			if err != nil {
				return "", nil, nil, nil, "", err
			}
			defaultCommand, err = sessionenv.WrapAccountEnvironmentCommand(
				executable, accountAgent, account, extra, defaultProgram,
			)
			if err != nil {
				return "", nil, nil, nil, "", err
			}
		} else {
			defaultCommand = wrapped
		}
	}
	return wrapped, launchEnv, importNames, sessionEnv, defaultCommand, nil
}

func accountSessionBoundaryNames(agent string) []string {
	names := append([]string(nil), sessionenv.AccountIdentityNames(agent)...)
	names = append(names, sessionenv.AccountShellStartupNames()...)
	sort.Strings(names)
	return names
}

func (t *TmuxSession) refreshRestoredAccountEnvironment() error {
	_, _, _, account, accountAgent, _ := t.launchSnapshot()
	if account == "" {
		return nil
	}
	_, _, _, sessionEnv, defaultCommand, err := t.prepareLaunchEnvironment()
	if err != nil {
		return err
	}
	selected := make(map[string]string, len(sessionEnv))
	for _, entry := range sessionEnv {
		name, value, ok := strings.Cut(entry, "=")
		if ok {
			selected[name] = value
		}
	}
	for _, name := range accountSessionBoundaryNames(accountAgent) {
		args := []string{"set-environment", "-r", "-t", exactTarget(t.sanitizedName), name}
		if value, ok := selected[name]; ok {
			args = []string{"set-environment", "-t", exactTarget(t.sanitizedName), name, value}
		}
		if err := t.runRestoredAccountCommand(args...); err != nil {
			return err
		}
	}
	return t.runRestoredAccountCommand(
		"set-option", "-t", exactTarget(t.sanitizedName), "default-command", defaultCommand,
	)
}

func (t *TmuxSession) runRestoredAccountCommand(args ...string) error {
	ctx, cancel := tmuxTimeoutContext()
	err := t.runTmuxBounded(ctx, args...)
	timedOut := ctx.Err() != nil
	cancel()
	if timedOut {
		return fmt.Errorf("%w: %s while refreshing %s", ErrTmuxTimeout, args[0], t.sanitizedName)
	}
	if err != nil {
		return fmt.Errorf("%s %s: %w", args[0], t.sanitizedName, err)
	}
	return nil
}

func removeEnvironmentNames(environ, names []string) []string {
	denied := make(map[string]struct{}, len(names))
	for _, name := range names {
		denied[name] = struct{}{}
	}
	out := environ[:0]
	for _, entry := range environ {
		name, _, ok := strings.Cut(entry, "=")
		if _, drop := denied[name]; ok && drop {
			continue
		}
		out = append(out, entry)
	}
	return out
}

func appendMissingEnvironmentNames(names, required []string) []string {
	// Sized from names alone, not len(names)+len(required). Both operands are
	// lengths of in-memory env-name slices, so that sum cannot realistically
	// overflow — but CodeQL's allocation-size-overflow rule reports the
	// unchecked addition as a high-severity finding and one new high alert
	// fails the required CodeQL check. Dropping the sum removes the flagged
	// expression instead of suppressing the rule; the map grows on its own for
	// the handful of boundary names required adds.
	set := make(map[string]struct{}, len(names))
	for _, name := range names {
		set[name] = struct{}{}
	}
	for _, name := range required {
		set[name] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
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
