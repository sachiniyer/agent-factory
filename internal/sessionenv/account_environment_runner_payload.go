package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// unwrapSu judges su and runuser. Their `-c`/`--command`/`--session-
// command` operand is a SCRIPT the child shell decodes — `co""dex` lands
// as `codex` — so every payload candidate is re-judged as a command line
// (Codex P2 on this head). The rest of the tail gets the argv-position
// scan docker's does. su preserves the caller's environment EXCEPT for
// `-`/`-l`/`--login` (and `-p`/`--preserve-environment` overrides even
// those), so only a login form counts as a reset — and under that reset
// the payload's DECODED words get the agent-reach check too.
func unwrapSu(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	payloads := runnerPayloads(words)
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand, memo) {
		return nil, true
	}
	if wrapperTailHidesAccountAssignment(words, false, names, memo) {
		return nil, true
	}
	if suResetsEnvironment(words) {
		// su's getopt permutes options ahead of the USER operand and
		// the user name itself can't be told from a command word, so
		// the whole tail is a dark region — the same ambiguity the
		// accepted `su - codex` over-refusal already documents.
		if ambiguousResetTailAgentReach(words, names, nil, memo.reach) {
			return nil, true
		}
		for _, payload := range payloads {
			if payloadReachesAgentProgram(payload, names, nil, memo.reach) {
				return nil, true
			}
		}
	}
	return nil, false
}

// unwrapCommandRunner judges command-string runners — script, whose
// `-c`/`--command` operand is a script the child decodes
// (`script -q -c 'export CODEX_""HOME=/other; codex'` exports the pin
// after the child's shell removed the quotes — Codex P1 on this head).
// The payload itself is gated once at the top level by
// decodedShellPayloadUnproven, which applies every check including the
// stdin-feed one this position cannot see (Codex P1 on this head); the
// rest of the argv gets the ordinary scans.
func unwrapCommandRunner(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand, memo) {
		return nil, true
	}
	if wrapperTailHidesAccountAssignment(words, false, names, memo) {
		return nil, true
	}
	return nil, false
}

// isRunnerCommandOption reports whether a long-option word names a
// script-carrying operand: `-c`/`--command`/`--session-command` or an
// unambiguous getopt_long abbreviation of one (`--comm`, `--sess` —
// anything that is a non-empty prefix of a payload option).
func isRunnerCommandOption(opt string) bool {
	return len(opt) > 2 && (strings.HasPrefix("--command", opt) ||
		strings.HasPrefix("--session-command", opt))
}

// runnerPayloads collects the script operands a command-string runner
// hands to a shell, in every spelling: `-c x`, `--command x`,
// `--command=x`, `--session-command x`, a clustered `-lc x`, the attached
// `-cx`, and unambiguous long abbreviations (`--comm x`, `--sess=x`).
// An unrecognized long option could abbreviate an operand-carrying
// option the walk doesn't know — its following literal word is a payload
// candidate too. `--` does NOT end the scan: it can itself be an option
// operand (`script -O --` names the logfile `--` — Codex P1 on this
// head), and even a real terminator's tail is handed to the shell that
// decodes `-c` itself, so payload spellings are collected anywhere.
func runnerPayloads(words []*syntax.Word) []string {
	var payloads []string
	for i := 0; i < len(words); i++ {
		literal, ok := launcherLiteralWord(words[i])
		if !ok {
			continue
		}
		if literal == "--" {
			continue
		}
		if literal == "-c" {
			// The next word is a candidate payload — but it is NOT
			// consumed: a previous option may have eaten THIS `-c` as
			// its operand (`su -c -c 'payload'` — the second `-c` is
			// the real option), so every candidate word is also
			// re-scanned as a possible option spelling itself and every
			// candidate payload is judged (Codex P1 on this head).
			if i+1 < len(words) {
				if l, ok2 := launcherLiteralWord(words[i+1]); ok2 {
					payloads = append(payloads, l)
				}
			}
			continue
		}
		if strings.HasPrefix(literal, "--") {
			opt, val, hasVal := strings.Cut(literal, "=")
			if isRunnerCommandOption(opt) {
				if hasVal {
					payloads = append(payloads, val)
				} else if i+1 < len(words) {
					if l, ok2 := launcherLiteralWord(words[i+1]); ok2 {
						payloads = append(payloads, l)
					}
				}
				continue
			}
			// Unknown long option — its operand (if it takes one) could
			// be a script: judge the next literal word as a payload.
			if i+1 < len(words) {
				if l, ok2 := launcherLiteralWord(words[i+1]); ok2 {
					payloads = append(payloads, l)
				}
			}
			continue
		}
		if strings.HasPrefix(literal, "-") && len(literal) > 1 {
			if idx := strings.IndexByte(literal[1:], 'c'); idx >= 0 {
				if rest := literal[2+idx:]; rest != "" {
					payloads = append(payloads, rest)
				} else if i+1 < len(words) {
					if l, ok2 := launcherLiteralWord(words[i+1]); ok2 {
						payloads = append(payloads, l)
					}
				}
			}
		}
	}
	return payloads
}

