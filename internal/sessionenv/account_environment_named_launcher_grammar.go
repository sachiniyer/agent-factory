package sessionenv

import "strings"

// This file holds the only per-launcher classification #7603 keeps — and
// it is NOT an option grammar. The review rule on this head (the sixth
// Codex round, decision on the PR) is that a modelled launcher's verdict
// never asks where its command starts: every literal piece of the tail —
// each argv word and every piece split on whitespace, `=`, `;`, `&`,
// `|`, quotes and parentheses — is checked for the account's agent
// program or a protected variable at identifier boundaries, and the tail
// refuses when any piece names one, carries command-substitution or
// dynamic-expansion text, or spells input the gate cannot see. What this
// file holds is input-source classification only: which WORDS name a
// descriptor, stdin or a language the gate does not parse — the same
// refusal class the gate gives `sh -s`, applied by word identity rather
// than by option position. Commands and config read from a FILE are the
// stated limit in docs/usage-limits.md — `ssh -F`, `tmux -f`/
// `source-file`, a pwsh/nu script path admit the way `bash ./build.sh`
// does: the gate judges the visible command line, and a file's content
// is outside it.

// ---------- hidden-input words ----------

// namedLauncherHiddenWords lists, per launcher, the option spellings
// whose operand is input the gate cannot see — a file, a descriptor,
// stdin, or a language it does not parse. It is classification, not an
// option grammar: the word refuses wherever it sits, attached or bare,
// and abbreviation forms are covered by literalNamesHiddenInput — a word
// the gate cannot attribute is already input it cannot see.
var namedLauncherHiddenWords = map[string][]string{
	// --env-fd sources the child environment from a descriptor;
	// --unset-env-fd clears the same way. A descriptor of environment
	// data is not the file limit — it is an injection vector the pane
	// cannot see at all.
	"flatpak-spawn": {"--env-fd", "--unset-env-fd"},
	// Nix -E/--expr/--arg/--argstr/-I/--include take Nix EXPRESSIONS —
	// code the gate does not parse, evaluated before the shell starts;
	// --arg-from-stdin reads the same input from stdin.
	"nix-shell": {"-E", "--expr", "--arg", "--argstr", "-I", "--include",
		"--arg-from-stdin"},
	"nix": {"-E", "--expr", "--arg", "--argstr", "-I", "--include",
		"--arg-from-stdin"},
	// pwsh's hidden forms (case-insensitive — PowerShell resolves
	// options case-insensitively): -EncodedCommand and its -e/-enc
	// abbreviations carry base64 the text cannot show, -File and the
	// console/settings/configuration/pipe-name options read files and
	// channels, -NoExit/-Interactive/-Login keep or open an interactive
	// stdin, and a bare `-` is the stdin marker.
	"pwsh": {"-e", "-ec", "-enc", "-encodedcommand",
		"-custompipename", "-noexit", "-i", "-interactive", "-l",
		"-login", "-noninteractive", "-configurationname", "-"},
	// nu's hidden forms (clap spellings, case-sensitive): the config and
	// plugin files are evaluated, -e/--execute falls through to the REPL
	// after its command, and stdin/login/mcp/ide forms all read input
	// the gate cannot see.
	"nu": {"-e", "--execute", "--stdin", "-i", "--interactive",
		"-l", "--login", "-m", "--mcp", "--ide-grep", "--ide-jump",
		"--ide-hover", "--ide-complete", "--ide-check", "--ide-ast", "-"},
}

