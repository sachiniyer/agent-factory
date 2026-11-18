package afcli

import (
	"io"

	"github.com/spf13/cobra"
)

// DaemonClaim is af's own verdict on an argv tail: does this argv run the
// daemon? The daemon-claim recognizer asks it of a LIVE process's argv —
// where the process still exists, something IS running — so "cobra decides
// not" and "this build cannot decide" are different answers (#7496 R1).
type DaemonClaim int

const (
	// DaemonClaimNo: cobra's decision is decided and is NOT the daemon — a
	// parse the CLI completes that does not reach the root RunE with
	// --daemon (help or version printed, daemon=false, a decided exit).
	// Whatever a live process under this argv is, af itself never made it a
	// daemon this way, so the claim is refused.
	DaemonClaimNo DaemonClaim = iota
	// DaemonClaimYes: cobra reaches the root RunE with --daemon — the argv
	// af itself runs a daemon under.
	DaemonClaimYes
	// DaemonClaimUndecidable: this build cannot decide. An unknown flag or
	// a tail it cannot resolve — a positional before "--" may name a
	// subcommand the recognizer's root does not carry, rerouting the parse
	// under a different flag set, and a flag a NEWER af build adds is
	// unknowable to this one. The claim is neither granted nor refused: on
	// the reap path it is protected and blocking (never Not, #7496 R1); on
	// the stop and kill paths it is not-ours.
	DaemonClaimUndecidable
)

// DaemonClaimOf is the daemon-claim recognizer: it runs args' tail through
// af's REAL root-flag definitions on a throwaway cobra command whose RunE
// only records that cobra reached it, so the verdict is cobra's own decision
// — Find, the flag parse, the help/version early-outs, and Args validation —
// not a hand-mirrored subset (#7496 R1: the previous mirrors missed explicit
// bool shorthand values, last-value-wins on a repeated --help, and then the
// post-"--" tail pflag leaves to Args, which root's nil Args validator —
// ArbitraryArgs — accepts, running the daemon on `af --daemon -- x`).
//
// The throwaway carries no subcommand tree, so a positional before "--"
// cannot be resolved: it might be a real subcommand name, which would
// reroute the whole parse under that command's flags and never reach the
// root RunE, or stray text ArbitraryArgs ignores while the daemon runs.
// Either way this build cannot tell, and the answer is Undecidable rather
// than a guess — a pflag-only parse would have called `af --daemon -- x`
// "not a daemon" on NArg() > 0 and unprotected a REAL daemon.
func DaemonClaimOf(args []string) DaemonClaim {
	if len(args) < 1 {
		return DaemonClaimNo
	}
	var program, upgradeTxn, daemonURL, token string
	var daemon, help, version bool
	ran := false
	root := &cobra.Command{
		Use: "af",
		// Non-empty so cobra exercises its real --version early-out — the
		// same path the real root takes with its ldflags-stamped Version.
		Version: "claim-recognizer",
		RunE: func(*cobra.Command, []string) error {
			ran = true
			return nil
		},
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	DefineRootFlags(root.Flags(), root.PersistentFlags(), RootFlagBinds{
		Program:            &program,
		Daemon:             &daemon,
		UpgradeTransaction: &upgradeTxn,
		DaemonURL:          &daemonURL,
		Token:              &token,
		Help:               &help,
		Version:            &version,
	})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs(args[1:])
	err := root.Execute()
	fs := root.Flags()
	switch {
	case ran && daemon && !help && !version:
		// A positional BEFORE "--" is the one thing the throwaway cannot
		// resolve: cobra's Find would have rerouted to a same-named
		// subcommand and never reached the root RunE, while a positional
		// after "--" can never be a command (stripFlags stops there) and
		// is decided daemon.
		preDash := fs.ArgsLenAtDash()
		if preDash < 0 {
			preDash = fs.NArg()
		}
		if preDash > 0 {
			return DaemonClaimUndecidable
		}
		return DaemonClaimYes
	case ran || err == nil:
		// The root ran with daemon false, or cobra completed without the
		// root RunE at all (help, version): decided, and not a daemon.
		return DaemonClaimNo
	default:
		// The parse itself failed — an unknown flag or a malformed value.
		// A live process this build cannot classify may still be a daemon
		// built by an af whose flag set differs, so this is never Not.
		return DaemonClaimUndecidable
	}
}
