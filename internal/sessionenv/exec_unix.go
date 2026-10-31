//go:build !windows

package sessionenv

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/internal/shellquote"
)

var processExec = syscall.Exec

// WrapCommand builds the shell command handed to tmux. It contains only the af
// executable path, the selected agent, explicit variable NAMES, and the
// original pane command; environment values never enter argv.
func WrapCommand(executable, agent string, extras []string, command string) (string, error) {
	return wrapCommand(executable, agent, extras, command)
}

// WrapAccountCommand builds the pane command for an account-scoped agent launch.
//
// It carries no account, agent, proof or passthrough name: those live in the
// launch capability the launcher minted (#4731), which the shim claims over the
// socket. Argv holds only where to claim, the bearer token, and the pane command
// — the last as the record paneLaunchIsRecorded matches the pane's recorded start
// command against. The shim IGNORES that trailing command and runs the capability's
// own, so editing it changes nothing about what launches.
func WrapAccountCommand(executable, socket, token, command string) (string, error) {
	return wrapClaimedCommand(executable, AccountExecMarker, socket, token, command)
}

// WrapAccountEnvironmentCommand applies a selected account to a sibling pane's
// environment through the same capability protocol. The pane command may be a
// shell or an arbitrary process; the account still resolves in the child and
// never falls back to ambient credentials.
func WrapAccountEnvironmentCommand(executable, socket, token, command string) (string, error) {
	return wrapClaimedCommand(executable, AccountEnvironmentExecMarker, socket, token, command)
}

func wrapClaimedCommand(executable, marker, socket, token, command string) (string, error) {
	if socket == "" || token == "" {
		return "", fmt.Errorf("account-scoped launch requires a claim endpoint and a launch capability")
	}
	args := []string{executable, marker, socket, token, command}
	quoted := make([]string, len(args))
	for idx, arg := range args {
		quoted[idx] = shellquote.Quote(arg)
	}
	return strings.Join(quoted, " "), nil
}

// WrapAgentServerCommand builds the effect-bound Docker/SSH handoff. Unlike the
// generic command wrapper, this protocol carries no caller-supplied agent claim
// and no nested executable path to authenticate: the receiving af derives the
// policy from these exact agent-server arguments and execs itself.
func WrapAgentServerCommand(executable string, extras, agentServerArgs []string) (string, error) {
	normalized, err := NormalizeExtraNames(extras)
	if err != nil {
		return "", err
	}
	if _, ok := agentServerProgram(agentServerArgs); !ok {
		return "", fmt.Errorf("malformed generated agent-server handoff")
	}
	args := []string{executable, AgentServerExecMarker, strconv.Itoa(len(normalized))}
	args = append(args, normalized...)
	args = append(args, agentServerArgs...)
	quoted := make([]string, len(args))
	for idx, arg := range args {
		quoted[idx] = shellquote.Quote(arg)
	}
	return strings.Join(quoted, " "), nil
}

// WrapAgentServerAccountCommand is WrapAgentServerCommand for an account-bound
// session: the receiving af scopes the agent-server's environment to the account
// agent's single identity before it execs (see AgentServerAccountExecMarker).
func WrapAgentServerAccountCommand(executable, accountAgent string, extras, agentServerArgs []string) (string, error) {
	if _, ok := SupportsAccounts(accountAgent); !ok {
		return "", fmt.Errorf("agent %q does not support multiple accounts", accountAgent)
	}
	normalized, err := NormalizeExtraNames(extras)
	if err != nil {
		return "", err
	}
	if _, ok := agentServerProgram(agentServerArgs); !ok {
		return "", fmt.Errorf("malformed generated agent-server handoff")
	}
	args := []string{executable, AgentServerAccountExecMarker, accountAgent, strconv.Itoa(len(normalized))}
	args = append(args, normalized...)
	args = append(args, agentServerArgs...)
	quoted := make([]string, len(args))
	for idx, arg := range args {
		quoted[idx] = shellquote.Quote(arg)
	}
	return strings.Join(quoted, " "), nil
}

func wrapCommand(executable, agent string, extras []string, command string) (string, error) {
	normalized, err := NormalizeExtraNames(extras)
	if err != nil {
		return "", err
	}
	args := []string{executable, ExecMarker, agent, strconv.Itoa(len(normalized))}
	args = append(args, normalized...)
	args = append(args, command)
	quoted := make([]string, len(args))
	for idx, arg := range args {
		quoted[idx] = shellquote.Quote(arg)
	}
	return strings.Join(quoted, " "), nil
}