// payloadReachesAgentProgram parses a `-c` script the way the shell will
// and applies the reset arm's agent scan to every call's decoded COMMAND
// word — `co""dex` reaches `codex` the same as a bare spelling, while
// `psql codex` does not because `codex` is psql's operand, not the
// program. preserved carries the outer launcher's preserved-name set so
// `sudo --preserve-env=CODEX_HOME su -c 'codex'` does not misjudge a
// still-pinned agent as reset.
// reachWorkLimit is the total literalize-and-parse budget one
// validation's reset-reach exploration may spend. The reset-reach
// judgement re-enters itself through nested launcher words — a named
// launcher's tail words re-enter the walk and reach another launcher's
// tail — so the bound is a TOTAL across the recursion, not only the
// #7610 depth cap: each literalize and each tail scan spends, each
// named-launcher word verdict is memoized once in the shared map, and
// spending past the limit is undecodable, which means refuse.
const reachWorkLimit = 16384

// reachBudget is the shared work bound for one validation's reset-reach
// judgements — carried on operandTailMemo so every launcher arm and
// buried suffix in the same command draws on it. namedWords memoizes the
// closure rule's per-word verdicts: a buried launcher's suffix
// judgements rescan the same tail words (ssh … ssh make), and without
// the memo each rescan pays a literalize per position per word —
// quadratic. wordLiterals memoizes the literalize itself so a nested
// launcher scan over the same words never re-pays it, and
// namedLiterals memoizes per-text verdicts for the pseudo-words nested
// scans split out of already-literal text.
type reachBudget struct {
	left          int
	seen          map[string]bool
	namedWords    map[namedWordKey]namedWordVerdict
	namedLiterals map[namedLiteralKey]namedWordVerdict
	wordLiterals  map[*syntax.Word]wordLiteral
}

// wordLiteral is a word's memoized literalize result — text and whether
// literalizing succeeded.
type wordLiteral struct {
	text string
	ok   bool
}

// newReachBudget builds the shared bound with every memo map live.
func newReachBudget() *reachBudget {
	return &reachBudget{
		left:          reachWorkLimit,
		seen:          map[string]bool{},
		namedWords:    map[namedWordKey]namedWordVerdict{},
		namedLiterals: map[namedLiteralKey]namedWordVerdict{},
		wordLiterals:  map[*syntax.Word]wordLiteral{},
	}
}

// literalWordBudgeted literalizes a word once per validation — the
// nested-launcher scans and the suffix walks revisit the same word
// nodes, and literalizing is the counted work the linear test bounds.
func literalWordBudgeted(word *syntax.Word, b *reachBudget) (string, bool) {
	if b != nil && b.wordLiterals != nil {
		if lit, seen := b.wordLiterals[word]; seen {
			return lit.text, lit.ok
		}
	}
	lit, ok := launcherLiteralWord(word)
	if b != nil && b.wordLiterals != nil {
		b.wordLiterals[word] = wordLiteral{text: lit, ok: ok}
	}
	return lit, ok
}

// spend charges n visits; a false return means the budget is spent and
// the judgement is undecodable — callers return the refusal.
func (b *reachBudget) spend(n int) bool {
	b.left -= n
	return b.left >= 0
}

