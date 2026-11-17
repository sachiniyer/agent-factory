package proctree

import (
	"io"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/internal/afcli"
	"github.com/spf13/pflag"
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
// (#7494 item 3, shared-definition form #7496): the tail is run through a
// pflag.FlagSet built from the same root-flag definitions commands uses, so
// the verdict equals what cobra computes for the same argv rather than a
// hand-mirrored subset that has missed pflag semantics twice (explicit bool
// shorthand values, then last-value-wins on a repeated flag). A live af
// daemon's argv is the binary plus a flags-only zone: a parse error, a
// positional — including the __watch-lifeline / __upgrade-recovery exec
// markers, consumed before cobra sees argv, and job argv after "--" — or a
// truthy help/version means no daemon RunE ever ran under that argv.
func ArgvHasDaemonFlag(args []string) bool {
	if len(args) < 2 {
		return false
	}
	var program, upgradeTxn, daemonURL, token string
	var daemon, help, version bool
	fs := pflag.NewFlagSet("af", pflag.ContinueOnError)
	fs.SetOutput(io.Discard)
	afcli.DefineRootFlags(fs, fs, afcli.RootFlagBinds{
		Program:            &program,
		Daemon:             &daemon,
		UpgradeTransaction: &upgradeTxn,
		DaemonURL:          &daemonURL,
		Token:              &token,
		Help:               &help,
		Version:            &version,
	})
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() > 0 {
		return false
	}
	return daemon && !help && !version
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
