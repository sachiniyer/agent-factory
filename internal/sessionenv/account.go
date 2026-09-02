package sessionenv

import (
	"fmt"
	"sort"
	"strings"
)

// Account is a per-session credential scope: exactly one of an agent's logged-in
// identities, held as a directory the agent CLI treats as its home.
//
// af never reads, stores, or forwards the secret itself. It decides which
// directory a session can see, and the agent's own login flow put the material
// there. A provider changing its token format costs this nothing.
type Account struct {
	Agent string
	Name  string
	// Dir is the AGENT HOME handed to the session, not merely a credential file.
	//
	// This distinction is a precondition on whoever creates the directory, and
	// getting it wrong is a silent functional regression rather than a security
	// one. CODEX_HOME relocates codex's WHOLE home — auth, history, settings, and
	// skills discovered under $CODEX_HOME/skills, as session/agentskill.go already
	// documents. So an account directory holding only auth.json gives that session
	// no history, no user skills, and none of af's managed guidance skill.
	// CLAUDE_CONFIG_DIR behaves the same way for claude's config root.
	//
	// Provisioning must therefore SEED the non-credential state — share or copy it
	// from the operator's real home — so that an account differs from the default
	// identity in credentials alone.
	//
	// With one exclusion that is easy to miss and defeats the whole feature: a
	// copied config.toml carrying `cli_auth_credentials_store = "keyring"` makes
	// Codex ignore the account's auth.json and use the MACHINE-WIDE identity, so
	// every account authenticates as the same person while ApplyAccount reports
	// success. Seeding must sanitize credential-source settings to the file-based
	// mode rather than copying settings unchanged. The command guard cannot catch
	// this — the override arrives through config, not through argv (#2983 review). ApplyAccount cannot verify that: it never
	// touches the filesystem and receives a path that may not exist yet. It is
	// stated here because the account-creation slice is the only place it can be
	// enforced, and a reader of this type would otherwise reasonably assume
	// "credential directory" meant a directory of credentials (#2983 review).
	//
	// It must also be a real path outside any temp directory: codex refuses to
	// operate under /tmp, and an account is durable state regardless.
	Dir string
	// TrustedWrapper is the exact af binary path the LAUNCHER generated this
	// session's handoff with, or empty when the program is not an af handoff.
	//
	// This is provenance, supplied rather than parsed. The docker and ssh
	// backends generate `/usr/local/bin/af agent-server …` and a staged absolute
	// path respectively, so a bare-name rule rejects af's OWN launch and refuses
	// every account-scoped session on those backends. No amount of inspecting the
	// string recovers "af wrote this"; the caller knows it, so it says so.
	//
	// Only an EXACT match is honoured — never a basename — so a repository file
	// that merely shares the name is still refused (#2983 review).
	TrustedWrapper string
	// TrustedExecutable is the exact agent executable af selected for this
	// launch, or empty when PATH must resolve the bare agent name.
	//
	// A path-qualified executable is otherwise unprovable: ./claude may be a
	// repository file that receives the selected credential root. The one safe
	// exception is an exact path from af's built-in auto-detected program
	// override. Like TrustedWrapper, this is provenance supplied by the launcher,
	// never inferred from a basename.
	TrustedExecutable string
	// GeneratedArgs are the argument words af authored for this session's
	// program, in order, unquoted. Usually those are launch-time additions; for
	// af's built-in detected Claude command they also include the detected
	// override's built-in arguments (#3108).
	//
	// This is the same shape of claim as TrustedWrapper, for the same reason. The
	// local launch rewrites a bare `claude` into `claude --session-id <uuid>
	// --plugin-dir <dir>` before the pane shim sees it, so the guard's no-arguments
	// rule refused af's OWN output and the pane exited 127 (#3083). No amount of
	// inspecting the string recovers "af wrote these"; the launcher knows, so it
	// says so.
	//
	// It is deliberately NOT a list of permitted flags. The guard exists because
	// enumerating unsafe forms failed across four grammars, and its rule is to prove
	// good shapes rather than hunt bad ones — so an allowlist of `--session-id` and
	// `--plugin-dir` would rebuild that mistake one layer in, and the next generated
	// flag would reopen it. These are VALUES the launcher produced on this launch:
	// the uuid and the directory are compared whole and positionally, so nothing a
	// repository can write into program_overrides matches them, and a flag af did
	// not generate is still refused however harmless it looks.
	//
	// Empty means af authored no arguments, which is every non-claude agent today
	// and the only behaviour before this field existed.
	GeneratedArgs []string
}

