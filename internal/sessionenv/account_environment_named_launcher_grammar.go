package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// This file holds the CLOSED option grammars and the shared argv scanner
// for #7603's named launchers (property B of the review on this head):
// every option a launcher can read has a stated arity — a bare flag, a
// one-word operand, a two-word operand, or an optional `=`-attached value
// — and an option word the grammar does not know makes the invocation
// undecodable, which means refuse. `ssh --frobnicate host make` refuses
// the same way `setpriv --reset-env --frobnicate codex` does: the tool
// may read an operand the scanner cannot see.

// ---------- option grammar tables ----------

// launcherGrammar describes one launcher's option surface. operand takes
// the next argv word (or an attached --x=v); operand2 takes the next two
// (nix's --option); optional accepts an attached --x=v but needs none
// (watch --differences=permanent); flag takes none. payload marks operand
// options whose operand is a command string the launcher hands to a shell
// or a remote environment. cluster lets single-dash words pack letters
// getopt-style: flagL letters pass, operandL letters take the rest of the
// cluster or the next word, and payloadL operand letters carry payloads.
type launcherGrammar struct {
	operand  map[string]bool
	operand2 map[string]bool
	payload  map[string]bool
	flag     map[string]bool
	optional map[string]bool
	cluster  bool
	operandL string
	payloadL string
	flagL    string
	// stopAfter, when nonzero, ends the option region after that many
	// positional words — ssh's destination, watch's command word, a tmux
	// subcommand, machinectl's host (2). The remaining argv is raw
	// command text, never options.
	stopAfter int
}

// pkexec's option grammar: --user/-u and --pid take operands; the rest
// are flags. pkexec always runs the child under a polkit-checked
// environment — never the pane's — so every form is reset-class.
var pkexecGrammar = launcherGrammar{
	operand: map[string]bool{"-u": true, "--user": true, "-p": true, "--pid": true},
	flag: map[string]bool{
		"--disable-internal-agent": true, "--keep-cwd": true,
		"--verbose": true, "--help": true, "--version": true,
	},
	stopAfter: 1,
}

// doas's option grammar: -u/-C/-T/-t take operands; getopt clusters are
// allowed (-nu root). doas resets the environment by configuration, not
// by a command-line flag — every form is reset-class.
var doasGrammar = launcherGrammar{
	operand: map[string]bool{"-u": true, "-C": true, "-T": true, "-t": true},
	flag:    map[string]bool{"-n": true, "-s": true, "-L": true, "-V": true, "-E": true},
	cluster: true, operandL: "uCTt", flagL: "nsLVE",
	stopAfter: 1,
}

// setpriv's option grammar — long options only; --groups takes the group
// list while --keep-groups/--clear-groups/--init-groups are operand-free
// flags on the real tool (checked against setpriv --help — a review on
// this head caught them consuming the command word).
var setprivGrammar = launcherGrammar{
	operand: map[string]bool{
		"--reuid": true, "--ruid": true, "--euid": true, "--suid": true,
		"--fuid": true, "--regid": true, "--rgid": true, "--egid": true,
		"--sgid": true, "--fgid": true, "--groups": true, "--securebits": true,
		"--selinux-label": true, "--apparmor-profile": true,
		"--bounding-set": true, "--inh-caps": true, "--ambient-caps": true,
		"--cap-bset": true, "--pdeathsig": true, "--keep-pgroups": true,
	},
	flag: map[string]bool{
		"--reset-env": true, "--no-new-privs": true, "--dump": true,
		"--list-caps": true, "--ambient": true, "--keep-groups": true,
		"--clear-groups": true, "--init-groups": true, "--help": true,
		"--version": true,
	},
	stopAfter: 1,
}

