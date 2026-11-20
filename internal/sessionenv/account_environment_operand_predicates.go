package sessionenv

import (
	"path/filepath"
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"

	"github.com/sachiniyer/agent-factory/internal/envcommand"
)

// accountEnvironmentNameDenied reports whether name marks a denied variable.
// The mark is computed on the name's exact spelling OR its escape- and
// quote-normalized spelling, never on one alone (#7627): an operand word's
// literal text can still carry backslash escapes or quote bytes (`CODEX\_HOME`
// inside a quoted word, `x\CODEX_HOME` unquoted), so each reading is checked —
// the exact one refuses `x\CODEX_HOME` whose boundary shows on the raw text,
// and the normalized one refuses the `CODEX\_HOME` a downstream decode would
// turn back into the protected name.
func accountEnvironmentNameDenied(name string, names map[string]struct{}) bool {
	if _, denied := names[name]; denied {
		return true
	}
	if normalized := escapeQuoteNormalizedSpelling(name); normalized != name {
		_, denied := names[normalized]
		return denied
	}
	return false
}

// escapeQuoteNormalizedSpelling removes the quoting and escape text a
// downstream decode can strip at ANY depth, in one flat pass: every run of
// backslashes goes, a line break (LF or CRLF) right after a run goes with it —
// the line-continuation form — and every quote character goes. The result is
// an over-approximation of POSIX escape resolution (XCU 2.2.1), closed under
// nesting: quoting quoted again reduces to the same text with no recursion
// and no fixpoint loop. Exact one-level resolution is depth-sensitive —
// inside double quotes `\\` is a literal backslash a deeper re-parse would
// still consume — so an escape can hide a name at a depth the exact rule
// misses; deleting the escapes themselves makes `co\\\\dex`, `co\\dex` and
// `co\dex` all read `codex`. It is used only as a second reading under a
// denied/hidden mark — a reading never replaces the text it came from, so no
// reading can hide a name another shows.
func escapeQuoteNormalizedSpelling(text string) string {
	if !strings.ContainsAny(text, "'\"`\\") {
		return text
	}
	var out strings.Builder
	out.Grow(len(text))
	for i := 0; i < len(text); i++ {
		switch text[i] {
		case '\\':
			for i < len(text) && text[i] == '\\' {
				i++
			}
			switch {
			case i < len(text) && text[i] == '\n':
				i++
			case i+1 < len(text) && text[i] == '\r' && text[i+1] == '\n':
				i += 2
			}
			i-- // the loop's i++ lands past the run (and any line break)
		case '\'', '"', '`':
			// Quote characters vanish at every depth.
		default:
			out.WriteByte(text[i])
		}
	}
	return out.String()
}

// mayNormalizeSpelling reports whether text carries a character the
// normalized reading can remove — a quote or a backslash. A word without
// one reads identically under both spellings, so its second reading is
// skipped outright and the word costs what it cost before the union.
func mayNormalizeSpelling(text string) bool {
	return strings.ContainsAny(text, "'\"`\\")
}

func variableTestMutatesAccountEnvironment(words []*syntax.Word) bool {
	for idx := 0; idx < len(words); idx++ {
		option, literal := literalShellWord(words[idx])
		if !literal {
			return true
		}
		if option != "-v" {
			continue
		}
		if idx+1 >= len(words) {
			return true
		}
		operand, literal := literalShellWord(words[idx+1])
		if !literal || strings.Contains(operand, "[") {
			return true
		}
		idx++
	}
	return false
}

func unaryTestMutatesAccountEnvironment(test *syntax.UnaryTest) bool {
	if test.Op != syntax.TsVarSet {
		return false
	}
	word, ok := test.X.(*syntax.Word)
	if !ok {
		return true
	}
	operand, literal := literalShellWord(word)
	return !literal || strings.Contains(operand, "[")
}