func payloadReachesAgentProgram(script string, names, preserved map[string]struct{}, b *reachBudget) bool {
	if b == nil {
		b = newReachBudget()
	}
	if b.seen[script] {
		// Judged already in this validation — a candidate that reached
		// the agent refused then, so re-answering clean is faithful.
		return false
	}
	b.seen[script] = true
	if !b.spend(len(script)) {
		return true
	}
	for _, variant := range []syntax.LangVariant{syntax.LangPOSIX, syntax.LangBash} {
		file, err := syntax.NewParser(syntax.Variant(variant)).Parse(strings.NewReader(script), "")
		if err != nil {
			return true // can't prove the payload is agent-free — refuse
		}
		reaches := false
		syntax.Walk(file, func(node syntax.Node) bool {
			call, ok := node.(*syntax.CallExpr)
			if !ok || len(call.Args) == 0 {
				return true
			}
			if !b.spend(len(call.Args)) {
				reaches = true
				return false
			}
			literal, ok := launcherLiteralWord(call.Args[0])
			if !ok {
				return true // unprovable command word — the mutation scan judges it
			}
			if resetProgramWordReachesAgent(literal, call.Args[1:], names, preserved, b) {
				reaches = true
				return false
			}
			return true
		})
		if reaches {
			return true
		}
	}
	return false
}

// unwrapScript judges util-linux script: `-c`'s payload is a shell script
// the child decodes, so it delegates to the command-runner judgment and
// the remaining argv (output files) gets the argv-position scan.
func unwrapScript(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	return unwrapCommandRunner(words, names, memo, requireCommand)
}

// suResetsEnvironment reports whether the invocation asks for a login
// environment (`-`, `-l`, `--login`, or `l` inside a single-dash cluster
// like `-lp`). util-linux su's precedence makes the login form WIN over
// -m/-p/--preserve-environment (it prints "ignoring --preserve-environment"),
// so a preservation request never cancels the reset — and su's getopt
// permutes options ahead of the USER operand, so the flag counts wherever
// it sits in the argv (Codex P1 on this head). getopt_long also accepts
// unambiguous abbreviations, and `--login` is su's only long option
// starting with l, so `--l`, `--lo`, `--log`, … all spell it (Codex P2).
func suResetsEnvironment(words []*syntax.Word) bool {
	for _, word := range words {
		literal, ok := launcherLiteralWord(word)
		if !ok {
			continue
		}
		if literal == "-" {
			return true
		}
		if strings.HasPrefix(literal, "-") && !strings.HasPrefix(literal, "--") &&
			strings.Contains(literal[1:], "l") {
			return true
		}
		if len(literal) > 2 && strings.HasPrefix("--login", literal) {
			return true
		}
	}
	return false
}

// commandRunnerPrograms name the launchers whose argv can carry a shell
// payload — their `-c` operand decodes AFTER an outer launcher's reset
// dropped the pane's pins, so `sudo su -c 'co""dex' root` reaches codex
// even though `co""dex` isn't a literal agent word in sudo's tail (Codex
// P2 on this head). #7603's named launchers are absent: their tails get
// the closure text rule — every piece judged by name — rather than a
// payload location.
var commandRunnerPrograms = map[string]struct{}{
	"su": {}, "runuser": {}, "script": {},
}

// commandRunnerProgram reports whether base names a command-string runner —
// a table entry or a shell, matched by what the program IS (the r-/versioned
// classifier — #7603), so `rbash -c 'codex'` and `bash5 -c 'codex'` decode
// for reach exactly the way `bash -c 'codex'` does.
func commandRunnerProgram(base string) bool {
	if _, ok := commandRunnerPrograms[base]; ok {
		return true
	}
	_, ok := shellBaseName(base)
	return ok
}

// resetPeelableWrappers name the words whose argv contains the real
// program position somewhere the gate can't locate without that tool's
// option grammar — argument wrappers exec their tail (`timeout 5
// codex`, `taskset 0x1 codex`, `chroot /x codex` all carry an operand
// BEFORE the program), and the dispatching shell builtins do the same
// inside a decoded `-c` payload (in outer argv, where sudo cannot exec
// a builtin, the same recursion merely over-refuses — the safe
// direction). Their whole tail is a dark region, so they get the
// ambiguous scan. The container CLIs are absent: a docker operand is a
// subcommand or image name, never the pane's program — and further
// exec-wrappers the gate does not model are #7603 territory.
var resetPeelableWrappers = map[string]struct{}{
	"env": {}, "nice": {}, "nohup": {}, "timeout": {}, "setsid": {},
	"stdbuf": {}, "ionice": {}, "taskset": {}, "strace": {}, "xargs": {},
	"chroot": {}, "doas": {}, "setpriv": {}, "nsenter": {}, "unshare": {},
	"chrt": {}, "numactl": {}, "firejail": {}, "watch": {}, "time": {},
	"exec": {}, "command": {}, "builtin": {}, "eval": {},
}

