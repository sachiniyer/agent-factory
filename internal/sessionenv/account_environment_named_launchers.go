package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// This file models #7603's named launchers — the reset-class programs,
// command-string runners and environment writers whose grammars the gate
// knows by name. Anything still unmodelled is judged exactly as master and
// named in the gate's stated limit (docs/usage-limits.md) — there is no
// generic scan over arbitrary commands. The option tables and the shared
// closed-grammar scanner live in
// account_environment_named_launcher_grammar.go; two properties govern
// every arm here (the review on this head):
//
//   - the judged payload is EXACTLY what the launcher executes — watch's
//     argv joined into the one `sh -c` string it builds, a GVariant `as`
//     operand decoded through its escapes, pwsh's hidden-input forms
//     refused even when a -c operand is also present;
//   - each launcher's option grammar is total — an option the grammar
//     does not know makes the invocation undecodable, which means refuse.

// ---------- payload collectors ----------
//
// Each collector returns the command-string candidates a launcher hands
// to a shell or a remote environment and whether the invocation decoded
// under the launcher's closed option grammar. An undecodable invocation
// refuses wherever a candidate would be judged.

// sshPayloads returns ssh's remote-command candidate: the words after the
// destination joined into the single command line the remote shell
// reads — the exact payload ssh builds.
func sshPayloads(words []*syntax.Word) ([]string, bool) {
	positionals, _, undecodable := launcherArgvScan(words, sshGrammar)
	if undecodable {
		return nil, true
	}
	if len(positionals) < 2 {
		return nil, false
	}
	return []string{joinScannedPositionals(positionals, 1)}, false
}

// watchPayloads returns watch's command-string candidate: every word
// after the option region joined into the one `sh -c` string watch
// builds — `watch export 'CODEX_""HOME=/o; codex'` decodes the export's
// joined name the same way the child's shell does.
func watchPayloads(words []*syntax.Word) ([]string, bool) {
	positionals, _, undecodable := launcherArgvScan(words, watchGrammar)
	if undecodable {
		return nil, true
	}
	if len(positionals) == 0 {
		return nil, false
	}
	return []string{joinScannedPositionals(positionals, 0)}, false
}

// tmuxPayloads returns tmux's command-string candidates: the global `-c`
// (default-command) operand and, for a pane-creating subcommand, the
// trailing words joined into the shell command the server runs.
func tmuxPayloads(words []*syntax.Word) ([]string, bool) {
	positionals, operands, undecodable := launcherArgvScan(words, tmuxGlobalGrammar)
	if undecodable || len(positionals) == 0 {
		return nil, undecodable
	}
	sub, ok := tmuxSubcommandAt(words, positionals)
	if !ok {
		return nil, true
	}
	if _, pane := tmuxPaneCreatingSubcommands[sub]; !pane {
		return nil, false
	}
	var payloads []string
	for _, operand := range operands {
		if operand.opt == "-c" {
			payloads = append(payloads, operand.literal)
		}
	}
	panePos, _, undec := launcherArgvScan(words[positionals[0].index+1:], tmuxPaneGrammar)
	if undec {
		return nil, true
	}
	if len(panePos) > 0 {
		payloads = append(payloads, joinScannedPositionals(panePos, 0))
	}
	return payloads, false
}

// tmuxSubcommandAt yields the subcommand word — the first positional.
func tmuxSubcommandAt(words []*syntax.Word, positionals []scannedOperand) (string, bool) {
	if len(positionals) == 0 {
		return "", false
	}
	return positionals[0].literal, true
}

// nixShellPayloads returns nix-shell's command-string candidates: the
// --run/--command operands in either spelling.
func nixShellPayloads(words []*syntax.Word) ([]string, bool) {
	_, operands, undecodable := launcherArgvScan(words, nixShellGrammar)
	if undecodable {
		return nil, true
	}
	var payloads []string
	for _, operand := range operands {
		if operand.opt == "--run" || operand.opt == "--command" {
			payloads = append(payloads, operand.literal)
		}
	}
	return payloads, false
}

