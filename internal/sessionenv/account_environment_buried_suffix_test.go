package sessionenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// #5034: ionice and taskset scan their child tail for a buried suffix that a
// PATH-shadowed wrapper (`shift N; exec "$@"`) would promote to the executed
// command; the five structurally identical wrappers and xargs historically
// returned the tail unscanned (the five fixed in #5711; xargs, which returns its
// child tail through the separate unwrapXargs/xargsChild input-reach path, was
// the last). Every wrapper must give the verdict ionice gives for the same
// buried payload — including the unmodeled argv-passthrough wrappers the default
// arm of peelAccountWrapper reaches (strace/perf/valgrind/gdb --args/...),
// which cd685703 left as the only wrapper class without the buried-suffix scan.
func TestWrapperChildTailBuriedSuffixMatchesIonice(t *testing.T) {
	names := map[string]struct{}{"CODEX_HOME": {}, "OPENAI_API_KEY": {}}
	for name := range accountShellStartupNames {
		names[name] = struct{}{}
	}
	wrappers := []string{
		"ionice", "taskset 0x1", "nice", "nohup", "timeout 5", "setsid", "stdbuf -o0",
		"xargs", "xargs -r", "/usr/bin/xargs",
		// Unmodeled argv-passthrough wrappers the default arm reaches: a
		// PATH-shadowed copy (`shift N; exec "$@"`) promotes the buried suffix
		// the same way a shadowed nohup does, so each must give the verdict
		// ionice gives. gdb --args is the argv-passthrough spelling; a bare gdb
		// (and /usr/bin/perf, ltrace, valgrind) are matched by basename. The
		// util-linux argv-passthrough launchers (chrt/numactl/unshare/prlimit)
		// exec a child and forward argv the same way, so they join the class.
		"strace", "perf", "valgrind", "gdb --args", "gdb", "/usr/bin/perf", "ltrace",
		"chrt 0", "numactl", "unshare", "prlimit",
	}
	payloads := []struct {
		tail string
		want bool
	}{
		// A separate-word xargs --process-slot-var NAME replaces the account root.
		{"echo a a a xargs --process-slot-var CODEX_HOME codex", true},
		{"echo unset CODEX_HOME", true},
		{"echo xargs --process-slot-var=CODEX_HOME codex", true},
		// A buried builtin that mutates the account environment (set -k flips
		// keyword mode; hash -p remaps a name; trap schedules a shell program):
		// a shadowed wrapper execs each the same way it execs a buried xargs.
		{"echo set -k codex CODEX_HOME=/other", true},
		{"echo a hash -p /tmp/evil x", true},
		{"echo trap 'unset CODEX_HOME' 0", true},
		// Controls: an ordinary tail, and a buried name that is not the account's.
		{"echo a a a", false},
		{"codex --model x", false},
		{"echo xargs --process-slot-var PORT codex", false},
		{"echo unset PORT", false},
		{"stat npm run dev", false},
	}
	for _, wrapper := range wrappers {
		for _, payload := range payloads {
			command := wrapper + " " + payload.tail
			require.Equal(t, payload.want, commandMutatesAccountEnvironment(command, names), "command %q", command)
		}
	}
	// The `--` and option-bearing spellings reach the same child tail.
	for _, command := range []string{
		"nice -n 5 echo unset CODEX_HOME",
		"nice -- echo unset CODEX_HOME",
		"nohup -- echo unset CODEX_HOME",
		"timeout -k 1 5 echo unset CODEX_HOME",
		"timeout -- 5 echo unset CODEX_HOME",
		"setsid -f echo unset CODEX_HOME",
		"setsid -- echo unset CODEX_HOME",
		"stdbuf -oL -- echo unset CODEX_HOME",
		"stdbuf -eL echo unset CODEX_HOME",
		"xargs -- echo unset CODEX_HOME",
		"xargs -r echo unset CODEX_HOME",
		"xargs -a /dev/null echo unset CODEX_HOME",
		"xargs --null echo unset CODEX_HOME",
		// The unmodeled wrappers' own option spells reach the same tail: strace's
		// option run (-f), perf's stat subcommand, and gdb's --args.
		"strace -f echo unset CODEX_HOME",
		"strace -o /dev/null echo unset CODEX_HOME",
		"strace -- echo unset CODEX_HOME",
		"perf stat echo unset CODEX_HOME",
		"gdb --args echo unset CODEX_HOME",
	} {
		require.True(t, commandMutatesAccountEnvironment(command, names), "command %q", command)
	}
}

