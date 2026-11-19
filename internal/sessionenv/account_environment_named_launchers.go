package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// This file models #7603's named launchers — the reset-class programs,
// command-string runners and environment writers whose grammars the gate
// knows by name. Anything still unmodelled is judged exactly as master and
// named in the gate's stated limit (docs/usage-limits.md) — there is no
// generic scan over arbitrary commands.
//
// The review rule on this head (the third Codex round) is that the
// verdict must not depend on parsing a launcher's options: option tables
// keep losing to the next unmodelled spelling, so they were removed.
// Instead EVERY reading of a launcher's tail is judged:
//
//   - every literal word is a possible start of the command — a word
//     whose final path component or identifier-boundary text names the
//     account's agent refuses, and a word spelling a protected name
//     refuses as a possible environment write;
//   - a word carrying shell syntax is a possible command string of its
//     own, and is judged by the full gate / the reset reach;
//   - the whole tail is judged twice — argv-preserved (each literal
//     quoted, so `'/opt/agent tools/codex' --help` still names codex)
//     and joined the way joiners build their `sh -c` string;
//   - tmux's `;`-separated command sequence is judged per segment;
//   - a word that does not literalize, or spells a hidden-input source
//     (a file, a descriptor, a language the gate does not parse),
//     refuses outright.
//
// It admits only when no reading can refuse — the corpus ADMIT rows
// carry neither the agent's name nor a protected name in any reading,
// so they pass without option tables. Work stays linear: one pass over
// the literals, a bounded candidate set, and the existing gate for each
// candidate.

// namedLauncherCandidateCap bounds the command-string candidates a tail
// produces: more shell-carrying words than this cannot be proven inside
// the gate's linear budget, so the tail refuses.
const namedLauncherCandidateCap = 64

// ---------- shared readings ----------

// literalReadingCandidates collects the command-string candidates a
// literal tail can execute: the argv-preserved spelling (every literal
// quoted — direct exec), the joined spelling (what `sh -c` receives
// when the launcher joins), and each literal that carries shell syntax
// of its own. The second return is ok — false means more candidates
// than the linear budget allows, which refuses.
func literalReadingCandidates(literals []string) ([]string, bool) {
	candidates := make([]string, 0, 4)
	seen := make(map[string]bool)
	push := func(candidate string) bool {
		if candidate == "" || seen[candidate] {
			return true
		}
		if len(candidates) >= namedLauncherCandidateCap {
			return false
		}
		seen[candidate] = true
		candidates = append(candidates, candidate)
		return true
	}
	if !push(quoteTailLiterals(literals)) || !push(joinTailLiterals(literals)) {
		return nil, false
	}
	for _, literal := range literals {
		if literalCarriesShellSyntax(literal) && !push(literal) {
			return nil, false
		}
		// The VALUE part of an attached option is a possible command
		// start of its own — ssh's `-o` operand `RemoteCommand=co""dex`
		// executes through the user's shell with the quotes the
		// whole-word boundary scan sees through. `--opt=VALUE` splits
		// on the first `=`; a single-dash word's attached value is
		// every proper suffix after the first option letter
		// (`-oVALUE`, and `-abVALUE` where `-a` is a flag and `-b`
		// takes the value — any reading counts).
		if eq := strings.IndexByte(literal, '='); eq >= 0 {
			if !push(literal[eq+1:]) {
				return nil, false
			}
		}
		if strings.HasPrefix(literal, "-") && !strings.HasPrefix(literal, "--") {
			for i := 2; i < len(literal); i++ {
				if !push(literal[i:]) {
					return nil, false
				}
			}
		}
	}
	return candidates, true
}

// namedResetLiteralRefuses applies the per-word reading every named
// launcher shares: the word as a possible program (its final path
// component or boundary text naming the account's agent, honouring
// `preserved` the way the reach check does — a `sudo -E` tail that
// keeps the config pin does not re-root), a possible protected-name
// write, and a possible hidden-input source.
func namedResetLiteralRefuses(literal string, names, preserved map[string]struct{}, hidden []string) bool {
	if wordNamesResetAgent(literal, names, preserved) {
		return true
	}
	if textNamesAgentBoundary(literal, names, preserved) {
		return true
	}
	if launcherSegmentsDenied(literal, names, false) {
		return true
	}
	return literalNamesHiddenInput(literal, hidden)
}

