package sessionenv

import (
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
		if accountEnvironmentArithmeticNonConstant(command, overrideNames) {
			return accountCommandValidationErrorf(
				"account %q cannot scope sibling environment for agent %q: its command contains shell arithmetic "+
					"whose operand is not a numeric constant — `(( x ))`, `let x`, `[[ x -eq 0 ]]`, `${arr[x]}`, "+
					"and similar forms all re-evaluate a stored variable's value as fresh arithmetic, so af cannot "+
					"prove what the expression evaluates to or whether it rewrites an identity or shell-startup "+
					"variable. Use a literal numeric operand or move the arithmetic out of the agent invocation string",
				account.Name, account.Agent)
		}
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

// accountEnvironmentArithmeticNonConstant reports whether a sibling refusal
// fired SOLELY because of the inverted arithmetic guard in
// commandMutatesAccountEnvironment — that is, whether the command contains an
// arithmetic context whose operand is not provably a numeric constant (a
// variable reference, parameter expansion, or command substitution inside
// `(( ))`, `$(( ))`, `let`, a numeric `[[ ]]` operator, an `${arr[i]}`
// subscript or `${x:offset:length}` slice, an `arr[i]=v` indexed assignment, or
// a C-style for loop), AND no node-level identity or shell-startup mutation is
// independently reachable by the walk in EITHER parse variant. It reuses the
// same arithmetic scan and the same node walk, so the cause detected here
// matches the cause that fired the refusal.
//
// C is checked across BOTH variants rather than only in the variant where the
// arithmetic guard fires: bash may parse a form whose inverted-guard arm runs
// while POSIX parses the same form so the letMutates/Assignment arms re-derive
// the identity mutation, e.g. `let 'CODEX_HOME[0]=42'; codex` (the bash
// LetClause's arithmetic arm fires; the POSIX bare-`let` CallExpr is caught by
// letMutatesAccountEnvironment). The refusal-message decision needs "did the
// walk find an identity mutation at all", so both variants are inspected.
//
// Used to render a refusal that names the real cause instead of the generic
// "sets an identity or shell-startup variable itself" message, which is false
// for this class: `(( x = 1 )); codex`, `let 'total += 1'`, and
// `x=42; [[ x -eq 0 ]]; codex` set no variable at all. When the walk does
// independently catch an identity or shell-startup mutation (`let
// CODEX_HOME=42; codex`, `let 'CODEX_HOME[0]=42'; codex`, …) the generic
// message is accurate, so this helper returns false and the caller keeps the
// generic refusal.
func accountEnvironmentArithmeticNonConstant(command string, names map[string]struct{}) bool {
	bFires, cFires := false, false
	for _, variant := range []syntax.LangVariant{syntax.LangPOSIX, syntax.LangBash} {
		file, err := syntax.NewParser(syntax.Variant(variant)).Parse(strings.NewReader(command), "")
		if err != nil {
			continue
		}
		if fileHasArithmeticContextWithVariableOperand(file) {
			bFires = true
		}
		if fileMutatesAccountIdentities(file, names) {
			cFires = true
		}
		if bFires && cFires {
			break
		}
	}
	return bFires && !cFires
}

// fileMutatesAccountIdentities reports whether the node walk in
// commandMutatesAccountEnvironment — the walk that runs AFTER the inverted
// arithmetic guard — independently catches an identity or shell-startup
// mutation in this parsed file. It is the same walk; factored here so the
// refusal-message decision can ask "was the inverted guard the SOLE reason?"
// without touching commandMutatesAccountEnvironment's signature.
func fileMutatesAccountIdentities(file syntax.Node, names map[string]struct{}) bool {
	mutates := false
	syntax.Walk(file, func(node syntax.Node) bool {
		if nodeMutatesAccountEnvironment(node, names, nil) {
			mutates = true
			return false
		}
		return true
	})
	return mutates
}
