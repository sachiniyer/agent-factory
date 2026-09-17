package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// ionice's `-c"$C"` and `-n"$N"` (#4460).
//
// The token is ONE argv word only while the expansion is non-empty. Empty, it
// is bare `-c`, and getopt takes the NEXT word as the class, which moves the
// child. So the token is not self-contained, and admitting it means proving
// the swallowing reading harmless. It is harmless when that reading runs no
// command at all, because ionice checks the value first. Measured on
// util-linux 2.39.3:
//
//	C=;  ionice -c"$C" /bin/echo X     unknown scheduling class: '/bin/echo'
//	C=;  ionice -c"$C" 2 /bin/echo X   X   (the class swallowed "2")
//	N=;  ionice -n"$N" /bin/echo X     invalid class data argument: '/bin/echo'
//
// When the next word cannot be a valid value, only the non-empty reading
// survives, and it is exactly `-c2 W1…`. No second reading is carried, so there
// is no candidate set to grow. When the next word COULD be valid, both readings
// run commands and they run different ones, so the command is refused.

// ioniceClassNames are the names ionice's parse_ioclass matches with strcasecmp.
var ioniceClassNames = []string{"none", "realtime", "best-effort", "idle"}

// ioniceDynamicValueFlag recognizes `-c"$C"` and `-n"$N"`: literal `-c` or `-n`
// followed by exactly one double-quoted plain parameter expansion. A
// double-quoted plain parameter is always exactly one argv word, empty or
// not, so these two readings are the only ones. Any other dynamic spelling
// keeps failing closed.
func ioniceDynamicValueFlag(word *syntax.Word) (byte, bool) {
	if word == nil || len(word.Parts) < 2 {
		return 0, false
	}
	last, ok := word.Parts[len(word.Parts)-1].(*syntax.DblQuoted)
	if !ok || last.Dollar || len(last.Parts) != 1 || !oneWordParameter(last.Parts[0]) {
		return 0, false
	}
	prefix, complete := plainLiteralPrefix(word.Parts[:len(word.Parts)-1])
	if !complete {
		return 0, false
	}
	switch prefix {
	case "-c":
		return 'c', true
	case "-n":
		return 'n', true
	}
	return 0, false
}

// oneWordParameter reports whether part is `$NAME` or `${NAME}` with no
// operator, other than `$@`, which inside double quotes expands to zero or
// more words. This mirrors mvdan's unexported ParamExp.simple.
func oneWordParameter(part syntax.WordPart) bool {
	param, ok := part.(*syntax.ParamExp)
	if !ok || param.Param == nil || param.Param.Value == "@" {
		return false
	}
	return param.Flags == nil && !param.Excl && !param.Length && !param.Width &&
		!param.IsSet && param.NestedParam == nil && param.Index == nil &&
		len(param.Modifiers) == 0 && param.Slice == nil && param.Repl == nil &&
		param.Names == 0 && param.Exp == nil
}

// ioniceEmptyValueReadingLive reports whether `-<flag>` could take rest[0] as
// its value and still go on to run a command. That happens only when rest[0]
// can be a value ionice accepts. It answers true whenever that is uncertain.
func ioniceEmptyValueReadingLive(flag byte, rest []*syntax.Word) bool {
	if len(rest) == 0 {
		// A missing value: ionice exits before running anything.
		return false
	}
	if rest[0] == nil {
		return true
	}
	prefix, complete := plainLiteralPrefix(rest[0].Parts)
	if flag == 'c' {
		return ioniceClassMayBeValid(prefix, complete)
	}
	return ioniceClassDataMayBeValid(prefix, complete)
}

// ioniceClassMayBeValid mirrors ionice's -c parsing: a value whose first byte
// is a digit goes to strtos32_or_err, and anything else must match a class
// name case-insensitively. prefix is the value's known leading text, and
// complete says whether that is the whole value. Every digit-led value counts
// as valid, although ionice rejects `2t`; that errs toward refusal.
func ioniceClassMayBeValid(prefix string, complete bool) bool {
	if prefix == "" {
		// Whole and empty: `unknown scheduling class: ''`. Unknown: anything.
		return !complete
	}
	if prefix[0] >= '0' && prefix[0] <= '9' {
		return true
	}
	for _, name := range ioniceClassNames {
		if complete && asciiEqualFold(prefix, name) {
			return true
		}
		if !complete && len(prefix) <= len(name) && asciiEqualFold(prefix, name[:len(prefix)]) {
			return true
		}
	}
	return false
}