func unwrapCommandBuiltin(words []*syntax.Word) ([]*syntax.Word, bool) {
	for len(words) > 0 {
		option, literal := literalShellWord(words[0])
		if !literal {
			return nil, true
		}
		if option == "--" {
			words = words[1:]
			break
		}
		if !strings.HasPrefix(option, "-") || option == "-" {
			break
		}
		for _, flag := range option[1:] {
			switch flag {
			case 'p':
			case 'v', 'V':
				return nil, false // query-only: command does not execute its operand
			default:
				return nil, true
			}
		}
		words = words[1:]
	}
	if len(words) > 0 {
		if _, literal := literalShellWord(words[0]); !literal {
			return nil, true
		}
	}
	return words, false
}

// envCallMutatesAccountEnvironment reports whether the env invocation formed by
// words mutates a denied name or execs something unprovable. requireCommand
// restricts that verdict to env invocations carrying a command word: print-mode
// env mutates only its own process, which is the right reading when the words
// sit in an unrecognized wrapper's tail rather than forming the command itself —
// an `env CODEX_HOME=/tmp` with nothing after it runs env's print path, not an
// override of a running agent.
//
// It is envcommand.Parse over the words literalized by envArgvWord — any word
// that fails envArgvWord refuses the call, wherever it sits — computed from
// per-position memos so that nested env words cost O(1) each (#4966).
func envCallMutatesAccountEnvironment(words []*syntax.Word, names map[string]struct{}, requireCommand bool, memo operandTailMemo) bool {
	if len(words) == 0 {
		return false
	}
	key := envCallKey{word: words[0], requireCommand: requireCommand}
	if answer, seen := memo.envCalls[key]; seen {
		return answer
	}
	answer := envCallMutatesAccountEnvironmentUncached(words, names, requireCommand, memo)
	memo.envCalls[key] = answer
	return answer
}

func envCallMutatesAccountEnvironmentUncached(words []*syntax.Word, names map[string]struct{}, requireCommand bool, memo operandTailMemo) bool {
	if memo.envArgvSuffixUnprovable(words) {
		return true
	}
	scan := memo.envScan(words, envcommand.Start, names)
	if scan.refused {
		return true
	}
	if requireCommand && scan.command == nil {
		return false
	}
	if scan.denied {
		return true
	}
	if scan.command == nil {
		return false
	}
	// env's command word is literal: envArgvWord passes a non-literal word
	// only when it spells an assignment, which the scan consumed as one. So
	// the operand-tail judgment — unwrap, then judge the peeled command — is
	// exactly the command's verdict, and shares its memo. The walk here has
	// no access to tainted; the taint check is applied at the arithmetic-node
	// level by nodeMutatesAccountEnvironment.
	return wrapperOperandTailMutates(scan.command, names, memo)
}

func shellCommandIsUnproven(words []*syntax.Word) bool {
	if len(words) == 0 {
		return false
	}
	command, literal := literalShellWord(words[0])
	if !literal || !KnownShellName(filepath.Base(command)) {
		return false
	}
	if accountShellCommandWordsProven(words) {
		return false
	}
	// #7604: a `sh -c`/`bash -c` payload is not hidden input — it is a
	// command string the gate can read, so every candidate payload is
	// judged by the top-level gate recursively instead of refused
	// outright (decodedShellPayloadUnproven does that, position-blind).
	// What is left here is the INVOCATION's provability: a dynamic or
	// absent payload word, a script file, an interactive/stdin spelling,
	// an unmodelled option letter, or a shell whose -c still reads a
	// startup file all stay unproven.
	if shellDashCCommandProven(words) {
		return false
	}
	return true
}

// shellDashCSafeFlagLetters are the single-dash option letters a shell's
// -c invocation can carry without changing what the gate must decode: no
// input source (i interactive, s stdin, E — ksh's -E reads ENV/kshrc the
// way zsh reads zshenv), no operand option the walk does not model (o is
// modeled below; O/R/T and the rest refuse), and no behavior that
// changes what an assignment word does (k keyword mode places every
// following argument word into the command's environment — the Codex P1
// on this head). Every letter outside this set makes the whole
// invocation undecodable: over-refusing a rare flag is the cheap
// direction.
const shellDashCSafeFlagLetters = "abCefhlmnpruvVx"