// nixPayloads returns `nix develop`/`nix shell` command candidates: `-c`/
// `--command` takes the REST of the argv as the command words, joined —
// `--command=x` carries it attached.
func nixPayloads(words []*syntax.Word) ([]string, bool) {
	sawDev := -1
	for i, word := range words {
		literal, ok := launcherLiteralWord(word)
		if !ok {
			continue
		}
		if literal == "develop" || literal == "shell" {
			sawDev = i
			break
		}
	}
	if sawDev < 0 {
		return nil, false
	}
	// Find the -c boundary: the option consumes the rest of the argv.
	for i := sawDev + 1; i < len(words); i++ {
		literal, ok := launcherLiteralWord(words[i])
		if !ok {
			continue
		}
		if strings.HasPrefix(literal, "--command=") {
			if _, _, undec := launcherArgvScan(words[sawDev+1:i], nixDevGrammar); undec {
				return nil, true
			}
			return []string{literal[len("--command="):]}, false
		}
		if literal == "-c" || literal == "--command" || isLauncherLongAbbrev(literal, "--command", nixDevGrammar) {
			if _, _, undec := launcherArgvScan(words[sawDev+1:i], nixDevGrammar); undec {
				return nil, true
			}
			tail := literalTailWords(words[i+1:])
			if len(tail) < len(words)-i-1 {
				return nil, true // a non-literal command word hides the payload
			}
			if len(tail) == 0 {
				return nil, false
			}
			return []string{strings.Join(tail, " ")}, false
		}
	}
	// No -c word: the pre-command argv still must decode.
	_, _, undecodable := launcherArgvScan(words[sawDev+1:], nixDevGrammar)
	return nil, undecodable
}

// isLauncherLongAbbrev reports whether word is a unique getopt_long
// abbreviation of want inside the grammar — nix accepts `--comm` for
// --command.
func isLauncherLongAbbrev(word, want string, g launcherGrammar) bool {
	resolved, known := resolveLauncherLongOption(word, g)
	return known && resolved == want
}

// distroboxPayloads returns distrobox enter/run command-string
// candidates: the -e/--command operands and the post-`--`/positional
// command words joined.
func distroboxPayloads(words []*syntax.Word) ([]string, bool) {
	sawSub := -1
	for i, word := range words {
		literal, ok := launcherLiteralWord(word)
		if !ok {
			continue
		}
		if literal == "enter" || literal == "run" || literal == "create" {
			sawSub = i
			break
		}
	}
	if sawSub < 0 {
		return nil, false
	}
	positionals, operands, undecodable := launcherArgvScan(words[sawSub+1:], distroboxGrammar)
	if undecodable {
		return nil, true
	}
	var payloads []string
	for _, operand := range operands {
		if distroboxGrammar.payload[operand.opt] {
			payloads = append(payloads, operand.literal)
		}
	}
	// The container name is positional[0]; any later positional is a bare
	// command the container runs — joined the way the container shell
	// reads it.
	if len(positionals) > 1 {
		payloads = append(payloads, joinScannedPositionals(positionals, 1))
	}
	return payloads, false
}

// toolboxPayloads returns `toolbox run` command candidates: the trailing
// words after run's options, joined into the command line the container
// executes.
func toolboxPayloads(words []*syntax.Word) ([]string, bool) {
	sawRun := -1
	for i, word := range words {
		literal, ok := launcherLiteralWord(word)
		if !ok {
			continue
		}
		if literal == "run" {
			sawRun = i
			break
		}
	}
	if sawRun < 0 {
		return nil, false
	}
	positionals, _, undecodable := launcherArgvScan(words[sawRun+1:], toolboxGrammar)
	if undecodable {
		return nil, true
	}
	if len(positionals) == 0 {
		return nil, false
	}
	return []string{joinScannedPositionals(positionals, 0)}, false
}

// pwshNuPayloads returns the pwsh command-string operand for the reach
// decode — an undecodable invocation (encoded spelling, hidden input,
// unknown option, interactive form) refuses.
func pwshNuPayloads(words []*syntax.Word) ([]string, bool) {
	payload, has, positional, info, undecodable := pwshNuClassify(words, pwshOptions, true)
	if undecodable || positional {
		return nil, true
	}
	if !has {
		// An interactive session reads input the gate cannot see —
		// undecidable under a reset; only a pure info form proves safe.
		if info {
			return nil, false
		}
		return nil, true
	}
	return []string{payload}, false
}

// nuPayloads mirrors pwshNuPayloads for nu's exact option spellings.
func nuPayloads(words []*syntax.Word) ([]string, bool) {
	payload, has, positional, info, undecodable := pwshNuClassify(words, nuOptions, false)
	if undecodable || positional {
		return nil, true
	}
	if !has {
		if info {
			return nil, false
		}
		return nil, true
	}
	return []string{payload}, false
}