// accountConfigVars maps an agent to the variable that relocates its credential
// ROOT — verified empirically per agent, never assumed from the allowlist.
//
// claude 2.1.223: CLAUDE_CONFIG_DIR pointed at an empty directory reports
// `"loggedIn": false` while the real one reports true, so it moves credential
// lookup and not merely settings.
//
// codex-cli 0.146.1: CODEX_HOME pointed at an empty directory reports "Not
// logged in" against "Logged in using ChatGPT".
//
// gemini 0.51.0: GEMINI_CLI_HOME pointed at an empty directory reports "Please
// set an Auth method in your <GEMINI_CLI_HOME>/.gemini/settings.json…" while the
// real home holds gemini-credentials.json, and every .gemini path it opens moves
// with the variable (#3387, verified under strace). The bundle earns it by
// SHADOWING homedir(): `function homedir(){ return process.env["GEMINI_CLI_HOME"]
// || os.homedir() }`, and FileKeychain joins that with ".gemini" — so the
// variable is a HOME-like root and the credentials land at <dir>/.gemini/, not
// directly in <dir> the way CLAUDE_CONFIG_DIR's do.
//
// amp, aider, opencode and devin are deliberately ABSENT, and #3387 replaced
// "not tested" with a measured reason for each:
//
//   - amp: credentials are $XDG_DATA_HOME/amp/secrets.json. AMP_HOME is the
//     INSTALL dir, not a config root — the allowlist's presence of it proved
//     nothing — and AMP_SETTINGS_FILE relocates only settings.json, which holds
//     no credential.
//   - opencode: OPENCODE_CONFIG and OPENCODE_CONFIG_DIR move config only;
//     `opencode auth list` keeps reading $HOME/.local/share/opencode/auth.json.
//   - devin: no DEVIN_* variable is referenced by the binary at all.
//   - aider: no credential store to relocate; identity arrives per-invocation as
//     API keys (flags, AIDER_*/provider env, or ~/.aider.conf.yml).
//
// Their only lever is a GENERIC XDG/HOME variable, which cannot express "this
// agent's account" without relocating every other tool's state too. An agent
// missing here reports unsupported rather than silently accepting an account
// selection that would do nothing (#2983).
var accountConfigVars = map[string]string{
	"claude": "CLAUDE_CONFIG_DIR",
	"codex":  "CODEX_HOME",
	"gemini": "GEMINI_CLI_HOME",
}

// accountCredentialNames are the variables that carry an IDENTITY for an agent
// and must therefore be removed from an account-scoped session.
//
// This is the subtraction half, and it is not defence in depth — it is what
// makes the selection real. For both of these CLIs an ambient API key takes
// precedence over the config directory's OAuth state, so a session that selected
// an account while ANTHROPIC_API_KEY passed through would authenticate as
// whoever that key belongs to, silently, while every visible signal said
// otherwise. That is the exact failure #2983 exists to fix.
//
// Routing and mode variables (ANTHROPIC_BASE_URL, the Bedrock/Vertex selectors,
// CODEX_CA_CERTIFICATE) are NOT here on purpose: they are the operator's
// deployment configuration rather than an identity, and dropping them would
// break a legitimate proxied setup without scoping anything. accountScopedNames
// enforces that every name is classified one way or the other, so this
// distinction cannot rot as the allowlists grow.
var accountCredentialNames = map[string]map[string]struct{}{
	"claude": nameSet(
		"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_OAUTH_TOKEN",
	),
	"codex": nameSet(
		"OPENAI_API_KEY", "CODEX_API_KEY", "CODEX_ACCESS_TOKEN",
	),
	// All three are read by the CLI as an identity that outranks the config
	// directory, so all three must be subtracted (#3387).
	//
	// GOOGLE_APPLICATION_CREDENTIALS was left OUT of this set when the roster
	// entry landed, on the reasoning that it is not on gemini's unconditional
	// allowlist — it is admitted only behind a cloud-mode selector (#2462), and an
	// active selector makes ApplyAccount refuse rather than scope. Measurement
	// falsified that: the allowlist governs what af PASSES, and gemini has a second
	// environment source af does not filter. With the account selecting
	// oauth-personal and a repository `.env` naming GOOGLE_APPLICATION_CREDENTIALS
	// and nothing else, gemini 0.51.0 signed a JWT with that service-account key —
	// no selector anywhere. So it is an identity for this agent whatever admitted
	// it, and accountLateEnvironmentNames is what keeps a late source from putting
	// it back (#3609 review).
	"gemini": nameSet(
		"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS",
	),
}