// HandleInternalExec handles the private session exec protocol when present.
// On an ordinary invocation it returns immediately. On a helper invocation it
// replaces the current process on success and exits 127 with a value-free error
// on failure.
func HandleInternalExec() {
	if len(os.Args) < 2 {
		return
	}
	if os.Args[1] == AgentServerExecMarker || os.Args[1] == AgentServerAccountExecMarker {
		var err error
		if os.Args[1] == AgentServerAccountExecMarker {
			err = agentServerAccountExecInvocation(os.Args[2:])
		} else {
			err = agentServerExecInvocation(os.Args[2:])
		}
		if err != nil {
			_, _ = fmt.Fprintln(os.Stderr, "af: could not start the filtered agent-server")
			os.Exit(127)
		}
		return
	}
	scoped := os.Args[1] == AccountExecMarker
	environmentOnly := os.Args[1] == AccountEnvironmentExecMarker
	if os.Args[1] != ExecMarker && !scoped && !environmentOnly {
		return
	}
	var err error
	if scoped || environmentOnly {
		err = claimedExecInvocation(os.Args[2:], environmentOnly)
	} else {
		err = execInvocation(os.Args[2:])
	}
	if err != nil {
		if strings.HasPrefix(err.Error(), "launch capability refused") {
			// Value-free, and says what to do: this is what a window sees when the
			// daemon that launched its session has since been replaced.
			_, _ = fmt.Fprintf(os.Stderr,
				"af: could not start this window in its account scope (%v); if the af daemon was restarted, run af daemon restart and reopen the session\n", err)
			os.Exit(127)
		}
		_, _ = fmt.Fprintln(os.Stderr, "af: could not start the filtered session process")
		os.Exit(127)
	}
}

// claimCapability is a seam for tests; production claims over the unix socket.
var claimCapability = ClaimCapability

// claimedExecInvocation is the account-scoped shim. args is exactly
// [socket, token, command]. Everything the launch means comes from the claimed
// capability — the agent, account, command, passthrough names and launch proof the
// launcher admitted. The shim's own argv command and environment are never
// consulted for them, so a config edit after admission, an LD_PRELOAD added to the
// passthrough list, or a re-derivation that disagrees with the launcher cannot
// change what runs (#4731). A missing or refused claim is fatal: it never falls
// back to the ambient identity.
func claimedExecInvocation(args []string, environmentOnly bool) error {
	if err := stampPaneRootPid(); err != nil {
		return err
	}
	if len(args) != 3 {
		return fmt.Errorf("malformed internal session environment invocation")
	}
	c, err := claimCapability(args[0], args[1])
	if err != nil {
		return err
	}
	if c.Account == "" || c.EnvironmentOnly != environmentOnly {
		return fmt.Errorf("launch capability refused: it does not describe this launch")
	}
	extras, err := NormalizeExtraNames(c.Extras)
	if err != nil {
		return err
	}
	if !environmentOnly && AgentForCommand(c.Command) != c.Agent {
		// The capability names an agent, but the command it carries must still
		// resolve to it: an account boundary must not inject material into a program
		// this check cannot attribute (#5265). The workdir-based grant check is NOT
		// repeated here — the launcher made it when it minted, and re-running it
		// against the pane's cwd is what let a worktree layout change the answer.
		return fmt.Errorf(
			"account-scoped launch refused: %q does not resolve to an operator-attributable %s executable",
			c.Command, c.Agent)
	}
	environ := FilterForCommand(os.Environ(), c.Agent, c.Command, extras)
	// The account boundary is applied HERE, in the pane, after filtering and
	// immediately before exec — the last point where anything can still change
	// what the agent will see. A failure REFUSES the launch (#3051).
	if environmentOnly {
		environ, err = applyAccountEnvironmentScope(environ, c.Agent, c.Account, c.Command)
	} else {
		environ, err = applyAccountScope(environ, c.Agent, c.Account, c.Command, c.Proof)
	}
	if err != nil {
		return err
	}
	shell := "/bin/sh"
	return processExec(shell, []string{shell, "-c", c.Command}, environ)
}

