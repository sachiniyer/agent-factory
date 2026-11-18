package commands

import (
	"io"
	"testing"

	"github.com/sachiniyer/agent-factory/internal/afcli"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
)

// The kill path's daemon classifier (proctree.ArgvDaemonClaim) decides an
// argv the way af itself would — af's real root flag definitions under
// cobra, on a throwaway root with a recording RunE, because proctree cannot
// import commands. This test is the contract that keeps the recognizer's
// verdict equal to what cobra ACTUALLY decides for the same argv (#7496 R1):
// the reference is a real Execute on af's genuine command tree — same flag
// set, same subcommands, same help/version early-outs — with only the root
// RunE replaced by a recorder. A flag added at root without updating the
// recognizer's shared definitions fails here.
//
// Two outcomes differ in kind, not degree: a DECIDED verdict from the
// recognizer must equal cobra's answer exactly, while UNDECIDABLE — a
// positional before "--" that may name a subcommand the throwaway does not
// carry, or a parse the build cannot complete — is the recognizer honestly
// reporting it cannot tell. Undecidable must never read as No on the reap
// path (`af --daemon -- x` reading not-a-daemon is how a real daemon lost
// signal protection), and never reads as Yes to a signaller.
func TestDaemonFlagParseMatchesAfRoot(t *testing.T) {
	reference := func(t *testing.T, tail []string) bool {
		t.Helper()
		cmd := NewRootCommand(Options{Version: "test"})
		// cobra adds --help at Execute; add it here so the reference flag set
		// is exactly what af parses.
		cmd.InitDefaultHelpFlag()
		fs := cmd.Flags()
		// The flags bind to package-level state the REAL cli parses into —
		// Execute writes straight into it. Compare against the real flagset
		// without ever changing it: snapshot every flag's value and Changed
		// bit and restore exactly, so a parity argv like `--program --daemon`
		// cannot leave "--daemon" parked as the program for the next test's
		// real rootCmd.RunE (#7496). Persistent flags merge into fs during
		// Execute and bind package vars too, so they are snapshotted beside
		// the locals.
		type flagState struct {
			value   string
			changed bool
		}
		saved := map[string]flagState{}
		capture := func(s *pflag.FlagSet) {
			s.VisitAll(func(f *pflag.Flag) {
				saved[f.Name] = flagState{f.Value.String(), f.Changed}
			})
		}
		restore := func(s *pflag.FlagSet) {
			s.VisitAll(func(f *pflag.Flag) {
				if st, ok := saved[f.Name]; ok {
					_ = f.Value.Set(st.value)
					f.Changed = st.changed
				}
			})
		}
		capture(fs)
		capture(cmd.PersistentFlags())
		// The restore runs the moment the answer is read, not at Cleanup —
		// a deferred restore would leave "--daemon" parked as --program's
		// value (and daemonFlag true) for the next parity argv AND for any
		// real rootCmd.RunE another commands test runs after this one.
		defer restore(fs)
		// cobra's own decision: Execute the REAL root — its own Find, flag
		// parse, help/version early-outs, and Args handling — and record only
		// whether the root RunE ran. Everything else (the subcommand tree,
		// PersistentPreRunE) is af's real wiring. A positional before "--"
		// routes to a real subcommand the way the installed binary routes it.
		ran := false
		savedRunE := cmd.RunE
		savedSilenceUsage := cmd.SilenceUsage
		cmd.RunE = func(*cobra.Command, []string) error {
			ran = true
			return nil
		}
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		cmd.SetArgs(tail)
		executed, _ := cmd.ExecuteC()
		cmd.RunE = savedRunE
		cmd.SilenceUsage = savedSilenceUsage
		cmd.SetArgs(nil)
		cmd.SetOut(nil)
		cmd.SetErr(nil)
		// cobra ran the daemon exactly when the root's own RunE ran with the
		// parsed --daemon value — the same conjunction the real RunE reads.
		return executed == cmd && ran && daemonFlag
	}

	assertParity := func(t *testing.T, tail []string) {
		t.Helper()
		daemon := reference(t, tail)
		claim := proctree.ArgvDaemonClaim(append([]string{"af"}, tail...))
		switch claim {
		case afcli.DaemonClaimYes:
			require.True(t, daemon,
				"decided claim contradicts cobra for argv tail %v: af does not run the daemon under it", tail)
		case afcli.DaemonClaimNo:
			require.False(t, daemon,
				"decided refusal contradicts cobra for argv tail %v: af DOES run the daemon under it", tail)
		default:
			// DaemonClaimUndecidable: allowed wherever the decision needed
			// something the throwaway root does not carry — and REQUIRED to
			// never masquerade as either decided answer (#7496 R1).
		}
	}

	// The hand-picked shapes the recognizer must answer: the terminator,
	// positionals, the internal exec markers, and argvs af refuses to parse.
	for _, tail := range [][]string{
		{"--daemon"},
		{"--daemon=true"},
		{"--daemon=false"},
		{"--daemon=foo"},
		{"--daemon", "--"},
		// #7496 R1: positional argv after "--" is the root's own tail —
		// ArbitraryArgs accepts it and the daemon RunE runs. The old
		// NArg()>0 mirror called this not-a-daemon and dropped a REAL
		// daemon's signal protection.
		{"--daemon", "--", "sh", "-c", "job", "--daemon"},
		{"--daemon", "--", "x"},
		{"--", "--daemon"},
		// A positional before "--" may name a subcommand the recognizer's
		// throwaway does not carry: cobra decides it on the real tree
		// (x is not a verb, so the daemon runs); the recognizer stays
		// undecidable rather than guess — protected on the reap path.
		{"x", "--daemon"},
		{"-h", "x", "-h=false", "--daemon"},
		// Pre-cobra markers: main consumes these before cobra ever parses.
		// The throwaway cannot see that — undecidable is the honest verdict.
		{"__watch-lifeline", "v1", "0", "--", "sh", "-c", "job", "--daemon"},
		{"__watch-lifeline", "--daemon"},
		{"__upgrade-recovery", "--daemon"},
		{"--bogus", "--daemon"},
		{"--program", "--daemon"},
		{"--program"},
		{"--daemon", "--daemon=false"},
		{"--daemon=false", "--daemon"},
		// #7496: a bool shorthand carries an explicit value — pflag parses
		// `-h=false` as help=false and the daemon claim stands. A bare -h,
		// a truthy one, and an unparseable bool all exit af first.
		{"-h=false", "--daemon"},
		{"--daemon", "-h=false"},
		{"-h", "--daemon"},
		{"-h=true", "--daemon"},
		{"-h=foo", "--daemon"},
		// #7496: pflag's last value wins — `--help --help=false` parses
		// help=false and the daemon claim stands. The recognizer decides
		// through cobra's own Execute now, so these are pinned by
		// construction, but the rows keep the contract explicit.
		{"--daemon", "--help", "--help=false"},
		{"--daemon", "--help", "--help=false", "--daemon=false"},
		{"--daemon", "--daemon", "--daemon=false"},
	} {
		assertParity(t, tail)
	}

	// And every flag the real root registers, in both its forms — this is
	// what pins the recognizer's flag set to root's actual flag set.
	cmd := NewRootCommand(Options{Version: "test"})
	cmd.InitDefaultHelpFlag()
	cmd.Flags().VisitAll(func(f *pflag.Flag) {
		if f.Shorthand != "" {
			if f.NoOptDefVal == "" {
				assertParity(t, []string{"-" + f.Shorthand, "v", "--daemon"})
				assertParity(t, []string{"-" + f.Shorthand + "v", "--daemon"})
			} else {
				assertParity(t, []string{"-" + f.Shorthand, "--daemon"})
				assertParity(t, []string{"-" + f.Shorthand + "=false", "--daemon"})
			}
		}
		if f.NoOptDefVal == "" {
			assertParity(t, []string{"--" + f.Name, "v", "--daemon"})
			assertParity(t, []string{"--" + f.Name + "=v", "--daemon"})
			assertParity(t, []string{"--" + f.Name, "--daemon"})
		} else {
			assertParity(t, []string{"--" + f.Name, "--daemon"})
			assertParity(t, []string{"--" + f.Name + "=false", "--daemon"})
		}
	})
}
