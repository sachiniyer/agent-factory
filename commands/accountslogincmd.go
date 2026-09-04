package commands

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/log"
)

// `af accounts login` (#3384) — af drives the agent's own login flow instead of
// printing it for the operator to paste.
//
// The thing this does NOT do is the whole design: af never sees the credential.
// It asks the daemon to run `<agent> <the agent's own login words>` in a tmux
// session with the account's credential-root variable set and every ambient
// identity variable removed — the exact environment `--account` gives a scoped
// session — and then hands the terminal to that pane so the interactive OAuth or
// device-code step happens between the operator and the agent. Afterwards it
// asks the FILESYSTEM whether the agent wrote its own credential file. There is
// no path in this command that reads, stores, or forwards credential material,
// and there is no flag that accepts a token.

// accountsLoginStdinIsTTY reports whether there is a terminal here to hand over.
// Without one — a script, a hook, a CI step — af prints how to attach instead of
// running a full-screen tmux client into a pipe. A var so the tests can drive
// both branches, matching stdoutIsTTYFn in autoupdate_launch.go.
var accountsLoginStdinIsTTY = func() bool { return term.IsTerminal(int(os.Stdin.Fd())) }

// accountsLoginNoAttach keeps the terminal and prints how to reach the pane.
// For a script, a hook, or an operator who wants the login running in a window
// they will pick up later.
var accountsLoginNoAttach bool

