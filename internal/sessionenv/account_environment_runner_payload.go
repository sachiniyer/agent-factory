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
	for _, payload := range payloads {
		if commandMutatesAccountEnvironment(payload, names) {
			return nil, true
		}
	}
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand) {
		return nil, true
	}
	if wrapperTailHidesAccountAssignment(words, false, names, memo) {
		return nil, true
	}
	if suResetsEnvironment(words) {
		if tailReachesResetAgentProgram(words, nil, false) {
			return nil, true
		}
		for _, payload := range payloads {
			if payloadReachesAgentProgram(payload, nil) {
				return nil, true
			}
		}
	}
	return nil, false
}

// unwrapCommandRunner judges command-string runners — sh, bash, zsh,
// dash, ksh, script and their kin — whose `-c`/`--command` operand is a
// script the child decodes (`script -q -c 'export CODEX_""HOME=/other;
// codex'` exports the pin after the child's shell removed the quotes —
// Codex P1 on this head). Every payload candidate is re-judged as a
// command line; the rest of the argv gets the ordinary scans.
func unwrapCommandRunner(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	for _, payload := range runnerPayloads(words) {
		if commandMutatesAccountEnvironment(payload, names) {
			return nil, true
		}
	}
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand) {
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
// and applies the reset arm's agent scan to every command's decoded
// words — `co""dex` reaches `codex` the same as a bare spelling.
// preserved carries the outer launcher's preserved-name set so
// `sudo --preserve-env=CODEX_HOME su -c 'codex'` does not misjudge a
// still-pinned agent as reset.
func payloadReachesAgentProgram(script string, preserved map[string]struct{}) bool {
	for _, variant := range []syntax.LangVariant{syntax.LangPOSIX, syntax.LangBash} {
		file, err := syntax.NewParser(syntax.Variant(variant)).Parse(strings.NewReader(script), "")
		if err != nil {
			return true // can't prove the payload is agent-free — refuse
		}
		reaches := false
		syntax.Walk(file, func(node syntax.Node) bool {
			if call, ok := node.(*syntax.CallExpr); ok && tailReachesResetAgentProgram(call.Args, preserved, false) {
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
// P2 on this head).
var commandRunnerPrograms = map[string]struct{}{
	"su": {}, "runuser": {}, "script": {},
}

// tailReachesResetAgentProgram reports whether a literal word in words
// names an account-capable agent program at an identifier boundary while
// the invocation does not preserve that agent's identity variable. The
// roster is accountConfigVars (claude/codex/gemini — the only agents af
// can pin), so `sudo -u root codex`, `sudo -u root bash -lc codex` and
// `su -l -c 'codex' u` all reach it, while `sudo -u postgres psql` does
// not. A non-literal word is skipped — the lexical scan already refused
// the command for carrying it. A command-runner word nests the shell
// payload under the SAME reset, so the first one's `-c` operand is
// decoded and scanned for agent names too — runnerPayloads already
// sweeps the whole suffix, so one descent covers every later payload
// spelling without a quadratic per-runner rescan.
func tailReachesResetAgentProgram(words []*syntax.Word, preserved map[string]struct{}, preserveAll bool) bool {
	if preserveAll {
		return false
	}
	runnerSeen := false
	for i, word := range words {
		literal, ok := launcherLiteralWord(word)
		if !ok {
			continue
		}
		for agent, configVar := range accountConfigVars {
			if _, kept := preserved[configVar]; kept {
				continue
			}
			if launcherSegmentNames(literal, agent, false) {
				return true
			}
		}
		if runnerSeen {
			continue
		}
		for runner := range commandRunnerPrograms {
			if launcherSegmentNames(literal, runner, false) {
				runnerSeen = true
				for _, payload := range runnerPayloads(words[i+1:]) {
					if payloadReachesAgentProgram(payload, preserved) {
						return true
					}
				}
				break
			}
		}
	}
	return false
}
