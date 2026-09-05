package commands

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
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

// accountEntry is one registered account, and it is daemon.AccountEntry rather
// than a struct of this package's own.
//
// `af accounts list` reads the LOCAL agent-factory home — it refuses a remote
// daemon — so no daemon is involved in producing these bytes. The type is shared
// anyway, because the daemon's ListAccounts (#3385) reports the same accounts to
// the TUI and the web, and two structs with the same field names are how a CLI
// and a UI end up disagreeing about whether an account is logged in. One
// definition cannot drift from itself.
type accountEntry = daemon.AccountEntry

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

add creates that directory and prints its path · login runs the agent's own login
flow against it · list shows what is registered.

  af accounts login codex work

That one command registers the account if needed, starts codex's own login in a
tmux session scoped to it, hands you the terminal for the device-code step, and
afterwards reports whether the account holds a credential — read from the agent's
own credential file, by checking that it exists.

Every login af runs is browser-free. The pane is on the daemon's host, which is
usually headless and remote, so the flow that fits is the device code: the CLI
prints a URL and a code, you sign in from whatever device you are holding, and
the CLI polls. af selects it per agent — see af accounts login --help.

You can still do it by hand, and af runs exactly these:

  CODEX_HOME=$(af accounts add codex work) codex login --device-auth
  CLAUDE_CONFIG_DIR=$(af accounts add claude work) BROWSER=true claude auth login
  GEMINI_CLI_HOME=$(af accounts add gemini work) NO_BROWSER=true gemini

Those variables do not all have the same shape, and mixing them up is the easy
mistake. CODEX_HOME and CLAUDE_CONFIG_DIR name the config directory itself.
GEMINI_CLI_HOME is a HOME-like root: gemini appends .gemini/ to it, so the account
directory af prints holds the credential at <dir>/.gemini/gemini-credentials.json.
Point the variable at the printed directory, never at a .gemini path inside it.

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
an explicit limit_account_candidates list may opt an unpinned local session into
switching after a usage limit. af skips registered candidates with a current
limit observation, says which identity changed in the session, and waits normally
when none is usable. Docker account-scoped creates remain supported, but
automatic Docker replacement is disabled until af can durably identify and reap a
crash-surviving container and freeze its complete provision plan. An explicit
--account is a permanent pin and is never overridden.` + accountsRegistrationOnlyHelp(),
}

// accountsRegistrationOnlyHelp appends the registration-only agents to the group
// help, derived rather than written down. An agent is registration-only while af
// has verified that its credential root relocates but not that the account
// boundary can prove how af launches it — so `add` and `list` work and `--account`
// refuses. Composing it from the roster means the help stops saying it by itself
// on the day the launch proof lands, instead of becoming the stale sentence that
// contradicts the CLI (#3609 review).
func accountsRegistrationOnlyHelp() string {
	var out strings.Builder
	for _, agent := range sessionenv.AccountAgents() {
		reason, ok := sessionenv.AccountRegistrationOnlyReason(agent)
		if !ok {
			continue
		}
		fmt.Fprintf(&out, "\n\n%s", wrapHelpParagraph("Registration only · "+reason, helpWrapColumns))
	}
	return out.String()
}

// helpWrapColumns is the width the rest of this group's help is written to by
// hand.
const helpWrapColumns = 80

// wrapHelpParagraph greedily wraps a single paragraph on spaces.
//
// The composed notice is assembled from a sentence sessionenv owns, so it cannot
// be hand-wrapped where it is written the way the literal help above is. Left
// unwrapped it renders as one 400-column line in `af accounts --help` and in the
// generated CLI reference, beside paragraphs that stop at 80.
//
// A word longer than the width keeps its own line rather than being split, which
// is what keeps the follow-up URL in one piece — a broken link is worse than a
// long line, and the link is the actionable half of the sentence.
//
// Width is counted in RUNES, not bytes. The sentence carries `—` and `·`, which
// are three bytes each and one column each, so len() would wrap this paragraph
// several columns short of every hand-written one beside it. Rune count is the
// right measure for this text specifically — it is prose with no wide or
// combining characters — and not a general terminal-width function.
func wrapHelpParagraph(text string, width int) string {
	var out strings.Builder
	column := 0
	for idx, word := range strings.Fields(text) {
		length := utf8.RuneCountInString(word)
		switch {
		case idx == 0:
			out.WriteString(word)
			column = length
		case column+1+length > width:
			out.WriteString("\n")
			out.WriteString(word)
			column = length
		default:
			out.WriteString(" ")
			out.WriteString(word)
			column += 1 + length
		}
	}
	return out.String()
}

var accountsAddCmd = &cobra.Command{
	Use:   "add <agent> <name>",
	Short: "Register a credential directory for an agent account",
	Long: `Create the credential directory for an account and print its path.

This makes a place; it does not log in. Run the agent's own login flow against
the printed directory to put credentials there.