// textNamesAgentBoundary reports whether text names the account's agent
// at an identifier boundary — the same rule pwsh/nu payloads use —
// skipping an agent whose config pin survives through `preserved`.
func textNamesAgentBoundary(text string, names, preserved map[string]struct{}) bool {
	for agent, configVar := range accountConfigVars {
		if _, pinned := names[configVar]; !pinned {
			continue
		}
		if _, kept := preserved[configVar]; kept {
			continue
		}
		if textNamesIdentifierBoundary(text, agent) {
			return true
		}
	}
	return false
}

// namedLauncherResetReach judges a reset-class launcher's tail under
// every reading: each literal word refuses by the shared per-word rule,
// and every command-string candidate — argv-preserved, joined, and each
// shell-carrying word — gets the reset reach check. It is also the reach
// half of the outer-launcher walk (`sudo -E pkexec codex` reaches the
// same judgement under sudo's preserved set).
func namedLauncherResetReach(words []*syntax.Word, names, preserved map[string]struct{}, launcher string, b *reachBudget) bool {
	if b == nil {
		b = &reachBudget{left: reachWorkLimit, seen: map[string]bool{}}
	}
	if !b.spend(len(words)) {
		return true
	}
	literals, ok := launcherTailLiterals(words)
	if !ok {
		return true
	}
	hidden := namedLauncherHiddenWords[launcher]
	for _, literal := range literals {
		if namedResetLiteralRefuses(literal, names, preserved, hidden) {
			return true
		}
	}
	candidates, ok := literalReadingCandidates(literals)
	if !ok {
		return true
	}
	for _, candidate := range candidates {
		if payloadReachesAgentProgram(candidate, names, preserved, b) {
			return true
		}
	}
	return false
}

// namedRunnerTailPayloads returns the command-string candidates a
// command-string runner's tail can execute — the same any-reading set —
// for the runner payload path. tmux's `;`-separated commands are
// collected per segment so the `;` separator itself is never a
// candidate. Undecodable propagates a refusal.
func namedRunnerTailPayloads(words []*syntax.Word, launcher string) ([]string, bool) {
	literals, ok := launcherTailLiterals(words)
	if !ok {
		return nil, true
	}
	if launcher != "tmux" {
		candidates, ok := literalReadingCandidates(literals)
		return candidates, !ok
	}
	var all []string
	seen := make(map[string]bool)
	for segment := range splitTmuxSegments(literals) {
		candidates, candOK := literalReadingCandidates(segment)
		if !candOK {
			return nil, true
		}
		for _, candidate := range candidates {
			if !seen[candidate] {
				seen[candidate] = true
				all = append(all, candidate)
			}
		}
	}
	return all, false
}

// ---------- launcher arms ----------

// unwrapNamedResetLauncher judges a reset-class launcher — pkexec, doas,
// setpriv, flatpak-spawn, ssh, machinectl, nix-shell, nix, distrobox,
// toolbox, and tmux's pane creators — under the any-reading rule. The
// generic env-injection and hidden-tail scans run first; then every
// literal word and command-string candidate gets the reach check.
func unwrapNamedResetLauncher(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool, launcher string) ([]*syntax.Word, bool) {
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand) {
		return nil, true
	}
	if wrapperTailHidesAccountAssignment(words, false, names, memo) {
		return nil, true
	}
	return nil, namedLauncherResetReach(words, names, nil, launcher, memo.reach)
}

// unwrapPkexec judges pkexec: always reset-class — the child runs under
// a polkit-checked environment, never the pane's.
func unwrapPkexec(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	return unwrapNamedResetLauncher(words, names, memo, requireCommand, "pkexec")
}

// unwrapDoas judges doas: always reset-class.
func unwrapDoas(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	return unwrapNamedResetLauncher(words, names, memo, requireCommand, "doas")
}

// unwrapSetpriv judges setpriv. --reset-env makes it reset-class; under
// the any-reading rule the arm does not need to prove the flag — a
// program or payload word that reaches the agent refuses either way,
// which covers --reset-env and every spelling of it.
func unwrapSetpriv(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	return unwrapNamedResetLauncher(words, names, memo, requireCommand, "setpriv")
}