// shellDashOSafeOptionNames is the allowlist of `set -o` names a decoded
// -c invocation may carry: every one changes error or execution flow — none
// changes where the shell reads commands or what an assignment word does.
// Any other -o name — keyword (assignments), the line-editing and expansion
// modes (emacs, vi, histexpand, braceexpand, interactive-comments), posix,
// privileged, allexport, history, or a name a future shell adds — makes the
// invocation undecodable and refuses, the same rule the safe-letter set
// above applies to single letters (Codex P1 on this head).
var shellDashOSafeOptionNames = map[string]struct{}{
	"errexit": {}, "errtrace": {}, "functrace": {}, "hashall": {},
	"ignoreeof": {}, "monitor": {}, "noclobber": {}, "noexec": {},
	"noglob": {}, "nolog": {}, "notify": {}, "nounset": {},
	"onecmd": {}, "physical": {}, "pipefail": {}, "verbose": {},
	"xtrace": {},
}

// shellDashCCommandProven reports whether words are a shell command-string
// invocation whose every payload candidate is decodable and judged clean by
// the gate — `sh -c 'make'` proves out exactly the way `make` does, and
// `bash -c 'unset CODEX_HOME; x'` refuses the way the bare line does. The
// head must be a shell with no unconditional startup file in non-interactive
// -c mode: zsh reads zshenv, csh/tcsh their rc files and fish its config
// regardless, so they stay unproven. (Bash's BASH_ENV and ksh's ENV name a
// file only through the environment, which is the gate's stated limit; a
// login `-l` sources the user's own startup files, the same responsibility
// the docs already place outside this check.) Every word must be literal —
// a dynamic word could hide the option that makes the rest a script name or
// stdin — and every option letter must be admissible: a safe flag, `c`, or
// `o` whose operand (the cluster's rest or the next word) is not `keyword`.
// A non-option word before the first -c cluster names a script file, so
// the invocation is not provable; `--` and `-` are admissible only after a
// -c cluster too. `-c` itself takes no attached operand in this family —
// `bash -ce 'x'` keeps `e` as an option and reads `x` as the string — so
// every literal word after a -c cluster is a candidate payload, and each
// one is re-judged: the ambiguity can only over-refuse positional argv
// that happen to look mutating.
func shellDashCCommandProven(words []*syntax.Word) bool {
	command, _ := literalShellWord(words[0])
	switch filepath.Base(command) {
	case "sh", "dash", "ash", "bash", "ksh", "mksh":
	default:
		return false
	}
	sawPayloadOption := false
	for i := 1; i < len(words); i++ {
		literal, ok := launcherLiteralWord(words[i])
		if !ok {
			return false
		}
		if !strings.HasPrefix(literal, "-") || literal == "-" {
			if !sawPayloadOption {
				return false
			}
			continue
		}
		if literal == "--" {
			// `sh -- file` runs file as a script; only after -c are the
			// following words positional argv.
			if !sawPayloadOption {
				return false
			}
			continue
		}
		if strings.HasPrefix(literal, "--") {
			return false
		}
		cluster := literal[1:]
		for j := 0; j < len(cluster); j++ {
			switch {
			case cluster[j] == 'c':
				sawPayloadOption = true
			case cluster[j] == 'o':
				// Bash takes -o's option name from the NEXT argv word,
				// never the cluster's trailing letters — `-oe keyword`
				// keeps e as an option and reads `keyword` as the name
				// (verified on real bash — independent-review P1 on this
				// head). The name must be on the allowlist of `set -o`
				// names known not to change where commands come from or
				// how assignments behave; any other — keyword, an
				// input-source or expansion mode, or a name a future
				// shell adds — is undecodable and refuses.
				if i+1 >= len(words) {
					return false
				}
				operand, ok := launcherLiteralWord(words[i+1])
				if !ok {
					return false
				}
				if _, ok := shellDashOSafeOptionNames[operand]; !ok {
					return false
				}
				i++
			case strings.IndexByte(shellDashCSafeFlagLetters, cluster[j]) < 0:
				return false
			}
		}
	}
	// A -c invocation with no operand word after its -c cluster has no
	// command string to prove — `sh -c` errors, `sh -cx` is -c -x still
	// missing its string — so it stays unproven.
	return len(shellDashCPayloadCandidates(words)) > 0
}