var accountsLoginCmd = &cobra.Command{
	Use:   "login <agent> <name>",
	Short: "Log in to an agent account by running the agent's own login flow",
	Long: `Run the agent's own login command against an account's credential directory.

af registers the account if it does not exist yet, asks the daemon to start the
agent's login flow in a tmux session scoped to that account, and hands you the
terminal so you can complete the browser or device-code step. When the flow ends,
af reports whether the account holds a credential — read from the agent's own
credential file, by checking that it exists, never by reading it.

  af accounts login codex work
  af accounts login claude personal

af never reads, stores, or forwards the credential. It sets one variable and runs
the agent's own flow:

  claude → claude auth login · codex → codex login · gemini → gemini

The login pane gets exactly the environment an account-scoped session gets: the
account's directory injected, and every other identity-bearing variable for that
agent REMOVED. The removal is what makes the login land in the account you asked
for — an ambient ANTHROPIC_API_KEY or OPENAI_API_KEY outranks the config
directory, so without it the CLI could report success against that key's identity
while the account directory stayed empty.

It removes identity, not environment: proxies, private CA roots and your git and
SSH configuration are passed through as always. If the login needs something else
— DISPLAY or BROWSER, on a host with a browser to open — add it to
session_env_passthrough.

The flow runs on the daemon's host, where the credential directory is. With
--no-attach af prints the tmux session to attach to instead of taking your
terminal.`,
	// ArbitraryArgs with the count checked in RunE, for the same reason as `add`:
	// cobra runs an Args validator BEFORE RunE, so ExactArgs would emit human
	// `Error:` text and usage even under --json, leaving an automation caller
	// unable to parse a malformed invocation (#3057 review).
	Args: cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		if len(args) != 2 {
			return jsonWrapError(cmd, accountsJSONFlag, fmt.Errorf(
				"af accounts login takes exactly two arguments, an agent and an account name (got %d): af accounts login <agent> <name>",
				len(args)))
		}
		// --daemon-url / AF_DAEMON_URL promises a REMOTE daemon, and this verb is
		// inseparable from the machine the account lives on: the credential
		// directory is on the daemon's filesystem, the login pane runs there, and
		// the terminal it needs is here. Attaching across that gap is not something
		// af can do, and a login that silently ran somewhere the operator is not
		// sitting is worse than a refusal. Same seam and same reasoning as
		// `af accounts add` and `af quota` (#3057 review).
		if apiclient.IsRemoteTarget() {
			return jsonWrapError(cmd, accountsJSONFlag, fmt.Errorf(
				"af accounts login runs the agent's login flow on the machine whose agent-factory home holds "+
					"the account, and hands you that pane's terminal; it cannot do that against a remote daemon. "+
					"Unset --daemon-url/AF_DAEMON_URL to act on this host, or run af accounts login on the "+
					"daemon's host"))
		}

		agent, name := args[0], args[1]
		// Validate the NAME here, before the daemon is asked to start: a bad name
		// otherwise reaches the operator wrapped in an RPC error, and this is the
		// one input they are most likely to get wrong.
		if err := agentaccount.ValidateName(name); err != nil {
			return jsonWrapError(cmd, accountsJSONFlag, err)
		}

		login, err := accountLoginViaDaemon(daemon.AccountLoginRequest{Agent: agent, Name: name})
		if err != nil {
			return jsonWrapError(cmd, accountsJSONFlag, err)
		}

		if accountsJSONFlag {
			// --json never attaches: a caller reading an envelope is not sitting at
			// the terminal this would take.
			return apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(login))
		}

		out, errOut := cmd.OutOrStdout(), cmd.ErrOrStderr()
		fmt.Fprintf(errOut, "Account %q for %s · %s\n", login.Name, login.Agent, login.Dir)
		for _, notice := range login.Notices {
			fmt.Fprintf(errOut, "Note · %s\n", notice)
		}
		if login.LoggedIn && !login.Finished {
			fmt.Fprintf(errOut,
				"This account already holds a %s credential; completing this flow replaces it.\n", login.Agent)
		}

		// The flow ended before af could hand anything over — a login that
		// answered itself. Report the outcome; there is no pane.
		if login.Finished {
			return reportAccountLoginOutcome(cmd, login.Agent, login.Name)
		}

		if login.Reused {
			fmt.Fprintf(errOut, "A login for this account is already running; joining it.\n")
		}
		fmt.Fprintf(errOut, "Running: %s\n", login.Program)

		if accountsLoginNoAttach || !accountsLoginStdinIsTTY() {
			// The bare attach command on stdout, so it can be captured or pasted
			// without parsing; the framing goes to stderr, as everywhere else in
			// this group.
			fmt.Fprintln(out, accountLoginAttachCommand(login.SessionName, login.SocketPath))
			fmt.Fprintf(errOut,
				"Attach with the command above to finish signing in.\n"+
					"Or run `af accounts login %s %s` again from a terminal — it joins this same flow "+
					"rather than starting a second one.\n",
				login.Agent, login.Name)
			return nil
		}

		if err := attachAccountLogin(login.SessionName, login.SocketPath); err != nil {
			// The pane is still running, so this is not the end of the login — say
			// how to get back to it rather than only what failed.
			return jsonWrapError(cmd, accountsJSONFlag, fmt.Errorf(
				"could not attach to the login pane: %w\nAttach by hand with: %s",
				err, accountLoginAttachCommand(login.SessionName, login.SocketPath)))
		}
		// DETACHING IS NOT FINISHING, and the two look identical from here: the
		// attach returns when the pane exits AND when the operator presses the tmux
		// detach key to go read something in a browser. Reporting the second as a
		// failed login — which is what asking only the account would do — turns an
		// ordinary mid-flow pause into a red error, so the pane is asked whether it
		// is still there first.
		if accountLoginPaneAlive(login.SessionName, login.SocketPath) {
			fmt.Fprintf(errOut,
				"Detached · the %s login is still running for %q. Rejoin it with `af accounts login %s %s`, "+
					"or with: %s\n",
				login.Agent, login.Name, login.Agent, login.Name,
				accountLoginAttachCommand(login.SessionName, login.SocketPath))
			return nil
		}
		return reportAccountLoginOutcome(cmd, login.Agent, login.Name)
	},
}

// accountLoginViaDaemon is a var so the CLI's own tests can drive every branch
// of this command without a daemon, exactly as api/projects.go does for
// RegisterProject.
var accountLoginViaDaemon = daemon.AccountLogin

// reportAccountLoginOutcome answers the question the operator actually has, from
// the agent's own artifact rather than from the flow's exit status.
//
// #3384 is explicit that a login must be VERIFIED and not assumed: several of
// these CLIs exit 0 from a flow the user abandoned at the browser step, and a
// registered-but-empty account then fails much later at session start, naming
// none of this. So a failed login is reported as a failure HERE, where the
// operator is still looking.
func reportAccountLoginOutcome(cmd *cobra.Command, agent, name string) error {
	home, err := config.GetConfigDir()
	if err != nil {
		return jsonWrapError(cmd, accountsJSONFlag, err)
	}
	loggedIn, err := agentaccount.LoggedIn(home, agent, name)
	if err != nil {
		return jsonWrapError(cmd, accountsJSONFlag, err)
	}
	if !loggedIn {
		return jsonWrapError(cmd, accountsJSONFlag, fmt.Errorf(
			"the %s login flow ended and account %q still holds no credential, so it is registered but not "+
				"logged in — a session started with --account %s would run unauthenticated. Run "+
				"`af accounts login %s %s` again to finish signing in",
			agent, name, name, agent, name))
	}
	fmt.Fprintf(cmd.ErrOrStderr(),
		"Account %q is logged in for %s · use it with `af sessions create --account %s`\n", name, agent, name)
	return nil
}

