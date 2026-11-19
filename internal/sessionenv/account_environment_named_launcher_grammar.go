package sessionenv

import (
	"slices"
	"strings"
)

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
	// options case-insensitively, on any of its option prefixes: `-`,
	// `/` and the Unicode dashes U+2013/U+2014/U+2015): -EncodedCommand
	// and -EncodedArguments and their -e/-enc/-ea abbreviations carry
	// base64 the text cannot show, the console/settings/configuration/
	// pipe-name options read channels, -NoExit/-Interactive/-Login keep
	// or open an interactive stdin, and a bare `-` is the stdin marker.
	"pwsh": {"-e", "-ec", "-enc", "-encodedcommand", "-ea",
		"-encodedarguments",
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
	// nix/nix-shell's documented non-hidden spellings — `-i` is
	// --ignore-environment, a visible flag, so an `i` body resolves to
	// it rather than abbreviating --include; `-c`/`--command` and the
	// rest are visible options the prefix check must not call hidden.
	"nix": {"-i", "--ignore-environment", "-c", "--command",
		"-k", "--keep", "-u", "--unset", "--impure", "--profile",
		"--installable", "-f", "--file", "-L", "--print-build-logs",
		"-j", "--cores", "--offline", "--refresh", "--version",
		"-v", "--verbose", "-h", "--help", "--show-trace"},
	"nix-shell": {"--pure", "-p", "--packages", "--run",
		"--command", "-j", "--cores", "--exclude",
		"--show-trace", "--repair", "-q", "--quiet", "-v",
		"--verbose", "-h", "--help", "--version", "-A", "--attr"},
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

// namedLauncherShellJoins names the launchers that hand their tail argv
// to a shell as ONE joined string — watch runs its command through
// `sh -c` by default, ssh joins the remote command for the remote
// shell, and tmux's and machinectl's command arguments are a shell
// command line. The downstream shell re-splits the joined text into
// words, so the re-split words are a required reading of the tail:
// every per-word check (hidden-input spellings, payload
// classification, name scans) runs over them, never only over the
// preserved argv fields — `watch pwsh '-enc x'` is `pwsh -enc x` to
// the shell, not one option word to pwsh.
var namedLauncherShellJoins = map[string]struct{}{
	"watch": {}, "ssh": {}, "tmux": {}, "machinectl": {},
}

// pwshOptionPrefixRunes is the leading-run set PowerShell's own switch
// parser accepts before an option name: `-`, `/`, and the Unicode
// dashes U+2013, U+2014 and U+2015. Every other modelled launcher takes
// `-` only.
const pwshOptionPrefixRunes = "-/–—―"

// optionWordBody canonicalizes an option spelling to the body the
// launcher resolves: the leading run of option-prefix characters goes
// (`--`, `/`, `–` all collapse to the same body), a `:` or `=` operand
// attachment is cut, and pwsh's body is lowered because its option
// resolution is case-insensitive. ok is false when the word carries no
// option prefix at all — `/tmp/x.ps1` and `-` are not option spellings.
func optionWordBody(literal, launcher string) (body string, ok bool) {
	prefixes := "-"
	if launcher == "pwsh" {
		prefixes = pwshOptionPrefixRunes
	}
	body = strings.TrimLeft(literal, prefixes)
	if body == literal || body == "" {
		return "", false
	}
	if cut := strings.IndexAny(body, ":="); cut >= 0 {
		body = body[:cut]
	}
	if launcher == "pwsh" {
		body = strings.ToLower(body)
	}
	return body, body != ""
}

// optionTableBody canonicalizes a table spelling the same way an argv
// word is canonicalized — strip the leading dash run, lower for pwsh —
// so words in every prefix form resolve against the stored names.
func optionTableBody(word, launcher string) string {
	body := strings.TrimLeft(word, "-")
	if launcher == "pwsh" {
		body = strings.ToLower(body)
	}
	return body
}

// singleDashOption reports whether a table spelling is a single-dash
// form — clap and nix-style parsers take the operand attached inside
// the word (`-Eexpr`, `-Ipath`, `-mfoo`). pwsh is excluded: its operand
// attaches through `:` instead, which optionWordBody already cuts.
func singleDashOption(word string) bool {
	return strings.HasPrefix(word, "-") && !strings.HasPrefix(word, "--")
}

// literalNamesHiddenInput reports whether literal spells a hidden-input
// word for the launcher. After stripping its leading run of option
// prefixes the body is an option spelling when it IS a hidden name
// (exact, including pwsh's documented `-e`/`-ec`/`-enc`/`-ea`/`-i`/`-l`
// aliases), or a case-insensitive non-empty PREFIX of one — pwsh's `-en`
// is -EncodedCommand and `-encodedarg` is -EncodedArguments on any dash.
// A body that also prefixes a non-hidden name is ambiguous in the real
// launcher — it errors — so only a hidden-only prefix refuses. For
// single-dash hidden spellings the operand attaches in-word (`-Eexpr`),
// which for pwsh goes through the `:` cut instead.
func literalNamesHiddenInput(literal, launcher string) bool {
	hidden := namedLauncherHiddenWords[launcher]
	if literal == "-" {
		return slices.Contains(hidden, "-") // the stdin marker
	}
	body, ok := optionWordBody(literal, launcher)
	if !ok {
		return false
	}
	// The word's own prefix shape decides whether an operand can attach
	// in-word: a single dash does (`-Eexpr`); `--`, `/` and the Unicode
	// dashes do not. pwsh's attachment is the `:` cut, handled in
	// optionWordBody.
	wordAttach := launcher != "pwsh" &&
		strings.HasPrefix(literal, "-") && !strings.HasPrefix(literal, "--")
	ambiguous := false
	for _, known := range namedLauncherNonHiddenNames[launcher] {
		knownBody := optionTableBody(known, launcher)
		if body == knownBody {
			return false // the body IS a non-hidden option
		}
		if strings.HasPrefix(knownBody, body) {
			ambiguous = true
		}
	}
	for _, word := range hidden {
		hiddenBody := optionTableBody(word, launcher)
		if hiddenBody == "" {
			continue
		}
		if body == hiddenBody {
			return true
		}
		if strings.HasPrefix(hiddenBody, body) {
			if !ambiguous {
				return true
			}
			continue
		}
		// `-xVALUE` — the operand attaches in-word, but only when the
		// WORD itself was single-dash: `--env-config` is a long option,
		// not `-e` carrying `nv-config`.
		if wordAttach && singleDashOption(word) &&
			len(body) > len(hiddenBody) && strings.HasPrefix(body, hiddenBody) {
			return true
		}
	}
	return false
}

// literalSpellsTailPayload reports whether literal spells a payload
// (command-string) option word for the launcher — the same canonical
// body form the hidden-input check uses, so `–c`, `/Command` and
// `-com` resolve like the plain spelling does.
func literalSpellsTailPayload(literal, launcher string) bool {
	body, ok := optionWordBody(literal, launcher)
	if !ok {
		return false
	}
	for _, word := range namedLauncherTailPayloadWords[launcher] {
		payloadBody := optionTableBody(word, launcher)
		if body == payloadBody || strings.HasPrefix(payloadBody, body) {
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
