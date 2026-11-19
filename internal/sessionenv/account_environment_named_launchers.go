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

// ---------- option grammar tables ----------

// pkexec's option grammar: --user/-u take operands; the rest are flags.
// pkexec always runs the child under a polkit-checked environment — never
// the pane's — so every form is reset-class.
var pkexecOperandOptions = map[string]bool{
	"-u": true, "--user": true, "-p": true, "--pid": true,
}
var pkexecFlags = map[string]bool{
	"--disable-internal-agent": true, "--keep-cwd": true, "--verbose": true,
}

// doas's option grammar: -u/-C/-T/-t take operands. doas resets the
// environment by configuration, not by a command-line flag — every form is
// reset-class (a keepenv entry in doas.conf is invisible to the gate, which
// judges the command line; the pane's pins cannot be shown preserved).
var doasOperandOptions = map[string]bool{
	"-u": true, "-C": true, "-T": true, "-t": true,
}
var doasFlags = map[string]bool{
	"-n": true, "-s": true, "-L": true, "-V": true,
}

// setpriv's option grammar — every setpriv option except --reset-env and
// the query forms takes an operand.
var setprivOperandOptions = map[string]bool{
	"--reuid": true, "--ruid": true, "--euid": true, "--suid": true,
	"--fuid": true, "--regid": true, "--rgid": true, "--egid": true,
	"--sgid": true, "--fgid": true, "--groups": true, "--securebits": true,
	"--selinux-label": true, "--apparmor-profile": true,
	"--bounding-set": true, "--inh-caps": true, "--ambient-caps": true,
	"--cap-bset": true, "--pdeathsig": true, "--keep-pgroups": true,
	"--clear-groups": true, "--keep-groups": true, "--init-groups": true,
}
var setprivFlags = map[string]bool{
	"--reset-env": true, "--no-new-privs": true, "--dump": true,
	"--list-caps": true, "--ambient": true,
}

// flatpak-spawn's option grammar. Every spawn is reset-class — the child
// runs in the sandbox's environment (or the host's own reset form), never
// the pane's (approved scope answer on #7603).
var flatpakSpawnOperandOptions = map[string]bool{
	"--env": true, "--env-fd": true, "--unset-env": true,
	"--forward-fd": true, "--sandbox-expose": true,
	"--sandbox-expose-ro": true, "--sandbox-expose-fd": true,
	"--userns": true, "--runtime-version": true, "--sandbox-flag": true,
	"--latest-version": true,
}
var flatpakSpawnFlags = map[string]bool{
	"--host": true, "--sandbox": true, "--clear-env": true,
	"--watch-bus": true, "--no-network": true, "--share-pid": true,
	"--verbose": true, "--debug": true,
}

// ssh's option grammar: operand-taking single letters and the bare flags.
// The first non-option word is the destination; every literal after it is
// part of the remote command line.
var sshOperandOptions = map[string]bool{
	"-B": true, "-b": true, "-c": true, "-D": true, "-E": true,
	"-F": true, "-I": true, "-i": true, "-J": true, "-L": true,
	"-l": true, "-m": true, "-O": true, "-o": true, "-p": true,
	"-Q": true, "-R": true, "-S": true, "-W": true, "-w": true,
}
var sshFlags = map[string]bool{
	"-4": true, "-6": true, "-A": true, "-a": true, "-C": true,
	"-f": true, "-G": true, "-g": true, "-K": true, "-k": true,
	"-M": true, "-N": true, "-n": true, "-P": true, "-q": true,
	"-s": true, "-T": true, "-t": true, "-V": true, "-v": true,
	"-X": true, "-x": true, "-Y": true, "-y": true,
}

// tmux's global option grammar and its pane-creating subcommands (with the
// documented aliases — scope answer on #7603). Pane commands run under the
// tmux SERVER's environment, never the pane's, so they are reset-class.
var tmuxOperandOptions = map[string]bool{
	"-c": true, "-f": true, "-L": true, "-S": true, "-T": true,
}
var tmuxFlags = map[string]bool{
	"-2": true, "-C": true, "-CC": true, "-l": true, "-u": true,
	"-v": true, "-V": true,
}
var tmuxPaneCreatingSubcommands = map[string]struct{}{
	"new-session": {}, "new": {},
	"new-window": {}, "neww": {},
	"split-window": {}, "splitw": {},
	"respawn-pane": {}, "respawnp": {},
	"respawn-window": {}, "respawnw": {},
}

