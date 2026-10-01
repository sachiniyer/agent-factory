package sessionenv

import "strings"

// These tables model how strace and GNU xargs read their long options, so
// the xargs input walk can tell which word an option consumes. They are
// measured, not transcribed from --help: every name below was probed on
// strace 6.8 and GNU findutils xargs 4.9 by running `--name` with nothing
// after it ("requires an argument" or not), and the strace list is closed
// under the names that every one- and two-letter prefix's ambiguity error
// reports (#4978). strace accepts undocumented aliases, e.g. --decode-pid,
// --detach, --trace-fd and --signals, so --help alone misses options that
// take a value.
//
// true means the option requires an argument, which may be the next word.
// false means it takes none, or only an attached `=value`.
var straceLongOptions = map[string]bool{
	"abbrev": true, "argv0": true, "attach": true, "columns": true,
	"const-print-style": true, "decode-pid": true, "decode-pids": true,
	"detach": true, "detach-on": true, "env": true, "fault": true, "inject": true,
	"interruptible": true, "kvm": true, "output": true, "raw": true, "read": true,
	"signal": true, "signals": true, "stack-trace-frame-limit": true,
	"status": true, "string-limit": true, "summary-columns": true,
	"summary-sort-by": true, "summary-syscall-overhead": true,
	"syscall-limit": true, "trace": true, "trace-fd": true, "trace-fds": true,
	"trace-path": true, "user": true, "verbose": true, "write": true,

	"absolute-timestamps": false, "daemonised": false, "daemonize": false,
	"daemonized": false, "debug": false, "decode-fd": false, "decode-fds": false,
	"failed-only": false, "failing-only": false, "follow-forks": false,
	"help": false, "instruction-pointer": false, "kill": false,
	"kill-on-exit": false, "no-abbrev": false, "output-append-mode": false,
	"output-separately": false, "pidns": false, "pidns-translation": false,
	"quiet": false, "relative-timestamps": false, "seccomp": false,
	"seccomp-bpf": false, "secontext": false, "silence": false, "silent": false,
	"stack-trace": false, "stack-traces": false, "strings-in-hex": false,
	"successful": false, "successful-only": false, "summary": false,
	"summary-only": false, "summary-wall-clock": false, "syscall-number": false,
	"syscall-times": false, "timestamp": false, "timestamps": false,
	"tips": false, "version": false,
}

var xargsLongOptions = map[string]bool{
	"arg-file": true, "delimiter": true, "max-args": true, "max-chars": true,
	"max-procs": true, "process-slot-var": true,

	"eof": false, "exit": false, "help": false, "interactive": false,
	"max-lines": false, "no-run-if-empty": false, "null": false,
	"open-tty": false, "replace": false, "show-limits": false,
	"verbose": false, "version": false,
}

// resolveLongOption resolves a long option name the way glibc getopt_long
// does: an exact name, or else a prefix of the names in table. A prefix
// matching several names still resolves when they all take the same kind of
// argument. Measured: strace takes --da (daemonize/daemonized/daemonised),
// --ki and --p as such aliases rather than refusing them as ambiguous. name
// is set only when the option is one specific entry.
//
// known is false for an unknown name or a prefix whose matches disagree. The
// real binary rejects those, but the caller treats them as unresolved rather
// than trusting that the table is complete.
func resolveLongOption(name string, table map[string]bool) (resolved string, requiresArg, known bool) {
	if requires, exact := table[name]; exact {
		return name, requires, true
	}
	if name == "" {
		return "", false, false
	}
	matches := 0
	for candidate, requires := range table {
		if !strings.HasPrefix(candidate, name) {
			continue
		}
		if matches > 0 && requires != requiresArg {
			return "", false, false
		}
		matches++
		resolved, requiresArg = candidate, requires
	}
	if matches == 0 {
		return "", false, false
	}
	if matches > 1 {
		resolved = ""
	}
	return resolved, requiresArg, true
}

// optionMarkerTarget names the option a marker-bearing word belongs to when
// the text before the marker fixes it: "--name" once a '=' precedes the
// marker (name as written, possibly an abbreviation), or "-X" for the first
// value-taking short flag before it. A word that does not start with '-'
// returns "" (an operand or the command). known is false when the substituted
// line can still choose the option: the marker opens the word, follows a bare
// "-" or a "--name" with no '=', or sits among short flags before any
// value-taking one.
func optionMarkerTarget(literal, marker, shortValueFlags string) (string, bool) {
	prefix := literal[:strings.Index(literal, marker)]
	switch {
	case prefix == "" || prefix == "-":
		return "", false
	case strings.HasPrefix(prefix, "--"):
		name, _, attached := strings.Cut(prefix[2:], "=")
		if !attached {
			return "", false
		}
		return "--" + name, true
	case strings.HasPrefix(prefix, "-"):
		for _, flag := range prefix[1:] {
			if strings.ContainsRune(shortValueFlags, flag) {
				return "-" + string(flag), true
			}
		}
		return "", false
	default:
		return "", true
	}
}