Registration is idempotent — running it again on an existing account reports the
same directory and preserves existing settings. Missing non-credential runtime
settings may be seeded; notices explaining them are printed to stderr.`,
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

		// The freshly registered account, including whether it already holds a
		// credential: `add` is idempotent, so this is routinely run against an
		// account that is already logged in, and reporting logged_in:false there
		// would be a claim rather than an omission.
		loggedIn, err := agentaccount.LoggedIn(home, agent, name)
		if err != nil {
			return jsonWrapError(cmd, accountsJSONFlag, err)
		}
		notices, err := agentaccount.CheckLoginPreconditions(agent, dir)
		if err != nil {
			return jsonWrapError(cmd, accountsJSONFlag, err)
		}
		for _, notice := range notices {
			fmt.Fprintln(cmd.ErrOrStderr(), notice)
		}
		entry := accountEntry{Agent: agent, Name: name, Dir: dir, LoggedIn: loggedIn}
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
		//
		// The af verb goes FIRST, because since #3384 it is the thing that actually
		// runs this — it registers, scopes, subtracts the ambient identity and
		// verifies the result, where a pasted command does only the first half. The
		// literal invocation stays underneath rather than being replaced: it is what
		// af runs, and an operator who can see it does not have to trust a sentence
		// about it.
		if program, err := agentaccount.LoginProgram(agent); err == nil {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"Log in to it with:\n  af accounts login %s %s\nwhich runs %s with %s=%s.\n",
				agent, name, program, configVar, config.ShellQuotePath(dir))
		} else {
			fmt.Fprintf(cmd.ErrOrStderr(), "Log in with %s's own login flow, with %s set to %s.\n",
				agent, configVar, config.ShellQuotePath(dir))
		}
		// Said HERE, at the moment the operator is being invited to put real
		// credentials somewhere. Registration succeeding and `--account` refusing
		// later is the contradiction #3609 was reviewed for; the account is still
		// worth creating — #3384's login verb needs it — but the operator has to
		// learn what it cannot do yet before they log in, not after.
		if reason, ok := sessionenv.AccountRegistrationOnlyReason(agent); ok {
			fmt.Fprintf(cmd.ErrOrStderr(), "Registration only · %s\n", reason)
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
					agentaccount.ErrUnsupportedAgent, args[0], sessionenv.AccountAgentsSummary()))
			}
			agents = []string{args[0]}
		}

		entries := []accountEntry{}
		for _, agent := range agents {
			names, err := agentaccount.List(home, agent)
			if err != nil {
				return jsonWrapError(cmd, accountsJSONFlag, err)
			}
			registrationOnly := sessionenv.AccountRegistrationOnly(agent)
			for _, name := range names {
				dir, err := agentaccount.Dir(home, agent, name)
				if err != nil {
					return jsonWrapError(cmd, accountsJSONFlag, err)
				}
				loggedIn, err := agentaccount.LoggedIn(home, agent, name)
				if err != nil {
					return jsonWrapError(cmd, accountsJSONFlag, err)
				}
				entries = append(entries, accountEntry{
					Agent: agent, Name: name, Dir: dir,
					RegistrationOnly: registrationOnly, LoggedIn: loggedIn,
				})
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
		// The marker is a FOURTH column rather than a rewritten line, so the three
		// fields a script already reads stay where they were and only a
		// registration-only row grows one.
		noted := make(map[string]struct{}, len(entries))
		for _, entry := range entries {
			// The three fields a script already reads keep their positions, and the
			// state is APPENDED — the same rule the registration-only marker followed
			// when it was added as a fourth column (#3609).
			row := fmt.Sprintf("%s\t%s\t%s\t%s", entry.Agent, entry.Name, entry.Dir,
				accountLoginStateLabel(entry.LoggedIn))
			if entry.RegistrationOnly {
				row += "\t" + sessionenv.AccountRegistrationOnlyMarker
			}
			fmt.Fprintln(cmd.OutOrStdout(), row)
			if _, seen := noted[entry.Agent]; seen {
				continue
			}
			noted[entry.Agent] = struct{}{}
			// The why goes to stderr, once per agent: the column says WHICH rows are
			// affected and a two-word marker cannot say what to do about it, but a
			// paragraph repeated per row would bury the listing it annotates.
			if reason, ok := sessionenv.AccountRegistrationOnlyReason(entry.Agent); ok {
				fmt.Fprintf(cmd.ErrOrStderr(), "Registration only · %s\n", reason)
			}
		}
		return nil
	},
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

// accountLoginStateLabel renders an account's logged-in state for the human
// listing.
//
// "logged in" is a claim about a FILE, not about a session: af reports that the
// agent's own credential is present in the account directory, by stat. It cannot
// say the credential still works — a revoked or expired one is still a file —
// and the wording stays on the side af can actually establish.
func accountLoginStateLabel(loggedIn bool) string {
	if loggedIn {
		return "logged in"
	}
	return "not logged in"
}