// shellDashCPayloadCandidates collects the words that can be a shell's -c
// command string. Unlike su/script's getopt, the shells attach no operand
// to -c: every letter in a -c cluster is an option, and the string is an
// operand word after it — `bash -ce 'x'` runs `x` with -e set, not the
// letter e. Which of the following words is the string depends on option
// operands the scan can misplace (`-o` consumes one), so every literal
// word after the FIRST -c cluster is a candidate — each is judged, and
// collecting the suffix once keeps the scan linear in argv length.
func shellDashCPayloadCandidates(words []*syntax.Word) []string {
	var candidates []string
	seenC := false
	for _, word := range words {
		literal, ok := launcherLiteralWord(word)
		if !ok {
			continue
		}
		if seenC {
			candidates = append(candidates, literal)
			continue
		}
		if literal != "-" && literal != "--" &&
			strings.HasPrefix(literal, "-") && !strings.HasPrefix(literal, "--") &&
			strings.ContainsRune(literal[1:], 'c') {
			seenC = true
		}
	}
	return candidates
}

func accountShellCommandWordsProven(words []*syntax.Word) bool {
	if len(words) == 0 {
		return false
	}
	command, literal := literalShellWord(words[0])
	// A sibling shell may read profiles, stdin, a script, or a command string.
	// The only statically proven form is the same absolute, startup-free command
	// AccountShellCommand generates for a dedicated shell tab. What stdin can
	// carry is a property of the whole command, not of these words, so
	// ValidateAccountEnvironmentCommand checks it separately
	// (commandFeedsProvenShell).
	if !literal || !filepath.IsAbs(command) {
		return false
	}
	// zsh's startup-freedom is only half in its argv: the other half is the
	// ZDOTDIR pin, which ApplyAccountEnvironment attaches to the exact
	// generated command alone. Inside a longer command, zsh runs with whatever
	// environment a wrapper or earlier statement hands it, so it is never
	// proven here; ValidateAccountEnvironmentCommand admits the exact form
	// before this walk runs (#4474 review).
	if filepath.Base(command) == "zsh" {
		return false
	}
	// Every rejection that needs only the head and the word count runs before
	// the arguments are literalized: fileHasProvenShell asks this of every
	// suffix of every call, so literalizing first made any long command
	// quadratic (#4966). A suffix is literalized only when its length already
	// matches the trusted form's, which is a handful of words.
	want := trustedAccountShellArgs(command)
	if want == nil || len(words)-1 != len(want) {
		return false
	}
	args, literal := literalCommandArgs(words[1:])
	return literal && slices.Equal(args, want)
}

// KnownShellName reports whether a basename names one of the shells the
// account-environment judgements recognize — the POSIX family plus fish and
// csh/tcsh. Callers feed it filepath.Base output: a path-qualified shell is
// the same shell (Codex 4599228958: argv[1] == "-c" reads as a command ONLY
// behind one of these — `codex -c check_for_update_on_startup=false` is codex
// taking a config flag, and `python -c <script>` names no agent at all).
func KnownShellName(name string) bool {
	switch name {
	case "ash", "bash", "csh", "dash", "fish", "ksh", "mksh", "sh", "tcsh", "zsh":
		return true
	default:
		return false
	}
}

func isAccountCommandName(word *syntax.Word, want string) bool {
	value, literal := literalShellWord(word)
	return literal && filepath.Base(value) == want
}