// The buried-literal-suffix scan the modeled arms gained in cd685703
// (#5711/#5714/#6329) was never applied to the default arm of peelAccountWrapper,
// so an unmodeled argv-passthrough wrapper (strace/perf/valgrind/gdb --args)
// accepted a buried literal mutation suffix the same payload under every
// modeled wrapper refused. This pins the report's evidence table — the payloads
// the modeled arms already refused — and the controls that must keep their
// verdicts.
func TestUnmodeledWrapperBuriedSuffixMatchesModeledWrappers(t *testing.T) {
	names := map[string]struct{}{"CODEX_HOME": {}, "OPENAI_API_KEY": {}}
	for name := range accountShellStartupNames {
		names[name] = struct{}{}
	}
	heads := []string{"perf", "strace", "valgrind", "gdb", "gdb --args", "/usr/bin/perf", "ltrace",
		"chrt 0", "numactl", "unshare", "prlimit"}
	// Parity cases: a payload whose unmodeled-wrapper verdict must equal the
	// verdict the modeled wrappers give for the same tail. Each is asserted
	// against the expected verdict and against nohup (the modeled reference).
	parityCases := []struct {
		tail string
		want bool
	}{
		// The report's evidence table: separate-word xargs --process-slot-var
		// NAME (the form the --opt=value rule misses), and the buried builtins
		// a shadowed wrapper execs when it shifts to the suffix.
		{"echo a a a xargs --process-slot-var CODEX_HOME codex", true},
		{"xargs --process-slot-var CODEX_HOME codex", true},
		{"echo unset CODEX_HOME", true},
		{"unset CODEX_HOME", true},
		{"set -k codex CODEX_HOME=/other", true},
		{"echo a hash -p /tmp/evil x", true},
		{"trap 'unset CODEX_HOME' 0", true},
		// The attached spelling was already refused by the --opt=value rule;
		// it stays refused now via the buried-suffix scan too.
		{"xargs --process-slot-var=CODEX_HOME codex", true},
		// A nested env with a command was already refused by the 5-signal scan's
		// env arm; it stays refused via that scan, not the buried suffix.
		{"env CODEX_HOME=/other codex", true},
		// Parity controls — inert tails and a non-account buried name — that
		// the modeled wrappers AND the unmodeled wrappers admit.
		{"echo a a a", false},
		{"codex \"$PROMPT\"", false},
		{"npm run dev", false},
		{"stat npm run dev", false},
		{"echo xargs --process-slot-var PORT codex", false},
		{"echo unset PORT", false},
	}
	for _, head := range heads {
		for _, c := range parityCases {
			command := head + " " + c.tail
			require.Equal(t, c.want, commandMutatesAccountEnvironment(command, names),
				"unmodeled wrapper %q must match the modeled verdict for %q", head, c.tail)
			require.Equal(t, c.want, commandMutatesAccountEnvironment("nohup "+c.tail, names),
				"nohup verdict for %q disagrees with the expected verdict %v", c.tail, c.want)
		}
	}
	// Leniency controls: the unmodeled wrapper is intentionally MORE lenient than
	// the modeled wrappers here, because its 5-signal tail scan
	// (unrecognizedWrapperHidesAccountAssignment) judges these shapes with a
	// stricter reading than the modeled arms' buried-suffix/env arm: a nested
	// env in print mode stays admitted (requireCommand=true, so `env NAME=v`
	// with no command overrides nothing), a bare trailing shell with no argv
	// stays admitted (the shell scan needs argv), and an option spell the
	// unmodeled wrapper might take (-p) stays admitted. The buried-suffix scan
	// added here EXCLUDES env and shells so it does not re-judge them with the
	// modeled (requireCommand=false, no-argv-gate) reading and regress these
	// noun uses the wrapper-guard and unmodeled-wrapper tests pin.
	for _, head := range heads {
		for _, tail := range []string{
			"env CODEX_HOME=/other",
			"sh",
			"-p 1234 codex",
		} {
			command := head + " " + tail
			require.False(t, commandMutatesAccountEnvironment(command, names),
				"leniency control %q must stay admitted under unmodeled wrapper %q", tail, head)
		}
	}
}