// flatpak-spawn's option grammar — every spawn is reset-class. --env-fd
// reads environment text from a descriptor the command line cannot show,
// so it is modelled as an operand whose content is hidden — the generic
// denied scan cannot judge it and the word refuses through the hidden
// set below.
var flatpakSpawnGrammar = launcherGrammar{
	operand: map[string]bool{
		"--env": true, "--env-fd": true, "--unset-env": true,
		"--forward-fd": true, "--sandbox-expose": true,
		"--sandbox-expose-ro": true, "--sandbox-expose-fd": true,
		"--userns": true, "--runtime-version": true, "--sandbox-flag": true,
		"--latest-version": true, "--directory": true, "--arch": true,
	},
	flag: map[string]bool{
		"--host": true, "--sandbox": true, "--clear-env": true,
		"--watch-bus": true, "--no-network": true, "--share-pid": true,
		"--verbose": true, "--debug": true, "--latest-commit": true,
		"--help": true, "--version": true,
	},
	stopAfter: 1,
}

// ssh's option grammar: operand-taking single letters and the bare flags
// (getopt clusters — -vv, -p22). -s carries a subsystem operand. The
// first non-option word is the destination; every literal after it joins
// into the remote command line.
var sshGrammar = launcherGrammar{
	operand: map[string]bool{
		"-B": true, "-b": true, "-c": true, "-D": true, "-E": true,
		"-F": true, "-I": true, "-i": true, "-J": true, "-L": true,
		"-l": true, "-m": true, "-O": true, "-o": true, "-p": true,
		"-Q": true, "-R": true, "-S": true, "-s": true, "-W": true,
		"-w": true,
	},
	flag: map[string]bool{
		"-4": true, "-6": true, "-A": true, "-a": true, "-C": true,
		"-f": true, "-G": true, "-g": true, "-K": true, "-k": true,
		"-M": true, "-N": true, "-n": true, "-q": true,
		"-T": true, "-t": true, "-V": true, "-v": true, "-X": true,
		"-x": true, "-Y": true, "-y": true,
	},
	cluster:  true,
	operandL: "BbcDEFIiJLlmOopQRSsWw", flagL: "46AaCfGgKkMnNqTtVvXxYy",
	stopAfter: 1,
}

// tmux's global option grammar and its pane-creating subcommands (with
// the documented aliases — scope answer on #7603). Pane commands run
// under the tmux SERVER's environment, never the pane's, so they are
// reset-class.
var tmuxGlobalGrammar = launcherGrammar{
	operand: map[string]bool{
		"-c": true, "-f": true, "-L": true, "-S": true, "-T": true,
	},
	flag: map[string]bool{
		"-2": true, "-C": true, "-CC": true, "-l": true, "-u": true,
		"-v": true, "-V": true,
	},
	cluster: true, operandL: "cfLST", flagL: "2CluvV",
	stopAfter: 1,
}

var tmuxPaneCreatingSubcommands = map[string]struct{}{
	"new-session": {}, "new": {},
	"new-window": {}, "neww": {},
	"split-window": {}, "splitw": {},
	"respawn-pane": {}, "respawnp": {},
	"respawn-window": {}, "respawnw": {},
}

// tmux pane subcommand grammar: -c start dir, -e env spelling (the
// generic denied scan judges its NAME=value operand), -F format, -n
// name, -t target, -l/-x/-y sizes, -s session name, -f flags operand.
var tmuxPaneGrammar = launcherGrammar{
	operand: map[string]bool{
		"-c": true, "-e": true, "-F": true, "-n": true, "-t": true,
		"-l": true, "-x": true, "-y": true, "-s": true, "-f": true,
	},
	flag: map[string]bool{
		"-d": true, "-D": true, "-A": true, "-a": true, "-k": true,
		"-P": true, "-E": true, "-b": true, "-h": true, "-v": true,
		"-Z": true, "-r": true, "-R": true,
	},
	cluster: true, operandL: "ceFntlysfx", flagL: "dDAakPEbhvZrR",
	stopAfter: 1,
}

