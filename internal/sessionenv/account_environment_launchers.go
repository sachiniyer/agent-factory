package sessionenv

import (
	"slices"
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// This file covers the env-injection launchers — sudo, docker (including
// `docker compose` and the standalone `docker-compose` binary) and
// systemd-run (including its `run0` argv[0] alias) — with a deliberately
// conservative lexical rule instead of a per-option grammar model. Exact
// grammar modelling kept leaking (each review round surfaced another
// accepted spelling the option tables misjudged: a hidden docker alias
// shifting the image boundary, an `=`-joined short-option operand, a
// consumed operand re-entering as the shadowed command head, a leading
// dynamic word escaping the tail), so the property here inverts the risk
// for these launchers: OVER-refusal is acceptable and UNDER-refusal is
// not. The rule is keyed by the binary's grammar, so every name a
// grammar ships under gets it — `docker-compose` lands docker's check
// and `run0` lands systemd-run's.
//
// The hole (#7565): each launcher runs a child command and can place a
// NAME=value into that child's environment, re-rooting the account's
// credential directory, and the unrecognized-wrapper tail scan treated a
// positional `NAME=value` as inert data. The rule that now applies:
//
//   - Any LITERAL argument word that can assign or NAME a protected
//     variable is refused. After leading dashes are stripped, the word is
//     cut on `=` and refused when any segment contains a protected name
//     at an identifier boundary — at the segment's start, after a
//     character outside [A-Za-z0-9_], or after a leading letters-only
//     short-option cluster. That one cut covers every accepted spelling
//     without parsing a single option: `-eN=v`, `-e N=v`, `-e=N=v`,
//     `-eN`, `--env N=v`, `--env=N=v`, `-EN=v`, `-E=N=v`, `--setenv
//     N=v`, `--setenv=N=v`, positional `sudo NAME=value` and `NAME+=`,
//     bare-copy forms like `--setenv CODEX_HOME` or `-e CODEX_HOME`, and
//     systemd's `--property=Environment=N=v` / `UnsetEnvironment=N` /
//     `PassEnvironment=N` chains, abbreviations included
//     (`--prop=Environment=…` cuts the same segments). The boundary is
//     what keeps ordinary use admitted — on both sides: `docker run -e
//     NODE_ENV=production app`, `sudo RAILS_ENV=production …` and
//     `docker run -e ENVIRONMENT=staging app` embed `ENV` next to a name
//     byte, and `MYENV=1`/`DEVENV=1` are bare words whose `MY`/`DEV`
//     prefix is no option cluster, so they run — while `-eENV=`,
//     `-itENV=`, `-ECODEX_HOME=` and `"A=1 CODEX_HOME=` still refuse.
//     Noun-uses pay for the conservatism — `docker run alpine echo
//     CODEX_HOME=x` is refused even though the assignment is only argv
//     text — because no lexical cut can separate argv text from an env
//     operand without modelling the options.
//   - Any DYNAMIC argument word is refused outright. A `$VAR`,
//     `${VAR}`, `$(…)`, backtick, arithmetic or glob expansion could
//     produce a protected assignment, an env option spelling, or extra
//     argv the visible command line cannot show, so its presence makes
//     the whole launcher command unverifiable wherever it sits — leading
//     (`sudo "$CMD" -i codex` with CMD=env) or trailing (`docker run
//     $IMG`). Ordinary dynamic uses pay the same price: `docker run -e
//     PORT="$PORT"` is refused although its expansion would stay
//     benign. A word-leading `~` is the one exception: tilde expansion
//     yields a home path, never env text, so `-v ~/.npm:/root/.npm`
//     reads as literal data.
//   - systemd-run words must be judged as systemd decodes them: a
//     property operand still holding a quote or a backslash AFTER the
//     launch shell's own quote removal is refused on sight — systemd
//     unquotes and C-unescapes property values, so
//     `Environment=CODEX""_HOME=/o` and `Environment=CODEX\x5fHOME=/o`
//     both land `CODEX_HOME` in the unit's environment even though the
//     literal segment hides it. Refusing is simpler than re-implementing
//     systemd's unquote+unescape.
//   - An env-file spelling refuses on sight whatever follows:
//     `--env-file`, compose's `env_file`, `--env-from-file` and systemd's
//     `EnvironmentFile=` read a file the command line cannot prove, and a
//     bare `Environment=` clears the unit's pins outright.
//   - Whatever survives is all-literal, and the tail is judged before
//     anything else in it is classified. A shadowed launcher (`shift N;
//     exec "$@"`) can reach every word of its tail as a command word, so
//     sudo and systemd-run's tails get the per-position child-tail
//     judgement (head plus every literal verdict suffix) — a `command -v`
//     or `builtin` prefix inside the tail is only ever argv text to the
//     launcher, and letting it peel at command position let
//     `command -v env -i codex` discard the `env -i codex` a shadowed
//     `shift 2` actually execs (Codex P1 on this head). docker's tail
//     gets the argv-position mutation scan instead: its leaf word is
//     config, not the pane's child executable — `docker exec` is not the
//     exec builtin and `docker history` is not the history builtin — and
//     neither path promotes a tail word into shell-builtin dispatch.
//
// requireCommand is envCallMutatesAccountEnvironment's rule applied to
// the launchers' denied spellings: at command position a protected-name
// word refuses on sight, but as a hypothesis inside another command's
// argv (`prog sudo CODEX_HOME=/other`) it refuses only once the line
// could still reach a command word — the same reason `prog env
// CODEX_HOME=/other` admits env's print mode. A dynamic word, a
// decode-carrying systemd word and an env file refuse under either rule,
// like env's own unprovable-word scan.
//
// The trade-off is deliberate: a harmless `docker run --name -e img`
// (which consumes `-e` as the container name) or `sudo nice
// CODEX_HOME=/x` (where `nice` fails to exec a word that is not a
// program) refuses because a word still NAMES a protected variable, and
// `sudo --preserve-env=CODEX_HOME` refuses although it only propagates
// the already-scoped value. That is the price of deleting the option
// tables: every refusal the grammar review produced is covered by
// construction, at the cost of refusing a few benign spellings.

// unwrapEnvInjectionLauncher applies the lexical rule above to the words
// after a launcher name. decodes marks the launchers that re-decode
// quote/escape text in their operands (systemd-run and run0): for them a
// literal word still carrying `"`, `'` or `\` is unverifiable. It returns
// whether the command line refuses on the launcher's own spellings; the
// caller still owes every surviving word the tail judgement.
func unwrapEnvInjectionLauncher(words []*syntax.Word, names map[string]struct{}, decodes bool, requireCommand bool) bool {
	denied := false
	commandish := false
	for index, word := range words {
		literal, ok := launcherLiteralWord(word)
		if !ok {
			// Any dynamic word could expand into a protected
			// assignment or env spelling the command line cannot
			// show — unverifiable, refused wherever it sits.
			return true
		}
		if launcherEnvFileSpelling(literal) {
			return true
		}
		if decodes && strings.ContainsAny(literal, "\"'\\") {
			// systemd-run unquotes and C-unescapes property
			// values, so `Environment=CODEX""_HOME=/o` and
			// `Environment=CODEX\x5fHOME=/o` both land
			// CODEX_HOME. A word still carrying quote or escape
			// text cannot be judged by the raw-text cut.
			return true
		}
		if launcherWordDenied(literal, names) {
			if !requireCommand {
				return true
			}
			denied = true
			continue
		}
		if index > 0 && !strings.HasPrefix(literal, "-") &&
			!strings.Contains(literal, "=") {
			commandish = true
		}
	}
	return requireCommand && denied && commandish
}

// unwrapSudo and unwrapSystemdRun refuse the launcher's own spellings and
// then judge the tail in place: a shadowed launcher execs at ANY word of
// its tail, so every suffix a shell could reach as a command is judged
// before a query-only wrapper near its front can classify the dangerous
// one away — `sudo command -v env -i codex` refuses on the `env -i codex`
// a shadowed `shift 2; exec "$@"` would run, where re-peeling the tail at
// command position let `command -v` discard it as query operands. The
// judgement draws on shadowedChildJudgementLimit, so the nested
// `sudo sudo …` chain that re-scanned the whole tail per peel level now
// fails closed at the shared budget instead of running quadratic (Codex
// P2 on this head).
func unwrapSudo(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand) {
		return nil, true
	}
	if shadowedLiteralChildTailMutates(words, names, memo) {
		return nil, true
	}
	return nil, false
}

