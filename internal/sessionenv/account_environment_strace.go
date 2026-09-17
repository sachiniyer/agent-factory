package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// straceFlatArgvLimit is the suffix scan's flat early-out: a command line with
// more words than this is not an ordinary invocation, so it refuses before the
// hazard record even reads it. It is no longer what bounds the quadratic —
// the shared meter charges each suffix judgment the length of the suffix it
// walks (#4466 review), so the budget bites first on any argv long enough to
// matter. This stays as the O(1) guard in front of that arithmetic.
const straceFlatArgvLimit = 1024

// unwrapStrace judges a strace invocation without a grammar for its options.
// strace's option set is an open grammar — operand spellings and long-option
// abbreviations change with each release — so this wrapper never computes
// where the child argv begins. It proves the command safe three ways instead
// (#4287):
//
//  1. EVERY SUFFIX MUST BE SAFE. The child boundary is never resolved: each
//     suffix of the argv is judged as a command line, so wherever strace's
//     real child starts, that verdict was evaluated. An option operand that
//     merely resembles a mutating command over-refuses — the accepted cost of
//     not parsing — but no child is ever judged by a shape strace cannot run.
//  2. A SMALL DECLARED HAZARD RECORD. The two option shapes that mutate or
//     exec outside the child argv — invisible to a suffix scan — are declared:
//     -E/--env injects or removes an environment variable in the traced child,
//     and -o/--output spawns its operand as a command when it begins with |
//     or ! (strace(1): the rest of the string is treated as a command and
//     executed). Their operands are judged directly.
//  3. ONE RULE FOR THE UNREDUCIBLE. A word that is not a literal cannot be
//     judged at all — its expansion is a suffix the scan cannot see — so the
//     whole argv fails closed.
func unwrapStrace(
	words []*syntax.Word,
	names map[string]struct{},
	evaluation *evaluationBudget,
) ([]*syntax.Word, bool) {
	if len(words) > straceFlatArgvLimit {
		return nil, true
	}
	// The hazard record reads every word once.
	evaluation.work += len(words)
	if evaluation.work >= accountEnvironmentEvaluationBudget {
		return nil, true
	}
	if straceArgvHazardous(words, names) {
		return nil, true
	}
	for i := range words {
		// Charge the suffix its own LENGTH, not one slot. Judging words[i:] as
		// a command line re-walks that whole tail, so a slot-per-suffix meter
		// understated the cost by a factor of n and the advertised bound held
		// only for a single short call: measured through
		// ValidateAccountEnvironmentCommand, 32 calls of a 1000-word argv took
		// 18.9s while every call stayed inside its own slot count (#4466
		// review). Charging the real walk puts flat argv, nested descent and
		// multi-call programs on one honest meter.
		evaluation.work += len(words) - i
		if evaluation.work >= accountEnvironmentEvaluationBudget {
			return nil, true
		}
		if head, ok := literalShellWordExpandableSafe(words[i]); ok {
			// A NAME=value word at the head of a suffix works like a shell
			// env-prefix: it mutates whatever runs after it.
			if eq := strings.IndexByte(head, '='); eq > 0 &&
				accountEnvironmentNameDenied(head[:eq], names) {
				return nil, true
			}
		}
		if accountCommandWordsMutateEnvironment(words[i:], names, evaluation) {
			return nil, true
		}
	}
	return nil, false
}

// straceArgvHazardous runs the declared hazard record over a strace argv
// (without the leading strace word — the caller may pass it anyway, a bare
// "strace" literal is benign). A word that is not provably literal — an
// expansion or a glob that /bin/sh -c would expand before strace sees argv —
// is unreducible and fails closed. Also used from the unrecognized-wrapper
// tail scan for a strace nested inside another wrapper's argv.
func straceArgvHazardous(words []*syntax.Word, names map[string]struct{}) bool {
	for i, word := range words {
		literal, ok := literalShellWordExpandableSafe(word)
		if !ok {
			return true
		}
		var next *syntax.Word
		if i+1 < len(words) {
			next = words[i+1]
		}
		if straceOptionMutatesAccountEnvironment(literal, next, names) {
			return true
		}
	}
	return false
}

// straceOptionMutatesAccountEnvironment applies the declared hazard record to
// one literal argv word. next is the following word — a separate-word operand
// when the option takes one — and may be nil at argv end.
func straceOptionMutatesAccountEnvironment(
	literal string,
	next *syntax.Word,
	names map[string]struct{},
) bool {
	if strings.HasPrefix(literal, "--") {
		name, value, attached := strings.Cut(literal[2:], "=")
		switch {
		case name != "" && strings.HasPrefix("env", name):
			// --e / --en / --env and any longer --env* spelling: getopt_long
			// resolves the unambiguous prefix, and --env injects or removes an
			// environment variable in the traced child.
			if attached {
				return accountEnvironmentOperandDenied(value, names)
			}
			return straceSeparateOperandDenied(next, names)
		case name != "" && strings.HasPrefix("output", name):
			// --o / --out / --output: the prefix resolves unambiguously or
			// getopt_long exits, and --output's operand execs as a command
			// when it begins with | or !.
			if attached {
				return straceOutputOperandExecs(value)
			}
			return straceSeparateOperandExecs(next)
		case name != "" && strings.HasPrefix(name, "output"):
			// --output-append-mode / --output-separately take no operand, so
			// the next word belongs to the child boundary question, not to
			// this option.
			return false
		case attached && accountEnvironmentOperandDenied(value, names):
			// The generic long-option rule for every other spelling: an
			// attached value that is a denied assignment or bare denied name
			// mutates before the option can even reach its argument.
			return true
		}
		return false
	}
	if len(literal) > 1 && literal[0] == '-' {
		// Short-option cluster: the first hazard flag wins the operand — the
		// rest of the word if any, else the next argv word. Hazard flags
		// buried in another flag's operand over-refuse; the reverse cannot
		// miss because only a flag position execs or injects.
		for idx := 1; idx < len(literal); idx++ {
			switch literal[idx] {
			case 'E':
				if idx+1 < len(literal) {
					return accountEnvironmentOperandDenied(literal[idx+1:], names)
				}
				return straceSeparateOperandDenied(next, names)
			case 'o':
				if idx+1 < len(literal) {
					return straceOutputOperandExecs(literal[idx+1:])
				}
				return straceSeparateOperandExecs(next)
			}
		}
		// The generic short-option rule, same as the long form: a word like
		// -e=NAME=value carries an attached value that may name or assign a
		// denied variable.
		if _, value, ok := strings.Cut(literal, "="); ok {
			return accountEnvironmentOperandDenied(value, names)
		}
	}
	return false
}

// straceSeparateOperandDenied judges the next argv word as a -E/--env
// operand, failing closed when there is no provably literal operand to judge.
func straceSeparateOperandDenied(next *syntax.Word, names map[string]struct{}) bool {
	if next == nil {
		return true
	}
	value, ok := literalShellWordExpandableSafe(next)
	return !ok || accountEnvironmentOperandDenied(value, names)
}

// straceSeparateOperandExecs judges the next argv word as a -o/--output
// operand, failing closed when there is no provably literal operand to judge.
func straceSeparateOperandExecs(next *syntax.Word) bool {
	if next == nil {
		return true
	}
	value, ok := literalShellWordExpandableSafe(next)
	return !ok || straceOutputOperandExecs(value)
}

// straceOutputOperandExecs reports whether an -o/--output operand pipes the
// trace output into a spawned command, which strace executes.
func straceOutputOperandExecs(operand string) bool {
	return strings.HasPrefix(operand, "|") || strings.HasPrefix(operand, "!")
}