// tmux pane subcommand operand options: -c (start dir), -e (env spelling —
// the generic denied scan judges its NAME=value operand), -F format, -n
// name, -t target, -l/-x/-y sizes, -s session name. The rest are flags.
var tmuxPaneOperandOptions = map[string]bool{
	"-c": true, "-e": true, "-F": true, "-n": true, "-t": true,
	"-l": true, "-x": true, "-y": true, "-s": true,
}

// watch's option grammar: -n/--interval and --differences take operands;
// the rest are flags. watch hands every remaining argv word, joined with
// spaces, to `sh -c`.
var watchOperandOptions = map[string]bool{
	"-n": true, "--interval": true, "--differences": true, "-h": true,
	"--help": true, "-v": true, "--version": true,
}
var watchFlags = map[string]bool{
	"-d": true, "-t": true, "-g": true, "-p": true, "-b": true,
	"-e": true, "-x": true, "-c": true, "-w": true, "--no-title": true,
	"--beep": true, "--chgexit": true, "--color": true, "--errexit": true,
	"--exec": true, "--equexit": true, "--precise": true, "--raw": true,
	"--no-wrap": true, "--differences=permanent": true,
}

// nix-shell's option grammar. --pure clears the environment (reset);
// --run/--command take a command-string operand (payload); -p/--packages
// and friends take name operands.
var nixShellOperandOptions = map[string]bool{
	"-p": true, "--packages": true, "-A": true, "-a": true,
	"--attr": true, "-E": true, "--expr": true, "--exclude": true,
	"--keep": true, "--arg": true, "--argstr": true, "--add-root": true,
	"--run": true, "--command": true,
}
var nixShellFlags = map[string]bool{
	"--pure": true, "-Q": true, "--quiet": true, "-v": true,
	"--verbose": true, "--show-trace": true, "--repair": true,
	"--dry-run": true, "--no-out-link": true, "--indirect": true,
}

// nix's develop/shell option grammar — -c/--command takes the command
// words, -i/--ignore-environment is the reset flag. The remaining common
// operands name flakes, profiles and files.
var nixDevOperandOptions = map[string]bool{
	"--expr": true, "--profile": true, "--out-link": true,
	"--command": true, "--phase": true, "--redirect": true,
	"--inputs-from": true, "--update-input": true, "--override-input": true,
	"--extra-experimental-features": true, "--option": true,
	"--installable": true, "--check-sigs": true,
}
var nixDevFlags = map[string]bool{
	"-i": true, "--ignore-environment": true, "--recreate-lock-file": true,
	"--no-write-lock-file": true, "--no-update-lock-file": true,
	"--unpack": true, "--build": true, "--print-build-logs": true,
	"--offline": true, "--refresh": true, "--quiet": true,
}

// machinectl's global options; shell/login carry --uid and --setenv
// operands.
var machinectlOperandOptions = map[string]bool{
	"-H": true, "--host": true, "-M": true, "--machine": true,
	"--uid": true, "--setenv": true, "-E": true, "--property": true,
	"-p": true, "--value": true, "-o": true, "--output": true,
}
var machinectlFlags = map[string]bool{
	"-q": true, "--quiet": true, "--no-legend": true, "--no-pager": true,
	"-a": true, "--all": true, "--full": true, "--system": true,
	"--user": true, "--read-only": true, "--mkdir": true,
	"--runtime": true, "--force": true, "-l": true, "--lines": true,
	"-n": true, "-s": true, "--signal": true,
}