// unwrapFlatpakSpawn judges flatpak-spawn: every spawn is reset-class —
// the child runs in the sandbox's environment either way.
func unwrapFlatpakSpawn(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	return unwrapNamedResetLauncher(words, names, memo, requireCommand, "flatpak-spawn")
}

// unwrapSSH judges ssh: the remote environment is never the pane's, so it
// is always reset-class — every word a possible start of the remote
// command, the tail joined the way the remote shell reads it.
func unwrapSSH(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	return unwrapNamedResetLauncher(words, names, memo, requireCommand, "ssh")
}

// unwrapMachinectl judges machinectl: shell/login run the remote command
// under the container's environment — reset-class.
func unwrapMachinectl(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	return unwrapNamedResetLauncher(words, names, memo, requireCommand, "machinectl")
}

// unwrapNixShell judges nix-shell: --pure drops the caller's environment
// (reset-class under the any-reading rule — the reach check needs no
// flag proof), and --run/--command carry a command string.
func unwrapNixShell(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	return unwrapNamedResetLauncher(words, names, memo, requireCommand, "nix-shell")
}

// unwrapNix judges `nix`: the develop/shell -i form resets and -c
// carries the command — reset-class under the same reading.
func unwrapNix(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	return unwrapNamedResetLauncher(words, names, memo, requireCommand, "nix")
}

// unwrapDistrobox judges distrobox enter/run: the container's
// environment is not the pane's — reset-class.
func unwrapDistrobox(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	return unwrapNamedResetLauncher(words, names, memo, requireCommand, "distrobox")
}

// unwrapToolbox judges toolbox: enter/run run in the container's
// environment — reset-class.
func unwrapToolbox(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	return unwrapNamedResetLauncher(words, names, memo, requireCommand, "toolbox")
}

// unwrapTmux judges tmux: its argv is a command SEQUENCE — `;`-literal
// words separate commands (`tmux list-sessions \; new-window codex`
// still runs codex), so every segment between `;` words gets the
// any-reading judgement: a pane-creating word makes the segment's
// command reset-class, setenv/set-environment literals write the
// server's environment through the shared protected-name check, and a
// word naming the agent anywhere in the sequence refuses.
func unwrapTmux(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand) {
		return nil, true
	}
	if wrapperTailHidesAccountAssignment(words, false, names, memo) {
		return nil, true
	}
	return nil, tmuxResetReach(words, names, nil, memo.reach)
}

// tmuxResetReach is the reach half of tmux's any-reading judgement —
// every `;`-separated command segment judged the same way a named
// reset-class tail is.
func tmuxResetReach(words []*syntax.Word, names, preserved map[string]struct{}, b *reachBudget) bool {
	if b == nil {
		b = &reachBudget{left: reachWorkLimit, seen: map[string]bool{}}
	}
	if !b.spend(len(words)) {
		return true
	}
	literals, ok := launcherTailLiterals(words)
	if !ok {
		return true
	}
	for segment := range splitTmuxSegments(literals) {
		for _, literal := range segment {
			if namedResetLiteralRefuses(literal, names, preserved, namedLauncherHiddenWords["tmux"]) {
				return true
			}
		}
		candidates, candOK := literalReadingCandidates(segment)
		if !candOK {
			return true
		}
		for _, candidate := range candidates {
			if payloadReachesAgentProgram(candidate, names, preserved, b) {
				return true
			}
		}
	}
	return false
}

// splitTmuxSegments yields the argv literal runs between `;` words —
// each run is one command in tmux's command sequence.
func splitTmuxSegments(literals []string) func(func([]string) bool) {
	return func(yield func([]string) bool) {
		start := 0
		for i, literal := range literals {
			if literal == ";" || literal == "\\;" {
				if !yield(literals[start:i]) {
					return
				}
				start = i + 1
			}
		}
		yield(literals[start:])
	}
}

// unwrapWatch judges watch: it keeps the pane's environment (no reset)
// but hands its argv, joined, to `sh -c` — a command-string runner whose
// any-reading candidates the top-level gate judges.
func unwrapWatch(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	return unwrapCommandRunner(words, names, memo, requireCommand)
}

