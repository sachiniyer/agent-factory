package sessionenv

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// systemdRunLongOptions models systemd-run's long options that take a value
// as the next argv word (--unit UNIT, --property NAME=VALUE, …). true means the
// option requires an argument (separate word or attached =value); false means it
// takes no argument. Measured from `systemd-run --help` (systemd 256); the
// attached --opt=value form is already caught by the generic --opt=DENIED arm in
// the non-strace wrapper scan, so this table only drives the separate-word form
// here. resolveLongOption resolves abbreviations the way getopt_long does.
var systemdRunLongOptions = map[string]bool{
	// Value-taking long options (separate word or attached =value).
	"host": true, "machine": true, "unit": true, "property": true,
	"description": true, "slice": true, "expand-environment": true,
	"service-type": true, "uid": true, "gid": true, "nice": true,
	"working-directory": true, "setenv": true, "json": true,
	"background": true, "path-property": true, "socket-property": true,
	"on-active": true, "on-boot": true, "on-startup": true,
	"on-unit-active": true, "on-unit-inactive": true, "on-calendar": true,
	"timer-property": true,

	// Boolean long options (no argument).
	"help": false, "version": false, "no-ask-password": false, "user": false,
	"scope": false, "slice-inherit": false, "no-block": false,
	"remain-after-exit": false, "wait": false, "send-sighup": false,
	"same-dir": false, "pty": false, "pipe": false, "quiet": false,
	"collect": false, "shell": false, "ignore-failure": false,
	"on-timezone-change": false, "on-clock-change": false,
}

// systemdRunShortValueFlags are systemd-run's short flags that take a value as
// the next argv word (or the rest of the cluster when the flag is not its last
// character). E is included so a cluster's value-taking E consumes its value the
// way a standalone -E does; a -E cluster's value is a --setenv NAME[=VALUE] and
// is judged by the standalone -E arm above when the cluster is exactly -E.
const systemdRunShortValueFlags = "EHMup"

// systemdRunOptionAwaitsValue reports whether a literal systemd-run option word
// takes its argument as the NEXT argv word, so the scan can step past that value
// instead of mistaking it for the command. A value-taking long option
// (--unit UNIT) or a short-option cluster whose first value-taking flag is its
// last character (-u UNIT) consumes the next word; an attached =value
// (--unit=UNIT, -uUNIT) or a cluster with a flag after the value-taking one
// (-up UNIT) does not. Unknown options are treated as not taking a value, so
// the scan stops at the command and a later -E/--setenv is judged.
func systemdRunOptionAwaitsValue(literal string) bool {
	switch {
	case strings.HasPrefix(literal, "--"):
		name, _, attached := strings.Cut(literal[2:], "=")
		if attached || name == "" {
			return false
		}
		_, requiresArg, known := resolveLongOption(name, systemdRunLongOptions)
		return known && requiresArg
	case strings.HasPrefix(literal, "-") && len(literal) > 1:
		flags := literal[1:]
		for i := 0; i < len(flags); i++ {
			if !strings.ContainsRune(systemdRunShortValueFlags, rune(flags[i])) {
				continue
			}
			// The first value-taking flag consumes the rest of the
			// cluster as its value when it is not the last character
			// (attached, e.g. -uUNIT); only when it is the last
			// character is the value the next argv word (-u UNIT).
			return i+1 == len(flags)
		}
		return false
	default:
		return false
	}
}

// systemdRunSeparateEnvOption reports whether literal is a systemd-run -E or
// --setenv option that takes its NAME[=VALUE] operand as the NEXT argv word —
// the separate-word form, as opposed to the attached -ENAME=VALUE or
// --setenv=NAME=VALUE form. It covers every spelling the option accepts: the
// exact -E and --setenv, the unambiguous --set abbreviation (resolveLongOption
// resolves --set to --setenv), and a short-option cluster whose first
// value-taking flag is E at the cluster's end (-qE … = -q -E …). The attached
// forms (-ENAME=VALUE, --setenv=NAME=VALUE) and a cluster whose E is followed by
// more flags have E's value attached in the cluster and are not separate-word
// options; they are recognized by systemdRunAttachedEnvOperand and the generic
// --opt=DENIED arm.
func systemdRunSeparateEnvOption(literal string) bool {
	switch {
	case strings.HasPrefix(literal, "--"):
		name, _, attached := strings.Cut(literal[2:], "=")
		if attached || name == "" {
			return false
		}
		resolved, requiresArg, known := resolveLongOption(name, systemdRunLongOptions)
		return known && requiresArg && resolved == "setenv"
	case strings.HasPrefix(literal, "-") && len(literal) > 1:
		flags := literal[1:]
		for i := 0; i < len(flags); i++ {
			if !strings.ContainsRune(systemdRunShortValueFlags, rune(flags[i])) {
				continue
			}
			// The first value-taking flag consumes the rest of the
			// cluster as its value when it is not the last character
			// (attached, e.g. -ECODEX_HOME); only when it is E and sits
			// at the cluster's end is the value the next argv word.
			return flags[i] == 'E' && i+1 == len(flags)
		}
		return false
	default:
		return false
	}
}

