package commands

import (
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

// `af accounts` manages the credential DIRECTORIES a session can be scoped to
// (#2983/#3051).
//
// The thing to understand before reading further: af never handles the secret.
// An account is a directory that the agent CLI treats as its home, and the
// agent's own `login` flow is what puts credential material there. af decides
// only which directory a session sees. That is why `add` creates an empty
// directory and tells you to log in, rather than asking for a token — a provider
// rotating its token format costs this code nothing, and af never becomes a
// place where credentials are stored or could leak from.

// accountsJSONFlag switches the subcommands to the shared {data,error} envelope,
// matching `af config` and `af token`.
var accountsJSONFlag bool

// accountEntry is one registered account. The directory is included because it
// is the thing the operator has to point the agent's login flow at, so omitting
// it would make the JSON strictly less useful than the human output.
type accountEntry struct {
	Agent string `json:"agent"`
	Name  string `json:"name"`
	Dir   string `json:"dir"`
}

// accountsFlagError routes a flag-parse failure through the same {data,error}
// envelope as every other failure in this group.
//
// Without it, the one input class an automation caller is most likely to get
// wrong — a mistyped flag — was the one class it could not parse, because cobra
// returns before RunE and prints human `Error:` plus usage. ArbitraryArgs moved
// argument-COUNT validation into RunE but cannot reach flag parsing, which
// happens earlier still.
//
// It reads the BOUND variable, which is already authoritative here. Three
// revisions of this function scanned os.Args instead, on the premise that a
// parse failure leaves the bound variable unset — and that premise is false.
// pflag calls Value.Set as it walks argv, so every occurrence BEFORE the failure
// has already landed by the time cobra calls this: all spellings, repeats with
// last-one-wins, and the `--` terminator, handled by the parser itself. The only
// thing a scanner could add is a `--json` occurring AFTER the failure point,
// which pflag never accepted and which therefore must not count.
//
// Each revision fixed one way the scan diverged from pflag — first occurrence,
// then boolean spellings, then repeats — and each was a closer simulation of a
// parser we can simply ask. That is the defect class, not the individual bugs
// (#3057 review).
func accountsFlagError(cmd *cobra.Command, err error) error {
	return jsonWrapError(cmd, accountsJSONFlag, err)
}

var accountsCmd = &cobra.Command{
	Use:   "accounts",
	Short: "Manage per-session agent credential directories",
	Long: `Prepare the credential directories a session can later be scoped to.

An account is one of an agent's logged-in identities, held as a directory the
agent CLI treats as its home. af never reads, stores, or forwards the credential
itself — it decides which directory a session sees, and the agent's own login
flow puts the material there.

add creates that directory and prints its path · list shows what is registered.
Register an account, then log in with the agent pointed at that directory:

  af accounts add codex work
  CODEX_HOME=$(af accounts add codex work) codex login

Select an account for a session with:

  af sessions create --account work

That both injects the account's directory and removes every other
identity-bearing variable for the agent. The removal is what makes the selection
real: an ambient ANTHROPIC_API_KEY or OPENAI_API_KEY outranks the config
directory, so without it a session would authenticate as whoever that key belongs
to while every visible signal reported the selected account.

Account-scoped sessions require the local or docker backend, and tmux 3.2 or newer. af
refuses rather than falling back, because a fallback would run on the ambient
account while reporting the one you asked for.

ssh, sandbox and hook refuse by design, not because the work is pending. docker
bind-MOUNTS the directory, so account writes land in your real account. ` +
		session.AccountWriteBackRationale + `

By default af never switches accounts on its own. With limit_auto_resume enabled,
an explicit limit_account_candidates list may opt an unpinned local
session into switching after a usage limit. af skips registered candidates with
a current limit observation, says which identity changed in the session, and
waits normally when none is usable. Docker account-scoped creates remain
supported, but automatic Docker replacement is disabled until af can durably
identify and reap a crash-surviving container and freeze its complete provision
plan. An explicit --account is a permanent pin and is never overridden.`,
}

var accountsAddCmd = &cobra.Command{
	Use:   "add <agent> <name>",
	Short: "Register a credential directory for an agent account",
	Long: `Create the credential directory for an account and print its path.

This makes a place; it does not log in. Run the agent's own login flow against
the printed directory to put credentials there.

Registration is idempotent — running it again on an existing account reports the
same directory and touches nothing inside it.`,
	// ArbitraryArgs, with the count checked inside RunE. cobra runs an Args
	// validator BEFORE RunE, so ExactArgs(2) emitted human `Error:` and usage text
	// even under --json — leaving an automation caller unable to parse the one
	// class of failure it is most likely to hit, a malformed invocation (#3057
	// review).
	Args: cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		if len(args) != 2 {
			return jsonWrapError(cmd, accountsJSONFlag,
				fmt.Errorf("af accounts add takes exactly two arguments, an agent and an account name (got %d): af accounts add <agent> <name>", len(args)))
		}

		// --daemon-url / AF_DAEMON_URL promises to target a REMOTE daemon, and this
		// command writes to the LOCAL AF home. Honouring the flag by ignoring it
		// hands the operator a valid-looking directory on the wrong machine — and
		// then invites them to log real credentials into it, for an account the
		// targeted daemon can never see. A credential in the wrong place is a worse
		// outcome than a refusal, so refuse. Same seam and same reasoning as
		// `af quota` (#3057 review).
		if apiclient.IsRemoteTarget() {
			return jsonWrapError(cmd, accountsJSONFlag, fmt.Errorf(
				"af accounts manages credential directories in this machine's agent-factory home and cannot "+
					"manage them on a remote daemon; unset --daemon-url/AF_DAEMON_URL to act on this host, "+
					"or run af accounts on the daemon's host"))
		}

		agent, name := args[0], args[1]
		home, err := config.GetConfigDir()
		if err != nil {
			return jsonWrapError(cmd, accountsJSONFlag, err)
		}
		dir, err := agentaccount.Register(home, agent, name)
		if err != nil {
			return jsonWrapError(cmd, accountsJSONFlag, err)
		}

		entry := accountEntry{Agent: agent, Name: name, Dir: dir}
		if accountsJSONFlag {
			return apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(entry))
		}
		// The bare path on stdout is deliberate: it makes the command substitutable
		// into the login invocation above without any parsing. Guidance goes to
		// stderr so it survives being captured.
		fmt.Fprintln(cmd.OutOrStdout(), dir)
		configVar, _ := sessionenv.SupportsAccounts(agent)
		// The guidance line is PASTEABLE, so the path in it must be shell-quoted:
		// an AGENT_FACTORY_HOME containing a space or a shell metacharacter would
		// otherwise produce a command that fails, or worse, one that parses into
		// something else and logs in somewhere other than the registered directory.
		// The stdout copy above stays bare on purpose — it is consumed by command
		// substitution, which needs the raw value (#3057 review).
		fmt.Fprintf(cmd.ErrOrStderr(), "Registered account %q for %s.\n", name, agent)
		// Per agent, never a universal "login": Claude Code puts it under `auth`,
		// so the generic template printed `claude login`, which is not a command.
		// An agent with no known invocation gets no printed command rather than a
		// guessed one — a next step that does not run reads as a broken account
		// (#3057 review).
		if words, ok := agentaccount.LoginCommand(agent); ok {
			fmt.Fprintf(cmd.ErrOrStderr(), "Log in with that account before using it:\n  %s=%s %s %s\n",
				configVar, config.ShellQuotePath(dir), agent, strings.Join(words, " "))
		} else {
			fmt.Fprintf(cmd.ErrOrStderr(), "Log in with %s's own login flow, with %s set to %s.\n",
				agent, configVar, config.ShellQuotePath(dir))
		}
		return nil
	},
}