// literalTailWords renders every literal word in a tail; the returned
// length tells the caller whether any word was non-literal.
func literalTailWords(words []*syntax.Word) []string {
	var out []string
	for _, word := range words {
		if l, ok := launcherLiteralWord(word); ok {
			out = append(out, l)
		}
	}
	return out
}

// ---------- launcher arms ----------

// namedLauncherResetReach judges a reset-class launcher's tail under its
// closed grammar: an undecodable invocation refuses outright (property
// B), otherwise the first positional is the program and gets the reach
// check.
func namedLauncherResetReach(words []*syntax.Word, g launcherGrammar, names map[string]struct{}, preserved map[string]struct{}) bool {
	positionals, _, undecodable := launcherArgvScan(words, g)
	if undecodable {
		return true
	}
	if len(positionals) == 0 {
		return false
	}
	return resetProgramWordReachesAgent(positionals[0].literal, words[positionals[0].index+1:], names, preserved)
}

// unwrapAlwaysResetLauncher judges a launcher whose child's environment
// is never the pane's — pkexec, doas, flatpak-spawn, ssh, the tmux pane
// creators, toolbox/distrobox. The generic env-injection and hidden-tail
// scans run first, then the reset arm's reach check under the
// launcher's closed option grammar.
func unwrapAlwaysResetLauncher(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool, grammar launcherGrammar) ([]*syntax.Word, bool) {
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand) {
		return nil, true
	}
	if wrapperTailHidesAccountAssignment(words, false, names, memo) {
		return nil, true
	}
	return nil, namedLauncherResetReach(words, grammar, names, nil)
}

// unwrapPkexec judges pkexec: always reset-class, program is the first
// positional.
func unwrapPkexec(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	return unwrapAlwaysResetLauncher(words, names, memo, requireCommand, pkexecGrammar)
}

// unwrapDoas judges doas: always reset-class.
func unwrapDoas(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	return unwrapAlwaysResetLauncher(words, names, memo, requireCommand, doasGrammar)
}

// unwrapFlatpakSpawn judges flatpak-spawn: every spawn is reset-class —
// the child runs in the sandbox's environment either way. --env-fd is a
// hidden input source: its operand comes from a descriptor the command
// line cannot show.
func unwrapFlatpakSpawn(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	for _, word := range words {
		if literal, ok := launcherLiteralWord(word); ok &&
			(literal == "--env-fd" || strings.HasPrefix(literal, "--env-fd=")) {
			return nil, true
		}
	}
	return unwrapAlwaysResetLauncher(words, names, memo, requireCommand, flatpakSpawnGrammar)
}

// unwrapSetpriv judges setpriv: reset-class only under --reset-env — the
// other forms keep the caller's environment, so the plain argv scans
// apply. The option grammar decodes either way (an unknown option still
// makes the invocation undecidable).
func unwrapSetpriv(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand) {
		return nil, true
	}
	if wrapperTailHidesAccountAssignment(words, false, names, memo) {
		return nil, true
	}
	positionals, _, undecodable := launcherArgvScan(words, setprivGrammar)
	if undecodable {
		return nil, true
	}
	if !literalTailHasWord(words, "--reset-env") {
		return nil, false
	}
	if len(positionals) == 0 {
		return nil, false
	}
	return nil, resetProgramWordReachesAgent(positionals[0].literal, words[positionals[0].index+1:], names, nil)
}

// unwrapSSH judges ssh: the remote environment is never the pane's, so it
// is always reset-class. The remote command is every word after the
// destination joined — the exact payload ssh builds — and an option the
// grammar does not know makes the invocation undecodable.
func unwrapSSH(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand) {
		return nil, true
	}
	if wrapperTailHidesAccountAssignment(words, false, names, memo) {
		return nil, true
	}
	payloads, undecodable := sshPayloads(words)
	if undecodable {
		return nil, true
	}
	for _, payload := range payloads {
		if payloadReachesAgentProgram(payload, names, nil) {
			return nil, true
		}
	}
	return nil, false
}

// unwrapMachinectl judges machinectl shell/login: they run in the
// container's environment — reset-class — and shell's trailing words are
// the remote command line, judged by the same joined-payload reach ssh
// uses.
func unwrapMachinectl(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand) {
		return nil, true
	}
	if wrapperTailHidesAccountAssignment(words, false, names, memo) {
		return nil, true
	}
	return nil, machinectlShellLoginReaches(words, names, nil)
}