func unwrapSystemdRun(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	if unwrapEnvInjectionLauncher(words, names, true, requireCommand) {
		return nil, true
	}
	if shadowedLiteralChildTailMutates(words, names, memo) {
		return nil, true
	}
	return nil, false
}

func unwrapDocker(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand) {
		return nil, true
	}
	// docker's first argument is a subcommand leaf, not the pane's child
	// executable — `docker exec` is not the exec builtin and `docker
	// history` is not the history builtin — so unlike sudo and
	// systemd-run's tails it is never promoted into command dispatch.
	// The leaf and its operands get the ordinary mutation scan every
	// unrecognized command's argv gets (a shadowed docker execs at `env`
	// or a buried launcher just as a shadowed `prog` does), then nothing
	// is left for the caller to judge.
	if wrapperTailHidesAccountAssignment(words, false, names, memo) {
		return nil, true
	}
	return nil, false
}

// launcherLiteralWord renders a word to its literal text for the launcher
// scan, admitting the one failure literalShellWord produces that cannot
// hide env text: a word-leading `~`. Tilde expansion yields a home path,
// never an assignment, so the word is re-judged with the leading tilde
// replaced by a plain character — every other expansion (glob, brace,
// $'…', a non-Lit part) still fails the walk exactly as before.
func launcherLiteralWord(word *syntax.Word) (string, bool) {
	if literal, ok := literalShellWord(word); ok {
		return literal, true
	}
	if len(word.Parts) == 0 {
		return "", false
	}
	first, ok := word.Parts[0].(*syntax.Lit)
	if !ok || !strings.HasPrefix(first.Value, "~") {
		return "", false
	}
	clone := *word
	clone.Parts = slices.Clone(word.Parts)
	lit := *first
	lit.Value = "x" + first.Value[1:]
	clone.Parts[0] = &lit
	return literalShellWord(&clone)
}

