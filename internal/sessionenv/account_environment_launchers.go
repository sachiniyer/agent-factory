package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// This file covers three env-injection launchers — sudo, docker and
// systemd-run — with a deliberately conservative lexical rule instead of a
// per-option grammar model. Exact grammar modelling kept leaking (each
// review round surfaced another accepted spelling the option tables
// misjudged: a hidden docker alias shifting the image boundary, an
// `=`-joined short-option operand, a consumed operand re-entering as the
// shadowed command head), so the property here inverts the risk for these
// three launchers: OVER-refusal is acceptable and UNDER-refusal is not.
//
// The hole (#7565): each launcher runs a child command and can place a
// NAME=value into that child's environment, re-rooting the account's
// credential directory, and the unrecognized-wrapper tail scan treated a
// positional `NAME=value` as inert data. The rule that now applies:
//
//   - Any argument word that can assign or NAME a protected variable is
//     refused. After leading dashes are stripped, the word is cut on `=`
//     and refused when any segment contains a protected name. That one
//     cut covers every accepted spelling without parsing a single option:
//     `-eN=v`, `-e N=v`, `-e=N=v`, `-eN`, `--env N=v`, `--env=N=v`,
//     `-EN=v`, `-E=N=v`, `--setenv N=v`, `--setenv=N=v`, positional
//     `sudo NAME=value` and `NAME+=`, bare-copy forms like
//     `--setenv CODEX_HOME` or `-e CODEX_HOME`, and systemd's
//     `--property=Environment=N=v` / `UnsetEnvironment=N` /
//     `PassEnvironment=N` chains. Noun-uses pay for the conservatism —
//     `docker run alpine echo CODEX_HOME=x` is refused even though the
//     assignment is only argv text — because no lexical cut can separate
//     argv text from an env operand without modelling the options.
//   - An env-file spelling refuses on sight whatever follows:
//     `--env-file`, compose's `env_file`, `--env-from-file` and systemd's
//     `EnvironmentFile=` read a file the command line cannot prove, and a
//     bare `Environment=` clears the unit's pins outright.
//   - A dynamic word is judged by its literal prefix: `PORT="$PORT"`
//     proves an allowed name and consumes; `CODEX_HOME=$V` refuses on the
//     prefix alone; an env spelling whose operand is unreadable
//     (`--env=$V`, `-e $V`, `--setenv $V`, `-pEnvironment=$V`) is
//     unprovable and refuses. Any other dynamic word refuses only where
//     it could still expand into env text the scan cannot see — a word
//     able to word-split inside sudo/systemd-run's argv or inside a
//     docker leaf that injects environment (run/create/exec — the
//     container exec included — and compose run/exec). Everywhere else it
//     is inert data, exactly as the ordinary tail scan treats it.
//   - Whatever survives returns the WHOLE tail, so the ordinary mutation
//     scan every unrecognized command's argv gets still runs over it —
//     a buried `env`, `nohup` or nested launcher inside `sudo …`,
//     `docker …` or `systemd-run …` keeps its coverage, and a leaf word
//     like `build` or `history` is never promoted into shell-builtin
//     dispatch.
//
// requireCommand is envCallMutatesAccountEnvironment's rule applied to
// the launchers' denied spellings: at command position a protected-name
// word refuses on sight, but as a hypothesis inside another command's
// argv (`prog sudo CODEX_HOME=/other`) it refuses only once the line
// could still reach a command word — the same reason `prog env
// CODEX_HOME=/other` admits env's print mode. An env file and an
// unprovable env operand refuse under either rule, like env's own scan.
//
// The trade-off is deliberate: a harmless `docker run --name -e img`
// (which consumes `-e` as the container name) or `sudo nice
// CODEX_HOME=/x` (where `nice` fails to exec a word that is not a
// program) refuses because a word still NAMES a protected variable, and
// `sudo --preserve-env=CODEX_HOME` refuses although it only propagates
// the already-scoped value. That is the price of deleting the option
// tables: every refusal Codex's grammar review produced is covered by
// construction, at the cost of refusing a few benign spellings.