// tmux setenv/set-environment grammar: -t takes the target session;
// -g/-r/-u/-F are flags; the positional name writes the server's
// environment.
var tmuxSetenvGrammar = launcherGrammar{
	operand: map[string]bool{"-t": true},
	flag:    map[string]bool{"-g": true, "-r": true, "-u": true, "-F": true},
	cluster: true, operandL: "t", flagL: "gruF",
}

// watch's option grammar: -n/--interval takes an operand, --differences
// takes an optional =-attached operand; the rest are flags. watch hands
// every remaining argv word, joined with spaces, to `sh -c`.
var watchGrammar = launcherGrammar{
	operand:  map[string]bool{"-n": true, "--interval": true},
	optional: map[string]bool{"--differences": true},
	flag: map[string]bool{
		"-d": true, "-t": true, "-g": true, "-p": true, "-b": true,
		"-e": true, "-x": true, "-c": true, "-w": true, "-s": true,
		"-h": true, "--help": true, "-v": true, "--version": true,
		"--no-title": true, "--beep": true, "--chgexit": true,
		"--color": true, "--errexit": true, "--exec": true,
		"--precise": true, "--raw": true, "--no-wrap": true,
	},
	cluster: true, operandL: "n", flagL: "dtgpbxecwshv",
	stopAfter: 1,
}

// nix-shell's option grammar. --pure clears the environment (reset);
// --run/--command take a command-string operand (payload); -p/--packages
// and friends take name operands; --option takes a name AND a value.
var nixShellGrammar = launcherGrammar{
	operand: map[string]bool{
		"-p": true, "--packages": true, "-A": true, "-a": true,
		"--attr": true, "-E": true, "--expr": true, "--exclude": true,
		"--keep": true, "--arg": true, "--argstr": true, "--add-root": true,
		"-I": true, "--include": true, "--log-type": true,
	},
	operand2: map[string]bool{"--option": true},
	payload:  map[string]bool{"--run": true, "--command": true},
	flag: map[string]bool{
		"--pure": true, "-Q": true, "--quiet": true, "-v": true,
		"--verbose": true, "--show-trace": true, "--repair": true,
		"--dry-run": true, "--no-out-link": true, "--indirect": true,
		"--readonly-mode": true, "--help": true, "--version": true,
	},
	cluster: true, operandL: "pAaEI", flagL: "Qv",
}

// nix's develop/shell option grammar — -c/--command takes the REST of
// the argv as the command words (collector-side), -i/--ignore-environment
// is the reset flag. --option takes a name AND a value.
var nixDevGrammar = launcherGrammar{
	operand: map[string]bool{
		"--expr": true, "--profile": true, "--out-link": true,
		"--phase": true, "--redirect": true, "--inputs-from": true,
		"--update-input": true, "--override-input": true,
		"--extra-experimental-features": true, "--installable": true,
		"--keep": true, "-k": true, "--unset": true, "-u": true,
		"--file": true, "-f": true, "--arg": true, "--argstr": true,
		"-I": true, "--include": true, "--flake": true, "--store": true,
		"--eval-store": true, "--access-tokens": true, "-j": true,
		"--max-jobs": true, "--reference-lock-file": true,
	},
	operand2: map[string]bool{"--option": true},
	flag: map[string]bool{
		"-i": true, "--ignore-environment": true, "--recreate-lock-file": true,
		"--no-write-lock-file": true, "--no-update-lock-file": true,
		"--unpack": true, "--build": true, "--print-build-logs": true,
		"--offline": true, "--refresh": true, "--quiet": true,
		"--verbose": true, "-v": true, "-L": true, "--check-sigs": true,
		"--no-registries": true, "--commit-lock-file": true,
		"--help": true, "-h": true, "--version": true, "--debug": true,
		"--show-trace": true,
	},
	cluster: true, operandL: "kufIj", flagL: "ivLh",
}