// namedLauncherNonHiddenNames lists, per launcher that needs it, the
// option names a hidden-word abbreviation must NOT be attributed to: a
// `-` word that prefixes only hidden names refuses, but one that also
// prefixes a visible option is ambiguous in the real launcher — it
// errors, which carries no hidden input. pwsh resolves unique
// single-dash prefixes (`-com` is -Command, `-enc` is -EncodedCommand),
// so it needs the full non-hidden set; nu's clap does no abbreviation,
// so only its long names matter for `--` prefix clashes.
var namedLauncherNonHiddenNames = map[string][]string{
	"pwsh": {"-c", "-command", "-commandwithargs", "-nologo",
		"-noprofile", "-sta", "-mta",
		"-outputformat", "-inputformat", "-windowstyle",
		"-executionpolicy", "-workingdirectory",
		"-experimentalfeature", "-version", "-v", "-help", "-h", "-?"},
	"nu": {"-c", "--commands", "--log-level", "--log-target",
		"--threads", "-t", "--testbin", "--error-style",
		"--no-config-file", "-n", "--no-std-lib", "--no-history",
		"-v", "--version", "-h", "--help"},
}

// namedLauncherTailPayloadWords lists, per launcher, the option
// spellings whose operand is the command string. A payload word at the
// very end of the tail reads its command from stdin, which is the same
// hidden input the table above refuses. Payload words WITH a following
// operand carry their command as text the piece scan already judges.
var namedLauncherTailPayloadWords = map[string][]string{
	"pwsh": {"-c", "-command", "-commandwithargs"},
	"nu":   {"-c", "--commands"},
}

// namedLauncherDecodesOperands names the launchers whose operand text is
// an encoded format the gate decodes before the piece scan — gdbus
// parses its call operands as GVariant text, so a `CODEX\u005fHOME`
// spelling lands the underscore the raw text cannot see.
var namedLauncherDecodesOperands = map[string]struct{}{
	"gdbus": {},
}

// literalNamesHiddenInput reports whether literal spells a hidden-input
// word for the launcher: the word itself, a `--x=value` attachment, a
// clustered `-Xvalue` spelling, or a prefix-abbreviated spelling
// (`--env-f` is `--env-fd`; pwsh's `-en` is -EncodedCommand). A prefix
// that also spells a non-hidden name is ambiguous in the real launcher —
// it errors — so only a hidden-only prefix refuses.
func literalNamesHiddenInput(literal, launcher string) bool {
	name := literal
	if launcher == "pwsh" {
		name = strings.ToLower(name)
	}
	hidden := namedLauncherHiddenWords[launcher]
	// pwsh resolves unique single-dash prefixes, so a word that spells a
	// non-hidden option (`-ex` for -ExecutionPolicy) is that option, not
	// an attached operand of the hidden name it starts with (`-e`).
	nonHiddenPrefix := false
	for _, known := range namedLauncherNonHiddenNames[launcher] {
		if strings.HasPrefix(known, name) || known == name {
			nonHiddenPrefix = true
			break
		}
	}
	for _, word := range hidden {
		if name == word || strings.HasPrefix(name, word+"=") {
			return true
		}
		if !nonHiddenPrefix && len(word) >= 2 && !strings.HasPrefix(word, "--") &&
			strings.HasPrefix(name, word) && len(name) > len(word) {
			// A single-dash word attaches its operand: `-Fcfg`, `-f/path`.
			// (A bare `-` word is the stdin marker only by itself.)
			return true
		}
	}
	for _, word := range hidden {
		if len(name) <= 2 || !strings.HasPrefix(name, "-") || !strings.HasPrefix(word, name) {
			continue
		}
		if !nonHiddenPrefix {
			return true
		}
	}
	return false
}

// literalSpellsTailPayload reports whether literal spells a payload
// (command-string) option word for the launcher — exactly, or as pwsh's
// single-dash unique-prefix abbreviation of one.
func literalSpellsTailPayload(literal, launcher string) bool {
	name := literal
	if launcher == "pwsh" {
		name = strings.ToLower(name)
	}
	for _, word := range namedLauncherTailPayloadWords[launcher] {
		if name == word {
			return true
		}
		if len(name) > 2 && strings.HasPrefix(name, "-") &&
			!strings.HasPrefix(name, "--") && strings.HasPrefix(word, name) {
			return true
		}
	}
	return false
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
