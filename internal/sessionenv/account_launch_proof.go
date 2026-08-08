package sessionenv

import (
	"fmt"
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

// GenerateAccountLaunchProof describes the change from base to final. A nil
// trustedBaseArgs means only words appended after base are af-authored. A
// non-nil slice also trusts the exact base executable and the named trailing
// base words. Any other base arguments stay undeclared.
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
	proof.TrustedExecutable = words[0]
	proof.GeneratedArgs = append(append([]string(nil), trustedBaseArgs...), added...)
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
		return fmt.Errorf(
			"account %q cannot scope agent %q: its program sets an identity variable itself, which overrides the account directory",
			account.Name, account.Agent)
	}
	if provable {
		return nil
	}
	if args, ok := undeclaredAccountArguments(command, proof); ok {
		return fmt.Errorf(
			"account %q cannot scope agent %q: its resolved program contains undeclared arguments %s; "+
				"only arguments af authored for this launch can accompany an account-scoped agent, so this program could not be proven safe",
			account.Name, account.Agent, quoteArguments(args))
	}
	return fmt.Errorf(
		"account %q cannot scope agent %q: its program could not be proven to be a direct %s invocation free of "+
			"identity assignments, and an unverifiable program is not evidence that the account would be used",
		account.Name, account.Agent, account.Agent)
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