// machinectl's grammar; shell/login carry --uid and --setenv/-E operands
// (the -E NAME=value operand is judged by the generic denied scan).
var machinectlGrammar = launcherGrammar{
	operand: map[string]bool{
		"-H": true, "--host": true, "-M": true, "--machine": true,
		"--uid": true, "--setenv": true, "-E": true, "--property": true,
		"-p": true, "--value": true, "-o": true, "--output": true,
		"--lines": true, "-l": true, "--when": true,
	},
	flag: map[string]bool{
		"-q": true, "--quiet": true, "--no-legend": true, "--no-pager": true,
		"-a": true, "--all": true, "--full": true, "--system": true,
		"--user": true, "--read-only": true, "--mkdir": true,
		"--runtime": true, "--force": true, "-n": true, "-s": true,
		"--signal": true, "--autofix": true, "--drop-in": true,
		"--help": true, "-h": true, "--version": true,
	},
	cluster: true, operandL: "HMplo", flagL: "qansh",
	stopAfter: 2,
}

// distrobox enter's option grammar — the container name is a positional
// operand, -e and -c/--command carry command strings, and a `--`
// terminator's tail is the command.
var distroboxGrammar = launcherGrammar{
	operand: map[string]bool{
		"-n": true, "--name": true, "--additional-flags": true,
		"--home": true, "--abs-path": true, "-a": true,
		"-r": true, "--release": true, "--root": true,
		"--absolutely-disable-root-password-i-am-really-positively-sure": true,
	},
	payload: map[string]bool{"-e": true, "--exec": true, "-c": true, "--command": true},
	flag: map[string]bool{
		"--no-workdir": true, "--dry-run": true, "-d": true, "-Y": true,
		"--yes": true, "-T": true, "--no-tty": true, "-i": true,
		"--image": true, "-U": true, "--unshare-groups": true,
		"--unshare-devsys": true, "--unshare-ipc": true,
		"--unshare-netns": true, "--unshare-process": true,
		"--unshare-all": true, "-H": true, "-v": true, "--verbose": true,
		"-V": true, "--version": true, "-h": true, "--help": true,
	},
	cluster: true, operandL: "nar", payloadL: "ec", flagL: "dYTiUHvVh",
}

// toolbox run's option grammar — -c/-r/-d take operands and the trailing
// words are the command. toolbox enter takes a container name only.
var toolboxGrammar = launcherGrammar{
	operand: map[string]bool{
		"-c": true, "--container": true, "-r": true, "--release": true,
		"-d": true, "--distro": true, "--image": true,
	},
	flag: map[string]bool{
		"-y": true, "--assumeyes": true, "-v": true, "--verbose": true,
		"--very-verbose": true, "-q": true, "--quiet": true,
		"--preserve-fds": true, "--help": true, "-h": true,
	},
	cluster: true, operandL: "crd", flagL: "yvqh",
	stopAfter: 1,
}

// busctl's global option grammar — the verb and its operands are
// positional words after it. --json takes an optional =-attached value.
var busctlGrammar = launcherGrammar{
	operand: map[string]bool{
		"--address": true, "-H": true, "--host": true, "-M": true,
		"--machine": true, "--bus": true, "--match": true,
		"--timeout": true, "--limit": true, "--offset": true,
	},
	optional: map[string]bool{"--json": true},
	flag: map[string]bool{
		"--user": true, "--system": true, "-q": true, "--quiet": true,
		"-v": true, "--verbose": true, "--expect-reply": true,
		"--no-legend": true, "--no-pager": true, "--auto-start": true,
		"--allow-interactive-authorization": true, "--augment-creds": true,
		"--list": true, "-l": true, "--full": true, "--acquired": true,
		"--activatable": true, "-j": true, "--help": true, "-h": true,
		"--version": true, "--show-machine": true, "--unique": true,
	},
	cluster: true, operandL: "HM", flagL: "qvljh",
	stopAfter: 1,
}