// systemdRunAttachedEnvOperand recognizes a systemd-run -E/--setenv attached env
// option in word's literal prefix and returns the literal portion of its
// operand — the NAME[=VALUE] text up to (not including) the first non-literal
// part. hasEnv is true when the prefix is a systemd-run env-option spelling. It
// mirrors straceAttachedEnvOperand for strace's -E/--env: a short-option cluster
// whose first value-taking flag is E (e.g. -E, -qE) and the long --setenv option
// or its unambiguous abbreviation with an attached =value (e.g. --setenv=,
// --set=). A non-attached --setenv whose literal name resolves to setenv (e.g.
// --setenv$V, --set$V) is reported with an empty operand: the expansion can
// complete the option and the next word can name a protected variable, so the
// caller fails closed.
func systemdRunAttachedEnvOperand(word *syntax.Word) (operand string, hasEnv bool) {
	lit := literalShellWordPrefix(word)
	switch {
	case strings.HasPrefix(lit, "--"):
		name, val, attached := strings.Cut(lit[2:], "=")
		if name == "" {
			return "", false
		}
		resolved, _, known := resolveLongOption(name, systemdRunLongOptions)
		if !known || resolved != "setenv" {
			return "", false
		}
		if !attached {
			return "", true
		}
		return val, true
	case strings.HasPrefix(lit, "-") && len(lit) > 1:
		flags := lit[1:]
		for i := 0; i < len(flags); i++ {
			if !strings.ContainsRune(systemdRunShortValueFlags, rune(flags[i])) {
				continue
			}
			if flags[i] != 'E' {
				return "", false
			}
			return flags[i+1:], true
		}
		return "", false
	default:
		return "", false
	}
}