// distrobox enter's option grammar — the container name is a positional
// operand, -e carries a command string, and a `--` terminator's tail is the
// command.
var distroboxOperandOptions = map[string]bool{
	"-n": true, "--name": true, "-e": true, "--additional-flags": true,
	"--absolutely-disable-root-password-i-am-really-positively-sure": true,
	"--home": true, "--root": true, "-a": true, "--abs-path": true,
	"-r": true, "--release": true, "-c": true, "--command": true,
}
var distroboxFlags = map[string]bool{
	"--no-workdir": true, "--dry-run": true, "-d": true, "-Y": true,
	"--yes": true, "-T": true, "--no-tty": true, "-i": true,
	"--image": true, "-U": true, "--unshare-groups": true,
	"--unshare-devsys": true, "--unshare-ipc": true,
	"--unshare-netns": true, "--unshare-process": true,
	"--unshare-all": true, "-H": true, "-v": true, "--verbose": true,
	"-V": true, "--version": true, "-h": true, "--help": true,
}

// toolbox run's option grammar — -c/-r take operands and the trailing
// words are the command. toolbox enter takes a container name only.
var toolboxOperandOptions = map[string]bool{
	"-c": true, "--container": true, "-r": true, "--release": true,
	"-d": true, "--distro": true,
}
var toolboxFlags = map[string]bool{
	"-y": true, "--assumeyes": true, "-v": true, "--verbose": true,
	"--very-verbose": true, "-q": true, "--quiet": true,
	"--preserve-fds": true,
}

// pwsh/nu command-string operand spellings. The gate does not parse their
// payload grammar — a payload is refused when its TEXT names a protected
// variable or the account's agent at identifier boundaries, and admitted
// otherwise (approved rule on #7603). Encoded-command spellings carry
// text the scan cannot read, so they refuse outright.
var pwshCommandOperandOptions = map[string]bool{
	"-c": true, "-command": true, "-commands": true,
}
var pwshEncodedOperandOptions = map[string]bool{
	"-ec": true, "-e": true, "-encodedcommand": true,
}
var nuCommandOperandOptions = map[string]bool{
	"-c": true, "--commands": true, "-e": true, "--execute": true,
}

// busctl/gdbus call words that WRITE a service manager's environment —
// the writer class of issue #7603 item 3.
var dbusEnvironmentWriteMethods = map[string]struct{}{
	"SetEnvironment": {}, "UnsetEnvironment": {},
	"ImportEnvironment": {}, "SetEnvironmentVariable": {},
}

// ---------- payload collectors ----------

// sshPayloads returns ssh's remote-command candidates: every literal word
// after the destination operand (the first non-option word once option
// operands are skipped).
func sshPayloads(words []*syntax.Word) []string {
	for i := 0; i < len(words); i++ {
		literal, ok := launcherLiteralWord(words[i])
		if !ok {
			continue
		}
		if literal == "--" {
			continue
		}
		if strings.HasPrefix(literal, "-") && literal != "-" {
			if sshOperandOptions[literal] {
				i++
			}
			continue
		}
		// The destination. Every following word is joined into the remote
		// command line.
		return literalTailWords(words[i+1:])
	}
	return nil
}

// watchPayloads returns watch's command-string candidates: every literal
// word that is not an option or an option's operand — watch joins them all
// into one `sh -c` string.
func watchPayloads(words []*syntax.Word) []string {
	var payloads []string
	for i := 0; i < len(words); i++ {
		literal, ok := launcherLiteralWord(words[i])
		if !ok {
			continue
		}
		if literal == "--" {
			continue
		}
		if strings.HasPrefix(literal, "-") && literal != "-" {
			if watchOperandOptions[literal] {
				i++
			}
			continue
		}
		payloads = append(payloads, literal)
	}
	return payloads
}

// tmuxPayloads returns tmux's command-string candidates: after the global
// options and the subcommand word, a pane-creating subcommand's trailing
// operand is a shell command the server runs; the global `-c` operand sets
// the default shell and is a candidate too.
func tmuxPayloads(words []*syntax.Word) []string {
	var payloads []string
	i := 0
	for ; i < len(words); i++ {
		literal, ok := launcherLiteralWord(words[i])
		if !ok {
			continue
		}
		if strings.HasPrefix(literal, "-") && literal != "-" {
			if tmuxOperandOptions[literal] {
				if i+1 < len(words) {
					if l, ok2 := launcherLiteralWord(words[i+1]); ok2 && literal == "-c" {
						payloads = append(payloads, l)
					}
				}
				i++
			}
			continue
		}
		break
	}
	if i >= len(words) {
		return payloads
	}
	literal, ok := launcherLiteralWord(words[i])
	if !ok {
		return payloads
	}
	if _, pane := tmuxPaneCreatingSubcommands[literal]; !pane {
		return payloads
	}
	// The pane subcommand's own options; trailing non-option words are the
	// shell command the server runs.
	for j := i + 1; j < len(words); j++ {
		sub, ok := launcherLiteralWord(words[j])
		if !ok {
			continue
		}
		if sub == "--" {
			continue
		}
		if strings.HasPrefix(sub, "-") && sub != "-" {
			if tmuxPaneOperandOptions[sub] {
				j++
			}
			continue
		}
		payloads = append(payloads, sub)
	}
	return payloads
}