// launcherWordDenied cuts a literal argument word on every `=` — after
// leading dashes — and refuses when any segment names a protected
// variable. That one cut covers every option spelling:
// `-eCODEX_HOME=x` reads `eCODEX_HOME`, `--env=CODEX_HOME=x` reads
// `CODEX_HOME`, `-E=CODEX_HOME` reads `CODEX_HOME`, and
// `property=Environment=CODEX_HOME=x` lands `CODEX_HOME` as its own
// segment. optionCluster marks a word that began with `-` — the only
// place a leading letters-only prefix reads as a short-option cluster.
func launcherWordDenied(literal string, names map[string]struct{}) bool {
	return launcherSegmentsDenied(strings.TrimLeft(literal, "-"), names,
		strings.HasPrefix(literal, "-"))
}

// launcherSegmentsDenied reports whether any `=`-separated segment of
// text names a protected variable at an identifier boundary. The
// short-option-cluster reading applies only to the first segment — the
// option text itself; a later segment is a VALUE (`--setenv=MYENV=1`
// sets MYENV, so `ENV` inside it is still only a suffix of another name).
func launcherSegmentsDenied(text string, names map[string]struct{}, optionCluster bool) bool {
	for index, segment := range strings.Split(text, "=") {
		cluster := optionCluster && index == 0
		for name := range names {
			if launcherSegmentNames(segment, name, cluster) {
				return true
			}
		}
	}
	return false
}

// launcherSegmentNames reports whether segment contains name at an
// identifier boundary ON BOTH SIDES. On the left: the segment's start,
// after a character outside [A-Za-z0-9_], or — only when the word began
// with a dash — after a leading letters-only short-option cluster
// (`-eENV`, `-itENV`: the cluster is option text, not a name prefix, so
// `-eENV=x` still refuses while the same letters in the bare word
// `MYENV=1` or `DEVENV=1` name a different variable). On the right: the
// name must end the segment or be followed by a character outside
// [A-Za-z0-9_] (the `=` and `+=` cut and every punctuation form), since
// `ENVIRONMENT=staging` names ENVIRONMENT, not ENV. `NODE_ENV` and
// `RAILS_ENV` embed `ENV` after `_`, which is not a boundary, so the
// ordinary spellings stay admitted.
func launcherSegmentNames(segment, name string, optionCluster bool) bool {
	for i := 0; i+len(name) <= len(segment); i++ {
		if segment[i:i+len(name)] != name {
			continue
		}
		if i+len(name) < len(segment) && isEnvNameByte(segment[i+len(name)]) {
			continue
		}
		if i == 0 || !isEnvNameByte(segment[i-1]) ||
			(optionCluster && lettersOnly(segment[:i])) {
			return true
		}
	}
	return false
}

func isEnvNameByte(c byte) bool {
	return c == '_' ||
		(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
		(c >= '0' && c <= '9')
}

func lettersOnly(text string) bool {
	for i := 0; i < len(text); i++ {
		c := text[i]
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			return false
		}
	}
	return len(text) > 0
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

// launcherSuffixMutates judges a launcher word found in an unrecognized
// wrapper's tail — `prog sudo CODEX_HOME=/other` — as if the words after it
// were the launcher's command line, under the hypothesis that a shadowed
// `prog` may discard its own operands and exec there. It is the same scan
// the env word already gets in this position (requireCommand), so the
// launcher only refuses here when its denied spelling still reaches a
// command word; a bare `prog sudo CODEX_HOME=/other` — which the real
// sudo rejects with no command to run — stays admitted, matching `prog
// env CODEX_HOME=/other`. A dynamic word still refuses on sight: a
// hypothesis cannot prove an expansion either.
func launcherSuffixMutates(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo) bool {
	*memo.childJudgements++
	if *memo.childJudgements > shadowedChildJudgementLimit {
		return true
	}
	var unsafe bool
	switch {
	case isAccountCommandName(words[0], "sudo"):
		_, unsafe = unwrapSudo(words[1:], names, memo, true)
	case isAccountCommandName(words[0], "docker") || isAccountCommandName(words[0], "docker-compose"):
		_, unsafe = unwrapDocker(words[1:], names, memo, true)
	default:
		_, unsafe = unwrapSystemdRun(words[1:], names, memo, true)
	}
	return unsafe
}