// machinectlShellLoginReaches applies the reach check to machinectl
// shell/login's remote command — the joined words after the host
// operand — under its closed grammar. Shared by the launcher arm and the
// reset-tail recursion so `sudo machinectl shell host codex` reaches the
// same way `machinectl shell host codex` does.
func machinectlShellLoginReaches(words []*syntax.Word, names, preserved map[string]struct{}) bool {
	positionals, _, undecodable := launcherArgvScan(words, machinectlGrammar)
	if undecodable {
		return true
	}
	if len(positionals) < 2 {
		return false
	}
	if positionals[0].literal != "shell" && positionals[0].literal != "login" {
		return false
	}
	payload := joinScannedPositionals(positionals, 2)
	if payload == "" {
		return false
	}
	return payloadReachesAgentProgram(payload, names, preserved)
}

// unwrapNixShell judges nix-shell: --pure resets (the reach check then
// applies to the --run/--command payloads), and every payload candidate
// is gated at the top level like the other runners.
func unwrapNixShell(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand) {
		return nil, true
	}
	if wrapperTailHidesAccountAssignment(words, false, names, memo) {
		return nil, true
	}
	payloads, undecodable := nixShellPayloads(words)
	if undecodable {
		return nil, true
	}
	if !literalTailHasWord(words, "--pure") {
		return nil, false
	}
	for _, payload := range payloads {
		if payloadReachesAgentProgram(payload, names, nil) {
			return nil, true
		}
	}
	return nil, false
}

// unwrapNix judges `nix develop`/`nix shell`: -i/--ignore-environment
// resets, and the -c/--command words are the payload.
func unwrapNix(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand) {
		return nil, true
	}
	if wrapperTailHidesAccountAssignment(words, false, names, memo) {
		return nil, true
	}
	payloads, undecodable := nixPayloads(words)
	if undecodable {
		return nil, true
	}
	resets := false
	for _, word := range words {
		literal, ok := launcherLiteralWord(word)
		if !ok {
			continue
		}
		if literal == "-i" || literal == "--ignore-environment" {
			resets = true
		}
	}
	if !resets {
		return nil, false
	}
	for _, payload := range payloads {
		if payloadReachesAgentProgram(payload, names, nil) {
			return nil, true
		}
	}
	return nil, false
}

// unwrapTmux judges tmux: a pane-creating subcommand runs its shell
// command under the tmux server's environment (reset-class, with the
// -e env spelling judged by the generic scan), and setenv/set-environment
// WRITES the server's environment — the writer rule of issue item 3.
func unwrapTmux(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand) {
		return nil, true
	}
	if wrapperTailHidesAccountAssignment(words, false, names, memo) {
		return nil, true
	}
	positionals, _, undecodable := launcherArgvScan(words, tmuxGlobalGrammar)
	if undecodable || len(positionals) == 0 {
		return nil, undecodable
	}
	sub := positionals[0].literal
	switch sub {
	case "setenv", "set-environment", "set-env":
		// tmux setenv -g NAME VALUE writes the server's global
		// environment — every later pane inherits it. A protected NAME
		// operand refuses; the value is a positional the gate cannot
		// scope.
		pos, _, undec := launcherArgvScan(words[positionals[0].index+1:], tmuxSetenvGrammar)
		if undec {
			return nil, true
		}
		for _, p := range pos {
			if launcherSegmentsDenied(p.literal, names, false) {
				return nil, true
			}
		}
		return nil, false
	default:
		if _, pane := tmuxPaneCreatingSubcommands[sub]; !pane {
			return nil, false
		}
		payloads, undec := tmuxPayloads(words)
		if undec {
			return nil, true
		}
		for _, payload := range payloads {
			if payloadReachesAgentProgram(payload, names, nil) {
				return nil, true
			}
		}
		return nil, false
	}
}

// unwrapDistrobox judges distrobox enter/run: the container's environment
// is not the pane's — reset-class — and the -e / post-`--` command
// strings get the payload reach.
func unwrapDistrobox(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand) {
		return nil, true
	}
	if wrapperTailHidesAccountAssignment(words, false, names, memo) {
		return nil, true
	}
	payloads, undecodable := distroboxPayloads(words)
	if undecodable {
		return nil, true
	}
	for _, payload := range payloads {
		if payloadReachesAgentProgram(payload, names, nil) {
			return nil, true
		}
	}
	return nil, false
}