// gdbus's global option grammar — GOption spelling (no clusters): -d/-o/
// -m/-t/-a take operands.
var gdbusGrammar = launcherGrammar{
	operand: map[string]bool{
		"-d": true, "--dest": true, "-o": true, "--object-path": true,
		"-m": true, "--method": true, "-t": true, "--timeout": true,
		"-a": true, "--address": true, "-y": true, "--peer": true,
		"-p": true, "--property": true, "-x": true, "--xml": true,
	},
	flag: map[string]bool{
		"--session": true, "--system": true, "--system-bus": true,
		"--systemd": true, "-r": true, "--help": true, "-h": true,
		"--version": true,
	},
	stopAfter: 1,
}

// busctl/gdbus call words that WRITE a service manager's environment —
// the writer class of issue #7603 item 3.
var dbusEnvironmentWriteMethods = map[string]struct{}{
	"SetEnvironment": {}, "UnsetEnvironment": {},
	"ImportEnvironment": {}, "SetEnvironmentVariable": {},
}

// ---------- the closed-grammar argv scanner ----------

// scannedOperand is one consumed option operand or positional word: the
// option that took it ("" for a positional), the literal text it carried,
// and its argv index.
type scannedOperand struct {
	opt     string
	literal string
	index   int
}

// launcherArgvScan walks a named launcher's argv under its closed grammar
// (property B): every literal must classify — a flag, a stated-arity
// operand option, or a per-letter cluster where the tool allows getopt
// clustering — and an option word the grammar does not know, a flag
// carrying an attached value, a truncated operand, or a non-literal word
// all make the invocation undecodable. `--` ends the option region;
// everything after it is positional. Returns the positional literals and
// the consumed operands in order.
func launcherArgvScan(words []*syntax.Word, g launcherGrammar) (positionals, operands []scannedOperand, undecodable bool) {
	for i := 0; i < len(words); i++ {
		literal, ok := launcherLiteralWord(words[i])
		if !ok {
			return nil, nil, true
		}
		if literal == "--" {
			for j := i + 1; j < len(words); j++ {
				l, ok2 := launcherLiteralWord(words[j])
				if !ok2 {
					return nil, nil, true
				}
				positionals = append(positionals, scannedOperand{literal: l, index: j})
			}
			return positionals, operands, false
		}
		if !strings.HasPrefix(literal, "-") || literal == "-" {
			positionals = append(positionals, scannedOperand{literal: literal, index: i})
			if g.stopAfter != 0 && len(positionals) >= g.stopAfter {
				for j := i + 1; j < len(words); j++ {
					l, ok2 := launcherLiteralWord(words[j])
					if !ok2 {
						return nil, nil, true
					}
					positionals = append(positionals, scannedOperand{literal: l, index: j})
				}
				return positionals, operands, false
			}
			continue
		}
		if strings.HasPrefix(literal, "--") {
			name, val, hasVal := strings.Cut(literal, "=")
			resolved, known := resolveLauncherLongOption(name, g)
			switch {
			case !known:
				return nil, nil, true
			case g.payload[resolved]:
				if hasVal {
					operands = append(operands, scannedOperand{opt: resolved, literal: val})
					continue
				}
				l, ok2 := launcherOperandAt(words, i+1)
				if !ok2 {
					return nil, nil, true
				}
				operands = append(operands, scannedOperand{opt: resolved, literal: l})
				i++
			case g.operand[resolved]:
				if hasVal {
					operands = append(operands, scannedOperand{opt: resolved, literal: val})
					continue
				}
				l, ok2 := launcherOperandAt(words, i+1)
				if !ok2 {
					return nil, nil, true
				}
				operands = append(operands, scannedOperand{opt: resolved, literal: l})
				i++
			case g.operand2[resolved]:
				l1, ok1 := launcherOperandAt(words, i+1)
				l2, ok2 := launcherOperandAt(words, i+2)
				if !ok1 || !ok2 {
					return nil, nil, true
				}
				operands = append(operands,
					scannedOperand{opt: resolved, literal: l1},
					scannedOperand{opt: resolved, literal: l2})
				i += 2
			case g.optional[resolved]:
				continue
			default: // a bare flag
				if hasVal {
					return nil, nil, true
				}
			}
			continue
		}
		// A single-dash word: a letter cluster when the tool's getopt
		// allows it, otherwise an exact table entry.
		if g.cluster {
			letters := literal[1:]
			j := 0
			for ; j < len(letters); j++ {
				c := letters[j]
				switch {
				case strings.IndexByte(g.flagL, c) >= 0:
					continue
				case strings.IndexByte(g.operandL, c) >= 0 || strings.IndexByte(g.payloadL, c) >= 0:
					rest := letters[j+1:]
					if rest == "" {
						l, ok2 := launcherOperandAt(words, i+1)
						if !ok2 {
							return nil, nil, true
						}
						operands = append(operands, scannedOperand{opt: "-" + string(c), literal: l})
						i++
					} else {
						operands = append(operands, scannedOperand{opt: "-" + string(c), literal: rest})
					}
					j = len(letters)
				default:
					return nil, nil, true
				}
			}
			continue
		}
		if g.operand[literal] || g.payload[literal] {
			l, ok2 := launcherOperandAt(words, i+1)
			if !ok2 {
				return nil, nil, true
			}
			operands = append(operands, scannedOperand{opt: literal, literal: l})
			i++
			continue
		}
		if g.flag[literal] || g.optional[literal] {
			continue
		}
		return nil, nil, true
	}
	return positionals, operands, false
}

