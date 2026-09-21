package sessionenv

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// AccountLaunchProof carries the facts only af's launcher can know about a
// command: the exact agent executable it selected and the exact argument words
// it authored. Neither is inferred from command spelling at the credential
// boundary.
type AccountLaunchProof struct {
	TrustedExecutable string
	GeneratedArgs     []string
}

// accountLaunchProofEnvVar carries the launcher's proof OUT OF BAND, through
// the pane environment. A shell re-invoking af can WRITE the variable just as
// easily as it could append an argv element, so a value read from the env var
// alone is not evidence af authored this invocation. The shim therefore ALSO
// re-derives what af's launcher would have produced (AccountLaunchProofResolver
// below) and refuses an env-supplied proof that does not match it: an env var
// the child can read is also an env var the child's parent shell can write, so
// a plaintext token in the environment cannot carry that evidence (#3123, #4731
// review).
//
// The marker argv is fully forgeable: a program_overrides value can re-invoke af
// as `af __af-session-env-exec-account <agent> <count> <account> <command>` and
// name any TrustedExecutable it likes, because nothing in that argv proves af
// authored it. The proof therefore no longer rides in argv at all. The launcher
// (session/tmux) installs this variable in the account-scoped pane's tmux
// session environment; the pane child inherits it, the shim reads it before
// exec, and FilterForCommand strips it again so it never reaches the agent or a
// subshell. The re-derivation, not the env var, is what an overwriting shell
// cannot reproduce.
//
// The value is not secret: TrustedExecutable and GeneratedArgs are already
// visible in the pane command a launcher wrote. base64 wrap keeps the JSON
// clear of any quoting concern at the tmux/env boundary.
const accountLaunchProofEnvVar = "__AF_ACCOUNT_LAUNCH_PROOF"

// errAccountLaunchProofAbsent is the sentinel a missing/empty proof resolves to,
// so the shim can name the cause without echoing any value.
var errAccountLaunchProofAbsent = errors.New("launcher account launch proof is absent")

// errAccountLaunchProofMismatch is the sentinel returned when the env-supplied
// proof does not match what af's launcher would have produced for this
// invocation. Naming the cause lets the caller surface "the proof channel was
// overwritten" without echoing any value the attacker wrote into the env.
var errAccountLaunchProofMismatch = errors.New("launcher account launch proof does not match what af would have produced for this invocation")

// AccountLaunchProofResolver re-derives the launch proof the launcher would
// have produced for an account-scoped pane whose command is `command`, the same
// way the launcher derives it in session/launch_program.go — by resolving the
// operator's config from the pane's working directory and feeding base+final to
// GenerateAccountLaunchProof. The shim uses it to refuse an env-supplied proof
// that an overwriting shell wrote into the env var (#3123, #4731 review): the
// env value is forgeable by a same-uid re-invocation, a config re-derivation is
// not, because nothing the attacker runs in the pane has write access to the
// resolved-operator-config the resolver reads.
//
// It returns (proof, nil) when a derivation is available for this (agent,
// command) pair, or (zero, err) when it cannot decide. A nil resolver short-
// circuits the matcher: the shim falls back to the env-supplied proof alone,
// which is the form this hook replaced and the form tests exercise directly.
// main.go wires the production resolver; tests install their own.
var AccountLaunchProofResolver func(agent, account, command string) (AccountLaunchProof, error)

// accountLaunchProofsMatch reports whether two proofs describe the same
// launcher-authored invocation. Slices are compared positionally and by
// length, never as values the caller could reorder.
func accountLaunchProofsMatch(a, b AccountLaunchProof) bool {
	if a.TrustedExecutable != b.TrustedExecutable {
		return false
	}
	if len(a.GeneratedArgs) != len(b.GeneratedArgs) {
		return false
	}
	for i := range a.GeneratedArgs {
		if a.GeneratedArgs[i] != b.GeneratedArgs[i] {
			return false
		}
	}
	return true
}

// AccountLaunchProofEnvEntry encodes proof as a NAME=VALUE environment entry for
// the account-scoped pane's session environment. It always emits an entry,
// including for an empty proof (a bare agent command), because the shim must
// distinguish "launcher set the proof, and it describes a bare invocation" from
// "no launcher was here at all". Returns "" only on an internal encode failure.
func AccountLaunchProofEnvEntry(proof AccountLaunchProof) (string, error) {
	data, err := json.Marshal(accountLaunchProofWire{
		TrustedExecutable: proof.TrustedExecutable,
		GeneratedArgs:     proof.GeneratedArgs,
	})
	if err != nil {
		return "", fmt.Errorf("encode account launch proof: %w", err)
	}
	return accountLaunchProofEnvVar + "=" + base64.StdEncoding.EncodeToString(data), nil
}