// unwrapPwshNu judges pwsh and nu — runners whose payload grammar the
// gate does NOT parse (approved rule on #7603): the option universe
// classifies input sources — hidden input (-File, -EncodedCommand/-enc
// prefixes, `-` stdin, `-Command -`, -NoExit, interactive forms,
// nu's --config/-e REPL forms) refuses, a script-file positional
// refuses, a pure info flag admits — and the joined tail text refuses
// when it names a protected variable or the account's agent at
// identifier boundaries.
func unwrapPwshNu(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool, options map[string]pwshNuOptionKind, abbrev bool) ([]*syntax.Word, bool) {
	payloads, positional, info, undecodable := pwshNuClassify(words, options, abbrev)
	if undecodable || positional {
		return nil, true
	}
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand) {
		return nil, true
	}
	if wrapperTailHidesAccountAssignment(words, false, names, memo) {
		return nil, true
	}
	literals, ok := launcherTailLiterals(words)
	if !ok {
		return nil, true
	}
	if len(payloads) == 0 {
		// No command string: a bare `pwsh`/`nu` is the interactive
		// session — input the gate cannot see. Only a pure info flag
		// (-Version/-Help) runs without input.
		return nil, !info
	}
	// The joined tail is the text the runner could hand to its command
	// parser — the boundary rule scans it once, covering every payload
	// operand and pwsh -Command's whole remaining argv.
	if pwshNuPayloadNamesBoundary(joinTailLiterals(literals), names) {
		return nil, true
	}
	for _, payload := range payloads {
		if pwshNuPayloadNamesBoundary(payload, names) {
			return nil, true
		}
	}
	return nil, false
}

// pwshNuPayloadNamesBoundary reports whether the payload text names a
// denied environment variable or the account's own agent program at an
// identifier boundary — the only judgement the gate can make over a
// command grammar it does not parse.
func pwshNuPayloadNamesBoundary(text string, names map[string]struct{}) bool {
	for name := range names {
		if textNamesIdentifierBoundary(text, name) {
			return true
		}
	}
	for agent, configVar := range accountConfigVars {
		if _, pinned := names[configVar]; !pinned {
			continue
		}
		if textNamesIdentifierBoundary(text, agent) {
			return true
		}
	}
	return false
}

// textNamesIdentifierBoundary reports whether name appears in text
// bounded on both sides by bytes that are not identifier bytes — the
// boundary `echo CODEX_HOME` satisfies and `CODEX_HOME_DIR` does not.
func textNamesIdentifierBoundary(text, name string) bool {
	for i := 0; i+len(name) <= len(text); i++ {
		if text[i:i+len(name)] != name {
			continue
		}
		if i > 0 && isEnvNameByte(text[i-1]) {
			continue
		}
		if i+len(name) < len(text) && isEnvNameByte(text[i+len(name)]) {
			continue
		}
		return true
	}
	return false
}

// pwshNuPayloads/nuPayloads collect the runner payload path's candidates —
// the classified payload operands, with undecodable propagating the refusal.
func pwshNuPayloads(words []*syntax.Word) ([]string, bool) {
	payloads, _, _, undecodable := pwshNuClassify(words, pwshOptions, true)
	return payloads, undecodable
}

func nuPayloads(words []*syntax.Word) ([]string, bool) {
	payloads, _, _, undecodable := pwshNuClassify(words, nuOptions, false)
	return payloads, undecodable
}

// unwrapBusctl judges busctl and gdbus: a call/set-property verb naming
// a SetEnvironment-family method plus a protected-name argument writes
// the service manager's environment — the writer rule of issue item 3.
// Under the any-reading rule every literal is a possible argument to
// that write, so the protected-name check runs per literal; gdbus
// parses its operands as GVariant text, so each literal is decoded
// through its escapes first — an escape the decoder does not know
// makes the invocation undecodable, which means refuse.
func unwrapBusctl(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool, decode bool) ([]*syntax.Word, bool) {
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand) {
		return nil, true
	}
	if wrapperTailHidesAccountAssignment(words, false, names, memo) {
		return nil, true
	}
	literals, ok := launcherTailLiterals(words)
	if !ok {
		return nil, true
	}
	for _, literal := range literals {
		if decode {
			decoded, decOK := gvariantDecode(literal)
			if !decOK {
				return nil, true
			}
			literal = decoded
		}
		if launcherSegmentsDenied(literal, names, false) {
			return nil, true
		}
	}
	return nil, false
}