// launcherOperandAt yields the literal operand a consumed option takes —
// a non-literal or missing operand makes the invocation undecodable.
func launcherOperandAt(words []*syntax.Word, i int) (string, bool) {
	if i >= len(words) {
		return "", false
	}
	return launcherLiteralWord(words[i])
}

// resolveLauncherLongOption maps a `--name` word to the grammar entry it
// spells — an exact match, or getopt_long's unique-prefix abbreviation
// (`--run` accepts `--ru`). An ambiguous prefix is one the tool itself
// cannot read — unknown either way.
func resolveLauncherLongOption(word string, g launcherGrammar) (string, bool) {
	for _, table := range []map[string]bool{g.operand, g.operand2, g.payload, g.flag, g.optional} {
		if table[word] {
			return word, true
		}
	}
	resolved := ""
	count := 0
	for _, table := range []map[string]bool{g.operand, g.operand2, g.payload, g.flag, g.optional} {
		for name := range table {
			if len(name) > 2 && strings.HasPrefix(name, word) {
				resolved, count = name, count+1
			}
		}
	}
	if count == 1 {
		return resolved, true
	}
	return "", false
}

// joinScannedPositionals renders positional literals joined with single
// spaces — the exact command string watch builds for `sh -c` and ssh
// builds for the remote shell.
func joinScannedPositionals(positionals []scannedOperand, from int) string {
	parts := make([]string, 0, len(positionals)-from)
	for _, p := range positionals[from:] {
		parts = append(parts, p.literal)
	}
	return strings.Join(parts, " ")
}

// ---------- pwsh / nu option classification ----------

// pwshNuOptionKind classifies one pwsh/nu option spelling — the gate does
// not parse their payload grammar, so each option maps to what it does to
// the input stream rather than to a flag/operand arity alone.
type pwshNuOptionKind int

const (
	pwshNuFlag    pwshNuOptionKind = iota // bare flag — no operand, no input
	pwshNuOperand                         // consumes the next word — data, not input
	pwshNuPayload                         // consumes the next word — the command string
	pwshNuHidden                          // reads input the gate cannot see — refuse
	pwshNuInfo                            // prints information and exits
)

