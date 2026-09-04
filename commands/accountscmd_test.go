package commands

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/internal/sessionenv"
	"github.com/sachiniyer/agent-factory/session"
)

// `af accounts` had no test file at all while its --json failure path was
// rewritten three times by review (#3057). Each rewrite hand-modelled one more
// piece of pflag's behaviour — first occurrence, then boolean spellings, then
// repeats — and each shipped on a manual check against the built binary, because
// the logic read os.Args and so could not be reached from an in-process test at
// all. These drive the real cobra command instead, pinning the envelope contract
// to what a caller observes on stdout and stderr.

// accountsEnvelope is the shared {data,error} envelope, decoded loosely: these
// tests care whether the output IS the envelope, not about the payload's shape.
type accountsEnvelope struct {
	Data  json.RawMessage `json:"data"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Hook launch_cmd may use shared storage or run on the daemon host, so the
// shared account help can state only the missing write-back guarantee — never
// a location or a mechanism.
func TestAccountsHelpDoesNotAssertRefusingBackendLocation(t *testing.T) {
	if !strings.Contains(accountsCmd.Long, session.AccountWriteBackRationale) {
		t.Fatalf("account help drifted from the shared runtime rationale:\n%s", accountsCmd.Long)
	}
	if strings.Contains(accountsCmd.Long, "machine it does not own") {
		t.Fatalf("account help asserts where hook runs:\n%s", accountsCmd.Long)
	}
	if strings.Contains(accountsCmd.Long, "only a mount") {
		t.Fatalf("account help asserts which mechanism could provide write-back:\n%s", accountsCmd.Long)
	}
}

// accountsTestDaemonURL is the remote-daemon target runAccountsHere pins for the
// duration of one case. Empty — the default — means "this host", which is what
// every case except the remote-refusal ones wants. Set it with
// withAccountsRemoteDaemon.
var accountsTestDaemonURL string

// withAccountsRemoteDaemon makes the accounts commands see a remote daemon
// target for one case, and puts it back afterwards.
func withAccountsRemoteDaemon(t *testing.T, url string) {
	t.Helper()
	prev := accountsTestDaemonURL
	accountsTestDaemonURL = url
	t.Cleanup(func() { accountsTestDaemonURL = prev })
}

// runAccounts drives `af accounts …` through the real root command and returns
// what the caller would see on each stream.
//
// It resets the shared flag state rather than constructing a fresh command,
// because the thing under test is the package-level wiring: accountsJSONFlag is
// bound by BOTH subcommands, and jsonWrapError latches SilenceUsage/SilenceErrors
// onto the command and its root. A hermetic copy of the command would not
// reproduce either.
func runAccounts(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	tempAFHome(t)
	return runAccountsHere(t, args...)
}

// runAccountsInHome runs against a home the CALLER owns, so one case can register
// an account and then list it. runAccounts makes a fresh home per invocation,
// which is right for the failure-envelope cases and useless for a round trip.
func runAccountsInHome(t *testing.T, home string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", home)
	leaveAmbientRepo(t)
	return runAccountsHere(t, args...)
}

func runAccountsHere(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	// --daemon-url / AF_DAEMON_URL makes every subcommand refuse (they act on the
	// LOCAL home), so an ambient value on the developer's box would turn every
	// case below into the remote-target refusal. It is PINNED rather than merely
	// left alone, in both spellings, because either one is enough to trip it.
	//
	// accountsTestDaemonURL is how a case that wants the refusal asks for it:
	// setting the environment before calling in cannot work, since this pins it.
	t.Setenv("AF_DAEMON_URL", accountsTestDaemonURL)
	prevFlagURL := apiclient.FlagDaemonURL
	apiclient.FlagDaemonURL = accountsTestDaemonURL

	prevJSON := accountsJSONFlag
	accountsJSONFlag = false
	resetAccountsJSONFlags()

	// os.Args is set to the argv a real shell would hand `af`, because the
	// behaviour these cases pin used to be decided by scanning it. Leaving it as
	// the `go test` binary's own argv would make every case pass for the wrong
	// reason — the scanner would simply find no --json to read. It is set here so
	// the same test is faithful on both sides of the change; that the answers no
	// longer depend on it is the point.
	prevArgs := os.Args
	os.Args = append([]string{"af", "accounts"}, args...)

	var out, errBuf bytes.Buffer
	rootCmd.SetOut(&out)
	rootCmd.SetErr(&errBuf)
	rootCmd.SetArgs(append([]string{"accounts"}, args...))
	t.Cleanup(func() {
		accountsJSONFlag = prevJSON
		apiclient.FlagDaemonURL = prevFlagURL
		os.Args = prevArgs
		resetAccountsJSONFlags()
		rootCmd.SetArgs(nil)
		rootCmd.SetOut(os.Stdout)
		rootCmd.SetErr(os.Stderr)
		resetCobraSilence(rootCmd)
	})

	err = rootCmd.Execute()
	return out.String(), errBuf.String(), err
}

// resetAccountsJSONFlags puts the bound --json flag back to its default on both
// subcommands. cobra keeps flag values and Changed across Execute calls, so
// without this a case that passes --json would leak into the next one.
func resetAccountsJSONFlags() {
	for _, cmd := range accountsCmd.Commands() {
		// Every bound boolean, not just --json: `login` also binds --no-attach, and
		// cobra keeps flag values and Changed across Execute calls, so a leaked
		// --no-attach would silently decide a later case's branch.
		for _, name := range []string{"json", "no-attach"} {
			if flag := cmd.Flags().Lookup(name); flag != nil {
				_ = flag.Value.Set("false")
				flag.Changed = false
			}
		}
	}
}

// requireEnvelope asserts the stream is exactly the shared envelope — no cobra
// `Error:` line, no usage block — which is the whole point of --json for an
// automation caller.
func requireEnvelope(t *testing.T, stream, label string) accountsEnvelope {
	t.Helper()
	var env accountsEnvelope
	if err := json.Unmarshal([]byte(stream), &env); err != nil {
		t.Fatalf("%s is not a parseable {data,error} envelope: %v\ngot: %q", label, err, stream)
	}
	return env
}

// requireHumanText asserts the opposite: the caller did NOT get an envelope.
func requireHumanText(t *testing.T, stream, label string) {
	t.Helper()
	var env accountsEnvelope
	if err := json.Unmarshal([]byte(stream), &env); err == nil && env.Error != nil {
		t.Fatalf("%s is a JSON envelope, want human text: %q", label, stream)
	}
	if !strings.Contains(stream, "Error:") {
		t.Fatalf("%s carries neither an envelope nor cobra's human error: %q", label, stream)
	}
}

// TestAccountsJSONEnvelopeOnFlagParseFailure is the motivating case for the
// whole --json failure path: an automation caller asks for JSON and mistypes a
// DIFFERENT flag. The failure it is most likely to hit must still be parseable.
func TestAccountsJSONEnvelopeOnFlagParseFailure(t *testing.T) {
	stdout, stderr, err := runAccounts(t, "add", "codex", "work", "--json", "--bogus")
	if err == nil {
		t.Fatal("expected an error for the unknown flag")
	}
	if stdout != "" {
		t.Fatalf("failure path wrote stdout: %q", stdout)
	}
	env := requireEnvelope(t, stderr, "stderr")
	if env.Error == nil || !strings.Contains(env.Error.Message, "--bogus") {
		t.Fatalf("envelope does not name the offending flag: %q", stderr)
	}
}

// TestAccountsJSONEnvelopeSurvivesAJSONFalseAfterTheFailure is the #3057 review
// repro, and it FAILS against the hand-rolled argv scanner this change deletes.
//
// pflag stops at `--bogus`, so it never reads the `--json=false` that follows —
// but the scanner walked the WHOLE of os.Args and let that unreached occurrence
// win, reporting human text to a caller whose --json pflag had already accepted.
// The bound variable does not have this bug because it only records what pflag
// actually parsed.
func TestAccountsJSONEnvelopeSurvivesAJSONFalseAfterTheFailure(t *testing.T) {
	_, stderr, err := runAccounts(t, "add", "codex", "work", "--json", "--bogus", "--json=false")
	if err == nil {
		t.Fatal("expected an error for the unknown flag")
	}
	env := requireEnvelope(t, stderr, "stderr")
	if env.Error == nil || !strings.Contains(env.Error.Message, "--bogus") {
		t.Fatalf("envelope does not name the offending flag: %q", stderr)
	}
}

// TestAccountsJSONNotRequestedWhenPflagNeverAcceptedIt pins the two deliberate
// narrowings that fall out of trusting the bound variable. In both, pflag never
// accepted the --json, so the command was never in JSON mode and human text is
// the honest answer.
func TestAccountsJSONNotRequestedWhenPflagNeverAcceptedIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
	}{
		// The parse fails BEFORE reaching --json, so pflag never set it.
		{"json after the failure", []string{"add", "codex", "work", "--bogus", "--json"}},
		// pflag REJECTS the value, so --json is not on either.
		{"value pflag rejects", []string{"add", "codex", "work", "--json=zzz"}},
		// The `accounts` group binds no --json at all, so the parse fails ON it.
		// Human text is also the more useful answer here: the usage block lists
		// the subcommands, which is what the caller actually needed.
		{"group command binds no json flag", []string{"--json"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, stderr, err := runAccounts(t, tc.args...)
			if err == nil {
				t.Fatal("expected a flag-parse error")
			}
			requireHumanText(t, stderr, "stderr")
		})
	}
}

// TestAccountsDashTerminatorIsNotAJSONRequest guards the operand case: after
// `--`, a bare `--json` is an ARGUMENT, and pflag treats it as one. It must not
// flip the command into JSON mode.
func TestAccountsDashTerminatorIsNotAJSONRequest(t *testing.T) {
	_, stderr, err := runAccounts(t, "add", "a", "b", "--", "--json")
	if err == nil {
		t.Fatal("expected the argument-count error: `--json` after `--` is a third operand")
	}
	requireHumanText(t, stderr, "stderr")
	if !strings.Contains(stderr, "exactly two arguments") {
		t.Fatalf("stderr is not the argument-count error: %q", stderr)
	}
}

// TestAccountsArgCountErrorIsEnveloped covers the other half of why `add` uses
// ArbitraryArgs: a cobra Args validator runs before RunE and would emit usage
// text, unparseable to the --json caller.
func TestAccountsArgCountErrorIsEnveloped(t *testing.T) {
	stdout, stderr, err := runAccounts(t, "add", "codex", "--json")
	if err == nil {
		t.Fatal("expected the argument-count error")
	}
	if stdout != "" {
		t.Fatalf("failure path wrote stdout: %q", stdout)
	}
	env := requireEnvelope(t, stderr, "stderr")
	if env.Error == nil || !strings.Contains(env.Error.Message, "exactly two arguments") {
		t.Fatalf("envelope is not the argument-count error: %q", stderr)
	}
}

// TestAccountsListFlagParseFailureIsEnveloped confirms the routing is on the
// list subcommand too, not just on add.
func TestAccountsListFlagParseFailureIsEnveloped(t *testing.T) {
	_, stderr, err := runAccounts(t, "list", "--json", "--bogus")
	if err == nil {
		t.Fatal("expected an error for the unknown flag")
	}
	env := requireEnvelope(t, stderr, "stderr")
	if env.Error == nil || !strings.Contains(env.Error.Message, "--bogus") {
		t.Fatalf("envelope does not name the offending flag: %q", stderr)
	}
}

// TestAccountsAddJSONSuccessEnvelope is the success side, so the failure cases
// above are not the only thing holding the --json contract.
func TestAccountsAddJSONSuccessEnvelope(t *testing.T) {
	stdout, _, err := runAccounts(t, "add", "codex", "work", "--json")
	if err != nil {
		t.Fatalf("add failed: %v", err)
	}
	env := requireEnvelope(t, stdout, "stdout")
	if env.Error != nil {
		t.Fatalf("success envelope carries an error: %q", stdout)
	}
	var entry accountEntry
	if err := json.Unmarshal(env.Data, &entry); err != nil {
		t.Fatalf("envelope data is not an accountEntry: %v (%q)", err, stdout)
	}
	if entry.Agent != "codex" || entry.Name != "work" || entry.Dir == "" {
		t.Fatalf("unexpected entry: %+v", entry)
	}
}

// #3639 verified gemini's launch proof, so `af accounts add gemini work` is a
// plain registration again: the account can be logged in AND a session can be
// scoped to it, so there is nothing to warn about.
//
// This is the NEGATIVE half of the registration-only contract. The positive half —
// that the notice appears, names the agent, and names its follow-up — is pinned in
// internal/sessionenv, which can put an agent into that state; this package can
// only observe the roster it is given (#3639).
func TestAccountsAddGeminiIsAPlainRegistration(t *testing.T) {
	_, stderr, err := runAccounts(t, "add", "gemini", "work")
	if err != nil {
		t.Fatalf("add failed: %v", err)
	}
	if !strings.Contains(stderr, "GEMINI_CLI_HOME") {
		t.Fatalf("add must print the login guidance with gemini's own variable:\n%s", stderr)
	}
	if strings.Contains(strings.ToLower(stderr), sessionenv.AccountRegistrationOnlyMarker) {
		t.Fatalf("gemini is launch-proven now, so registration must carry no caveat:\n%s", stderr)
	}
}

// `af accounts list` marks only the rows a session cannot be scoped to. With the
// roster and the launch proof agreeing, that is none of them — every row keeps the
// three columns a script reads, and the JSON says so explicitly rather than by
// omission.
func TestAccountsListMarksNothingWhileTheRosterAgrees(t *testing.T) {
	home := t.TempDir()
	for _, agent := range []string{"gemini", "codex"} {
		if _, _, err := runAccountsInHome(t, home, "add", agent, "work"); err != nil {
			t.Fatalf("add %s failed: %v", agent, err)
		}
	}

	stdout, stderr, err := runAccountsInHome(t, home, "list")
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		if fields := strings.Split(line, "\t"); len(fields) != 3 {
			t.Fatalf("every row must keep exactly three columns while nothing is registration-only: %q", line)
		}
	}
	if strings.Contains(strings.ToLower(stderr), sessionenv.AccountRegistrationOnlyMarker) {
		t.Fatalf("nothing is registration-only, so the listing must annotate nothing:\n%s", stderr)
	}

	jsonOut, _, err := runAccountsInHome(t, home, "list", "--json")
	if err != nil {
		t.Fatalf("list --json failed: %v", err)
	}
	env := requireEnvelope(t, jsonOut, "stdout")
	var entries []accountEntry
	if err := json.Unmarshal(env.Data, &entries); err != nil {
		t.Fatalf("envelope data is not a list of accountEntry: %v (%q)", err, jsonOut)
	}
	if len(entries) != 2 {
		t.Fatalf("both accounts must be listed: %q", jsonOut)
	}
	for _, entry := range entries {
		if entry.RegistrationOnly {
			t.Fatalf("%s/%s must be registration_only:false: %q", entry.Agent, entry.Name, jsonOut)
		}
	}
	// The field is EMITTED, not omitted — an automation caller needs the difference
	// between "false" and "this af is too old to say".
	if !strings.Contains(jsonOut, "\"registration_only\"") {
		t.Fatalf("registration_only must be present even when false: %q", jsonOut)
	}
}

// The group help must show the gemini form, with the shape that makes it
// different: GEMINI_CLI_HOME is a HOME-like root and the CLI appends .gemini/
// itself, so an operator who copies the CODEX_HOME line and swaps names points
// the variable one level too deep and logs in somewhere af will not look.
func TestAccountsHelpShowsTheGeminiHomeRootShape(t *testing.T) {
	for _, want := range []string{
		"GEMINI_CLI_HOME=$(af accounts add gemini work) gemini",
		"<dir>/.gemini/gemini-credentials.json",
	} {
		if !strings.Contains(accountsCmd.Long, want) {
			t.Fatalf("account help is missing %q:\n%s", want, accountsCmd.Long)
		}
	}
	// And it carries no registration-only paragraph while the roster and the launch
	// proof agree — the paragraph is composed from the roster, so it removes itself
	// rather than needing an edit (#3639).
	if strings.Contains(strings.ToLower(accountsCmd.Long), sessionenv.AccountRegistrationOnlyMarker) {
		t.Fatalf("no agent is registration-only, so the help must say nothing about it:\n%s", accountsCmd.Long)
	}
}