var accountsListCmd = &cobra.Command{
	Use:   "list [agent]",
	Short: "List registered agent accounts",
	Long: `List the registered accounts, for one agent or for every agent that
supports account scoping.

An agent absent from this list is one whose credential relocation af has not
verified, not one that is merely unconfigured — af reports unsupported rather
than accepting a selection that would silently do nothing.`,
	// ArbitraryArgs for the same reason as `add` — see the note there.
	Args: cobra.ArbitraryArgs,
	RunE: func(cmd *cobra.Command, args []string) error {
		log.Initialize(false)
		defer log.Close()

		if len(args) > 1 {
			return jsonWrapError(cmd, accountsJSONFlag,
				fmt.Errorf("af accounts list takes at most one argument, an agent name (got %d): af accounts list [agent]", len(args)))
		}

		// --daemon-url / AF_DAEMON_URL promises to target a REMOTE daemon, and this
		// command reads the LOCAL AF home. Ignoring the flag would report this
		// machine's accounts as though they were the remote daemon's, which is how
		// an operator concludes an account exists there and starts a session
		// against it. Same seam and same reasoning as `af quota` (#3057 review).
		if apiclient.IsRemoteTarget() {
			return jsonWrapError(cmd, accountsJSONFlag, fmt.Errorf(
				"af accounts manages credential directories in this machine's agent-factory home and cannot "+
					"manage them on a remote daemon; unset --daemon-url/AF_DAEMON_URL to act on this host, "+
					"or run af accounts on the daemon's host"))
		}

		home, err := config.GetConfigDir()
		if err != nil {
			return jsonWrapError(cmd, accountsJSONFlag, err)
		}

		agents := sessionenv.AccountAgents()
		if len(args) == 1 {
			if _, ok := sessionenv.SupportsAccounts(args[0]); !ok {
				return jsonWrapError(cmd, accountsJSONFlag, fmt.Errorf("%w: %s (supported: %s)",
					agentaccount.ErrUnsupportedAgent, args[0], joinAgents(agents)))
			}
			agents = []string{args[0]}
		}

		entries := []accountEntry{}
		for _, agent := range agents {
			names, err := agentaccount.List(home, agent)
			if err != nil {
				return jsonWrapError(cmd, accountsJSONFlag, err)
			}
			for _, name := range names {
				dir, err := agentaccount.Dir(home, agent, name)
				if err != nil {
					return jsonWrapError(cmd, accountsJSONFlag, err)
				}
				entries = append(entries, accountEntry{Agent: agent, Name: name, Dir: dir})
			}
		}
		sort.Slice(entries, func(i, j int) bool {
			if entries[i].Agent != entries[j].Agent {
				return entries[i].Agent < entries[j].Agent
			}
			return entries[i].Name < entries[j].Name
		})

		if accountsJSONFlag {
			return apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(entries))
		}
		if len(entries) == 0 {
			fmt.Fprintf(cmd.OutOrStdout(),
				"No accounts registered · add one with `af accounts add %s <name>`\n", agents[0])
			return nil
		}
		for _, entry := range entries {
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", entry.Agent, entry.Name, entry.Dir)
		}
		return nil
	},
}

func joinAgents(agents []string) string {
	out := ""
	for idx, agent := range agents {
		if idx > 0 {
			out += ", "
		}
		out += agent
	}
	return out
}

func init() {
	accountsAddCmd.Flags().BoolVar(&accountsJSONFlag, "json", false, "Output the {data,error} JSON envelope")
	accountsListCmd.Flags().BoolVar(&accountsJSONFlag, "json", false, "Output the {data,error} JSON envelope")
	accountsAddCmd.SetFlagErrorFunc(accountsFlagError)
	accountsListCmd.SetFlagErrorFunc(accountsFlagError)
	accountsCmd.SetFlagErrorFunc(accountsFlagError)
	accountsCmd.AddCommand(accountsAddCmd)
	accountsCmd.AddCommand(accountsListCmd)
}