// unwrapToolbox judges toolbox: `enter` is reset-class with no command
// string, `run` carries trailing command words under the container's
// environment.
func unwrapToolbox(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand) {
		return nil, true
	}
	if wrapperTailHidesAccountAssignment(words, false, names, memo) {
		return nil, true
	}
	payloads, undecodable := toolboxPayloads(words)
	if undecodable {
		return nil, true
	}
	for _, payload := range payloads {
		if payloadReachesAgentProgram(payload, names, nil) {
			return nil, true
		}
	}
	return nil, false
}

// unwrapWatch judges watch: it keeps the pane's environment (no reset)
// but hands its argv, joined, to `sh -c` — a command-string runner whose
// payload candidates the top-level gate decodes.
func unwrapWatch(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	return unwrapCommandRunner(words, names, memo, requireCommand)
}

// unwrapPwshNu judges pwsh and nu — runners whose payload grammar the
// gate does NOT parse (approved rule on #7603): the -c/-Command/--commands
// operand's TEXT refuses when it names a protected variable or the
// account's agent at identifier boundaries (`pwsh -c 'echo CODEX_HOME'`
// and `pwsh -c 'codex'` refuse), and admits otherwise (`pwsh -c
// Get-Process` runs). The option grammar is total: an encoded-command
// spelling carries unreadable text, the hidden-input forms (-File,
// -EncodedCommand/-enc prefixes, `-` stdin, `-Command -`, -NoExit,
// -Interactive) refuse even when a -c operand is present, and an option
// the universe does not know makes the invocation undecodable — all
// refuse.
func unwrapPwshNu(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool, options map[string]pwshNuOptionKind, abbrev bool) ([]*syntax.Word, bool) {
	payload, has, positional, info, undecodable := pwshNuClassify(words, options, abbrev)
	if undecodable || positional {
		return nil, true
	}
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand) {
		return nil, true
	}
	if wrapperTailHidesAccountAssignment(words, false, names, memo) {
		return nil, true
	}
	if !has {
		// No command string: a bare `pwsh`/`nu` is the interactive
		// session — input the gate cannot see. Only a pure info flag
		// (-Version/-Help) runs without input.
		return nil, !info
	}
	return nil, pwshNuPayloadNamesBoundary(payload, names)
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

// unwrapBusctl judges busctl and gdbus: a call/set-property verb naming a
// SetEnvironment-family method plus a protected-name argument writes the
// service manager's environment — the writer rule of issue item 3.
// gdbus parses its operands as GVariant text, so each operand is decoded
// through its escapes before the name check — an escape the decoder does
// not know makes the operand undecodable, which means refuse.
func unwrapBusctl(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool, grammar launcherGrammar, decode bool) ([]*syntax.Word, bool) {
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand) {
		return nil, true
	}
	if wrapperTailHidesAccountAssignment(words, false, names, memo) {
		return nil, true
	}
	positionals, operands, undecodable := launcherArgvScan(words, grammar)
	if undecodable || len(positionals) == 0 {
		return nil, undecodable
	}
	verb := positionals[0].literal
	if verb != "call" && verb != "set-property" {
		return nil, false
	}
	method := verb == "set-property"
	for _, operand := range operands {
		// gdbus's -m/--method operand carries the method name — the
		// writer check lands on the last `.` segment
		// (`org.a.SetEnvironment` spells SetEnvironment).
		if operand.opt == "-m" || operand.opt == "--method" {
			methodName := operand.literal
			if i := strings.LastIndexByte(methodName, '.'); i >= 0 {
				methodName = methodName[i+1:]
			}
			if _, write := dbusEnvironmentWriteMethods[methodName]; write {
				method = true
			}
		}
	}
	for _, p := range positionals[1:] {
		// A call word may spell the method qualified — busctl's
		// interface.member shape — so the writer check lands on the
		// last `.` segment.
		methodName := p.literal
		if i := strings.LastIndexByte(methodName, '.'); i >= 0 {
			methodName = methodName[i+1:]
		}
		if _, write := dbusEnvironmentWriteMethods[methodName]; write {
			method = true
			continue
		}
		if !method {
			continue
		}
		if launcherSegmentsDenied(p.literal, names, false) {
			return nil, true
		}
		if decode {
			decoded, ok := gvariantDecode(p.literal)
			if !ok {
				return nil, true
			}
			if launcherSegmentsDenied(decoded, names, false) {
				return nil, true
			}
		}
	}
	return nil, false
}

// literalTailHasWord reports whether a literal word appears in the tail.
func literalTailHasWord(words []*syntax.Word, want string) bool {
	for _, word := range words {
		if l, ok := launcherLiteralWord(word); ok && l == want {
			return true
		}
	}
	return false
}
