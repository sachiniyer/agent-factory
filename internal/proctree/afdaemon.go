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

// ArgvHasDaemonFlag reports whether argv contains "--daemon" as a discrete
// argument (either bare or in the "--daemon=value" form). It deliberately
// rejects substring matches like "--daemonize" or "--daemon-mode". Because it
// scans real argv elements, spaces inside another argument (such as a spaced
// binary path in argv[0]) can never fabricate or hide a "--daemon" token
// (#1214).
func ArgvHasDaemonFlag(args []string) bool {
	for _, a := range args {
		if a == "--daemon" {
			return true
		}
		value, ok := strings.CutPrefix(a, "--daemon=")
		if !ok {
			continue
		}
		// `--daemon=false` is a client saying, explicitly, that it is NOT a
		// daemon. Matching the prefix and calling it one made every such client
		// a daemon to every caller: doctor counted it as a duplicate, the host
		// scan offered it for a kill, and the #1004 pid guard would have
		// accepted it as ours. The flag's VALUE is the answer; its name is only
		// where the answer lives.
		//
		// Only an explicitly FALSE value flips the answer. An unparseable one
		// ("--daemon=foo") keeps the long-standing "the form is present, so
		// treat it as a daemon flag" reading that TestArgsHaveDaemonFlag has
		// pinned since #342: that case is about recognizing the `--daemon=`
		// FORM (as against `--daemonize`), and its value is a placeholder, not
		// a boolean. It is also unobservable in practice — cobra rejects a
		// non-boolean here, so no such process is ever live to classify — and
		// narrowing a seam that gates signals on a hypothetical is not worth
		// the blast radius.
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return true
		}
		return enabled
	}
	return false
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