// nixShellPayloads returns nix-shell's command-string candidates: the
// --run/--command operands in either spelling.
func nixShellPayloads(words []*syntax.Word) []string {
	var payloads []string
	for i := 0; i < len(words); i++ {
		literal, ok := launcherLiteralWord(words[i])
		if !ok {
			continue
		}
		if strings.HasPrefix(literal, "--run") {
			if literal == "--run" || strings.HasPrefix(literal, "--run=") {
				if literal == "--run" {
					if i+1 < len(words) {
						if l, ok2 := launcherLiteralWord(words[i+1]); ok2 {
							payloads = append(payloads, l)
						}
					}
				} else {
					payloads = append(payloads, literal[len("--run="):])
				}
			}
			continue
		}
		if literal == "--command" {
			if i+1 < len(words) {
				if l, ok2 := launcherLiteralWord(words[i+1]); ok2 {
					payloads = append(payloads, l)
				}
			}
			continue
		}
		if strings.HasPrefix(literal, "--command=") {
			payloads = append(payloads, literal[len("--command="):])
			continue
		}
	}
	return payloads
}

// nixPayloads returns `nix develop`/`nix shell` command candidates:
// everything after -c/--command (the option takes the rest of the argv as
// the command words), plus the `--command=` joined form.
func nixPayloads(words []*syntax.Word) []string {
	var payloads []string
	sawDev := false
	for i := 0; i < len(words); i++ {
		literal, ok := launcherLiteralWord(words[i])
		if !ok {
			continue
		}
		if !sawDev {
			if strings.HasPrefix(literal, "-") {
				continue
			}
			if literal == "develop" || literal == "shell" {
				sawDev = true
			}
			continue
		}
		if literal == "-c" || literal == "--command" {
			for j := i + 1; j < len(words); j++ {
				if l, ok2 := launcherLiteralWord(words[j]); ok2 {
					payloads = append(payloads, l)
				}
			}
			return payloads
		}
		if strings.HasPrefix(literal, "--command=") {
			payloads = append(payloads, literal[len("--command="):])
			continue
		}
	}
	return payloads
}

// distroboxPayloads returns distrobox enter's command-string candidates:
// the -e operand and every literal after a `--` terminator.
func distroboxPayloads(words []*syntax.Word) []string {
	var payloads []string
	sawEnter := false
	for i := 0; i < len(words); i++ {
		literal, ok := launcherLiteralWord(words[i])
		if !ok {
			continue
		}
		if !sawEnter {
			if strings.HasPrefix(literal, "-") {
				continue
			}
			if literal == "enter" || literal == "run" {
				sawEnter = true
			}
			continue
		}
		if literal == "--" {
			for j := i + 1; j < len(words); j++ {
				if l, ok2 := launcherLiteralWord(words[j]); ok2 {
					payloads = append(payloads, l)
				}
			}
			return payloads
		}
		if literal == "-e" {
			if i+1 < len(words) {
				if l, ok2 := launcherLiteralWord(words[i+1]); ok2 {
					payloads = append(payloads, l)
				}
			}
			continue
		}
	}
	return payloads
}

// toolboxPayloads returns `toolbox run` command candidates: the trailing
// non-option words after run's options.
func toolboxPayloads(words []*syntax.Word) []string {
	var payloads []string
	sawRun := false
	for i := 0; i < len(words); i++ {
		literal, ok := launcherLiteralWord(words[i])
		if !ok {
			continue
		}
		if !sawRun {
			if strings.HasPrefix(literal, "-") {
				continue
			}
			sawRun = literal == "run"
			continue
		}
		if literal == "--" {
			continue
		}
		if strings.HasPrefix(literal, "-") {
			if toolboxOperandOptions[literal] {
				i++
			}
			continue
		}
		payloads = append(payloads, literal)
	}
	return payloads
}