// tailReachesResetAgentProgram reports whether the account's OWN agent
// program sits at a PROGRAM position of a resetting launcher's tail —
// the "reach" narrowing from the b531f5e00 review. The program word is
// the first literal that is not a provable option word, a provable
// option's operand or a NAME=value environment prefix: `sudo -u root
// codex` reaches codex, while `sudo npm i -g @openai/codex` does not —
// npm's package operand is not a program. Once an option word can't be
// classified the region is dark — a following word could be its operand
// OR the command — so every later non-dash literal is a candidate.
// Names that merely contain the agent's name are not reach either: the
// agent must be the word's FINAL path component (`codex`,
// `/usr/bin/codex`, `./codex`), and only the account's own agent
// counts — `sudo claude` under a codex account is claude's reset, not
// this account's re-root. A non-literal word is skipped — the lexical
// scan already refused the command for carrying it.
func tailReachesResetAgentProgram(words []*syntax.Word, names map[string]struct{}, preserved map[string]struct{}, preserveAll bool, operandOptions, knownFlags map[string]bool, b *reachBudget) bool {
	if preserveAll {
		return false
	}
	for i := 0; i < len(words); i++ {
		literal, ok := launcherLiteralWord(words[i])
		if !ok {
			continue
		}
		if strings.HasPrefix(literal, "-") {
			switch {
			case literal == "--":
				// The first word after `--` is still the command.
			case operandOptions[literal]:
				i++ // a provable option operand — never the program
			case knownFlags[literal] || strings.Contains(literal, "="):
				// Provably bare flag or `=`-joined operand — keep going.
			default:
				return ambiguousResetTailAgentReach(words[i+1:], names, preserved, b)
			}
			continue
		}
		if strings.Contains(literal, "=") {
			continue // NAME=value environment prefix — never a program
		}
		return resetProgramWordReachesAgent(literal, words[i+1:], names, preserved, b)
	}
	return false
}

// resetProgramWordReachesAgent applies the reach check to one resolved
// PROGRAM word and the argv after it: the account's agent name as the
// word's final path component reaches outright; a wrapper, launcher or
// runner name recurses into its own tail the same way; anything else is
// a fixed program whose operands are not program positions.
func resetProgramWordReachesAgent(literal string, rest []*syntax.Word, names, preserved map[string]struct{}, b *reachBudget) bool {
	// The program word is an operand the launcher reads — the same
	// flat normalization the piece rule applies, so an escaped or
	// quote-carried exec name cannot hide (#7627).
	literal = normalizeNamedLiteral(literal)
	if wordNamesResetAgent(literal, names, preserved) {
		return true
	}
	base := literal
	if i := strings.LastIndexByte(base, '/'); i >= 0 {
		base = base[i+1:]
	}
	if _, ok := resetPeelableWrappers[base]; ok {
		// The wrapper's argv can't be parsed without its own option
		// grammar — `chroot /x codex` carries a DIRECTORY operand before
		// the program — so the whole tail is a dark region.
		return ambiguousResetTailAgentReach(rest, names, preserved, b)
	}
	if _, ok := commandRunnerPrograms[base]; ok {
		if !b.spend(len(rest)) {
			return true
		}
		payloads, undecodable := runnerPayloadCandidates(base, rest)
		if undecodable {
			return true
		}
		for _, payload := range payloads {
			if payloadReachesAgentProgram(payload, names, preserved, b) {
				return true
			}
		}
		return ambiguousResetTailAgentReach(rest, names, preserved, b)
	}
	if _, ok := shellBaseName(base); ok {
		payloads, undecodable := runnerPayloadCandidates(base, rest)
		if undecodable {
			return true
		}
		for _, payload := range payloads {
			if payloadReachesAgentProgram(payload, names, preserved, b) {
				return true
			}
		}
		return ambiguousResetTailAgentReach(rest, names, preserved, b)
	}
	switch base {
	case "sudo":
		return tailReachesResetAgentProgram(rest, names, preserved, false, sudoOperandOptions, sudoFlags, b)
	case "systemd-run", "run0":
		return tailReachesResetAgentProgram(rest, names, preserved, false, systemdRunOperandOptions, systemdRunFlags, b)
	case "bwrap", "systemctl", "dbus-update-activation-environment":
		return tailReachesResetAgentProgram(rest, names, preserved, false, nil, nil, b)
	}
	if _, ok := accountNamedLauncherNames[base]; ok {
		// The closure rule: every piece of the named launcher's tail is
		// judged by name — no payload location, no option grammar.
		return namedLauncherTailRefuses(rest, names, preserved, base, b)
	}
	return false
}