// accountLateEnvironmentNames are the identity names af must leave DEFINED AND
// EMPTY rather than merely remove, because the agent's own CLI reads a
// REPOSITORY-CONTROLLED environment file after af's boundary has installed the
// session environment.
//
// Subtraction is the whole point of ApplyAccount, and for claude and codex it is
// enough: nothing else writes to the process environment between the boundary and
// the agent. gemini is different. It walks up from the workspace looking for
// `<dir>/.gemini/.env` and `<dir>/.env` at every level, falling back to the same
// two under its home, and applies what it finds — so a checked-in `.env` recreates
// exactly the names this package just removed, and the session then authenticates
// as the repository while every visible signal reports the selected account.
//
// Measured against gemini 0.51.0 under a throwaway HOME (#3609 review). With a
// repository `.env` naming GEMINI_API_KEY and GOOGLE_API_KEY and neither name in
// the process environment, the CLI announced "Both GOOGLE_API_KEY and
// GEMINI_API_KEY are set. Using GOOGLE_API_KEY." and sent a real request that came
// back `400 API key not valid` — the project's key, used. With both names present
// but EMPTY, the same run was byte-identical to a run in a directory with no
// `.env` at all: the CLI reported no auth method. That is the loader's rule —
// it assigns only when the name is not already an own property of the environment,
// and an empty string is one.
//
// The alternatives were measured and rejected rather than assumed:
//
//   - `--ignore-env` is read straight from argv by the loader but is not a
//     declared flag, so yargs exits first with "Unknown arguments: ignore-env".
//   - `advanced.ignoreLocalEnv` in the account's settings blocks `<dir>/.env` but
//     NOT `<dir>/.gemini/.env`, and a workspace `.gemini/settings.json` setting it
//     back to false wins — the repository being defended against writes the
//     setting.
//   - `advanced.excludedEnvVars` has the same `.gemini/.env` blind spot, by
//     construction: the loader skips the exclusion list for that path.
//
// Only the empty value covers every location, and it cannot be overridden by the
// repository at all. It also does not divert the account: with an account whose
// settings select oauth-personal, empty values produced exactly the behaviour of
// the names being absent.
//
// The two cloud-mode selectors are in the set for the same measured reason. A
// `.env` setting GOOGLE_GENAI_USE_VERTEXAI=true moved the session onto Vertex and
// the project named beside it; pinned empty, it did not. An operator's selector is
// still answered by the refusal above rather than by this, because that one is in
// the process environment where ApplyAccount can see it — this only stops a
// repository from turning a cloud mode on after the fact.
//
// An empty value is what the docker backend has always installed (`-e NAME=`),
// so this brings the local shim's boundary into line with the container's rather
// than inventing a shape.
var accountLateEnvironmentNames = map[string]map[string]struct{}{
	"gemini": nameSet(
		"GEMINI_API_KEY", "GOOGLE_API_KEY", "GOOGLE_APPLICATION_CREDENTIALS",
		"GOOGLE_GENAI_USE_VERTEXAI", "GOOGLE_GENAI_USE_GCA",
	),
}

