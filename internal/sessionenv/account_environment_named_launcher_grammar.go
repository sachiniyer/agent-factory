package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// This file holds the small classification tables #7603's named
// launchers still need — and ONLY those. The review rule on this head
// (the third Codex round) is that the verdict must not depend on
// parsing a launcher's option grammar: the gate refuses when ANY
// reading of the tail could reach the account's agent under a reset or
// write a protected name, so option arity tables were removed. What
// remains is input-source classification (a word whose operand is input
// the gate cannot see is a refusal, never a parse) and the launchers'
// own verb/name tables.

// tmux's pane-creating subcommands and their documented aliases (scope
// answer on #7603). Pane commands run under the tmux SERVER's
// environment, never the pane's, so they are reset-class. The #7464
// respawn guard exempts these two map-literal lines by exact content —
// a respawn string anywhere else still fails the guard.
var tmuxPaneCreatingSubcommands = map[string]struct{}{
	"new-session": {}, "new": {},
	"new-window": {}, "neww": {},
	"split-window": {}, "splitw": {},
	"respawn-pane": {}, "respawnp": {},
	"respawn-window": {}, "respawnw": {},
}

// busctl/gdbus call words that WRITE a service manager's environment —
// the writer class of issue #7603 item 3.
var dbusEnvironmentWriteMethods = map[string]struct{}{
	"SetEnvironment": {}, "UnsetEnvironment": {},
	"ImportEnvironment": {}, "SetEnvironmentVariable": {},
}

// ---------- shared tail readings ----------

// launcherTailLiterals renders every tail word as its literal — a word
// that does not literalize (a substitution, an expansion) is input the
// gate cannot see and makes the whole tail undecodable, which means
// refuse.
func launcherTailLiterals(words []*syntax.Word) ([]string, bool) {
	literals := make([]string, 0, len(words))
	for _, word := range words {
		literal, ok := launcherLiteralWord(word)
		if !ok {
			return nil, false
		}
		literals = append(literals, literal)
	}
	return literals, true
}

// joinTailLiterals renders literals joined with single spaces — the
// command string a joining launcher (watch, ssh, tmux's one-word pane
// command, nix -c) hands to `sh -c`.
func joinTailLiterals(literals []string) string {
	return strings.Join(literals, " ")
}

// quoteTailLiterals renders literals as a shell string that parses back
// to EXACTLY that argv — every element single-quoted — for launchers
// that exec their command argv without a shell. `/opt/agent
// tools/codex` stays one program word whose final component names the
// agent, where a space join would repurpose `tools/codex` as an
// operand.
func quoteTailLiterals(literals []string) string {
	parts := make([]string, 0, len(literals))
	for _, literal := range literals {
		parts = append(parts, "'"+strings.ReplaceAll(literal, "'", `'\''`)+"'")
	}
	return strings.Join(parts, " ")
}

// literalCarriesShellSyntax reports whether a literal can bind two
// program texts into one word or carry command syntax of its own —
// anything outside the plain program-word alphabet `[A-Za-z0-9_./:+-]`.
// Such a word is a candidate command string in its own right (`ssh host
// 'a; codex'`, `tmux new-window 'CODEX_HOME=/o codex'`).
func literalCarriesShellSyntax(literal string) bool {
	for i := 0; i < len(literal); i++ {
		c := literal[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '_' || c == '.' || c == '/' || c == ':' || c == '+' || c == '-':
		default:
			return true
		}
	}
	return false
}

// ---------- hidden-input words ----------

// namedLauncherHiddenWords lists, per launcher, the option spellings
// whose operand is input the gate cannot see — a file, a descriptor, or
// a language it does not parse. This is not an option grammar: a hidden
// word refuses in every reading, and long names also match their
// getopt_long unique-prefix abbreviations (`--env-f` is `--env-fd`).
var namedLauncherHiddenWords = map[string][]string{
	// --env-fd sources the child environment from a descriptor;
	// --unset-env-fd clears the same way.
	"flatpak-spawn": {"--env-fd", "--unset-env-fd"},
	// -F sources ssh's options from a config file that can carry
	// ProxyCommand — a command the gate cannot read.
	"ssh": {"-F"},
	// Nix -E/--expr/--arg/--argstr/-I/--include take Nix EXPRESSIONS —
	// code the gate does not parse, evaluated before the shell starts.
	"nix-shell": {"-E", "--expr", "--arg", "--argstr", "-I", "--include"},
	"nix":       {"-E", "--expr", "--arg", "--argstr", "-I", "--include"},
}

// literalNamesHiddenInput reports whether literal spells a hidden-input
// word for the launcher: the word itself, a `--x=value` attachment, a
// clustered `-Xvalue` spelling, or a unique-prefix abbreviation of a
// `--` entry.
func literalNamesHiddenInput(literal string, hidden []string) bool {
	for _, word := range hidden {
		if literal == word {
			return true
		}
		if strings.HasPrefix(literal, word+"=") {
			return true
		}
		if !strings.HasPrefix(word, "--") && strings.HasPrefix(literal, word) && len(literal) > len(word) {
			// A single-dash word attaches its operand: `-Fcfg`.
			return true
		}
	}
	// Long-option unique-prefix abbreviations: a `--` word that prefixes
	// a hidden entry may spell it (`--env-f` -> `--env-fd`). A prefix
	// cannot be attributed without the launcher's full table, so any
	// prefix match at all refuses: an operand the gate cannot attribute
	// is already input it cannot see.
	for _, word := range hidden {
		if strings.HasPrefix(word, literal) && strings.HasPrefix(literal, "--") && len(literal) > 2 {
			return true
		}
	}
	return false
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
	pwshNuTail                            // the whole REMAINING argv joins into the command (pwsh -Command)
	pwshNuHidden                          // reads input the gate cannot see — refuse
	pwshNuInfo                            // prints information and exits
)