// ambiguousResetTailAgentReach is the dark-region half of the reach
// check: when the option walk cannot prove which word is the command,
// every literal is a candidate program word, so each one's final path
// component is checked and a command-runner word's decoded payloads are
// scanned under the same reset.
func ambiguousResetTailAgentReach(words []*syntax.Word, names, preserved map[string]struct{}, b *reachBudget) bool {
	done := make(map[string]bool)
	for i, word := range words {
		if !b.spend(1) {
			return true
		}
		literal, ok := launcherLiteralWord(word)
		if !ok {
			continue
		}
		literal = normalizeNamedLiteral(literal)
		if wordNamesResetAgent(literal, names, preserved) {
			return true
		}
		base := literal
		if idx := strings.LastIndexByte(base, '/'); idx >= 0 {
			base = base[idx+1:]
		}
		if _, named := accountNamedLauncherNames[base]; named && !done["named:"+base] {
			done["named:"+base] = true
			// A named launcher's tail gets the closure text rule — its
			// whole suffix judged once per family, never re-scanned per
			// later word.
			if namedLauncherTailRefuses(words[i+1:], names, preserved, base, b) {
				return true
			}
		}
		if !commandRunnerProgram(base) || done[base] {
			continue
		}
		done[base] = true
		// One collector call per runner family per tail — the first
		// head's scan already literalizes the whole
		// suffix and a later same-name word's tail is covered inside
		// those candidates' own parses (the same family dedupe
		// callRunnerPayloadCandidates keeps). Without it N runner
		// words rescan N tails — the quadratic the shared reach
		// budget's memoization removes.
		if !b.spend(len(words) - i) {
			return true
		}
		payloads, undecodable := runnerPayloadCandidates(base, words[i+1:])
		if undecodable {
			return true
		}
		for _, payload := range payloads {
			if payloadReachesAgentProgram(payload, names, preserved, b) {
				return true
			}
		}
	}
	return false
}

// runnerPayloadCandidates picks the payload collector for a command-string
// runner: su/runuser/script take util-linux's getopt spellings (runnerPayloads
// reads attached `-cX` and `--command` forms), while the shells attach no
// operand to -c — `sh -ce 'x'` runs `x`, not `e` — so a shell word hands its
// tail to shellDashCPayloadCandidates (Codex P1 on this head). The #7603
// named launchers are absent: their tails get the closure text rule, not a
// payload location. A collector's undecodable mark refuses wherever a
// payload would be judged.
func runnerPayloadCandidates(base string, words []*syntax.Word) ([]string, bool) {
	if _, ok := shellBaseName(base); ok {
		return shellDashCPayloadCandidates(words), false
	}
	return runnerPayloads(words), false
}

// wordNamesResetAgent reports whether literal's final path component is
// THIS account's agent program — the agent whose config variable the
// denied set pins — so `codex` matches `codex`, `/usr/bin/codex` and
// `./codex`, while `~/.codex`, `/etc/codex/config.toml`, `codex-proxy`,
// `claude-squad` and the OTHER agents' names do not.
func wordNamesResetAgent(literal string, names, preserved map[string]struct{}) bool {
	literal = normalizeNamedLiteral(literal)
	for agent, configVar := range accountConfigVars {
		if _, pinned := names[configVar]; !pinned {
			continue
		}
		if _, kept := preserved[configVar]; kept {
			continue
		}
		if literal == agent || strings.HasSuffix(literal, "/"+agent) {
			return true
		}
	}
	return false
}