func unsetMutatesAccountEnvironment(words []*syntax.Word, names map[string]struct{}) bool {
	functionsOnly := false
	options := true
	for _, word := range words {
		value, literal := literalShellWord(word)
		if !literal {
			return true
		}
		if options {
			switch value {
			case "--":
				options = false
				continue
			case "-f":
				functionsOnly = true
				continue
			case "-v":
				functionsOnly = false
				continue
			}
			if strings.HasPrefix(value, "-") {
				return true
			}
			options = false
		}
		if !functionsOnly {
			if accountEnvironmentOperandDenied(value, names) {
				return true
			}
		}
	}
	return false
}

// hashMutatesAccountEnvironment reports whether a `hash` call remaps a command
// name to a path of its own choosing.
//
// `hash -p pathname name` makes `name` resolve to `pathname`, so every later
// executable-name check in this walk answers about a different binary than the
// one that will actually run: after `hash -p /usr/bin/env runner`, the
// otherwise-unknown `runner` IS env, and `runner CODEX_HOME=/other codex`
// applies the replacement root through a name this walk never modelled.
//
// Only the remapping form is refused — `hash -r` and `hash name` merely
// maintain the lookup cache and leave every name meaning what it meant.
func hashMutatesAccountEnvironment(words []*syntax.Word) bool {
	for _, word := range words {
		value, literal := literalShellWord(word)
		if !literal {
			return true
		}
		if value == "--" || !strings.HasPrefix(value, "-") {
			return false
		}
		if strings.ContainsRune(value[1:], 'p') {
			return true
		}
	}
	return false
}

func isAccountDeclarationBuiltin(word *syntax.Word) bool {
	for _, name := range []string{"export", "readonly", "declare", "typeset", "local"} {
		if isBareName(word, name) {
			return true
		}
	}
	return false
}

func declarationMutatesAccountEnvironment(words []*syntax.Word, names map[string]struct{}) bool {
	options := true
	for _, word := range words {
		if name, assignment := shellWordAssignmentName(word); assignment {
			if accountEnvironmentOperandDenied(name, names) {
				return true
			}
			options = false
			continue
		}
		value, literal := literalShellWord(word)
		if !literal {
			return true
		}
		if options {
			if value == "--" {
				options = false
				continue
			}
			if strings.HasPrefix(value, "-") || strings.HasPrefix(value, "+") {
				if value == "-n" || value == "+n" || value == "--nameref" || value == "-i" {
					return true
				}
				if len(value) != 2 {
					return true
				}
				continue
			}
			options = false
		}
		if accountEnvironmentOperandDenied(value, names) {
			return true
		}
	}
	return false
}

func readMutatesAccountEnvironment(words []*syntax.Word, names map[string]struct{}) bool {
	for len(words) > 0 && wordEquals(words[0], "-r") {
		words = words[1:]
	}
	for _, word := range words {
		value, literal := literalShellWord(word)
		if !literal {
			return true
		}
		if strings.HasPrefix(value, "-") {
			return true
		}
		if accountEnvironmentOperandDenied(value, names) {
			return true
		}
	}
	return false
}

func getoptsMutatesAccountEnvironment(words []*syntax.Word, names map[string]struct{}) bool {
	if len(words) < 2 {
		return false
	}
	name, literal := literalShellWord(words[1])
	if !literal {
		return true
	}
	return accountEnvironmentOperandDenied(name, names)
}