// pwshNuPayloadOperand returns the pwsh/nu command-string operand — the
// word after -c/-Command/--commands (or a --commands= join), with pwsh's
// unique-prefix abbreviations (-com → -Command, -enc → -EncodedCommand)
// resolved by prefix. Their payload grammar is not parsed: the text gets
// the boundary-name scan instead.
func pwshNuPayloadOperand(words []*syntax.Word, operandOptions, encodedOptions map[string]bool) (string, bool) {
	for i := 0; i < len(words); i++ {
		literal, ok := launcherLiteralWord(words[i])
		if !ok {
			continue
		}
		lower := strings.ToLower(literal)
		if operandOptions[lower] || pwshNuOptionAbbrev(lower, operandOptions) {
			if i+1 < len(words) {
				if l, ok2 := launcherLiteralWord(words[i+1]); ok2 {
					return l, true
				}
			}
			continue
		}
		if encodedOptions[lower] || pwshNuOptionAbbrev(lower, encodedOptions) {
			// An encoded-command spelling carries text the scan cannot
			// read — opaque, never admitted.
			return "", true
		}
		if strings.HasPrefix(lower, "--commands=") {
			return literal[len("--commands="):], true
		}
	}
	return "", false
}

// pwshNuOptionAbbrev reports whether word is a non-empty single-dash
// prefix of an option name in the set — pwsh accepts unique-prefix
// abbreviations, so `-com` is `-Command` and `-enc` is `-EncodedCommand`.
func pwshNuOptionAbbrev(word string, options map[string]bool) bool {
	if len(word) < 3 || word[0] != '-' || strings.HasPrefix(word, "--") {
		return false
	}
	for opt := range options {
		if strings.HasPrefix(opt, word) {
			return true
		}
	}
	return false
}

// pwshNuPayloads returns pwsh/nu's operand words for the reach decode —
// under an outer reset the `-c` operand is judged for agent reach by the
// ordinary payload path (an unparseable payload fails closed there).
func pwshNuPayloads(words []*syntax.Word) []string {
	if payload, has := pwshNuPayloadOperand(words, pwshCommandOperandOptions, pwshEncodedOperandOptions); has && payload != "" {
		return []string{payload}
	}
	return nil
}

// nuPayloads mirrors pwshNuPayloads for nu's option spellings (-c,
// --commands, -e, --execute all hand nu a command string).
func nuPayloads(words []*syntax.Word) []string {
	if payload, has := pwshNuPayloadOperand(words, nuCommandOperandOptions, nil); has && payload != "" {
		return []string{payload}
	}
	return nil
}

// literalTailWords renders every literal word in a tail.
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

// unwrapAlwaysResetLauncher judges a launcher whose child's environment is
// never the pane's — pkexec, doas, flatpak-spawn, ssh, the tmux pane
// creators, toolbox/distrobox. The generic env-injection and hidden-tail
// scans run first, then the reset arm's reach check on the launcher-specific
// option grammar.
func unwrapAlwaysResetLauncher(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool, operandOptions, knownFlags map[string]bool) bool {
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand) {
		return true
	}
	if wrapperTailHidesAccountAssignment(words, false, names, memo) {
		return true
	}
	return tailReachesResetAgentProgram(words, names, nil, false, operandOptions, knownFlags)
}

// unwrapPkexec judges pkexec: always reset-class, program is the first
// non-option word.
func unwrapPkexec(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	return nil, unwrapAlwaysResetLauncher(words, names, memo, requireCommand, pkexecOperandOptions, pkexecFlags)
}

// unwrapDoas judges doas: always reset-class.
func unwrapDoas(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	return nil, unwrapAlwaysResetLauncher(words, names, memo, requireCommand, doasOperandOptions, doasFlags)
}

// unwrapFlatpakSpawn judges flatpak-spawn: every spawn is reset-class —
// the child runs in the sandbox's environment either way.
func unwrapFlatpakSpawn(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	return nil, unwrapAlwaysResetLauncher(words, names, memo, requireCommand, flatpakSpawnOperandOptions, flatpakSpawnFlags)
}