// systemdRunEnvOptionHides reports whether systemd-run's -E/--setenv option sets
// or removes a protected variable. systemd-run's -E is the short form of
// --setenv and takes NAME[=VALUE] as the NEXT argv word (separate) or attached
// to the option (-ENAME=VALUE, --setenv=NAME=VALUE), analogous to strace's
// -E/--env. It covers every spelling the option accepts: the exact -E/--setenv,
// the unambiguous --set abbreviation, a short-option cluster whose first
// value-taking flag is E at the cluster's end (-qE … = -q -E …), and the
// attached -ENAME=VALUE and --setenv=NAME=VALUE forms. A non-literal operand
// fails closed (its expansion can name a protected variable); a literal
// operand is denied when its NAME (the text before the first '=', or the whole
// operand when no '=' is present) is denied.
//
// The attached --setenv=NAME=VALUE form is also caught by the generic
// --opt=DENIED arm the delegation applies, but the attached -ENAME=VALUE form
// is not (the generic arm splits on the first '=' and checks the value, not the
// NAME), so this function judges both attached forms.
//
// systemd-run takes other value-taking options before the command (--unit UNIT,
// --property NAME=VALUE, -p NAME=VALUE, …); their operand is the NEXT argv
// word, so the scan must consume it rather than mistaking it for the command and
// stopping before a later -E/--setenv (strace systemd-run --unit demo -E
// CODEX_HOME=/other codex: --unit consumes demo, so -E is systemd-run's
// --setenv, not the command). A value-taking option whose operand is a
// non-literal UNQUOTED expansion fails closed: the expansion can word-split into
// a further -E option the scan never sees as a separate argv word (strace
// systemd-run --unit $V codex with V='demo -E CODEX_HOME=/other').
func systemdRunEnvOptionHides(words []*syntax.Word, names map[string]struct{}) bool {
	for i := 0; i < len(words); i++ {
		literal, ok := literalShellWord(words[i])
		if !ok {
			// A non-literal option word whose literal prefix is a
			// systemd-run env option (--setenv=CODEX_HOME="$V",
			// -E"$V", --set"$V") can set or complete a protected
			// variable: the literal NAME is denied (or empty, so the
			// expansion can name one), and a QUOTED expansion stays one
			// argv word so systemd-run receives the option with the
			// protected NAME. Fail closed when the literal prefix is a
			// systemd-run env option (the attached form has a literal
			// NAME, the separate form's option word has a literal
			// spelling the next word can complete). A non-literal word
			// that is not a systemd-run env option is left to the
			// generic tail scan (a non-option word is the command).
			if _, isEnv := systemdRunAttachedEnvOperand(words[i]); isEnv {
				return true
			}
			if lit := literalShellWordPrefix(words[i]); systemdRunSeparateEnvOption(lit) {
				return true
			}
			return false
		}
		if literal == "--" {
			break
		}
		if !strings.HasPrefix(literal, "-") || literal == "-" {
			break
		}
		// The separate-word -E/--setenv form takes NAME[=VALUE] as the
		// next argv word (the exact -E/--setenv, the unambiguous --set
		// abbreviation, or a cluster ending in E like -qE). A non-literal
		// operand fails closed (its expansion can name a protected
		// variable), and a literal operand is denied when its NAME is
		// denied. A backslash in the NAME (before the first '=') is not
		// shell-stable, so the operand is denied the same way an
		// attached -E\CODEX_HOME is (the shell removes the backslash and
		// systemd-run receives the dequoted denied name).
		if systemdRunSeparateEnvOption(literal) {
			if i+1 >= len(words) {
				return true
			}
			value, valueOk := literalShellWord(words[i+1])
			if !valueOk || wordHasUnquotedBackslashBeforeEq(words[i+1]) ||
				accountEnvironmentOperandDenied(value, names) {
				return true
			}
			i++
			continue
		}
		// The attached -ENAME=VALUE form has the operand in the same
		// word; the generic --opt=DENIED arm already catches
		// --setenv=NAME=VALUE, but -ENAME=VALUE is not a --opt=DENIED
		// arm (the generic arm splits on the first '=' and checks the
		// value, not the NAME), so judge it here. A backslash in the
		// NAME is not shell-stable, so the operand is denied the same
		// way the separate-word form's NAME is.
		if strings.HasPrefix(literal, "-E") && len(literal) > 2 {
			if wordHasUnquotedBackslashBeforeEq(words[i]) ||
				accountEnvironmentOperandDenied(literal[2:], names) {
				return true
			}
			continue
		}
		// A value-taking option consumes its operand from the next argv
		// word, so the scan steps past it rather than ending the option
		// region at the operand (which would let a later -E/--setenv hide
		// behind a value the scan mistook for the command). A
		// non-literal UNQUOTED operand can word-split into a further
		// -E option the scan never sees as a separate argv word (strace
		// systemd-run --unit $V codex with V='demo -E CODEX_HOME=/other'),
		// so fail closed when the operand is a non-literal unquoted
		// expansion; a QUOTED expansion stays one option value.
		if systemdRunOptionAwaitsValue(literal) {
			if i+1 >= len(words) {
				break
			}
			if wordHasUnquotedExpansion(words[i+1]) {
				return true
			}
			i++
		}
	}
	return false
}

// systemdRunCommandTail scans systemd-run's option words and returns the tail
// starting at the command — the first non-option word, or the word after a
// literal "--" terminator. A non-literal option word stops the scan (the
// command is unprovable); a non-option word is the command. It mirrors
// systemdRunEnvOptionHides's option-consume logic but returns the tail rather
// than judging the env options, so the caller can re-enter the strace scan for
// an inner strace (strace systemd-run strace -E CODEX_HOME=/other codex).
func systemdRunCommandTail(words []*syntax.Word) []*syntax.Word {
	for i := 0; i < len(words); i++ {
		literal, ok := literalShellWord(words[i])
		if !ok {
			return nil
		}
		if literal == "--" {
			return words[i+1:]
		}
		if !strings.HasPrefix(literal, "-") || literal == "-" {
			return words[i:]
		}
		if systemdRunSeparateEnvOption(literal) {
			if i+1 >= len(words) {
				return nil
			}
			i++
			continue
		}
		if systemdRunOptionAwaitsValue(literal) {
			if i+1 >= len(words) {
				return nil
			}
			i++
		}
	}
	return nil
}