// unwrapEnvInjectionLauncher applies the lexical rule above to the words
// after a launcher name. strict marks command lines whose env-injection
// surface reaches a child a word-split could still hide: always true for
// sudo and systemd-run (every word after them is candidate env text), and
// true for docker only when the leaf injects environment — a docker leaf
// that cannot launch with env keeps a dynamic word inert because the
// returned tail still gets the ordinary scan. It returns the tail the
// caller judges and whether the launcher's own spellings already refuse.
func unwrapEnvInjectionLauncher(words []*syntax.Word, names map[string]struct{}, strict bool, requireCommand bool) ([]*syntax.Word, bool) {
	denied := false
	commandish := false
	envOperand := false
	tail := -1
	for index, word := range words {
		literal, ok := literalShellWord(word)
		if !ok {
			switch verdict := launcherDynamicVerdict(word, names); {
			case verdict == launcherDenied:
				if !requireCommand {
					return nil, true
				}
				denied = true
			case verdict == launcherConsume:
				// A proven `NAME=` target — `PORT="$PORT"` is inert env
				// text even in `-e`'s operand slot.
			case verdict == launcherUnprovable, envOperand,
				strict && launcherWordCanSplit(word):
				// An env spelling with an unreadable operand, an
				// operand word after `-e`/`-E`/`--env`/`--setenv`/`-p`
				// that cannot prove its name, or — on a launcher's
				// injectable tail — a word able to split into env text
				// the scan cannot see.
				return nil, true
			default:
				// launcherOpaque elsewhere: inert data, though in a
				// launcher tail it may still be the command word the
				// line reaches.
				if index > 0 {
					commandish = true
				}
			}
			envOperand = false
			continue
		}
		if tail < 0 {
			// The returned tail opens on the first literal word: a
			// leading dynamic word was judged by this scan already, and
			// as a command head it would only fail closed on the
			// unprovable-name check.
			tail = index
		}
		if launcherEnvFileSpelling(literal) {
			return nil, true
		}
		if launcherWordDenied(literal, names) {
			if !requireCommand {
				return nil, true
			}
			denied = true
			envOperand = false
			continue
		}
		if index > 0 && !strings.HasPrefix(literal, "-") && !strings.Contains(literal, "=") {
			commandish = true
		}
		envOperand = launcherEnvOperandWord(literal)
	}
	if tail < 0 {
		return nil, false
	}
	return words[tail:], requireCommand && denied && commandish
}

func unwrapSudo(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	return unwrapEnvInjectionLauncher(words, names, true, requireCommand)
}

func unwrapSystemdRun(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	return unwrapEnvInjectionLauncher(words, names, true, requireCommand)
}

func unwrapDocker(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	tail, unsafe := unwrapEnvInjectionLauncher(words, names, dockerLeafInjectsEnv(words), requireCommand)
	if unsafe || len(tail) == 0 {
		return nil, unsafe
	}
	// docker's first argument is a subcommand leaf, not the pane's child
	// executable — `docker exec` is not the exec builtin and `docker
	// history` is not the history builtin — so unlike sudo and
	// systemd-run's tails it is never promoted into command dispatch.
	// The leaf and its operands get the ordinary mutation scan every
	// unrecognized command's argv gets (a shadowed docker execs at `env`
	// or a buried launcher just as a shadowed `prog` does), then nothing
	// is left for the caller to judge.
	if wrapperTailHidesAccountAssignment(tail, false, names, memo) {
		return nil, true
	}
	return nil, false
}

// launcherWordDenied cuts a literal argument word on every `=` — after
// leading dashes — and refuses when any segment contains a protected
// name. Containment rather than equality is what makes one cut cover
// every option spelling: `-eCODEX_HOME=x` reads `eCODEX_HOME`,
// `--env=CODEX_HOME=x` reads `CODEX_HOME`, `-E=CODEX_HOME` reads
// `CODEX_HOME`, and `property=Environment=CODEX_HOME=x` lands
// `CODEX_HOME` as its own segment.
func launcherWordDenied(literal string, names map[string]struct{}) bool {
	return launcherSegmentsDenied(strings.TrimLeft(literal, "-"), names)
}

// launcherSegmentsDenied reports whether any `=`-separated segment of
// text contains a protected name.
func launcherSegmentsDenied(text string, names map[string]struct{}) bool {
	for _, segment := range strings.Split(text, "=") {
		for name := range names {
			if strings.Contains(segment, name) {
				return true
			}
		}
	}
	return false
}

