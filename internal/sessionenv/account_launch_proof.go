package sessionenv

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/sachiniyer/agent-factory/internal/envcommand"
)

// AccountLaunchProof carries the facts only af's launcher can know about a
// command: the exact agent executable it selected and the exact argument words
// it authored. Neither is inferred from command spelling at the credential
// boundary.
type AccountLaunchProof struct {
	TrustedExecutable string
	GeneratedArgs     []string
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
	words, ok := literalCommandArgs(call.Args)
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
		trustedWrapper:    account.TrustedWrapper,
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

// ValidateAccountEnvironmentCommand rejects a shell/process pane that pins an
// identity in its command. Unlike ValidateAccountCommand it does not require the
// command to be the agent itself; it protects the selected environment that the
// sibling pane receives.
func ValidateAccountEnvironmentCommand(command string, account Account) error {
	proof := commandProof{agent: account.Agent}
	overrides, provable := commandOverridesName(command, proof)
	if overrides {
		return accountCommandValidationErrorf(
			"account %q cannot scope sibling environment for agent %q: its command sets an identity variable itself, which overrides the account directory",
			account.Name, account.Agent)
	}
	if !provable {
		if args, selectedAgent := accountEnvironmentAgentArguments(command, account.Agent); selectedAgent {
			if len(args) > 0 {
				return accountCommandValidationErrorf(
					"account %q cannot scope sibling environment for agent %q: its direct agent command contains undeclared arguments %s, which can override the selected identity",
					account.Name, account.Agent, quoteArguments(args))
			}
			return accountCommandValidationErrorf(
				"account %q cannot scope sibling environment for agent %q: af cannot prove this direct agent executable uses the selected identity",
				account.Name, account.Agent)
		}
	}
	if !provable && accountEnvironmentCommandNeedsProof(command) {
		return accountCommandValidationErrorf(
			"account %q cannot scope sibling environment for agent %q: af cannot prove this interpreter or shell wrapper preserves the selected account identity",
			account.Name, account.Agent)
	}
	return nil
}

// accountEnvironmentAgentArguments recognizes a literal sibling command that
// directly runs the selected agent, optionally through exec or the modelled env
// wrapper, and returns its arguments. A sibling may run arbitrary ordinary
// processes under the selected environment, but agent arguments are themselves
// an identity mechanism (for example Codex -c can select the machine keyring),
// so an undeclared argument list must be named and refused.
func accountEnvironmentAgentArguments(command, agent string) ([]string, bool) {
	call, ok := singleSimpleCall(command)
	if !ok || !callIsLiteral(call) {
		return nil, false
	}
	words, ok := literalCommandArgs(call.Args)
	if !ok {
		return nil, false
	}
	if len(words) > 0 && words[0] == "exec" {
		words = words[1:]
		if len(words) > 0 && words[0] == "--" {
			words = words[1:]
		}
	}
	if len(words) > 0 && filepath.Base(words[0]) == "env" {
		invocation, err := envcommand.Parse(words[1:], envcommand.Policy{AllowAssignments: true})
		if err != nil || invocation.CommandIndex < 0 {
			return nil, false
		}
		words = words[1+invocation.CommandIndex:]
	}
	if len(words) == 0 || filepath.Base(words[0]) != agent {
		return nil, false
	}
	return append([]string(nil), words[1:]...), true
}

// accountEnvironmentCommandNeedsProof identifies shell syntax that constructs
// another command rather than merely consuming the selected environment. Plain
// literal process commands remain valid sibling panes: af does not claim to
// prove arbitrary application behavior. Shell programs, known command-launch
// wrappers, expansions, pipelines, and compound commands are different because
// the command string itself is a second launch mechanism where an identity
// assignment or selected-agent argument can be hidden.
func accountEnvironmentCommandNeedsProof(command string) bool {
	call, ok := singleSimpleCall(command)
	if !ok || !callIsLiteral(call) {
		return true
	}
	words, ok := literalCommandArgs(call.Args)
	if !ok {
		return true
	}
	if len(words) > 0 && words[0] == "exec" {
		words = words[1:]
		if len(words) > 0 && words[0] == "--" {
			words = words[1:]
		}
	}
	if len(words) == 0 {
		return false
	}
	if filepath.Base(words[0]) == "env" {
		invocation, err := envcommand.Parse(words[1:], envcommand.Policy{AllowAssignments: true})
		if err != nil || invocation.CommandIndex < 0 {
			return err != nil
		}
		words = words[1+invocation.CommandIndex:]
	}
	if len(words) == 0 {
		return false
	}
	switch filepath.Base(words[0]) {
	case "sh", "bash", "dash", "ksh", "mksh", "zsh":
		return true
	case "command", "nice":
		return true
	default:
		return false
	}
}

func undeclaredAccountArguments(command string, proof commandProof) ([]string, bool) {
	call, ok := singleSimpleCall(command)
	if !ok || len(call.Assigns) > 0 || !callIsLiteral(call) {
		return nil, false
	}
	words := call.Args
	if len(words) > 0 && wordEquals(words[0], "exec") {
		words = words[1:]
		if len(words) > 0 && wordEquals(words[0], "--") {
			words = words[1:]
		}
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
