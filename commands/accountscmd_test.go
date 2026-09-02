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
	// --daemon-url / AF_DAEMON_URL makes both subcommands refuse (they write the
	// LOCAL home), so an ambient value on the developer's box would turn every
	// case below into the remote-target refusal.
	t.Setenv("AF_DAEMON_URL", "")
	prevFlagURL := apiclient.FlagDaemonURL
	apiclient.FlagDaemonURL = ""

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
		if flag := cmd.Flags().Lookup("json"); flag != nil {
			_ = flag.Value.Set("false")
			flag.Changed = false
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

// `af accounts add gemini work` must say, at the moment it invites the operator
// to log real credentials in, that no session can be scoped to the account yet.
//
// The roster and the launch boundary answer different questions, and gemini sits
// between them: its credential root relocates, so registration and login work,
// but af has not verified how it launches, so `--account` refuses. Registration
// succeeding while the launch refusal said "supported: claude, codex" was the
// contradiction #3609 was reviewed for — the operator had no way to tell which
// surface was lying (#3609 review).
func TestAccountsAddGeminiReportsRegistrationOnly(t *testing.T) {
	_, stderr, err := runAccounts(t, "add", "gemini", "work")
	if err != nil {
		t.Fatalf("add failed: %v", err)
	}
	// Case-insensitively: the marker is the lowercase form a listing COLUMN takes,
	// and prose at the head of a line is sentence case. Both must be the same words.
	if !strings.Contains(strings.ToLower(stderr), sessionenv.AccountRegistrationOnlyMarker) {
		t.Fatalf("add did not report the registration-only state:\n%s", stderr)
	}
	if !strings.Contains(stderr, "https://github.com/sachiniyer/agent-factory/issues/3639") {
		t.Fatalf("the notice must name the follow-up that lifts it:\n%s", stderr)
	}
	if !strings.Contains(stderr, "GEMINI_CLI_HOME") {
		t.Fatalf("add must still print the login guidance for a registration-only agent:\n%s", stderr)
	}

	// A launch-proven agent gets no notice, so the guard cannot pass by printing
	// it for everyone.
	_, codexErr, err := runAccounts(t, "add", "codex", "work")
	if err != nil {
		t.Fatalf("add codex failed: %v", err)
	}
	if strings.Contains(strings.ToLower(codexErr), sessionenv.AccountRegistrationOnlyMarker) {
		t.Fatalf("codex is launch-proven and must not be marked:\n%s", codexErr)
	}
}

// `af accounts list` marks the rows a session cannot be scoped to, and says why
// once — on stderr, so the listing on stdout stays parseable.
func TestAccountsListMarksRegistrationOnlyRows(t *testing.T) {
	home := t.TempDir()
	if _, _, err := runAccountsInHome(t, home, "add", "gemini", "work"); err != nil {
		t.Fatalf("add gemini failed: %v", err)
	}
	if _, _, err := runAccountsInHome(t, home, "add", "codex", "work"); err != nil {
		t.Fatalf("add codex failed: %v", err)
	}

	stdout, stderr, err := runAccountsInHome(t, home, "list")
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	var gemini, codex string
	for _, line := range strings.Split(strings.TrimSpace(stdout), "\n") {
		switch {
		case strings.HasPrefix(line, "gemini\t"):
			gemini = line
		case strings.HasPrefix(line, "codex\t"):
			codex = line
		}
	}
	if gemini == "" || codex == "" {
		t.Fatalf("both accounts must be listed:\n%s", stdout)
	}
	if fields := strings.Split(gemini, "\t"); len(fields) != 4 ||
		fields[3] != sessionenv.AccountRegistrationOnlyMarker {
		t.Fatalf("the gemini row must carry the marker as a fourth column: %q", gemini)
	}
	// The three fields a script already reads must not move for a launch-proven
	// agent, so the marker cannot be a rewritten line.
	if fields := strings.Split(codex, "\t"); len(fields) != 3 {
		t.Fatalf("the codex row must keep its three columns: %q", codex)
	}
	if !strings.Contains(stderr, "https://github.com/sachiniyer/agent-factory/issues/3639") {
		t.Fatalf("the listing must explain the marker on stderr:\n%s", stderr)
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
	seen := map[string]bool{}
	for _, entry := range entries {
		seen[entry.Agent] = entry.RegistrationOnly
	}
	if !seen["gemini"] {
		t.Fatalf("gemini must be registration_only:true in the envelope: %q", jsonOut)
	}
	if seen["codex"] {
		t.Fatalf("codex must be registration_only:false in the envelope: %q", jsonOut)
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
	if !strings.Contains(strings.ToLower(accountsCmd.Long), sessionenv.AccountRegistrationOnlyMarker) {
		t.Fatalf("account help must name the registration-only roster:\n%s", accountsCmd.Long)
	}
}
