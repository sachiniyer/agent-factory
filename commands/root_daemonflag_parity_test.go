package commands

import (
	"testing"

	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/spf13/pflag"
	"github.com/stretchr/testify/require"
)

// The kill path's daemon classifier (proctree.ArgvHasDaemonFlag) replicates
// af's own root-flag parse in a leaf package, because proctree cannot import
// commands. This test is the contract that keeps them equal (#7494 item 3):
// for every flag the REAL root command registers — and for the positional,
// terminator, marker, and error argvs that decide whether any daemon could
// be alive under that argv — the two parsers must agree. A flag added at
// root without updating proctree's table fails here.
func TestDaemonFlagParseMatchesAfRoot(t *testing.T) {
	reference := func(t *testing.T, tail []string) bool {
		cmd := NewRootCommand(Options{Version: "test"})
		// cobra adds --help at Execute; add it here so the reference flag set
		// is exactly what af parses.
		cmd.InitDefaultHelpFlag()
		fs := cmd.Flags()
		// The flags bind to package-level state the REAL cli parses into —
		// fs.Parse writes straight into it. Compare against the real flagset
		// without ever changing it: snapshot every flag's value and Changed
		// bit and restore exactly, so a parity argv like `--program --daemon`
		// cannot leave "--daemon" parked as the program for the next test's
		// real rootCmd.RunE (#7496).
		type flagState struct {
			value   string
			changed bool
		}
		saved := map[string]flagState{}
		fs.VisitAll(func(f *pflag.Flag) {
			saved[f.Name] = flagState{f.Value.String(), f.Changed}
		})
		// The restore runs the moment the answer is read, not at Cleanup —
		// a deferred restore would leave "--daemon" parked as --program's
		// value (and daemonFlag true) for the next parity argv AND for any
		// real rootCmd.RunE another commands test runs after this one.
		defer fs.VisitAll(func(f *pflag.Flag) {
			_ = f.Value.Set(saved[f.Name].value)
			f.Changed = saved[f.Name].changed
		})
		// A parse error, a positional, or help/version: real af never reaches
		// the daemon RunE, so no live daemon exists under that argv.
		if err := fs.Parse(tail); err != nil || fs.NArg() > 0 {
			return false
		}
		if truthy, err := fs.GetBool("help"); err == nil && truthy {
			return false
		}
		if truthy, err := fs.GetBool("version"); err == nil && truthy {
			return false
		}
		return daemonFlag
	}

	assertParity := func(t *testing.T, tail []string) {
		t.Helper()
		require.Equal(t, reference(t, tail),
			proctree.ArgvIsAFDaemon(append([]string{"af"}, tail...)),
			"daemon claim parity for argv tail %v", tail)
	}

	// The hand-picked shapes item 3 names: the terminator, positionals, the
	// internal exec markers, and argvs af refuses to parse.
	for _, tail := range [][]string{
		{"--daemon"},
		{"--daemon=true"},
		{"--daemon=false"},
		{"--daemon=foo"},
		{"--daemon", "--"},
		{"--daemon", "--", "sh", "-c", "job", "--daemon"},
		{"--", "--daemon"},
		{"x", "--daemon"},
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
		// help=false and the daemon claim stands. The recognizer IS a real
		// flagset parse now, so these are pinned by construction, but the
		// rows keep the contract explicit.
		{"--daemon", "--help", "--help=false"},
		{"--daemon", "--help", "--help=false", "--daemon=false"},
		{"--daemon", "--daemon", "--daemon=false"},
	} {
		assertParity(t, tail)
	}

	// And every flag the real root registers, in both its forms — this is
	// what pins proctree's value-flag table to root's actual flag set.
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
