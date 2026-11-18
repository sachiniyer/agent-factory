package proctree

import (
	"path/filepath"

	"github.com/sachiniyer/agent-factory/internal/afcli"
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
//
// Only a DECIDED claim counts here — ArgvDaemonClaimYes — because the stop
// and kill paths this feeds must act on "ours" alone: an argv this build
// cannot decide is protected on the reap path but never answered "yes" to a
// signaller (#7496 R1).
func ArgvIsAFDaemon(args []string) bool {
	return ArgvDaemonClaim(args) == afcli.DaemonClaimYes
}

// ArgvDaemonClaim is af's own verdict on whether args could name a LIVE af
// daemon under this build: argv[0] must be an af binary (ArgvIsAFBinary),
// and the tail is run through af's real root flag definitions under cobra —
// see afcli.DaemonClaimOf. The three answers matter differently to different
// callers (#7496 R1):
//
//   - DaemonClaimYes: a decided claim — the reap path attests the kernel
//     executable, and the stop/kill paths may act.
//   - DaemonClaimNo: a decided refusal — help/version, daemon=false, or an
//     argv af itself could never run a daemon under.
//   - DaemonClaimUndecidable: this build cannot tell (an unknown flag, a
//     pre-"--" positional that may name a subcommand the recognizer does not
//     carry). On the reap path it must NEVER collapse to Not — it is
//     Unverified, protected and blocking; on the stop and kill paths it is
//     not-ours.
func ArgvDaemonClaim(args []string) afcli.DaemonClaim {
	if !ArgvIsAFBinary(args) {
		return afcli.DaemonClaimNo
	}
	return afcli.DaemonClaimOf(args)
}

// ArgvHasDaemonFlag reports whether argv's daemon claim is DECIDED: cobra
// reaches af's root RunE with --daemon under af's own flag definitions
// (#7494 item 3, cobra-decision form #7496). An undecidable argv answers
// false — on the stop and kill paths "can't tell" is not-ours; the reap
// path reads the full verdict through ArgvDaemonClaim instead, where it is
// protected and blocking, never Not.
func ArgvHasDaemonFlag(args []string) bool {
	return ArgvDaemonClaim(args) == afcli.DaemonClaimYes
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