func execInvocation(args []string) error {
	if err := stampPaneRootPid(); err != nil {
		return err
	}
	if len(args) < 3 {
		return fmt.Errorf("malformed internal session environment invocation")
	}
	agent := args[0]
	count, err := strconv.Atoi(args[1])
	if err != nil || count < 0 {
		return fmt.Errorf("malformed internal session environment invocation")
	}
	// The exact total: a length that merely fits leaves room for an unaccounted
	// argument, and this argv is what the boundary's whole claim rests on.
	if len(args) != 2+count+1 {
		return fmt.Errorf("malformed internal session environment invocation")
	}
	extras, err := NormalizeExtraNames(args[2 : 2+count])
	if err != nil {
		return err
	}
	command := args[len(args)-1]
	filterAgent := agent
	if GrantAgentForCommand(command, os.Environ(), paneWorkingDirectory()) != agent {
		// The argv protocol names an agent, but it is not authority by itself: a
		// repository can invoke the private marker too. Re-derive the grant from the
		// direct command through the SAME predicate the launcher used, and on
		// disagreement retain only common/explicit values.
		filterAgent = ""
	}
	environ := FilterForCommand(os.Environ(), filterAgent, command, extras)
	// tmux runs shell-command through the system shell, not the user's login
	// shell. Keep that POSIX contract: program overrides and injected commands
	// commonly use assignment prefixes, redirects, and quoting that fish/tcsh
	// interpret differently.
	shell := "/bin/sh"
	return processExec(shell, []string{shell, "-c", command}, environ)
}

// stampPaneRootPid records the pane's identity bundle in pane-scoped user
// options, while the root is trivially alive to report it: this process IS
// the pane root — the launch chain preserves the pid (the spawn's
// shell-command execs this single simple invocation, and the processExec
// below replaces the image without forking), so #{pane_pid} expands to this
// process when the write runs. tmux >=3.8 stops answering pane_pid once the
// root exits; the stamp is what lets a remain-on-exit dead pane still name
// its root at teardown (#5319).
//
// The stamp is incarnation-bound because the pane option OUTLIVES the
// incarnation that wrote it: `respawn-pane` reuses the pane struct — its
// options and its %N id — while a later reader can never ask which run wrote
// the stamp. tmux fires no hook for a respawn (measured on 3.8:
// after-respawn-pane does not exist, and pane-died/pane-exited/
// after-kill-pane are all silent through `respawn-pane -k`), and the only
// per-allocation field tmux reports after death — pane_tty — is just a name:
// `respawn-pane -k` frees the old devpts index before allocating the
// replacement, so Linux can hand the same pathname (even the same inode
// slot) straight back, and the node itself is gone by teardown regardless,
// so nothing post-mortem can read a per-allocation value off it. What makes
// the stale record impossible to RUN under is this function instead: a
// respawn re-executes this shim through the recorded start command, and the
// write below either lands a fresh bundle or, when it fails and the option
// already names an earlier run, the shim returns an error rather than exec
// the command — the pane then dies carrying a stale stamp with NO
// replacement process behind it, which teardown can trust harmlessly (the
// stamped pid's session is still only this session's own dead run). The
// alternative — launching anyway — is what lets the stale record hide a
// live replacement root.
//
// What remains unprovable is a respawn that never runs this shim at all —
// one respawning a foreign command bypasses it — and that case the command
// pin catches at the row instead: the pinned pane_start_command no longer
// equals the recorded one. And a respawned pane whose shim can neither
// write nor read the option launches under the stale record — the accepted
// residual above — because a first launch must never be refused over
// bookkeeping.
//
// Best-effort on purpose on the FIRST launch, and deliberately NOT
// version-gated: pane options (`set-option -p`, tmux >=3.0), pane hooks
// (`set-hook -p`, tmux >=3.2), and the `e|`/`==` format modifiers all exist
// below af's own floor; a tmux too old for them just lacks the record, a
// launch must never fail over a bookkeeping write it has no way to prove
// needed one, and an unparseable `tmux -V` cannot suppress it the way a
// probe gate would. The bound keeps a wedged tmux server from delaying the
// launch by more than a moment.
func stampPaneRootPid() error {
	paneID := os.Getenv("TMUX_PANE")
	if paneID == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := paneBookkeepingWrite(ctx, paneID)
	if err == nil {
		return nil
	}
	// The write failed. If the option is EMPTY the pane is a fresh launch —
	// run without the record, exactly like a pre-stamp pane. If it still
	// names an earlier run, this IS a respawn, and launching would leave the
	// stale stamp covering a live replacement root: the one state teardown
	// can mistake for fresh bookkeeping. Refuse instead — the pane dies
	// without its command running, and the dead stamp can then only point at
	// a run that genuinely produced no processes. A stamp naming THIS pid
	// means the write actually landed and only the error report was lost —
	// already fresh, so launch. And if the READ fails the pane cannot be
	// told from a fresh one at all: a first launch must never die over
	// bookkeeping, so it launches. That is the accepted residual — a respawn
	// whose shim can neither write nor read the option (`respawn-pane -e`
	// poisoning the pane env, a tmux channel dead at birth) runs under the
	// stale record, which is manual respawn-pane sabotage beyond the
	// deaths/command/pty pins the row checks below keep.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel2()
	prior, readErr := paneStampRead(ctx2, paneID)
	if readErr != nil {
		return nil
	}
	if pidField, _, _ := strings.Cut(prior, ":"); pidField == strconv.Itoa(os.Getpid()) {
		return nil
	}
	if strings.TrimSpace(prior) != "" {
		_, _ = fmt.Fprintf(os.Stderr,
			"af: pane %s still carries an earlier run's identity stamp and its restamp failed; refusing to launch under a record teardown would misattribute\n",
			paneID)
		return fmt.Errorf("pane %s carries a stale identity stamp and could not be restamped", paneID)
	}
	return nil
}