// A replace marker that is itself a verdict word is substituted away in the
// argv the walk continues with — `xargs -Iunset echo a unset CODEX_HOME`
// reads as `echo a $xargs_item CODEX_HOME`, and the literal-boundary suffix
// scan deliberately skips the synthetic word. The pre-substitution tail still
// carries the literal suffix a shadowed xargs would shift to, so it must be
// judged too (#6320).
func TestXargsVerdictWordMarkerDoesNotHideTheLiteralTail(t *testing.T) {
	names := map[string]struct{}{"CODEX_HOME": {}, "OPENAI_API_KEY": {}}
	for _, command := range []string{
		"xargs -Iunset echo a unset CODEX_HOME",
		"xargs -I unset echo a unset CODEX_HOME",
		"xargs --replace=unset echo a unset CODEX_HOME",
		"xargs --replace unset echo a unset CODEX_HOME",
	} {
		require.True(t, commandMutatesAccountEnvironment(command, names), "command %q", command)
	}
	// Controls: an inert marker over the same tail, and the denied payload
	// arriving only through substitution input — both stay allowed/refused on
	// their own rules.
	require.False(t, commandMutatesAccountEnvironment("xargs -I{} echo a unset PORT", names))
	require.False(t, commandMutatesAccountEnvironment("xargs -I{} echo a {} CODEX_HOME", names))
}

// The exec boundary is decided at COMMAND POSITION (#5034): a dynamic word
// there refuses, but once the command word is literal its arguments are opaque,
// and only a recognized wrapper in that position is recursed into.
func TestWrapperCommandPositionBoundary(t *testing.T) {
	names := map[string]struct{}{"CODEX_HOME": {}, "OPENAI_API_KEY": {}}
	for _, wrapper := range []string{"nice", "nice -n 5", "nohup", "timeout 5", "setsid", "stdbuf -o0", "xargs", "xargs -r", "/usr/bin/xargs"} {
		for _, test := range []struct {
			tail string
			want bool
		}{
			{`$CMD codex`, true},
			{`"$X" codex`, true},
			{`$(z) codex`, true},
			{"nice $CMD", true},
			{`codex "$PROMPT"`, false},
			{`echo a "$P"`, false},
			{`nice codex "$PROMPT"`, false},
		} {
			command := wrapper + " " + test.tail
			require.Equal(t, test.want, commandMutatesAccountEnvironment(command, names), "command %q", command)
		}
	}
}

// ionice, taskset and xargs follow the same command-position rule as the five
// siblings (#5034): a dynamic word where the command goes refuses, a literal
// command's dynamic arguments are opaque, and a buried LITERAL suffix still
// refuses.
func TestIoniceTasksetCommandPositionBoundary(t *testing.T) {
	names := map[string]struct{}{"CODEX_HOME": {}, "OPENAI_API_KEY": {}}
	for _, wrapper := range []string{"ionice", "ionice -c 3", "ionice --", "taskset 0x1", "taskset -c 0-3", "xargs", "xargs -r", "xargs --", "/usr/bin/xargs"} {
		for _, test := range []struct {
			tail string
			want bool
		}{
			{`$CMD codex`, true},
			{`"$X" codex`, true},
			{"nice $CMD", true},
			{`codex "$PROMPT"`, false},
			{`echo a "$P"`, false},
			{`nice codex "$PROMPT"`, false},
			{"echo a a a xargs --process-slot-var CODEX_HOME codex", true},
			{"echo unset CODEX_HOME", true},
			{`echo "$P" unset CODEX_HOME`, true},
		} {
			command := wrapper + " " + test.tail
			require.Equal(t, test.want, commandMutatesAccountEnvironment(command, names), "command %q", command)
		}
	}
}
