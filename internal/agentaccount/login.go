package agentaccount

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// The login half of an account (#3384). af holds both pieces already —
// loginCommands says what the agent's own sign-in invocation is, accountConfigVars
// says which variable points it at the account — and until now it only PRINTED
// them for the operator to paste. This file is what lets af RUN that invocation.
//
// The design principle does not change and is what bounds everything here: af
// never reads, stores, or forwards the credential. It sets one variable, hands
// the terminal to the agent's own flow, and afterwards asks the FILESYSTEM
// whether the agent wrote its own artifact — by stat, never by open. Anything
// that parses or persists a token belongs in the agent, not in af.

// LoginProgram is the shell command af runs to log an account in: the agent
// itself, plus the agent's own login words.
//
// It REFUSES rather than guessing. Appending "login" universally produces
// `claude login`, which is not a command at all, and a printed-but-wrong next
// step reads as a broken account rather than as af's mistake (#3057). The
// refusal names the agents that do have a flow, because "this one cannot" is
// only actionable beside "these can".
func LoginProgram(agent string) (string, error) {
	words, ok := loginCommands[agent]
	if !ok {
		return "", fmt.Errorf(
			"%w: af cannot log in to %q — af drives the agent's OWN login command and no verified one is "+
				"recorded for it, and af will not guess an invocation that may not exist (agents af can log in: %s)",
			ErrUnsupportedAgent, agent, strings.Join(LoginAgents(), ", "))
	}
	// TrimSpace, not Join alone: gemini's login words are deliberately EMPTY (its
	// sign-in is the bare CLI), and a trailing space would reach a shell command
	// string and a pasteable hint.
	return strings.TrimSpace(agent + " " + strings.Join(words, " ")), nil
}

// LoginAgents lists the agents af can drive a login for, sorted.
//
// TestLoginAgentsMatchTheAccountRoster holds this equal to accountConfigVars:
// an agent af can scope a session to but cannot log in would leave an operator
// with a registered account and no way to fill it, and an agent af offers to log
// in but cannot scope would spend a login on a directory no session can select.
func LoginAgents() []string {
	out := make([]string, 0, len(loginCommands))
	for agent := range loginCommands {
		out = append(out, agent)
	}
	sort.Strings(out)
	return out
}

// LoginSessionName is the tmux session name for one account's login pane.
//
// It is DERIVED from the account rather than a counter, which is what lets a
// second `af accounts login codex work` attach to the flow already running
// instead of starting a competing one — two concurrent logins into one directory
// is a race over the same auth.json. The agent is part of the name because
// account names live in per-agent namespaces: `codex/work` and `claude/work` are
// different accounts.
//
// The af- prefix keeps it inside the namespace every af cleanup path recognizes
// (tmux.NewTmuxSession adds the af_ prefix on top, exactly as the config agent's
// name is built).
func LoginSessionName(agent, name string) string {
	return "af-login-" + agent + "-" + name
}

// accountCredentialArtifacts is the file the AGENT writes when its login
// succeeds, relative to the ACCOUNT DIRECTORY — the evidence af reports
// logged-in state from.
//
// It mirrors session's agentCredentialFiles, which lists the same files relative
// to HOME, and TestAccountCredentialArtifactsMatchTheMountedCredentials (in
// package session, which can import this one) holds the two together in both
// directions. The mirror is unavoidable — session imports this package, so this
// package cannot import that table — but a drift between them is not: the whole
// difference is the ROOT each agent's config variable replaces, which is why the
// test derives one from the other rather than comparing two hand-written lists.
//
//   - claude: CLAUDE_CONFIG_DIR replaces ~/.claude, so ~/.claude/.credentials.json
//     becomes <account>/.credentials.json.
//   - codex: CODEX_HOME replaces ~/.codex, so ~/.codex/auth.json becomes
//     <account>/auth.json.
//   - gemini: GEMINI_CLI_HOME is a HOME-like root — the bundle shadows homedir()
//     and FileKeychain joins ".gemini" onto it (#3387) — so the credential stays
//     one level down at <account>/.gemini/.
//
// google_accounts.json is deliberately NOT here although the mount table lists
// it: it records WHICH accounts the CLI knows, not that any of them is
// authenticated, so presence of it is not evidence of a completed login.
var accountCredentialArtifacts = map[string][]string{
	"claude": {".credentials.json"},
	"codex":  {"auth.json"},
	"gemini": {
		filepath.Join(".gemini", "oauth_creds.json"),
		filepath.Join(".gemini", "gemini-credentials.json"),
	},
}