// accountLaunchProvenAgents answers a DIFFERENT question from accountConfigVars,
// and the two must not be collapsed.
//
// accountConfigVars answers "does this agent have a credential-root variable af
// can point at an account directory" — measured per agent, and true for gemini.
// This answers "has anyone verified that the account boundary can PROVE how af
// launches this agent", which is about af's own command rewriting, not the
// agent's environment. claude is here because the launcher declares its
// `--session-id`/`--plugin-dir` rewrite as GeneratedArgs, so the rewritten command
// is provable rather than refused (#3083); codex is here because af leaves its
// command unmodified.
//
// Keeping them separate is what makes a newly rostered agent fail CLOSED at the
// launch boundary until someone checks the second question (#3051, #3083). What
// changed in #3609 is only where the list lives: session's launch gate used to
// keep its own copy, so `af accounts add gemini work` could succeed while
// `--account work` answered "supported: claude, codex" — a statement the roster
// had just made false. One list, read by every surface, cannot say two things.
var accountLaunchProvenAgents = nameSet("claude", "codex")

// accountLaunchProofIssueURL is the follow-up that removes the registration-only
// state for gemini. It is a URL rather than a bare issue number because these
// messages reach operators outside this repository, for whom "#3639" resolves to
// nothing.
const accountLaunchProofIssueURL = "https://github.com/sachiniyer/agent-factory/issues/3639"

// AccountLaunchProven reports whether af has established that the account
// boundary can verify how this agent launches. An agent that supports accounts
// but is not launch-proven can be registered and logged in; a session cannot be
// scoped to it yet.
func AccountLaunchProven(agent string) bool {
	_, ok := accountLaunchProvenAgents[agent]
	return ok
}

// AccountRegistrationOnly reports an agent that is on the account roster but
// whose sessions cannot yet be scoped to an account.
func AccountRegistrationOnly(agent string) bool {
	_, supported := SupportsAccounts(agent)
	return supported && !AccountLaunchProven(agent)
}

// AccountRegistrationOnlyMarker is the short form, for a listing column.
const AccountRegistrationOnlyMarker = "registration only"

// AccountRegistrationOnlyReason is the ONE sentence every surface says about a
// registration-only agent — `af accounts add`, `af accounts list` and the launch
// refusal all embed this clause, so none of them can describe the state
// differently from the others. It carries no leading capital, so a caller can
// frame it, and it ENDS IN A URL, so a caller must not append punctuation: a
// period fused to the link is one a terminal hands over as part of the address.
//
// The second return is false for an agent that is not registration-only, so a
// caller cannot print the notice for claude or codex by forgetting to check.
func AccountRegistrationOnlyReason(agent string) (string, bool) {
	if !AccountRegistrationOnly(agent) {
		return "", false
	}
	return fmt.Sprintf(
		"a session cannot be scoped to a %s account yet — af has not verified that the account boundary can "+
			"prove how it launches %s, so --account refuses rather than risk starting the session on the "+
			"ambient identity while reporting the account you asked for. Registering and logging in work "+
			"today, and the launch proof is tracked at %s",
		agent, agent, accountLaunchProofIssueURL), true
}

// SupportsAccounts reports whether an agent can be account-scoped, and the
// variable used to do it.
func SupportsAccounts(agent string) (string, bool) {
	v, ok := accountConfigVars[agent]
	return v, ok
}

// AccountAgents lists the agents that support account scoping, sorted. These are
// bare names, for a caller that iterates them; a caller writing a SENTENCE wants
// AccountAgentsSummary, which marks the ones a session cannot be scoped to yet.
func AccountAgents() []string {
	out := make([]string, 0, len(accountConfigVars))
	for agent := range accountConfigVars {
		out = append(out, agent)
	}
	sort.Strings(out)
	return out
}

// AccountAgentsSummary names the account-scoped agents for a message, marking
// each registration-only one.
//
// Every "supported: …" list a user reads comes from here. A bare join said
// "supported: claude, codex, gemini" while `--account` refused gemini outright,
// which is the same contradiction from the other side (#3609 review).
func AccountAgentsSummary() string {
	agents := AccountAgents()
	for idx, agent := range agents {
		if AccountRegistrationOnly(agent) {
			agents[idx] = agent + " (" + AccountRegistrationOnlyMarker + ")"
		}
	}
	return strings.Join(agents, ", ")
}