// decodeAccountLaunchProofEnv is the shim-side counterpart. An empty value
// means the variable was not set by the launcher, which the caller treats as a
// refusal rather than as a bare-invocation proof. Anything that fails to
// round-trip is also a refusal: the credential boundary fails closed, and a
// forged value is not evidence that af authored it.
func decodeAccountLaunchProofEnv(value string) (AccountLaunchProof, error) {
	if value == "" {
		return AccountLaunchProof{}, errAccountLaunchProofAbsent
	}
	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return AccountLaunchProof{}, fmt.Errorf("decode account launch proof: %w", err)
	}
	var wire accountLaunchProofWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return AccountLaunchProof{}, fmt.Errorf("decode account launch proof: %w", err)
	}
	return AccountLaunchProof{TrustedExecutable: wire.TrustedExecutable, GeneratedArgs: wire.GeneratedArgs}, nil
}

type accountLaunchProofWire struct {
	TrustedExecutable string   `json:"t"`
	GeneratedArgs     []string `json:"g,omitempty"`
}

type accountCommandValidationError struct {
	message string
}

func (e *accountCommandValidationError) Error() string {
	return e.message
}

func accountCommandValidationErrorf(format string, args ...any) error {
	return &accountCommandValidationError{message: fmt.Sprintf(format, args...)}
}

// IsAccountCommandValidationError reports whether err came from the
// command-shape half of the account boundary. Provisioners use this to add
// runtime-specific context without changing the priority of earlier account
// refusals such as cloud authentication mode.
func IsAccountCommandValidationError(err error) bool {
	var target *accountCommandValidationError
	return errors.As(err, &target)
}

// GenerateAccountLaunchProof describes the change from base to final. A nil
// trustedBaseArgs means only words appended after base are af-authored. A
// non-nil slice also trusts an absolute base executable and the named trailing
// base words. Relative executable spellings are resolved again from the pane's
// workdir, so their arguments can be declared but their identity cannot. Any
// other base arguments stay undeclared.
//
// A base whose exec builtin carries the `--` separator declares nothing, for the
// same reason ValidateAccountCommand refuses it: dash runs no such command, so a
// proof about it would describe a pane that exits 127 (#3557). Producer and
// consumer share stripExecPrefix so neither can drift into accepting it alone.
func GenerateAccountLaunchProof(base, final string, trustedBaseArgs []string) (AccountLaunchProof, bool) {
	added, ok := GeneratedArgsBetween(base, final)
	if !ok {
		return AccountLaunchProof{}, false
	}
	proof := AccountLaunchProof{GeneratedArgs: added}
	if trustedBaseArgs == nil {
		return proof, true
	}

	call, ok := singleSimpleCall(base)
	if !ok || !callIsLiteral(call) || len(call.Assigns) > 0 {
		return AccountLaunchProof{}, false
	}
	stripped, execSeparator := stripExecPrefix(call.Args)
	if execSeparator {
		return AccountLaunchProof{}, false
	}
	words, ok := literalCommandArgs(stripped)
	if !ok || len(words) == 0 {
		return AccountLaunchProof{}, false
	}
	if len(words) < len(trustedBaseArgs)+1 {
		return AccountLaunchProof{}, false
	}
	baseSuffix := words[len(words)-len(trustedBaseArgs):]
	for idx, want := range trustedBaseArgs {
		if baseSuffix[idx] != want {
			return AccountLaunchProof{}, false
		}
	}
	proof.GeneratedArgs = append(append([]string(nil), trustedBaseArgs...), added...)
	if filepath.IsAbs(words[0]) {
		proof.TrustedExecutable = words[0]
	}
	return proof, true
}