// AccountCredentialArtifacts returns the account-relative paths whose presence
// means this agent's login completed. The result is a copy: it is read by the
// drift test in another package and must not be mutable through this call.
func AccountCredentialArtifacts(agent string) []string {
	return append([]string(nil), accountCredentialArtifacts[agent]...)
}

// LoggedIn reports whether the agent's own login flow has left its credential in
// this account, by STAT alone.
//
// The verification requirement in #3384 is that a login be confirmed by the
// agent's artifact rather than by the login command's exit code: a flow the user
// abandoned at the browser step exits 0 in several of these CLIs, and a
// registered-but-empty account then fails much later, at session start, naming
// none of this.
//
// It never opens the file, and TestLoggedIn_NeverOpensTheCredential proves that
// against a mode-0000 credential. af decides which directory a session sees; the
// bytes inside are the agent's business and af has no business being able to
// read them.
//
// An EMPTY file is not a login. That is the state an interrupted flow leaves,
// and calling it success recreates exactly the dead account this exists to catch.
func LoggedIn(home, agent, name string) (bool, error) {
	dir, err := Dir(home, agent, name)
	if err != nil {
		return false, err
	}
	// The same ancestor guard every other reader runs, and for the same reason: a
	// component swapped for a symlink since registration would have af answer
	// "logged in" from a directory outside the registry (#3057 review).
	if err := refuseSymlinkedAncestor(home, dir); err != nil {
		return false, err
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return false, fmt.Errorf(
			"account %q is not registered for %s; run `af accounts add %s %s` first",
			name, agent, agent, name)
	}
	for _, artifact := range accountCredentialArtifacts[agent] {
		// Lstat, not Stat: a symlinked credential points somewhere af did not
		// create, so its presence is not evidence about THIS account.
		artifactInfo, err := os.Lstat(filepath.Join(dir, artifact))
		if err != nil {
			continue
		}
		if artifactInfo.Mode().IsRegular() && artifactInfo.Size() > 0 {
			return true, nil
		}
	}
	return false, nil
}

// codexCredentialStoreSetting is the codex config key that decides whether the
// account directory's auth.json is used at all.
const codexCredentialStoreSetting = "cli_auth_credentials_store"

// codexKeyringStore is the value that collapses every account into one identity.
const codexKeyringStore = "keyring"