// pwsh's option universe (dash-stripped, lowercase). pwsh accepts unique
// single-dash prefixes — `-com` is -Command, `-enc` is -EncodedCommand —
// so classification resolves a word by prefix across this whole table;
// an ambiguous or unknown prefix makes the invocation undecodable.
// Settings/console/configuration files and the named-pipe forms are
// hidden input: they load files or channels whose content executes.
var pwshOptions = map[string]pwshNuOptionKind{
	"c": pwshNuTail, "command": pwshNuTail, "commandwithargs": pwshNuPayload,
	"encodedcommand": pwshNuHidden, "ec": pwshNuHidden, "e": pwshNuHidden,
	"file": pwshNuHidden, "f": pwshNuHidden, "psconsolefile": pwshNuHidden,
	"custompipename": pwshNuHidden, "noexit": pwshNuHidden,
	"interactive": pwshNuHidden, "i": pwshNuHidden, "login": pwshNuHidden,
	"settingsfile": pwshNuHidden, "configurationname": pwshNuHidden,
	"configurationfile": pwshNuHidden,
	"nologo":            pwshNuFlag, "noprofile": pwshNuFlag, "noninteractive": pwshNuFlag,
	"sta": pwshNuFlag, "mta": pwshNuFlag,
	"outputformat": pwshNuOperand, "inputformat": pwshNuOperand,
	"windowstyle": pwshNuOperand, "executionpolicy": pwshNuOperand,
	"workingdirectory": pwshNuOperand, "experimentalfeature": pwshNuOperand,
	"version": pwshNuInfo, "v": pwshNuInfo, "help": pwshNuInfo,
	"h": pwshNuInfo, "?": pwshNuInfo,
}

// nu's option universe — clap spellings, exact match only: -c/--commands
// takes a bounded command string; --config/--env-config/--plugin-config
// load files that are parsed and evaluated, -e/--execute falls through
// to the REPL after its command, and the ide/mcp/stdin forms all read
// input the gate cannot see.
var nuOptions = map[string]pwshNuOptionKind{
	"c": pwshNuPayload, "commands": pwshNuPayload,
	"e": pwshNuHidden, "execute": pwshNuHidden,
	"config": pwshNuHidden, "env-config": pwshNuHidden,
	"plugin-config": pwshNuHidden,
	"log-level":     pwshNuOperand, "log-target": pwshNuOperand,
	"threads": pwshNuOperand, "t": pwshNuOperand,
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
// command operand was found (`pwsh -NoExit -Command Get-Date` still
// drops to an interactive stdin, `pwsh -Command -` reads stdin, and
// `nu --config f -c ls` evaluates f first). positional marks a bare word
// before the first payload operand — a script file; words after a
// single-operand payload are the script's argv and carry no judgement.
// Returns the payload texts (every payload operand — `nu -c a -c b` runs
// both), a positional-before-payload marker, an info flag marker, and
// undecodable.
func pwshNuClassify(words []*syntax.Word, options map[string]pwshNuOptionKind, abbrev bool) (payloads []string, positional, info, undecodable bool) {
	payloadSeen := false
	for i := 0; i < len(words); i++ {
		literal, ok := launcherLiteralWord(words[i])
		if !ok {
			return nil, false, false, true
		}
		if literal == "-" {
			// A bare `-` is pwsh's stdin marker — command input the
			// gate cannot see.
			return nil, false, false, true
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
			return nil, false, false, true
		}
		switch kind {
		case pwshNuHidden:
			return nil, false, false, true
		case pwshNuInfo:
			info = true
		case pwshNuFlag:
			if hasAttached {
				return nil, false, false, true
			}
		case pwshNuOperand:
			if _, ok2 := launcherOperandAt(words, i+1); !ok2 {
				return nil, false, false, true
			}
			i++
		case pwshNuTail:
			// pwsh's -Command: EVERY word after it joins into the
			// command — option parsing is over, so `pwsh -Command
			// Write-Output '; codex'` judges the whole tail, not its
			// first word. A `-Command=x` spelling is not a form pwsh
			// accepts — undecodable, which means refuse.
			if hasAttached {
				return nil, false, false, true
			}
			var tail []string
			for j := i + 1; j < len(words); j++ {
				l, ok2 := launcherLiteralWord(words[j])
				if !ok2 {
					return nil, false, false, true
				}
				tail = append(tail, l)
			}
			if len(tail) == 0 {
				// `-Command` with nothing after it reads the command
				// from stdin — hidden input.
				return nil, false, false, true
			}
			if len(tail) == 1 && tail[0] == "-" {
				// `-Command -` reads the command from stdin.
				return nil, false, false, true
			}
			payloads = append(payloads, strings.Join(tail, " "))
			payloadSeen = true
			i = len(words)
		case pwshNuPayload:
			if hasAttached {
				payloads, payloadSeen = append(payloads, attached), true
				continue
			}
			l, ok2 := launcherOperandAt(words, i+1)
			if !ok2 {
				return nil, false, false, true
			}
			if l == "-" {
				// `-Command -` reads the command from stdin.
				return nil, false, false, true
			}
			payloads, payloadSeen = append(payloads, l), true
			i++
		}
	}
	return payloads, positional, info, false
}

// launcherOperandAt yields the literal operand a consumed option takes —
// a non-literal or missing operand makes the invocation undecodable.
func launcherOperandAt(words []*syntax.Word, i int) (string, bool) {
	if i >= len(words) {
		return "", false
	}
	return launcherLiteralWord(words[i])
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
// undecodable, which means refuse.
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
