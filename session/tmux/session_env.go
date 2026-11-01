package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/log"
)

var sessionEnvExecutable = os.Executable

// SetRemainOnExit marks the session's pane to persist as a held dead pane after
// its process exits, exposing pane_dead/pane_dead_status so a completed process
// tab restores as finished evidence instead of an absence to re-spawn (#4479).
// Call before Start; ApplyRemainOnExit is the already-running form.
func (t *TmuxSession) SetRemainOnExit() {
	t.programMu.Lock()
	defer t.programMu.Unlock()
	t.remainOnExit = true
}

// ApplyRemainOnExit sets the window option on a session that already exists,
// healing a process tab spawned before #4479 so its eventual exit still records
// a dead pane rather than vanishing into an ambiguous absence. It also stamps
// the pane's @af_pane_pid while the root is still alive to answer: tmux ≥3.8
// stops reporting pane_pid at death (#5319), and a pane launched by a
// pre-stamp af has no shim-recorded value. `-o` never overwrites — a pane
// already dead is exactly when re-expanding #{pane_pid} would come back
// empty and clobber a good stamp. Best-effort: a wedged server just loses
// the heal, never the tab.
func (t *TmuxSession) ApplyRemainOnExit() {
	ctx, cancel := tmuxTimeoutContext()
	args := []string{"set-option", "-w", "-t", ExactTarget(t.sanitizedName), "remain-on-exit", "on"}
	args = append(append(args, ";"), panePidStampArgs(ExactTarget(t.sanitizedName))...)
	_ = t.runTmuxBounded(ctx, args...)
	cancel()
}

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

// SetInstanceID records the instance id the next Start stamps into the session
// environment as AF_INSTANCE_ID (#6258). Call it before Start; it only labels
// the session and cannot affect the pane.
func (t *TmuxSession) SetInstanceID(id string) {
	t.programMu.Lock()
	t.instanceID = id
	t.programMu.Unlock()
}

func (t *TmuxSession) launchInstanceID() string {
	t.programMu.RLock()
	defer t.programMu.RUnlock()
	return t.instanceID
}