// accountLoginAttachCommand renders the exact command that reaches the pane.
//
// The socket is PINNED with -S when the daemon could resolve it, because the
// daemon and this process can resolve different TMUX_TMPDIRs, so the default
// socket is not something either side may assume (#2019). Empty means the daemon
// could not resolve it, and the default socket is then the best available answer.
func accountLoginAttachCommand(sessionName, socketPath string) string {
	if socketPath == "" {
		return fmt.Sprintf("tmux attach-session -t %s", config.ShellQuotePath(sessionName))
	}
	return fmt.Sprintf("tmux -S %s attach-session -t %s",
		config.ShellQuotePath(socketPath), config.ShellQuotePath(sessionName))
}

// attachAccountLogin hands this terminal to the login pane and takes it back
// when the flow ends or the operator detaches.
//
// $TMUX is scrubbed for the reason the TUI's config-agent attach scrubs it: when
// af was launched from inside a tmux pane it inherits $TMUX, and
// `tmux attach-session` then refuses to nest ("sessions should be nested with
// care, unset $TMUX to force") and exits 1. Dropping it is exactly what tmux's
// own error instructs. TMUX_TMPDIR is deliberately kept — it participates in
// default-socket resolution, which is the fallback when -S is absent (#2019).
//
// A var so the CLI tests can drive the attach branch without a tmux server.
var attachAccountLogin = func(sessionName, socketPath string) error {
	args := make([]string, 0, 5)
	if socketPath != "" {
		args = append(args, "-S", socketPath)
	}
	args = append(args, "attach-session", "-t", sessionName)
	attach := exec.Command("tmux", args...)
	attach.Stdin, attach.Stdout, attach.Stderr = os.Stdin, os.Stdout, os.Stderr
	attach.Env = accountLoginAttachEnv()
	return attach.Run()
}

// accountLoginPaneAlive reports whether the login pane is still running.
//
// It answers only on tmux's POSITIVE answer. `has-session` fails for reasons
// that have nothing to do with the session — no server reachable, a socket af
// cannot read — and treating those as "still running" would suppress the
// verification #3384 requires. So anything but a clean exit means "gone", and the
// account is then asked the real question.
//
// A var so the tests can drive both sides of the detach branch.
var accountLoginPaneAlive = func(sessionName, socketPath string) bool {
	args := make([]string, 0, 5)
	if socketPath != "" {
		args = append(args, "-S", socketPath)
	}
	// The `=` prefix is tmux's exact-match syntax: without it a session name is a
	// PREFIX pattern, so `af_af-login-codex-work` would match a hypothetical
	// `…-work-2` and report the wrong pane alive.
	args = append(args, "has-session", "-t", "="+sessionName)
	return exec.Command("tmux", args...).Run() == nil
}

// accountLoginAttachEnv is this process's environment with the TMUX marker
// removed. The `TMUX=` prefix match is exact, so it drops `TMUX=…` without
// touching `TMUX_TMPDIR=…`, whose key does not start with `TMUX=`.
func accountLoginAttachEnv() []string {
	source := os.Environ()
	out := make([]string, 0, len(source))
	for _, kv := range source {
		if strings.HasPrefix(kv, "TMUX=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

func init() {
	accountsLoginCmd.Flags().BoolVar(&accountsJSONFlag, "json", false, "Output the {data,error} JSON envelope")
	accountsLoginCmd.Flags().BoolVar(&accountsLoginNoAttach, "no-attach", false,
		"Start the login flow and print how to attach instead of taking this terminal")
	accountsLoginCmd.SetFlagErrorFunc(accountsFlagError)
	accountsCmd.AddCommand(accountsLoginCmd)
}
