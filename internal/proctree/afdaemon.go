package proctree

import (
	"path/filepath"
	"strconv"
	"strings"
)

// ArgvIsAFDaemon reports whether args is the argv of an agent-factory daemon:
// an af (or source-built agent-factory) binary invoked with the --daemon flag.
// It is the shared classifier daemon/process.go's PID validation and host-wide
// scans are built on, mirrored here so a kill-path in another package can
// positively identify — and thereby EXCLUDE — a daemon subtree without
// importing daemon/ (#7435). Detection operates on real argv elements, not a
// space-joined string, so a binary installed under a path containing spaces
// is classified correctly (#1214), and an exact "--daemon" token (or the
// "--daemon=..." form) is required, so flags like --daemonize never match.
func ArgvIsAFDaemon(args []string) bool {
	return ArgvHasDaemonFlag(args) && ArgvIsAFBinary(args)
}

// ArgvHasDaemonFlag reports whether argv claims the daemon by af's OWN parse
// of its root --daemon flag (#7494 item 3), not merely whether the token
// appears somewhere. af registers --daemon as a root-local cobra flag, so a
// live af daemon's argv is the binary plus a flags-only zone: every token is
// a known flag or a flag's value, terminated by "--" or the first positional.
// Anything after "--" is job argv (`af __watch-lifeline v1 N -- sh -c 'job'
// --daemon` passes --daemon to the job, not to af), and a positional token —
// including the __watch-lifeline / __upgrade-recovery exec markers, which are
// consumed before cobra sees argv — is a subcommand or a fatal "unknown
// command", so no daemon is live under it either. An argv real af would have
// refused (unknown flag, missing value, unparseable bool, --help/--version)
// is not a daemon claim: the process carrying it cannot be af's daemon.
//
// The root flag table is duplicated here because proctree is a leaf and
// commands/root.go is not; commands' own test pins the two parsers to parity
// so a flag added at root fails that test until this table learns it.
func ArgvHasDaemonFlag(args []string) bool {
	if len(args) < 2 {
		return false
	}
	rest := args[1:]
	claim := false
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case a == "--":
			// Remaining tokens are positionals; a live af daemon has none.
			return claim && i+1 == len(rest)
		case len(a) < 2 || a[0] != '-':
			return false
		case a[1] == '-':
			name, value, hasEq := cutFlagValue(a[2:])
			switch {
			case name == "daemon":
				if !hasEq {
					claim = true
				} else if v, err := strconv.ParseBool(value); err == nil {
					claim = v
				} else {
					return false
				}
			case name == "help" || name == "version":
				if truthy, err := strconv.ParseBool(value); !hasEq || (err == nil && truthy) {
					// cobra prints help/version and exits before RunE.
					return false
				}
			case afRootFlagTakesValue[name]:
				if !hasEq {
					if i+1 >= len(rest) {
						return false
					}
					i++
				}
			default:
				return false
			}
		default:
			// Shorthand zone: root has -h/--help and -v/--version (bools)
			// plus -p/--program (value). pflag reads a cluster
			// char-by-char: a bool shorthand may carry an explicit =value,
			// and a value shorthand takes the rest of the token or the
			// next argument.
			for j := 1; j < len(a); j++ {
				switch a[j] {
				case 'h', 'v':
					// -h=false / -v=false parse the bool false and the
					// run continues (#7496); a bare shorthand anywhere in
					// the cluster, a truthy one, or an unparseable bool
					// exits af before RunE (help/version print).
					if j+1 < len(a) && a[j+1] == '=' {
						if v, err := strconv.ParseBool(a[j+2:]); err != nil || v {
							return false
						}
						j = len(a)
					} else {
						return false
					}
				case 'p':
					// The rest of the token is the value — -pfoo or
					// -p=foo — or the next argument when nothing follows.
					if j+1 < len(a) {
						j = len(a)
					} else if i+1 < len(rest) {
						i++
					} else {
						return false
					}
				default:
					return false
				}
			}
		}
	}
	return claim
}

// afRootFlagTakesValue is root's value-consuming flags at commands/root.go:
// anything else in the flag zone is a bool (--daemon, --help, --version, all
// handled inline above). Kept in sync by the commands parity test.
var afRootFlagTakesValue = map[string]bool{
	"program":             true,
	"upgrade-transaction": true,
	"daemon-url":          true,
	"token":               true,
}

func cutFlagValue(s string) (name, value string, hasEq bool) {
	if j := strings.IndexByte(s, '='); j >= 0 {
		return s[:j], s[j+1:], true
	}
	return s, "", false
}

// ArgvIsAFBinary reports whether argv[0] is an agent-factory binary: installed
// as "af" or built from source (`go build .`) as "agent-factory". The
// host-wide pgrep scan in daemon/sigterm_fallback.go matches any process
// carrying a "--daemon" token, so this restores the binary-name specificity
// the old "af --daemon" substring pattern provided — while still catching
// source-built `agent-factory --daemon` daemons that the old pattern missed
// (#937).
//
// argv[0] is a single argv element, so filepath.Base sees the whole executable
// path even when it contains spaces (e.g. "/home/John Smith/.local/bin/af" →
// base "af"). The previous implementation space-joined the argv and re-split
// on whitespace, which turned that same path into base "John" and made every
// spaced-install daemon undetectable (#1214).
func ArgvIsAFBinary(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch filepath.Base(args[0]) {
	case "af", "agent-factory":
		return true
	default:
		return false
	}
}