// AccountIdentityNames lists every environment variable whose value can select
// an identity instead of the named account: the agent's credential variables,
// its credential-root variable, and anything its own late environment source
// could use to reintroduce one. Provisioners use the same classification as
// ApplyAccount so a remote/container launch cannot drift from the local shim's
// account boundary.
func AccountIdentityNames(agent string) []string {
	configVar, ok := SupportsAccounts(agent)
	if !ok {
		return nil
	}
	denied := accountScopedNames(agent, configVar)
	for name := range accountLateEnvironmentNames[agent] {
		denied[name] = struct{}{}
	}
	out := make([]string, 0, len(denied))
	for name := range denied {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// accountPinnedEmptyNames is the sorted set ApplyAccount installs as empty
// values, never the config variable itself — that one is injected with the
// account directory, and an empty copy of it would erase the selection this
// function exists to make.
func accountPinnedEmptyNames(agent, configVar string) []string {
	pinned := accountLateEnvironmentNames[agent]
	out := make([]string, 0, len(pinned))
	for name := range pinned {
		if name == configVar {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// ApplyAccount scopes an already-filtered session environment to exactly one
// account: it INJECTS that account's credential root and REMOVES every other
// identity-bearing variable for the agent.
//
// Both halves are required and neither is sufficient.
//
// Injection alone leaves an ambient API key in place, which wins over the
// directory — the selection is then silently ignored. Subtraction alone leaves
// the session with no identity at all.
//
// It must also be an INJECTION and not an allowlist entry. Filter is subtractive
// over the daemon's own environment, so a variable delivered through the
// allowlist carries the daemon's single value to every session: correct for
// AF_DAEMON_TOKEN, which is global by nature, and wrong for an account, which is
// per-session by definition. An allowlist implementation passes a presence check
// and a smoke test while giving every session the same account (#2983).
func ApplyAccount(env []string, command string, account Account) ([]string, error) {
	return applyAccount(env, command, account, true)
}

// ApplyAccountEnvironment scopes a sibling process to account without requiring
// that process to be the agent itself. It still rejects a command that directly
// replaces the selected identity after the environment is installed.
func ApplyAccountEnvironment(env []string, command string, account Account) ([]string, error) {
	if err := ValidateAccountEnvironmentCommand(command, account); err != nil {
		return nil, err
	}
	scoped, err := applyAccount(env, command, account, false)
	if err != nil {
		return nil, err
	}
	// Every environment-only command is launched by the exec shim through
	// /bin/sh -c. Strip shell startup code before that outer shell runs, not only
	// when the user's command is itself an interactive shell.
	scoped = stripAccountShellStartupEnvironment(scoped)
	return scoped, nil
}

func applyAccount(env []string, command string, account Account, validateCommand bool) ([]string, error) {
	configVar, ok := SupportsAccounts(account.Agent)
	if !ok {
		return nil, fmt.Errorf(
			"agent %q does not support multiple accounts; supported: %s",
			account.Agent, AccountAgentsSummary())
	}
	if strings.TrimSpace(account.Dir) == "" {
		return nil, fmt.Errorf("account %q for agent %q has no credential directory",
			account.Name, account.Agent)
	}

	// A CLOUD MODE authenticates somewhere else entirely. Bedrock, Vertex and
	// Foundry make the CLI use AWS/Google/Azure credentials, which
	// FilterForCommand deliberately admits for exactly those modes — so the
	// account directory stops being the session's identity while still appearing
	// to be. Refusing is the honest answer: removing the selector instead would
	// silently move the session off the deployment mode its operator configured,
	// which is a different surprise rather than a smaller one (#2983 review).
	if selectors := ResolveAuthSelectors(env, account.Agent, command); len(selectors) > 0 {
		return nil, fmt.Errorf(
			"account %q cannot scope agent %q while cloud mode %s is active: the session authenticates "+
				"through that provider's credentials, not through the account directory; unset it to use accounts",
			account.Name, account.Agent, strings.Join(selectors, ", "))
	}

	// A COMMAND-LOCAL ASSIGNMENT wins over anything injected here: the launch runs
	// the program through `/bin/sh -c`, which applies `CODEX_HOME=... codex` after
	// this environment is installed. program_overrides is reachable from a
	// repository's checked-in config, so without this a repo could silently
	// redirect whose quota a session spends. Unprovable commands fail closed —
	// an unparsed command is not evidence of safety.
	// EVERY identity-bearing name, not just the config root. Subtraction removed
	// the ambient copies, but `/bin/sh -c` applies a command-local assignment
	// afterwards — so `OPENAI_API_KEY=sk-other codex` RECREATES an identity that
	// outranks the account directory, and the guard would have called it safe
	// because it only watched CODEX_HOME.
	//
	// The guarded cloud selectors are in the set too. A selector assignment whose
	// value the parser cannot evaluate (`CLAUDE_CODE_USE_BEDROCK=$HOME claude`)
	// reads as DISABLED to ResolveAuthSelectors, so the cloud-mode refusal above
	// never fires while the shell expands it to a non-empty string and Claude
	// authenticates through ~/.aws instead. Refusing any command-local assignment
	// to a selector removes the need to evaluate it (#2983 review).
	if validateCommand {
		if err := ValidateAccountCommand(command, account); err != nil {
			return nil, err
		}
	}

	denied := accountScopedNames(account.Agent, configVar)
	pinned := accountPinnedEmptyNames(account.Agent, configVar)
	for _, name := range pinned {
		denied[name] = struct{}{}
	}
	out := make([]string, 0, len(env)+len(pinned)+1)
	for _, kv := range env {
		name, _, found := strings.Cut(kv, "=")
		if !found {
			continue
		}
		if _, drop := denied[name]; drop {
			continue
		}
		out = append(out, kv)
	}
	// DEFINED AND EMPTY, not absent. The agent's own late environment source only
	// assigns a name it does not already find, so an empty value is what holds a
	// repository `.env` off the identity this session was scoped to — removal
	// alone leaves the name free for that file to claim. Nothing here is a
	// credential: every pinned name is one accountLateEnvironmentNames measured as
	// identity-bearing for this agent, and the account directory injected below is
	// what the session actually authenticates with.
	for _, name := range pinned {
		out = append(out, name+"=")
	}
	// Appended last so it wins over any ambient copy that survived, though the
	// removal above means there should not be one.
	out = append(out, configVar+"="+account.Dir)
	return out, nil
}

// accountScopedNames is every variable removed from an account-scoped session:
// the agent's credential names, plus the config variable itself so an ambient
// value cannot shadow the injected one.
//
// It reads from accountCredentialNames rather than from agentNames on purpose. A
// blanket "drop everything for this agent" would also strip the operator's
// routing and mode configuration, breaking a proxied or Bedrock deployment to
// scope an identity. The cost of the explicit list is that a newly allowlisted
// credential could be forgotten, which is why TestAccountCredentialsAreClassified
// fails when an agentNames entry for a scoped agent is neither classified as a
// credential nor listed as deliberately kept.
func accountScopedNames(agent, configVar string) map[string]struct{} {
	denied := make(map[string]struct{}, len(accountCredentialNames[agent])+1)
	for name := range accountCredentialNames[agent] {
		denied[name] = struct{}{}
	}
	denied[configVar] = struct{}{}
	return denied
}

// accountNonCredentialNames records the allowlisted names for a scoped agent
// that are deliberately NOT identity-bearing, so the classification test can
// tell "reviewed and kept" from "forgotten".
var accountNonCredentialNames = map[string]map[string]struct{}{
	"claude": nameSet(
		"ANTHROPIC_BASE_URL", "CLAUDE_CONFIG_DIR",
		"CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY",
	),
	"codex": nameSet(
		"CODEX_HOME", "CODEX_SQLITE_HOME", "CODEX_CA_CERTIFICATE",
	),
	// The config var itself, plus the two cloud-mode selectors — the operator's
	// deployment signal, handled by the cloud-mode refusal above rather than by
	// subtraction, exactly as Claude's Bedrock/Vertex/Foundry selectors are.
	//
	// "Kept" is about this classification only. An operator's selector, which is
	// the one ApplyAccount can see, still produces the refusal — but the selectors
	// are also in accountLateEnvironmentNames, because a repository `.env` can turn
	// a cloud mode on AFTER that refusal has had its look, and a session that
	// silently moved onto Vertex is not the operator's deployment choice being
	// respected (#3609 review).
	"gemini": nameSet(
		"GEMINI_CLI_HOME", "GOOGLE_GENAI_USE_VERTEXAI", "GOOGLE_GENAI_USE_GCA",
	),
}