// paneBookkeepingWrite and paneStampRead are seams for tests: one client call
// installs the deaths hook, the pid/deaths/tty stamp, and the command pin
// (atomic at tmux's command queue), and one reads the stamp back.
var paneBookkeepingWrite = func(ctx context.Context, paneID string) error {
	return exec.CommandContext(ctx, "tmux",
		"set-hook", "-p", "-t", paneID, PaneDeathsHookName, PaneDeathsHookCommand,
		";",
		"set-option", "-F", "-p", "-t", paneID, PanePidUserOption, PanePidStampFormat,
		";",
		"set-option", "-F", "-p", "-t", paneID, PaneCommandUserOption, "#{pane_start_command}").Run()
}

var paneStampRead = func(ctx context.Context, paneID string) (string, error) {
	out, err := exec.CommandContext(ctx, "tmux",
		"display-message", "-p", "-t", paneID, "#{"+PanePidUserOption+"}").Output()
	return string(out), err
}

// paneWorkingDirectory reports the directory the pane command resolves
// against — the working tree the shared grant predicate checks an absolute
// executable for containment in. Getwd can only fail when the directory has
// been unlinked out from under the pane, in which case the predicate is told
// the directory is unknown rather than guessed.
func paneWorkingDirectory() string {
	dir, err := os.Getwd()
	if err != nil {
		return ""
	}
	return dir
}

// agentServerArgv0 is the argv[0] the re-exec'd agent-server keeps. On Linux
// os.Executable resolves symlinks, so a sandbox session whose <sessionDir>/af
// links into the shared binary cache would otherwise re-exec as the cache path
// and the reaper's argv[0] identity check (remotePIDIdentityKillScript) would
// no longer recognise the server (#5571 W2.2). An absolute original spelling is
// preserved; anything else falls back to the resolved executable.
func agentServerArgv0(executable string) string {
	if len(os.Args) > 0 && strings.HasPrefix(os.Args[0], "/") {
		return os.Args[0]
	}
	return executable
}

func agentServerExecInvocation(args []string) error {
	return execAgentServer(args, "")
}

// agentServerAccountExecInvocation is the account-bound variant: args[0] is the
// account's agent, the rest is the ordinary agent-server handoff.
func agentServerAccountExecInvocation(args []string) error {
	if len(args) < 1 {
		return fmt.Errorf("malformed internal agent-server environment invocation")
	}
	if _, ok := SupportsAccounts(args[0]); !ok {
		return fmt.Errorf("malformed internal agent-server environment invocation")
	}
	return execAgentServer(args[1:], args[0])
}

// execAgentServer filters the environment for the program the arguments launch
// and, when accountAgent is set, scopes it to that agent's one account.
func execAgentServer(args []string, accountAgent string) error {
	if err := stampPaneRootPid(); err != nil {
		return err
	}
	if len(args) < 2 {
		return fmt.Errorf("malformed internal agent-server environment invocation")
	}
	extraCount, err := strconv.Atoi(args[0])
	if err != nil || extraCount < 0 || extraCount > len(args)-2 {
		return fmt.Errorf("malformed internal agent-server environment invocation")
	}
	extras, err := NormalizeExtraNames(args[1 : 1+extraCount])
	if err != nil {
		return err
	}
	serverArgs := args[1+extraCount:]
	program, ok := agentServerProgram(serverArgs)
	if !ok {
		return fmt.Errorf("malformed internal agent-server environment invocation")
	}
	// agent-server receives the resolved program string but no trusted identity
	// for a path-qualified child. Keep that state structurally distinct from a
	// bare supported-agent invocation: reducing ./codex to the basename "codex"
	// would grant credentials to a repository-controlled executable.
	agent := credentialAgentForCommand(program)
	environ := FilterForCommand(os.Environ(), agent, program, extras)
	if accountAgent != "" {
		scoped, err := ScopeAgentServerAccount(environ, accountAgent, program)
		if err != nil {
			return err
		}
		environ = scoped
	}
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve current af executable: %w", err)
	}
	return processExec(executable, append([]string{agentServerArgv0(executable)}, serverArgs...), environ)
}