// launcherEnvFileSpelling reports whether a literal argument word names
// an env file or clears the environment — both refuse regardless of the
// denied-name cut because their effect is not readable from the command
// line.
func launcherEnvFileSpelling(literal string) bool {
	word := strings.TrimLeft(literal, "-")
	return word == "env-file" || strings.HasPrefix(word, "env-file=") ||
		word == "env_file" || strings.HasPrefix(word, "env_file=") ||
		word == "env-from-file" || strings.HasPrefix(word, "env-from-file=") ||
		strings.Contains(word, "EnvironmentFile") ||
		strings.HasSuffix(word, "Environment=")
}

// launcherEnvOperandWord reports whether a literal word ends in a
// position whose NEXT word is an env operand: a bare `-e`, `-E`, `--env`,
// `--setenv`, `-p`/`--property` (the systemd property operand is NAME=
// text too), a `--` long form that matches exactly, or a single-dash
// cluster ending in `e`, `E` or `p` (`-ite X`, `-tE X`, `-tp X`), where
// getopt/pflag hands the next word to that option. The word the mark
// applies to is judged by the denied-name cut or the dynamic-operand
// rules, so over-marking is harmless — an ordinary operand word simply
// gets the same scan it would anywhere else.
func launcherEnvOperandWord(literal string) bool {
	if !strings.HasPrefix(literal, "-") || literal == "-" {
		return false
	}
	word := strings.TrimLeft(literal, "-")
	if launcherEnvOperandToken(word) {
		return true
	}
	if strings.HasPrefix(literal, "--") {
		return false
	}
	return strings.HasSuffix(word, "e") || strings.HasSuffix(word, "E") ||
		strings.HasSuffix(word, "p")
}

func launcherEnvOperandToken(word string) bool {
	switch word {
	case "e", "E", "env", "setenv", "env-add", "env-rm", "p", "property":
		return true
	}
	return false
}

// launcherDynamicVerdict is the verdict a non-literal argument word gets:
// denied when its literal prefix already names a protected variable,
// unprovable when the prefix spells an env option whose operand the
// expansion hides (`--env=$V`, `-e$V`, `--setenv=$V`, `-pEnvironment=$V`),
// consume when the prefix proves an ordinary `NAME=` target, and opaque
// when no `=` pin exists at all — the caller then decides from position
// and word-split potential.
const (
	launcherConsume = iota
	launcherDenied
	launcherUnprovable
	launcherOpaque
)

func launcherDynamicVerdict(word *syntax.Word, names map[string]struct{}) int {
	prefix := launcherLiteralPrefix(word)
	if launcherSegmentsDenied(strings.TrimLeft(prefix, "-"), names) {
		return launcherDenied
	}
	if strings.IndexByte(prefix, '=') < 0 {
		if launcherEnvOperandToken(strings.TrimLeft(prefix, "-")) {
			return launcherUnprovable
		}
		return launcherOpaque
	}
	segments := strings.Split(strings.TrimLeft(prefix, "-"), "=")
	for len(segments) > 0 && segments[len(segments)-1] == "" {
		segments = segments[:len(segments)-1]
	}
	if len(segments) == 0 {
		return launcherUnprovable
	}
	// An earlier segment that is a property sigil leaves a dynamic
	// NAME=VALUE operand unreadable (`--property=$V`,
	// `-pEnvironment=$V`); an env-assign sigil's name simply opens the
	// last segment (`--env=PORT=$V` proves PORT), so only the
	// env-property family fails closed here.
	for _, segment := range segments[:len(segments)-1] {
		if launcherPropertySegment(segment) {
			return launcherUnprovable
		}
	}
	last := segments[len(segments)-1]
	if launcherEnvOperandToken(last) ||
		strings.Contains(last, "Environment") ||
		strings.HasSuffix(last, "e") || strings.HasSuffix(last, "E") ||
		strings.HasSuffix(last, "p") {
		return launcherUnprovable
	}
	return launcherConsume
}

func launcherPropertySegment(segment string) bool {
	switch segment {
	case "p", "property", "path-property", "timer-property", "socket-property":
		return true
	}
	return strings.Contains(segment, "Environment")
}

