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
	return wrapCommand(executable, agent, "", extras, command)
}

// WrapAccountCommand is WrapCommand for a session scoped to a named account.
//
// Only the account NAME travels in argv — never its directory and never a
// credential. The shim resolves the name against its own AF home, so argv stays
// free of anything worth reading out of `ps` (#3051).
//
// The proof (the exact executable and argument words af authored for the pane
// command) NO LONGER rides in argv: argv is forgeable by a repository-controlled
// program_overrides value re-invoking af under this marker, so an argv-supplied
// TrustedExecutable is not evidence af wrote it. session/tmux installs the
// proof out of band through the pane environment (AccountLaunchProofEnvEntry);
// the shim reads it from there and refuses an account scope that arrives
// without it (#3123 review, #3051 fail-closed).
func WrapAccountCommand(executable, agent, account string, extras []string, command string) (string, error) {
	if strings.TrimSpace(account) == "" {
		return "", fmt.Errorf("account-scoped launch requires an account name")
	}
	return wrapCommand(executable, agent, account, extras, command)
}

// WrapAccountEnvironmentCommand applies a selected account to a sibling pane's
// environment. Unlike WrapAccountCommand, the pane command may be a shell or an
// arbitrary process; the account still resolves in the child and never falls
// back to ambient credentials.
func WrapAccountEnvironmentCommand(executable, agent, account string, extras []string, command string) (string, error) {
	if strings.TrimSpace(account) == "" {
		return "", fmt.Errorf("account-scoped environment requires an account name")
	}
	return wrapCommandWithMarker(executable, AccountEnvironmentExecMarker, agent, account, extras, command)
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

func wrapCommand(executable, agent, account string, extras []string, command string) (string, error) {
	marker := ExecMarker
	if account != "" {
		marker = AccountExecMarker
	}
	return wrapCommandWithMarker(executable, marker, agent, account, extras, command)
}

func wrapCommandWithMarker(executable, marker, agent, account string, extras []string, command string) (string, error) {
	normalized, err := NormalizeExtraNames(extras)
	if err != nil {
		return "", err
	}
	// The account-scoped markers carry the account NAME only. The launch proof
	// (TrustedExecutable / GeneratedArgs) is no longer appended here: an
	// argv-supplied proof is forgeable by a re-invoking shell command, so it is
	// carried out of band through the pane environment instead (#3123 review).
	args := []string{executable, marker, agent, strconv.Itoa(len(normalized))}
	if account != "" {
		args = append(args, account)
	}
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
	if os.Args[1] == AgentServerExecMarker {
		if err := agentServerExecInvocation(os.Args[2:]); err != nil {
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
	if err := execInvocationMode(os.Args[2:], scoped || environmentOnly, environmentOnly); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, "af: could not start the filtered session process")
		os.Exit(127)
	}
}

func execInvocation(args []string, scoped bool) error {
	return execInvocationMode(args, scoped, false)
}

func execInvocationMode(args []string, scoped, environmentOnly bool) error {
	if err := stampPaneRootPid(); err != nil {
		return err
	}
	// The account-scoped markers now carry only `<agent> <extras-count>
	// <account> <extras...> <command>`: the launch proof (TrustedExecutable /
	// GeneratedArgs) left argv for an out-of-band environment channel that a
	// re-invoking shell command cannot forge (#3123 review). The minimum length
	// is the agent, the count, the account, and the command.
	trailing := 3
	if scoped {
		trailing = 4
	}
	if len(args) < trailing {
		return fmt.Errorf("malformed internal session environment invocation")
	}
	agent := args[0]
	count, err := strconv.Atoi(args[1])
	if err != nil || count < 0 {
		return fmt.Errorf("malformed internal session environment invocation")
	}
	offset := 2
	account := ""
	proof := AccountLaunchProof{}
	if scoped {
		account = args[2]
		offset = 3
		if !environmentOnly {
			// The proof is the launcher's affirmation that it authored this
			// command's executable and generated words. It now arrives only through
			// the pane environment, never argv; a repository re-invocation has no
			// such proof, so the account scope is REFUSED rather than granted on an
			// unprovable argv claim — the #3051 fail-closed property, applied to
			// provenance. Environment-only sibling panes never need the proof: their
			// command is not the agent executable.
			proof, err = decodeAccountLaunchProofEnv(os.Getenv(accountLaunchProofEnvVar))
			if err != nil {
				return fmt.Errorf("account-scoped launch refused: %w", err)
			}
			// The env var is writable by the same shell that re-invokes af, so a
			// decoded value alone is not evidence af's launcher produced it. Re-derive
			// what the launcher would have produced from the pane's resolved
			// operator config and refuse a mismatch: an attacker who overwrites the
			// env var with their own TrustedExecutable cannot also overwrite what
			// the resolver reads (#3123, #4731 review). A missing resolver — e.g.
			// in tests that drive the shim directly without wiring main.go — keeps
			// the historical standalone-env behaviour so the existing
			// proof-channel regression witnesses still hold.
			if AccountLaunchProofResolver != nil {
				expected, resolveErr := AccountLaunchProofResolver(agent, account, args[len(args)-1])
				if resolveErr != nil {
					// A resolver error must REFUSE, not fall back to the env proof
					// alone: a repository-controlled parent that can re-invoke af
					// under this marker can also deliberately make re-derivation
					// fail (e.g. by chdir'ing into a directory a sibling shell
					// removes before the spawn), and a forged env var is then the
					// only "proof" left. Bypassing the cross-check on a resolver
					// error re-opens the forgeable-env channel this gate closed
					// (#4731 review, Codex P1 on f903b934).
					return fmt.Errorf("account-scoped launch refused: %w", resolveErr)
				}
				if !accountLaunchProofsMatch(proof, expected) {
					return fmt.Errorf("account-scoped launch refused: %w", errAccountLaunchProofMismatch)
				}
			}
		}
	}
	// Compared against the REMAINING room, never `offset+count`: a maximum-sized
	// integer makes that addition overflow and the slice / exact-total below
	// would PANIC or mis-decide instead of returning this generic refusal (#3083
	// review). Subtraction cannot underflow here because len(args) >= trailing >
	// offset, already checked, so the count is bounded by a non-negative number.
	if count > len(args)-offset {
		return fmt.Errorf("malformed internal session environment invocation")
	}
	// The exact total, checked AFTER the count is known. A length that merely
	// fits leaves room for an unaccounted argument between the lists, and this
	// argv is what the boundary's whole claim rests on.
	if len(args) != offset+count+1 {
		return fmt.Errorf("malformed internal session environment invocation")
	}
	extras, err := NormalizeExtraNames(args[offset : offset+count])
	if err != nil {
		return err
	}
	command := args[len(args)-1]
	filterAgent := agent
	if !environmentOnly && AgentForCommand(command) != agent {
		// The argv protocol names an agent, but it is not authority by itself: a
		// repository can invoke the private marker too. The command is what
		// actually receives the account credentials, so on disagreement an
		// account-scoped launch is REFUSED rather than narrowed — granting the
		// account scope to a command that is not the selected agent is the silent
		// wrong-identity outcome this feature exists to prevent (#3051, #4356).
		// The non-scoped ExecMarker only narrows the environment filter, since no
		// account scope is at stake. Agent-server uses its effect-bound protocol
		// above instead of asking this generic path to infer identity from nested
		// argv.
		if scoped {
			return fmt.Errorf(
				"account-scoped launch refused: the resolved command does not resolve to the selected agent %q",
				agent)
		}
		filterAgent = ""
	}
	environ := FilterForCommand(os.Environ(), filterAgent, command, extras)
	// Defense in depth: the proof variable must never reach the agent, even if a
	// future change accidentally admits it through the filter. FilterForCommand
	// already drops it (it is not an allowlisted name), but the boundary does not
	// rely on the allowlist alone.
	environ = dropEnvVar(environ, accountLaunchProofEnvVar)
	// The account boundary is applied HERE, in the pane, after filtering and
	// immediately before exec — the last point where anything can still change
	// what the agent will see. A failure REFUSES the launch rather than falling
	// through to the ambient identity, which would be the silent wrong-account
	// outcome the whole feature exists to prevent (#3051).
	if scoped {
		if environmentOnly {
			environ, err = applyAccountEnvironmentScope(environ, agent, account, command)
		} else {
			environ, err = applyAccountScope(environ, agent, account, command, proof)
		}
		if err != nil {
			return err
		}
	}
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

// dropEnvVar returns environ without any entry named `name`. Env entries here
// are NAME=VALUE; an entry equal to `name` (no `=`) is not a variable Filter
// produces and is left untouched.
func dropEnvVar(environ []string, name string) []string {
	prefix := name + "="
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		if strings.HasPrefix(kv, prefix) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func agentServerExecInvocation(args []string) error {
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
	executable, err := os.Executable()
	if err != nil {
		return fmt.Errorf("resolve current af executable: %w", err)
	}
	return processExec(executable, append([]string{executable}, serverArgs...), environ)
}