// unwrapSetpriv judges setpriv: reset-class only under --reset-env — the
// other forms keep the caller's environment, so the plain argv scans apply.
func unwrapSetpriv(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand) {
		return nil, true
	}
	if wrapperTailHidesAccountAssignment(words, false, names, memo) {
		return nil, true
	}
	if !literalTailHasWord(words, "--reset-env") {
		return nil, false
	}
	return nil, tailReachesResetAgentProgram(words, names, nil, false, setprivOperandOptions, setprivFlags)
}

// unwrapSSH judges ssh: the remote environment is never the pane's, so it
// is always reset-class. The remote command is every word after the
// destination — the reach check parses the joined command line so `ssh
// host psql codex` (an operand position) does not reach the way `ssh host
// codex` (the program) does.
func unwrapSSH(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand) {
		return nil, true
	}
	if wrapperTailHidesAccountAssignment(words, false, names, memo) {
		return nil, true
	}
	payload := strings.Join(sshPayloads(words), " ")
	if payload == "" {
		return nil, false
	}
	return nil, payloadReachesAgentProgram(payload, names, nil)
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
	// Find the subcommand word, then the host operand after it.
	i := 0
	for ; i < len(words); i++ {
		literal, ok := launcherLiteralWord(words[i])
		if !ok {
			continue
		}
		if strings.HasPrefix(literal, "-") && literal != "-" {
			if machinectlOperandOptions[literal] {
				i++
			}
			continue
		}
		break
	}
	if i >= len(words) {
		return nil, false
	}
	sub, _ := launcherLiteralWord(words[i])
	if sub != "shell" && sub != "login" {
		return nil, false
	}
	// After the subcommand, the first non-option word is the host; the
	// command line is everything after it.
	for j := i + 1; j < len(words); j++ {
		literal, ok := launcherLiteralWord(words[j])
		if !ok {
			continue
		}
		if strings.HasPrefix(literal, "-") && literal != "-" {
			if machinectlOperandOptions[literal] {
				j++
			}
			continue
		}
		payload := strings.Join(literalTailWords(words[j+1:]), " ")
		if payload == "" {
			return nil, false
		}
		return nil, payloadReachesAgentProgram(payload, names, nil)
	}
	return nil, false
}