// ioniceClassDataMayBeValid mirrors ionice's -n parsing through strtoimax:
// optional leading whitespace, an optional sign, then digits. Trailing text and
// range are not checked, which errs toward refusal.
func ioniceClassDataMayBeValid(prefix string, complete bool) bool {
	digits := strings.TrimLeft(prefix, " \t\n\v\f\r")
	if digits != "" && (digits[0] == '+' || digits[0] == '-') {
		digits = digits[1:]
	}
	if digits == "" {
		return !complete
	}
	return digits[0] >= '0' && digits[0] <= '9'
}

// plainLiteralPrefix returns the leading text of parts that the shell passes
// through unchanged, and whether that text is ALL of parts. It stops at the
// first part it cannot read that way:
//   - any expansion;
//   - $'…' or $"…";
//   - unquoted text containing a backslash, a brace or a glob character;
//   - double-quoted text containing a backslash;
//   - a leading tilde.
//
// Stopping early only shortens the prefix, which callers treat as less known.
func plainLiteralPrefix(parts []syntax.WordPart) (string, bool) {
	var text strings.Builder
	for index, part := range parts {
		switch part := part.(type) {
		case *syntax.Lit:
			if strings.ContainsAny(part.Value, `\{*?[`) ||
				(index == 0 && strings.HasPrefix(part.Value, "~")) {
				return text.String(), false
			}
			text.WriteString(part.Value)
		case *syntax.SglQuoted:
			if part.Dollar {
				return text.String(), false
			}
			text.WriteString(part.Value)
		case *syntax.DblQuoted:
			if part.Dollar {
				return text.String(), false
			}
			for _, nested := range part.Parts {
				lit, ok := nested.(*syntax.Lit)
				if !ok || strings.Contains(lit.Value, `\`) {
					return text.String(), false
				}
				text.WriteString(lit.Value)
			}
		default:
			return text.String(), false
		}
	}
	return text.String(), true
}

// asciiEqualFold is strcasecmp's equality in the C locale: ASCII letters fold,
// other bytes must match exactly.
func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for index := 0; index < len(a); index++ {
		x, y := a[index], b[index]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// ioniceProofScope keeps the #4460 admission inside the option set its proof
// was reviewed against. #4465 admitted terminal options, process selectors,
// long-option abbreviations and pinned quoted tokens under proofs of their
// own. Neither proof covers a command that uses both, so such a command is
// refused whichever part comes first. The combination is very likely inert:
// an empty value swallows the option-shaped word and exits with `unknown
// scheduling class: '--help'` on util-linux 2.39.3. Admitting it widens both
// proofs, though, and #4465 pins `ionice -c"$CLASS" --help` as refused.
type ioniceProofScope struct{ dynamicValue, extended bool }

// admitDynamicValue records a `-c"$C"`/`-n"$N"` token and reports whether it
// may stand in this command.
func (s *ioniceProofScope) admitDynamicValue() bool {
	s.dynamicValue = true
	return !s.extended
}

// admitExtension records a word only #4465's proofs admit (a quoted selector
// or pinned token, or a literal option outside ioniceOriginalOption) and
// reports whether it may stand in this command.
func (s *ioniceProofScope) admitExtension() bool {
	s.extended = true
	return !s.dynamicValue
}

func (s *ioniceProofScope) admitLiteralOption(option string) bool {
	return ioniceOriginalOption(option) || s.admitExtension()
}

// ioniceOriginalOption reports whether a literal word belongs to the set the
// swallow proof was reviewed against: the child (any word not starting with
// '-'), `--`, -t/--ignore, and the exact -c/-n/--class/--classdata spellings,
// separate or attached. It is an allowlist, so an option admitted later stays
// out of the combination until the proof is extended to cover it.
func ioniceOriginalOption(option string) bool {
	name, _, _ := strings.Cut(option, "=")
	return !strings.HasPrefix(option, "-") ||
		option == "--" || option == "-t" || option == "--ignore" ||
		name == "--class" || name == "--classdata" ||
		strings.HasPrefix(option, "-c") || strings.HasPrefix(option, "-n")
}