// launcherLiteralPrefix returns the concatenated literal text of a word's
// leading literal parts — `PORT=` in `PORT="$PORT"`, or `--env=PORT=` in
// `--env=PORT="$PORT"` — stopping at the first dynamic part.
func launcherLiteralPrefix(word *syntax.Word) string {
	var b strings.Builder
	for _, part := range word.Parts {
		lit, ok := part.(*syntax.Lit)
		if !ok {
			break
		}
		b.WriteString(lit.Value)
	}
	return b.String()
}

// launcherWordCanSplit reports whether a word can still expand into more
// than one argv word: an unquoted parameter expansion, command
// substitution, arithmetic expansion or glob is the only shape that can
// smuggle env option text past the lexical scan (`docker run $V` where V
// spells `-e CODEX_HOME=x`). A double-quoted expansion pins one word, so
// `docker run -v "$PWD:/app"` and `alpine:"$TAG"` stay inert data.
func launcherWordCanSplit(word *syntax.Word) bool {
	for _, part := range word.Parts {
		switch part.(type) {
		case *syntax.ParamExp, *syntax.CmdSubst, *syntax.ArithmExp, *syntax.ExtGlob:
			return true
		}
	}
	return false
}

// dockerLeafInjectsEnv reports whether the leaf of this docker command
// line can place environment into a child: `run`/`create`/`exec` at top
// level or under `docker container`, and `run`/`exec` under `docker
// compose`. The leaf is read from the first non-dash literal words —
// `compose` and the management groups hand the leaf slot to the next
// word — and a command line with no provable leaf is treated as
// launching, so a dynamic word that can word-split never gets the inert
// treatment on an unproven leaf.
func dockerLeafInjectsEnv(words []*syntax.Word) bool {
	var first, second string
	found := 0
	for _, word := range words {
		literal, ok := literalShellWord(word)
		if !ok || strings.HasPrefix(literal, "-") {
			continue
		}
		if found == 0 {
			first = literal
		} else {
			second = literal
		}
		if found++; found == 2 {
			break
		}
	}
	if found == 0 {
		return true
	}
	group, leaf := "", first
	switch {
	case leaf == "compose":
		group, leaf = "compose", second
	case dockerManagementGroup(leaf):
		group, leaf = leaf, second
	}
	switch {
	case group == "compose":
		return leaf == "run" || leaf == "exec"
	case group == "":
		return leaf == "run" || leaf == "create" || leaf == "exec"
	default:
		return group == "container" &&
			(leaf == "run" || leaf == "create" || leaf == "exec")
	}
}

// dockerManagementGroup names docker's subcommand groups — the word whose
// NEXT word is the real leaf (`docker container exec`, `docker image
// build`).
func dockerManagementGroup(word string) bool {
	switch word {
	case "builder", "buildx", "checkpoint", "config", "container", "context",
		"image", "manifest", "model", "network", "node", "plugin", "scout",
		"secret", "service", "stack", "swarm", "system", "trust", "volume":
		return true
	}
	return false
}

// launcherSuffixMutates judges a launcher word found in an unrecognized
// wrapper's tail — `prog sudo CODEX_HOME=/other` — as if the words after it
// were the launcher's command line, under the hypothesis that a shadowed
// `prog` may discard its own operands and exec there. It is the same scan
// the env word already gets in this position (requireCommand), so the
// launcher only refuses here when its denied spelling still reaches a
// command word; a bare `prog sudo CODEX_HOME=/other` — which the real
// sudo rejects with no command to run — stays admitted, matching `prog
// env CODEX_HOME=/other`.
func launcherSuffixMutates(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) bool {
	*memo.childJudgements++
	if *memo.childJudgements > shadowedChildJudgementLimit {
		return true
	}
	var tail []*syntax.Word
	var unsafe bool
	switch {
	case isAccountCommandName(words[0], "sudo"):
		tail, unsafe = unwrapSudo(words[1:], names, memo, true)
	case isAccountCommandName(words[0], "docker"):
		tail, unsafe = unwrapDocker(words[1:], names, memo, true)
	default:
		tail, unsafe = unwrapSystemdRun(words[1:], names, memo, true)
	}
	if unsafe || len(tail) == 0 {
		return unsafe
	}
	return wrapperOperandTailMutates(tail, names, memo)
}