// pwsh's option universe (dash-stripped, lowercase). pwsh accepts unique
// single-dash prefixes — `-com` is -Command, `-enc` is -EncodedCommand —
// so classification resolves a word by prefix across this whole table;
// an ambiguous or unknown prefix makes the invocation undecodable.
var pwshOptions = map[string]pwshNuOptionKind{
	"c": pwshNuPayload, "command": pwshNuPayload, "commandwithargs": pwshNuPayload,
	"encodedcommand": pwshNuHidden, "ec": pwshNuHidden, "e": pwshNuHidden,
	"file": pwshNuHidden, "f": pwshNuHidden, "psconsolefile": pwshNuHidden,
	"custompipename": pwshNuHidden, "noexit": pwshNuHidden,
	"interactive": pwshNuHidden, "i": pwshNuHidden, "login": pwshNuHidden,
	"nologo": pwshNuFlag, "noprofile": pwshNuFlag, "noninteractive": pwshNuFlag,
	"sta": pwshNuFlag, "mta": pwshNuFlag,
	"outputformat": pwshNuOperand, "inputformat": pwshNuOperand,
	"windowstyle": pwshNuOperand, "executionpolicy": pwshNuOperand,
	"settingsfile": pwshNuOperand, "configurationname": pwshNuOperand,
	"configurationfile": pwshNuOperand, "workingdirectory": pwshNuOperand,
	"experimentalfeature": pwshNuOperand,
	"version":             pwshNuInfo, "v": pwshNuInfo, "help": pwshNuInfo,
	"h": pwshNuInfo, "?": pwshNuInfo,
}

// nu's option universe — clap spellings, exact match only: -c/--commands
// and -e/--execute take command strings; the ide/mcp/stdin forms read
// input the gate cannot see.
var nuOptions = map[string]pwshNuOptionKind{
	"c": pwshNuPayload, "commands": pwshNuPayload,
	"e": pwshNuPayload, "execute": pwshNuPayload,
	"config": pwshNuOperand, "env-config": pwshNuOperand,
	"plugin-config": pwshNuOperand, "log-level": pwshNuOperand,
	"log-target": pwshNuOperand, "threads": pwshNuOperand, "t": pwshNuOperand,
	"testbin": pwshNuOperand, "error-style": pwshNuOperand,
	"stdin": pwshNuHidden, "i": pwshNuHidden, "interactive": pwshNuHidden,
	"l": pwshNuHidden, "login": pwshNuHidden, "m": pwshNuHidden,
	"mcp": pwshNuHidden, "ide-grep": pwshNuHidden, "ide-jump": pwshNuHidden,
	"ide-hover": pwshNuHidden, "ide-complete": pwshNuHidden,
	"ide-check": pwshNuHidden, "ide-ast": pwshNuHidden,
	"no-config-file": pwshNuFlag, "n": pwshNuFlag, "no-std-lib": pwshNuFlag,
	"no-history": pwshNuFlag,
	"v":          pwshNuInfo, "version": pwshNuInfo, "h": pwshNuInfo, "help": pwshNuInfo,
}