// CheckLoginPreconditions refuses a login whose result the agent would ignore,
// and returns the things the operator has to be told BEFORE they use the account
// rather than after.
//
// Both come from #3384's preconditions, which are recorded from empirical
// testing in internal/sessionenv/account.go and are invisible to ApplyAccount —
// it never touches the filesystem. A login verb is the first place they become
// detectable, which is why they are enforced here.
//
// THE KEYRING COLLAPSE is the dangerous one. With cli_auth_credentials_store =
// "keyring" in the account's config.toml, Codex ignores the account's auth.json
// and uses the MACHINE-WIDE identity — so every account authenticates as the
// same person while `af accounts list` shows several, and a naive "is it logged
// in?" probe passes against the ambient user. Running a login whose result is
// discarded is worse than refusing, so this refuses and names the fix.
//
// A config.toml af cannot PARSE is a warning, not a refusal. af does not own
// that file, go-toml is not codex's parser, and refusing a login because af's
// reader was stricter than the agent's would block the operator from a flow that
// would have worked. The notice says what af could not check, which is the
// honest report.
func CheckLoginPreconditions(agent, dir string) ([]string, error) {
	var notices []string
	if agent == "codex" {
		configPath := filepath.Join(dir, "config.toml")
		store, read, err := codexCredentialStore(configPath)
		switch {
		case err != nil:
			notices = append(notices, fmt.Sprintf(
				"af could not read %s (%v), so it could not check whether this account sets %s = %q — "+
					"if it does, codex ignores this account's auth.json and uses the machine-wide identity, and "+
					"every account authenticates as the same person",
				configPath, err, codexCredentialStoreSetting, codexKeyringStore))
		case read && strings.EqualFold(store, codexKeyringStore):
			return nil, fmt.Errorf(
				"account %s sets %s = %q in config.toml: codex would ignore this account's auth.json and "+
					"authenticate with the machine-wide keyring identity, so every account would be the same "+
					"person while af reported several — set %s = \"file\" in %s (or remove the line) and run "+
					"this again",
				dir, codexCredentialStoreSetting, codexKeyringStore,
				codexCredentialStoreSetting, configPath)
		}
	}
	return append(notices, RegistrationNotices(agent, dir)...), nil
}

// RegistrationNotices explains account settings without enforcing login-only
// preconditions. Both add and login use these same messages.
func RegistrationNotices(agent, dir string) []string {
	var notices []string
	switch agent {
	case "codex":
		notices = append(notices, fmt.Sprintf(
			"CODEX_HOME relocates codex's WHOLE home, not just its credentials: this account starts with no "+
				"history and no skills of its own (they live under %s). That is a fresh identity, "+
				"not a broken codex.", dir))
		notices = append(notices, codexSettingsNotice(dir))
	case "claude":
		notices = append(notices, fmt.Sprintf(
			"CLAUDE_CONFIG_DIR relocates claude's whole config root: this account starts with no history and "+
				"no settings of its own (they live under %s). That is a fresh identity, not a broken claude.", dir))
	case "gemini":
		notices = append(notices, fmt.Sprintf(
			"GEMINI_CLI_HOME is a HOME-like root, so gemini keeps this account's credentials one level down at "+
				"%s. Point the variable at the account directory itself, never at the .gemini path inside it.",
			filepath.Join(dir, ".gemini")))
		// The one place an operator is told that af writes into the agent's own
		// settings, and the only surface where that is not a surprise: it is
		// printed at the login this account was registered for. Both files are
		// named so it can be checked rather than believed (#3858).
		notices = append(notices, fmt.Sprintf(
			"af has recorded this account's answers to gemini's two start-up questions in %s and %s — the "+
				"Google sign-in as the auth type · the account directory as trusted — so the pane opens on the "+
				"authorization code rather than on two questions about af's own directory. Neither is a "+
				"credential, and an answer already in those files is left alone.",
			filepath.Join(dir, ".gemini", geminiSettingsFile),
			filepath.Join(dir, ".gemini", geminiTrustedFoldersFile)))
	}
	return notices
}

// codexCredentialStore reads ONE setting out of an account's codex config.
//
// It reports (value, found, error). A missing file is the ordinary case for a
// freshly registered account and is neither found nor an error.
//
// This reads a SETTINGS file, never a credential: config.toml is codex's own
// configuration, the account's auth.json is what af must not open, and the two
// are different files. The struct decodes exactly one key so nothing else in
// that file — an operator's model choice, their MCP servers — is even
// materialized, let alone retained.
func codexCredentialStore(path string) (string, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	var settings struct {
		Store string `toml:"cli_auth_credentials_store"`
	}
	if err := toml.Unmarshal(data, &settings); err != nil {
		return "", false, err
	}
	return settings.Store, settings.Store != "", nil
}