// printfMutatesAccountEnvironment reports whether a `printf` call assigns to a
// denied variable. Bash's printf takes its assignment target from a `-v name`
// option, parses EVERY `-v` it sees, and applies the LAST one — so
// `printf -v A -v CODEX_HOME '%s' /other` assigns CODEX_HOME, not A (verified
// on real bash). The option stream stays open across repeated `-v` pairs: the
// first word that is not a literal `-v` is the format string, and `--` ends
// option parsing the way it does for the sibling builtins.
//
// This walks the whole option stream the way waitMutatesAccountEnvironment
// does for `wait -p`: each literal `-v` pair is judged and consumed, and the
// scan keeps going because a later `-v` retargets the assignment. A non-literal
// word reached while options are still open fails closed — an expansion can
// yield `-v` and retarget at a denied name (`X=-v; printf -v A $X CODEX_HOME y`
// sets CODEX_HOME=y on real bash) — exactly the dynamic retarget `wait`'s
// option loop refuses. Attached `-vXNN`/`-v=` spellings and a bare `-v` with no
// operand (a bash usage error) keep their existing verdicts.
func printfMutatesAccountEnvironment(words []*syntax.Word, names map[string]struct{}) bool {
	for len(words) > 0 {
		option, literal := literalShellWord(words[0])
		if !literal {
			// A dynamic word is unsafe while the option stream is still
			// open: an expansion that yields `-v` retargets the assignment
			// at whatever follows, so fail closed as wait does.
			return true
		}
		if option == "--" {
			// -- ends option parsing; the next word is the format string
			// and everything after it is printf data.
			return false
		}
		if strings.HasPrefix(option, "-v") && option != "-v" {
			// Refuse attached -vXNN / -v= spellings whose assignment target
			// cannot be proven (existing fail-closed arm).
			return true
		}
		if option != "-v" {
			// Not a -v option: this word is the format string (or an
			// unrecognized option bash errors on), so no further -v target
			// can follow in this call. The remaining words are printf data.
			return false
		}
		// option == "-v": the next word is the assignment target.
		if len(words) < 2 {
			// `printf -v` with no operand is a bash usage error, so it
			// mutates nothing.
			return false
		}
		name, literal := literalShellWord(words[1])
		if !literal {
			return true
		}
		if accountEnvironmentOperandDenied(name, names) {
			return true
		}
		// Keep scanning: a later literal `-v <name>` retargets the
		// assignment, and bash applies the LAST one.
		words = words[2:]
	}
	return false
}

func accountEnvironmentOperandDenied(value string, names map[string]struct{}) bool {
	return accountEnvironmentOperandDeniedWith(value, names, true)
}

// accountEnvironmentFreeTextOptionValueDenied judges the value of an attached
// `--opt=value` on a wrapper tail this walk does not model. Such a value is
// free text unless it names a denied variable or carries a denied assignment,
// so the subscript rule applies only where the value is SHAPED like a variable
// operand: an identifier immediately followed by `[`. A bracket anywhere else
// is a glob or character class (`--ignore=[a-c]*`), which is no evidence of a
// mutation — and the same text as a separate word is never judged at all.
func accountEnvironmentFreeTextOptionValueDenied(value string, names map[string]struct{}) bool {
	bracket := strings.IndexByte(value, '[')
	subscripted := bracket > 0 && isShellIdentifier(value[:bracket])
	return accountEnvironmentOperandDeniedWith(value, names, subscripted || bracket < 0)
}

func isShellIdentifier(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || i > 0 && c >= '0' && c <= '9' {
			continue
		}
		return false
	}
	return true
}

// accountEnvironmentOperandDeniedWith is the shared rule; subscriptRule false
// skips only the blanket bracket refusal, never the name/assignment check.
func accountEnvironmentOperandDeniedWith(value string, names map[string]struct{}, subscriptRule bool) bool {
	name := value
	if subscriptRule && strings.IndexByte(name, '[') >= 0 {
		// Bash evaluates indexed-array subscripts as arithmetic and can assign a
		// protected variable while resolving an unrelated base name. Proving the
		// subscript is side-effect-free requires runtime semantics, so fail closed.
		return true
	}
	if equal := strings.IndexByte(name, '='); equal >= 0 {
		// Cut the operand at the first `=` to reach its assignment target. bash's
		// `NAME+=value` append operator leaves a trailing `+` on the target once
		// the cut lands (`CODEX_HOME+=/other` -> `CODEX_HOME+`), so a guarded name
		// then misses the denied-set match and an append-assignment mutates the
		// very variable this predicate exists to protect. Strip the trailing `+`
		// so the append form is judged by its real target. The bare top-level
		// form is caught earlier by the parser's `*syntax.Assign` (Append) node;
		// this is the path that reaches a wrapper-peeled word list.
		name = name[:equal]
		name = strings.TrimSuffix(name, "+")
	}
	return accountEnvironmentNameDenied(name, names)
}
