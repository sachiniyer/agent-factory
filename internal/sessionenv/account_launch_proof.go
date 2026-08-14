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
	if isAccountShellCommand(command) {
		return nil
	}
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
					"account %q cannot scope sibling environment for agent %q: its command contains the selected agent with undeclared arguments %s; "+
						"af cannot prove an enclosing executable treats it as data rather than a nested launch",
					account.Name, account.Agent, quoteArguments(args))
			}
			return accountCommandValidationErrorf(
				"account %q cannot scope sibling environment for agent %q: its command contains the selected agent behind another executable; "+
					"af cannot prove the enclosing executable treats it as data rather than a nested launch",
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

// accountEnvironmentAgentArguments recognizes the selected agent anywhere in a
// literal sibling command after the modelled exec/env prefixes and returns the
// words after it. A non-leading occurrence may be data or a nested launch, but
// af cannot distinguish those at the identity boundary. Refusing both is the
// conservative answer: agent arguments are themselves an identity mechanism
// (for example Codex -c can select the machine keyring), and an arbitrary
// executable cannot be trusted to preserve the selected environment.
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
	for idx, word := range words {
		if filepath.Base(word) == agent {
			return append([]string(nil), words[idx+1:]...), true
		}
	}
	return nil, false
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
	commandName := filepath.Base(words[0])
	switch commandName {
	case "sh", "bash", "csh", "dash", "fish", "ksh", "mksh", "tcsh", "zsh":
		return true
	case ".", "eval", "source", "trap":
		return true
	case "builtin", "command", "nice", "nohup", "setsid", "timeout", "chrt", "ionice", "stdbuf", "xargs":
		return true
	case "sudo", "doas", "pkexec", "su", "runuser":
		return true
	case "make", "gmake":
		return true
	case "npm", "npx", "pnpm", "pnpx", "yarn", "bun", "bunx":
		// Package managers and their one-shot runners execute program text from
		// package metadata or fetched entrypoints. The literal argv cannot prove
		// that hidden program preserves the selected account identity.
		return true
	case "cargo", "rustup", "cross":
		// Cargo package targets, build scripts, tests, and toolchain wrappers are
		// another hidden launch boundary; even a build can execute build.rs.
		return true
	}
	if commandName == "go" && len(words) > 1 && words[1] == "run" {
		return true
	}
	return accountEnvironmentInterpreter(commandName)
}

// accountEnvironmentInterpreter identifies executables whose ordinary
// arguments are program text or a program file. Their child launches and
// environment mutations are hidden from the shell parser above, so treating
// them like a plain process would turn an unreadable identity boundary into an
// assumption. Versioned runtime names are accepted only when the suffix is
// numeric (with optional dots), avoiding broad prefix matches on unrelated
// tools such as python-config.
func accountEnvironmentInterpreter(commandName string) bool {
	switch commandName {
	case "node", "nodejs", "java", "awk", "gawk", "mawk", "nawk", "sed", "Rscript", "julia", "tclsh", "wish", "groovy":
		return true
	}
	for _, stem := range []string{"python", "pypy", "perl", "ruby", "php", "lua", "luajit"} {
		if commandName == stem || numericVersionSuffix(commandName, stem) {
			return true
		}
	}
	return false
}

func numericVersionSuffix(commandName, stem string) bool {
	suffix := strings.TrimPrefix(commandName, stem)
	if suffix == commandName || suffix == "" {
		return false
	}
	hasDigit := false
	for _, char := range suffix {
		switch {
		case char >= '0' && char <= '9':
			hasDigit = true
		case char == '.':
		default:
			return false
		}
	}
	return hasDigit
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
