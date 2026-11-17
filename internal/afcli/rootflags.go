// Package afcli holds the flag definitions af's root command accepts,
// shared between the real CLI — commands binds its package state into
// them — and the daemon-claim recognizer in proctree, which cannot import
// commands but whose "would af reach the daemon RunE under this argv"
// verdict must equal what pflag computes, not a hand-mirrored subset
// (#7496: the mirror missed explicit bool shorthand values, then
// last-value-wins on a repeated --help; the third miss is the one we stop
// writing by hand).
package afcli

import "github.com/spf13/pflag"

// RootFlagBinds are the destinations af's root flags write to. commands
// points each at the package variable the real CLI reads; a recognizer
// points them at throwaways it reads back after Parse. Help and Version
// stay nil for the CLI — cobra auto-registers those on the real root
// command — and non-nil for a recognizer parsing a bare tail, where the
// names and shorthands must exist to parse.
type RootFlagBinds struct {
	Program            *string
	ProgramUsage       string
	Daemon             *bool
	UpgradeTransaction *string
	DaemonURL          *string
	Token              *string
	Help               *bool
	Version            *bool
}

// DefineRootFlags registers af's root flags: the local ones on local, the
// persistent remote-target pair on persistent — pass the same flagset
// twice to parse a bare argv tail with no command tree. These are the ONE
// set of definitions both call sites parse with: change a flag here and
// the CLI and the recognizer change together.
func DefineRootFlags(local, persistent *pflag.FlagSet, b RootFlagBinds) {
	local.StringVarP(b.Program, "program", "p", "", b.ProgramUsage)
	local.BoolVar(b.Daemon, "daemon", false,
		"Run the background daemon that schedules tasks and monitors session status.")
	if err := local.MarkHidden("daemon"); err != nil {
		panic(err)
	}
	// Internal only (#2212 R2): the recovery actor's StartCandidate passes
	// this to launch a candidate in upgrade probation. Hidden so it never
	// appears in help.
	local.StringVar(b.UpgradeTransaction, "upgrade-transaction", "",
		"Internal: run the daemon in upgrade probation for this transaction id")
	if err := local.MarkHidden("upgrade-transaction"); err != nil {
		panic(err)
	}
	if b.Help != nil {
		local.BoolVarP(b.Help, "help", "h", false, "help for af")
	}
	if b.Version != nil {
		local.BoolVarP(b.Version, "version", "v", false, "version for af")
	}
	// Remote-daemon target (#1592 Phase 3 PR4; HTTP-only since 2026-07-14).
	// Persistent so they apply to the bare TUI and every `af sessions
	// ...`/`af tasks ...` subcommand. Unset ⇒ the local unix socket
	// (unchanged); each has an AF_DAEMON_* env fallback resolved in
	// apiclient. The remote listener is plain HTTP; a client reaches it
	// over http:///ws:// directly, or over https:///wss:// through a TLS
	// front (tailscale serve, a proxy), verified against the system CAs
	// (W9.5).
	persistent.StringVar(b.DaemonURL, "daemon-url", "",
		"Target a REMOTE daemon at this http://, ws://, https:// or wss:// URL instead of the local unix socket "+
			"(env: AF_DAEMON_URL). https:// and wss:// are verified against the system CAs, for a daemon behind a TLS front such as tailscale serve.")
	// NOTE: no backticks in these usage strings. pflag's UnquoteUsage
	// treats the first backticked span as the flag's arg-name placeholder,
	// so `af token show` would render "--token af token show" instead of
	// "--token string" on every help screen (#1749). Use plain quotes for
	// inline example commands.
	persistent.StringVar(b.Token, "token", "",
		"Bearer token for a remote daemon set with --daemon-url (env: AF_DAEMON_TOKEN). "+
			"Get it with 'af token show' on the daemon host.")
}