// unwrapNixShell judges nix-shell: --pure resets (the reach check then
// applies to the --run/--command payloads), and every payload candidate is
// gated at the top level like the other runners.
func unwrapNixShell(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand) {
		return nil, true
	}
	if wrapperTailHidesAccountAssignment(words, false, names, memo) {
		return nil, true
	}
	if !literalTailHasWord(words, "--pure") {
		return nil, false
	}
	for _, payload := range nixShellPayloads(words) {
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
	for _, payload := range nixPayloads(words) {
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
	sub, _ := tmuxSubcommand(words)
	switch sub {
	case "setenv", "set-environment", "set-env":
		// tmux setenv -g NAME VALUE writes the server's global
		// environment — every later pane inherits it. A protected NAME
		// operand refuses; the value is a positional the gate cannot
		// scope.
		for _, word := range words {
			literal, ok := launcherLiteralWord(word)
			if !ok {
				continue
			}
			if literal == "setenv" || literal == "set-environment" || literal == "set-env" {
				continue
			}
			if launcherSegmentsDenied(literal, names, false) {
				return nil, true
			}
		}
		return nil, false
	default:
		if _, pane := tmuxPaneCreatingSubcommands[sub]; !pane {
			return nil, false
		}
		payload := strings.Join(tmuxPayloads(words), " ")
		if payload == "" {
			return nil, false
		}
		return nil, payloadReachesAgentProgram(payload, names, nil)
	}
}

// tmuxSubcommand returns the first non-option word — tmux's subcommand —
// after its global options.
func tmuxSubcommand(words []*syntax.Word) (string, bool) {
	for i := 0; i < len(words); i++ {
		literal, ok := launcherLiteralWord(words[i])
		if !ok {
			continue
		}
		if strings.HasPrefix(literal, "-") && literal != "-" {
			if tmuxOperandOptions[literal] {
				i++
			}
			continue
		}
		return literal, true
	}
	return "", false
}

// unwrapDistrobox judges distrobox enter/run: the container's environment
// is not the pane's — reset-class — and the -e / post-`--` command strings
// get the payload reach.
func unwrapDistrobox(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand) {
		return nil, true
	}
	if wrapperTailHidesAccountAssignment(words, false, names, memo) {
		return nil, true
	}
	payload := strings.Join(distroboxPayloads(words), " ")
	if payload == "" {
		return nil, false
	}
	return nil, payloadReachesAgentProgram(payload, names, nil)
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
	payload := strings.Join(toolboxPayloads(words), " ")
	if payload == "" {
		return nil, false
	}
	return nil, payloadReachesAgentProgram(payload, names, nil)
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
// Get-Process` runs). An encoded-command spelling is opaque and refuses.
// A non-literal operand word fails closed via the generic scans.
func unwrapPwshNu(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool, commandOptions, encodedOptions map[string]bool) ([]*syntax.Word, bool) {
	payload, has := pwshNuPayloadOperand(words, commandOptions, encodedOptions)
	if has && payload == "" {
		return nil, true // encoded command — unreadable text
	}
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand) {
		return nil, true
	}
	if wrapperTailHidesAccountAssignment(words, false, names, memo) {
		return nil, true
	}
	if !has {
		// No command string: an interactive or script-file form reads
		// input the gate cannot see — a bare positional word names a
		// script (the `sh script.sh` class), the interactive/login and
		// stdin flags read a session, and a bare `pwsh`/`nu` IS the
		// interactive session. Only a pure info flag (-v/-h/-?) runs
		// without input.
		infoOnly := false
		for _, word := range words {
			literal, ok := launcherLiteralWord(word)
			if !ok {
				continue
			}
			if pwshNuHiddenInputWord(literal) {
				return nil, true
			}
			switch strings.ToLower(literal) {
			case "-v", "--version", "-version", "-h", "--help", "-?", "-help":
				infoOnly = true
			}
		}
		return nil, !infoOnly
	}
	return nil, pwshNuPayloadNamesBoundary(payload, names)
}

// pwshNuHiddenInputWord reports whether a literal argv word is a pwsh/nu
// interactive/login flag, a script-file option, or a bare positional —
// forms whose real input the gate cannot read.
func pwshNuHiddenInputWord(literal string) bool {
	if !strings.HasPrefix(literal, "-") || literal == "-" {
		return true // a bare positional word names a script file
	}
	switch strings.ToLower(literal) {
	case "-i", "--interactive", "-l", "--login",
		"-f", "-file", "-psconsolefile",
		"-ip", "-interactive", "-interactiveprompt",
		"--stdin", "-noni", "-noninteractive":
		return true
	}
	return false
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

// textNamesIdentifierBoundary reports whether name appears in text bounded
// on both sides by bytes that are not identifier bytes — the boundary
// `echo CODEX_HOME` satisfies and `CODEX_HOME_DIR` does not.
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

// unwrapBusctl judges busctl and gdbus: a call word naming a
// SetEnvironment-family method plus a protected-name argument writes the
// service manager's environment — the writer rule of issue item 3.
func unwrapBusctl(words []*syntax.Word, names map[string]struct{}, memo operandTailMemo, requireCommand bool) ([]*syntax.Word, bool) {
	if unwrapEnvInjectionLauncher(words, names, false, requireCommand) {
		return nil, true
	}
	if wrapperTailHidesAccountAssignment(words, false, names, memo) {
		return nil, true
	}
	method := false
	for _, word := range words {
		literal, ok := launcherLiteralWord(word)
		if !ok {
			continue
		}
		// A call word may spell the method qualified — gdbus's
		// `-m org.a.SetEnvironment` and busctl's interface.member
		// shape — so the writer check lands on the last `.` segment.
		methodName := literal
		if i := strings.LastIndexByte(methodName, '.'); i >= 0 {
			methodName = methodName[i+1:]
		}
		if _, write := dbusEnvironmentWriteMethods[methodName]; write {
			method = true
			continue
		}
		if method && launcherSegmentsDenied(literal, names, false) {
			return nil, true
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