// ValidateAccountCommand applies the command-shape half of the account
// boundary before launch.
func ValidateAccountCommand(command string, account Account) error {
	proof := commandProof{
		agent:             account.Agent,
		trustedExecutable: account.TrustedExecutable,
		generated:         account.GeneratedArgs,
	}
	overrides, provable := commandOverridesName(command, proof)
	if overrides {
		return accountCommandValidationErrorf(
			"account %q cannot scope agent %q: its program sets an identity variable itself, which overrides the account directory",
			account.Name, account.Agent)
	}
	if provable {
		return nil
	}
	if CommandUsesExecSeparator(command) {
		return accountCommandValidationErrorf(
			"account %q cannot scope agent %q: its program begins with `exec --`, and af runs that program through "+
				"/bin/sh, where the separator is not portable; dash — /bin/sh on Debian and Ubuntu — gives its exec "+
				"builtin no options, so it takes `--` as the command name and the pane exits 127 with "+
				"`exec: --: not found`. Remove the `--`: af accepts the same program written `exec <agent> …`",
			account.Name, account.Agent)
	}
	if args, ok := undeclaredAccountArguments(command, proof); ok {
		return accountCommandValidationErrorf(
			"account %q cannot scope agent %q: its resolved program contains undeclared arguments %s; "+
				"only arguments af authored for this launch can accompany an account-scoped agent, so this program could not be proven safe",
			account.Name, account.Agent, quoteArguments(args))
	}
	return accountCommandValidationErrorf(
		"account %q cannot scope agent %q: its program could not be proven to be a direct %s invocation free of "+
			"identity assignments, and an unverifiable program is not evidence that the account would be used",
		account.Name, account.Agent, account.Agent)
}

// ValidateAccountEnvironmentCommand protects a shell/process sibling's selected
// account environment without requiring the sibling command to be the agent.
// A direct identity assignment would run after the boundary and override it, so
// it is refused just as it is for the agent pane itself.
func ValidateAccountEnvironmentCommand(command string, account Account) error {
	if isAccountShellCommand(command) {
		return nil
	}
	configVar, supported := SupportsAccounts(account.Agent)
	if !supported {
		return nil
	}
	overrideNames := accountScopedNames(account.Agent, configVar)
	for _, selector := range AgentAuthSelectors(account.Agent) {
		overrideNames[selector] = struct{}{}
	}
	for name := range accountShellStartupNames {
		overrideNames[name] = struct{}{}
	}
	if commandMutatesAccountEnvironment(command, overrideNames) {
		return accountCommandValidationErrorf(
			"account %q cannot scope sibling environment for agent %q: its command sets an identity or shell-startup variable itself, which can override the account directory",
			account.Name, account.Agent)
	}
	if commandFeedsProvenShell(command) {
		return accountCommandValidationErrorf(
			"account %q cannot scope sibling environment for agent %q: its command gives an interactive shell input "+
				"other than the terminal (a pipe, input redirection, here-document, or coprocess), and that shell would "+
				"run the supplied text as commands, which can override the account directory; start the shell without "+
				"redirecting its input",
			account.Name, account.Agent)
	}
	return nil
}

func undeclaredAccountArguments(command string, proof commandProof) ([]string, bool) {
	call, ok := singleSimpleCall(command)
	if !ok || len(call.Assigns) > 0 || !callIsLiteral(call) {
		return nil, false
	}
	words, execSeparator := stripExecPrefix(call.Args)
	if execSeparator {
		// The refusal is about the separator, not about the arguments behind it.
		return nil, false
	}
	stripped, ok := stripDeclaredSuffixForDiagnostics(words, proof.generated)
	if !ok || len(stripped) < 2 {
		return nil, false
	}
	args, ok := literalCommandArgs(stripped[1:])
	return args, ok
}

// stripDeclaredSuffixForDiagnostics is deliberately weaker than
// stripGeneratedArgs: it may leave user arguments in front of af's exact
// declared suffix so the refusal can name them. It is never an admission check;
// commandOverridesName has already refused the command through the strict
// executable-plus-exactly-generated rule before this runs.
func stripDeclaredSuffixForDiagnostics(words []*syntax.Word, generated []string) ([]*syntax.Word, bool) {
	if len(generated) == 0 {
		return words, true
	}
	if len(words) < len(generated)+1 {
		return nil, false
	}
	start := len(words) - len(generated)
	for idx, want := range generated {
		got, ok := literalShellWord(words[start+idx])
		if !ok || got != want {
			return nil, false
		}
	}
	return words[:start], true
}

func quoteArguments(args []string) string {
	quoted := make([]string, len(args))
	for idx, arg := range args {
		quoted[idx] = strconv.Quote(arg)
	}
	return strings.Join(quoted, " ")
}