// pwshNuClassify walks pwsh/nu argv under its option universe: EVERY `-`
// word must classify — payload operand, plain operand, flag, hidden
// input, or info — and hidden input refuses regardless of whether a
// command operand was found (the property-A rule on this head: `pwsh
// -NoExit -Command Get-Date` still drops to an interactive stdin after
// the command, and `pwsh -Command -` reads the command from stdin).
// positional marks a bare word before the payload operand — a script
// file; words after the payload operand are the script's argv and carry
// no judgement. Returns the payload text ("" when absent or unreadable),
// whether one was found, a positional-before-payload marker, an info flag
// marker, and undecodable.
func pwshNuClassify(words []*syntax.Word, options map[string]pwshNuOptionKind, abbrev bool) (payload string, hasPayload, positional, info, undecodable bool) {
	payloadSeen := false
	for i := 0; i < len(words); i++ {
		literal, ok := launcherLiteralWord(words[i])
		if !ok {
			return "", false, false, false, true
		}
		if literal == "-" {
			// A bare `-` is pwsh's stdin marker — command input the
			// gate cannot see.
			return "", false, false, false, true
		}
		if !strings.HasPrefix(literal, "-") {
			if !payloadSeen {
				positional = true
			}
			continue
		}
		name := strings.ToLower(strings.TrimLeft(literal, "-"))
		optName, attached, hasAttached := strings.Cut(name, "=")
		kind, known := pwshNuResolveOption(optName, options, abbrev)
		if !known {
			return "", false, false, false, true
		}
		switch kind {
		case pwshNuHidden:
			return "", false, false, false, true
		case pwshNuInfo:
			info = true
		case pwshNuFlag:
			if hasAttached {
				return "", false, false, false, true
			}
		case pwshNuOperand:
			if _, ok2 := launcherOperandAt(words, i+1); !ok2 {
				return "", false, false, false, true
			}
			i++
		case pwshNuPayload:
			if hasAttached {
				payload, hasPayload, payloadSeen = attached, true, true
				continue
			}
			l, ok2 := launcherOperandAt(words, i+1)
			if !ok2 {
				return "", false, false, false, true
			}
			if l == "-" {
				// `-Command -` reads the command from stdin.
				return "", false, false, false, true
			}
			payload, hasPayload, payloadSeen = l, true, true
			i++
		}
	}
	return payload, hasPayload, positional, info, false
}

// pwshNuResolveOption resolves a dash-stripped option name against the
// table — exact match, or pwsh's unique single-dash prefix abbreviation.
func pwshNuResolveOption(name string, options map[string]pwshNuOptionKind, abbrev bool) (pwshNuOptionKind, bool) {
	if kind, ok := options[name]; ok {
		return kind, true
	}
	if !abbrev || len(name) < 2 {
		return pwshNuFlag, false
	}
	matches := 0
	var found pwshNuOptionKind
	for opt, kind := range options {
		if len(opt) > len(name) && strings.HasPrefix(opt, name) {
			matches++
			found = kind
		}
	}
	if matches == 1 {
		return found, true
	}
	return pwshNuFlag, false
}

// ---------- GVariant text decoding ----------

// gvariantDecode decodes the escapes a GVariant text-format literal can
// carry (gdbus parses its call operands as GVariant text — `CODEX\u005f`
// lands the underscore the raw scan cannot see). Every backslash must
// resolve: an escape the decoder does not know makes the operand
// undecodable, which means refuse (property A on this head).
func gvariantDecode(text string) (string, bool) {
	if !strings.ContainsRune(text, '\\') {
		return text, true
	}
	var out strings.Builder
	out.Grow(len(text))
	for i := 0; i < len(text); i++ {
		c := text[i]
		if c != '\\' {
			out.WriteByte(c)
			continue
		}
		i++
		if i >= len(text) {
			return "", false
		}
		switch text[i] {
		case 'a':
			out.WriteByte('\a')
		case 'b':
			out.WriteByte('\b')
		case 'f':
			out.WriteByte('\f')
		case 'n':
			out.WriteByte('\n')
		case 'r':
			out.WriteByte('\r')
		case 't':
			out.WriteByte('\t')
		case 'v':
			out.WriteByte('\v')
		case '\\', '\'', '"', '/':
			out.WriteByte(text[i])
		case 'x', 'u', 'U':
			widths := map[byte]int{'x': 2, 'u': 4, 'U': 8}
			width := widths[text[i]]
			if i+width >= len(text)+1 || i+width > len(text) {
				return "", false
			}
			var value rune
			for j := 0; j < width; j++ {
				i++
				if i >= len(text) {
					return "", false
				}
				d := hexDigitValue(text[i])
				if d < 0 {
					return "", false
				}
				value = value*16 + rune(d)
			}
			out.WriteRune(value)
		default:
			return "", false
		}
	}
	return out.String(), true
}

func hexDigitValue(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	case c >= 'A' && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}