// Account is the credential account this session's next Start launches under,
// or "" for the ambient identity.
func (t *TmuxSession) Account() string {
	t.programMu.RLock()
	defer t.programMu.RUnlock()
	return t.account
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

// AccountAgentForTest reports the namespace the declared account is pinned to,
// so session-package tests can assert which command the environment refresh
// derived it from (#4430 review round 3).
func (t *TmuxSession) AccountAgentForTest() string {
	t.programMu.Lock()
	defer t.programMu.Unlock()
	return t.accountAgent
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

// SetAccountLoginForAgent scopes an account LOGIN pane (#3854).
//
// It is SetAccountEnvironmentForAgent plus the environment that makes the
// agent's own sign-in browser-free, and it is a separate setter rather than a
// flag on that one because the two panes want opposite things. A login pane
// exists to print a URL and a code; a working session's shell or process tab
// exists to do work, and handing it NO_BROWSER or a no-op BROWSER would change
// how the agent behaves for reasons that have nothing to do with the session.
//
// The operator's own pass-through WINS. A name already in session_env_passthrough
// is skipped here, so the value they configured flows through untouched — af's
// browser-free default is a default, not a wall, on a daemon host that really
// does have a browser to open. Call it AFTER SetEnvPassthrough, which
// accountlogin does.
func (t *TmuxSession) SetAccountLoginForAgent(agent, name string) {
	t.programMu.Lock()
	defer t.programMu.Unlock()
	t.account = name
	t.accountAgent = agent
	t.accountEnvironmentOnly = name != ""
	t.accountLoginEnv = nil
	if name == "" {
		return
	}
	for _, entry := range sessionenv.AccountLoginEnvironment(agent) {
		envName, _, ok := strings.Cut(entry, "=")
		if !ok || slices.Contains(t.envPassthrough, envName) {
			continue
		}
		t.accountLoginEnv = append(t.accountLoginEnv, entry)
	}
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
func (t *TmuxSession) launchSnapshot() (program string, proof sessionenv.AccountLaunchProof, extras []string, account, accountAgent string, accountEnvironmentOnly bool, accountLoginEnv []string) {
	t.programMu.RLock()
	defer t.programMu.RUnlock()
	return t.program, sessionenv.AccountLaunchProof{
			TrustedExecutable: t.trustedExecutable,
			GeneratedArgs:     append([]string(nil), t.generatedArgs...),
		},
		append([]string(nil), t.envPassthrough...),
		t.account,
		t.accountAgent,
		t.accountEnvironmentOnly,
		append([]string(nil), t.accountLoginEnv...)
}

// workDir is the pane's working directory — the tree the shared grant
// predicate checks an absolute program executable for containment in. The
// "" form leaves that check open for callers that only need the launch
// pieces an absolute-inside-tree answer cannot change (account session env,
// the scoped default command).
//
// An account-scoped launch mints its launch capabilities here (#4731): the
// pane's command carries only where to claim and a bearer token, and the shim
// runs what the CAPABILITY admitted, not what its own argv or environment say.
// A failed prepare revokes only what it minted.
func (t *TmuxSession) prepareLaunchEnvironment(workDir string) (wrapped string, launchEnv, importNames, sessionEnv []string, defaultCommand string, err error) {
	// What an earlier prepare installed stays valid until THIS one is installed
	// (commitCapabilities); a failure revokes only what this one minted.
	t.beginCapabilityPrepare()
	wrapped, launchEnv, importNames, sessionEnv, defaultCommand, err = t.prepareLaunchEnvironmentMinting(workDir)
	if err != nil {
		t.revokePendingCapabilities()
	}
	return wrapped, launchEnv, importNames, sessionEnv, defaultCommand, err
}

func (t *TmuxSession) prepareLaunchEnvironmentMinting(workDir string) (string, []string, []string, []string, string, error) {
	program, proof, extra, account, accountAgent, accountEnvironmentOnly, loginEnv := t.launchSnapshot()
	source := os.Environ()
	// Two different questions, two classifiers: the credential GRANT is bound
	// to the shared predicate (only an operator-attributable executable may
	// carry it, #5265), while the account-namespace check below asks the
	// identity question — which agent this command RUNS — so a named account
	// does not newly depend on the agent being installed on the daemon's PATH.
	agent := sessionenv.GrantAgentForCommand(program, source, workDir)
	identityAgent := sessionenv.AgentForCommand(program)
	executable, err := sessionEnvExecutable()
	if err != nil {
		return "", nil, nil, nil, "", err
	}
	// The login pane's names join the pass-through allowlist for THIS launch
	// rather than being written back onto the session, so calling the setter twice
	// cannot mistake af's own additions for the operator's and drop them. The
	// exec shim re-filters os.Environ() against exactly this list immediately
	// before exec, so a name missing here is a value the agent never sees (#3854).
	if len(loginEnv) > 0 {
		extra, err = sessionenv.NormalizeExtraNames(append(extra, environmentEntryNames(loginEnv)...))
		if err != nil {
			return "", nil, nil, nil, "", err
		}
	}
	var wrapped string
	if account != "" {
		if identityAgent != accountAgent && !accountEnvironmentOnly {
			resolved := identityAgent
			if resolved == "" {
				resolved = "an unrecognized command"
			}
			return "", nil, nil, nil, "", fmt.Errorf(
				"account %q was selected for %s, but the launch program resolves to %s; refusing rather than "+
					"looking up the same account name in another agent's namespace",
				account, accountAgent, resolved)
		}
		// The account's material may only ride an executable af can attribute
		// to the operator — the same credential-grant question the ambient
		// filter answers. A scoped program whose grant fails (a relative
		// spelling, a repository-controlled PATH hit) would otherwise still
		// receive the selected account's environment via the account shim
		// (#5265 review).
		if agent != accountAgent && !accountEnvironmentOnly {
			return "", nil, nil, nil, "", fmt.Errorf(
				"account %q was selected for %s, but af cannot attribute the launch executable to the "+
					"operator; refusing rather than letting account credentials reach repository content",
				account, accountAgent)
		}
		// tmux < 3.2 REFUSES rather than falling back. A fallback would launch on
		// the ambient account while every visible signal reported the selected one,
		// spending someone else's quota (#3051).
		if !SessionEnvSupported() {
			return "", nil, nil, nil, "", fmt.Errorf(
				"account %q cannot be used on this tmux: account-scoped sessions require tmux 3.2 or newer, "+
					"and af refuses rather than starting the session on the ambient account", account)
		}
		wrapped, err = t.wrapClaimedLaunch(executable, launchSpec{
			agent: accountAgent, account: account, command: program, extras: extra,
			proof: proof, environmentOnly: accountEnvironmentOnly,
		})
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
	launchEnv := sessionenv.FilterForCommand(source, filterAgent, program, extra)
	importNames := sessionenv.ImportNamesForCommand(source, filterAgent, program, extra)
	if !SessionEnvSupported() {
		// Older tmux cannot replace these with new-session -e. Remove the
		// daemon's ancestry from a fresh server's environment, and explicitly
		// unset any stale global values in an existing server's new session.
		markers := []string{EnvMarkerSession, EnvMarkerGeneration}
		launchEnv = removeEnvironmentNames(launchEnv, markers)
		importNames = appendMissingEnvironmentNames(importNames, markers)
	}
	var sessionEnv []string
	if account != "" {
		// The login pane's browser-free names go through the SAME three steps as
		// the account boundary's, and for the same reason: af has to be the one
		// that decides their value. Stripped from the client environment so the
		// daemon's own BROWSER cannot ride along, named in update-environment so an
		// existing server unsets a stale copy, and then set explicitly below
		// (#3854).
		boundaryNames := appendMissingEnvironmentNames(
			accountSessionBoundaryNames(accountAgent), environmentEntryNames(loginEnv))
		launchEnv = removeEnvironmentNames(launchEnv, boundaryNames)
		selectedEnv, resolveErr := sessionenv.ResolveAccountEnvironment(accountAgent, account)
		if resolveErr != nil {
			return "", nil, nil, nil, "", resolveErr
		}
		// Selected roots belong to this tmux session, not the client environment:
		// a fresh server copies its first client's environment globally.
		sessionEnv = append(selectedEnv, loginEnv...)
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
		}
		if ownsLaunchCapabilities() {
			// The initial pane's capability is one-shot; the default-command runs
			// for every later window, so it gets its own per-pane one, owned by
			// this long-lived process.
			defaultCommand, err = t.wrapClaimedLaunch(executable, launchSpec{
				agent: accountAgent, account: account, command: defaultProgram, extras: extra,
				environmentOnly: true, perPane: true,
			})
			if err != nil {
				return "", nil, nil, nil, "", err
			}
		} else {
			defaultCommand = nonOwnerDefaultCommand
		}
	}
	return wrapped, launchEnv, importNames, sessionEnv, defaultCommand, nil
}

// wrapClaimedLaunch mints spec's capability and returns the pane command that
// claims it.
func (t *TmuxSession) wrapClaimedLaunch(executable string, spec launchSpec) (string, error) {
	socket, token, err := t.mintLaunchCapability(spec)
	if err != nil {
		return "", err
	}
	if spec.environmentOnly {
		return sessionenv.WrapAccountEnvironmentCommand(executable, socket, token, spec.command)
	}
	return sessionenv.WrapAccountCommand(executable, socket, token, spec.command)
}

func accountSessionBoundaryNames(agent string) []string {
	names := append([]string(nil), sessionenv.AccountIdentityNames(agent)...)
	names = append(names, sessionenv.AccountShellStartupNames()...)
	sort.Strings(names)
	return names
}

func (t *TmuxSession) refreshRestoredAccountEnvironment() (err error) {
	// The capability this refresh mints is retired-into only once its
	// default-command is installed; a failure leaves the one the live session's
	// default-command already names usable.
	defer func() {
		if err != nil {
			t.revokePendingCapabilities()
		} else {
			t.commitCapabilities(false) // a refresh launches no pane: only the per-pane capability is kept
		}
	}()
	_, _, _, account, accountAgent, _, _ := t.launchSnapshot()
	if account == "" {
		return nil
	}
	_, _, _, sessionEnv, defaultCommand, err := t.prepareLaunchEnvironment("")
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
		args := []string{"set-environment", "-r", "-t", ExactTarget(t.sanitizedName), name}
		if value, ok := selected[name]; ok {
			args = []string{"set-environment", "-t", ExactTarget(t.sanitizedName), name, value}
		}
		if err := t.runRestoredAccountCommand(args...); err != nil {
			return err
		}
	}
	if !ownsLaunchCapabilities() {
		// Not the owner: the daemon wrote this session's default-command and it is
		// not ours to replace with a capability this process cannot keep alive.
		return nil
	}
	return t.runRestoredAccountCommand(
		"set-option", "-t", ExactTarget(t.sanitizedName), "default-command", defaultCommand,
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

// environmentEntryNames reads the names out of NAME=VALUE entries, dropping any
// malformed one rather than inventing a name for it.
func environmentEntryNames(entries []string) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if name, _, ok := strings.Cut(entry, "="); ok {
			names = append(names, name)
		}
	}
	return names
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
//
// The restore CANNOT rely on the queue alone: tmux abandons a ;-chained queue
// at the first failed command, so a mid-queue failure would leave the server's
// option permanently expanded to the credential import list. The second return
// is the bounded restore Start defers onto every exit path — it re-applies the
// baseline even when the queue's own trailing restore never ran (#5390).
//
// The baseline is the read value minus every name af only ever adds transiently
// (sessionenv.ImportedCredentialNames). The true pre-expansion value of a
// server an older build already left expanded is unrecoverable, but af never
// intends those names to persist, so dropping them heals the stored option on
// the next launch instead of re-persisting the leak as "previous".
func (t *TmuxSession) importClientEnvironmentArgs(newSessionArgs, names []string) (args, restore []string, err error) {
	ctx, cancel := tmuxTimeoutContext()
	previousRaw, err := t.outputTmuxBounded(ctx, "show-options", "-gv", "update-environment")
	timedOut := ctx.Err() != nil
	cancel()
	if err != nil {
		if timedOut {
			return nil, nil, fmt.Errorf("%w: read existing tmux update-environment option", ErrTmuxTimeout)
		}
		if tmuxServerAbsent(err) {
			// No server is the ordinary first-session case. A fresh server snapshots
			// the already-filtered client environment, so no import override is needed.
			return newSessionArgs, nil, nil
		}
		return nil, nil, fmt.Errorf("read existing tmux update-environment option: %w", err)
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

	// The option fields are bare names, not NAME=VALUE entries, so this filters
	// by whole-field membership rather than reusing removeEnvironmentNames.
	denied := make(map[string]struct{}, len(previous))
	for _, name := range sessionenv.ImportedCredentialNames() {
		denied[name] = struct{}{}
	}
	var baseline []string
	for _, name := range previous {
		if _, drop := denied[name]; !drop {
			baseline = append(baseline, name)
		}
	}
	if !slices.Equal(baseline, previous) {
		log.WarningLog.Printf(
			"tmux update-environment held credential import name(s) an interrupted earlier launch left behind; " +
				"restoring it without them rather than keeping them server-wide")
	}

	restore = []string{"set-option", "-g", "update-environment", strings.Join(baseline, " ")}
	args = []string{"set-option", "-g", "update-environment", strings.Join(combined, " "), ";"}
	args = append(args, newSessionArgs...)
	args = append(args, ";")
	args = append(args, restore...)
	return args, restore, nil
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

// ValidateAccountLaunchSupport checks the host-only prerequisites that must be
// known before an account swap stops the old panes. Command and environment
// validation live in sessionenv; this keeps the tmux capability probe shared
// with the actual launch instead of re-deriving its version rule in session.
func ValidateAccountLaunchSupport(account string) error {
	if !SessionEnvSupported() {
		return fmt.Errorf(
			"account %q cannot be used on this tmux: account-scoped sessions require tmux 3.2 or newer, "+
				"and af refuses rather than starting the session on the ambient account", account)
	}
	if _, err := sessionEnvExecutable(); err != nil {
		return fmt.Errorf("locate af's account environment shim: %w", err)
	}
	return nil
}
