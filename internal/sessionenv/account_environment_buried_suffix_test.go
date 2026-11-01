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
// the last). Every MODELED wrapper must give the verdict ionice gives for the
// same buried payload. The unmodeled argv-passthrough wrappers
// (strace/perf/valgrind/gdb --args/...) are no longer scanned for a buried
// suffix; they are refused whatever their tail contains — see
// TestListedPassthroughWrapperRefusedWhateverItsTail.
func TestWrapperChildTailBuriedSuffixMatchesIonice(t *testing.T) {
	names := map[string]struct{}{"CODEX_HOME": {}, "OPENAI_API_KEY": {}}
	for name := range accountShellStartupNames {
		names[name] = struct{}{}
	}
	wrappers := []string{
		"ionice", "taskset 0x1", "nice", "nohup", "timeout 5", "setsid", "stdbuf -o0",
		"xargs", "xargs -r", "/usr/bin/xargs",
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
	} {
		require.True(t, commandMutatesAccountEnvironment(command, names), "command %q", command)
	}
}

// A listed argv-passthrough wrapper (strace/perf/valgrind/gdb/ltrace and the
// util-linux launchers chrt/numactl/unshare/prlimit) runs a child command and
// forwards argv, so a PATH-shadowed copy can replace the selected account
// environment exactly the way a shadowed nohup or ionice can. A static
// command-line check cannot enumerate every shape a shadowed wrapper can shift
// to (a buried-suffix scan was bypassed twice), so peelAccountWrapper's
// default arm refuses such a wrapper whatever its tail contains: the effect of
// an unmodeled wrapper on the account environment is unknown, so the command is
// unverifiable rather than clean. This pins that property over the benign tails
// the buried-suffix scan used to let through (an ordinary tail, a non-account
// buried name, a trailing shell with no argv, env print mode) — every one now
// refuses, the same way a tail that buries a mutation refuses.
func TestListedPassthroughWrapperRefusedWhateverItsTail(t *testing.T) {
	names := map[string]struct{}{"CODEX_HOME": {}, "OPENAI_API_KEY": {}}
	for name := range accountShellStartupNames {
		names[name] = struct{}{}
	}
	heads := []string{"perf", "strace", "valgrind", "gdb", "gdb --args", "/usr/bin/perf", "ltrace",
		"chrt 0", "numactl", "unshare", "prlimit"}
	// Tails the old buried-suffix scan let through (benign) and tails it refused
	// (mutating) alike — a listed wrapper refuses both, whatever its tail contains.
	tails := []string{
		// Benign tails that were admitted before the fail-closed redesign.
		"echo a a a",
		"codex --model x",
		"npm run dev",
		"stat npm run dev",
		"echo xargs --process-slot-var PORT codex",
		"echo unset PORT",
		"sh",
		"env CODEX_HOME=/other",
		"-p 1234 codex",
		// Mutating tails a shadowed wrapper would exec; still refused.
		"echo a a a xargs --process-slot-var CODEX_HOME codex",
		"echo unset CODEX_HOME",
		"set -k codex CODEX_HOME=/other",
		"trap 'unset CODEX_HOME' 0",
		"env CODEX_HOME=/other codex",
		"sh -c 'unset CODEX_HOME; codex'",
	}
	for _, head := range heads {
		for _, tail := range tails {
			command := head + " " + tail
			require.True(t, commandMutatesAccountEnvironment(command, names),
				"listed passthrough wrapper %q must be refused whatever its tail contains (tail %q)",
				head, tail)
		}
	}
	// The wrapper's own option spells (strace -f, strace -o, strace --, perf stat,
	// gdb --args) reach the same refusal: the verdict is set by the wrapper head,
	// not by what follows it.
	for _, command := range []string{
		"strace -f echo unset CODEX_HOME",
		"strace -o /dev/null echo unset CODEX_HOME",
		"strace -- echo unset CODEX_HOME",
		"perf stat echo unset CODEX_HOME",
		"gdb --args echo unset CODEX_HOME",
		"strace -f codex --model x",
		"strace -p 1234 codex",
		"strace -- codex",
		"strace",
	} {
		require.True(t, commandMutatesAccountEnvironment(command, names), "command %q", command)
	}
}

// The fail-closed redesign leaves unlisted wrappers (any executable the checker
// does not model, e.g. firejail) to the visible-assignment scan
// (unrecognizedWrapperHidesAccountAssignment): an unlisted wrapper with a
// visible account-environment assignment is refused, and one without is admitted
// as today. A plain agent command is unchanged. af cannot see inside a user's
// own wrapper script, so a script's internals are outside what this check
// verifies — the scope is the visible command line and the modeled wrappers.
func TestUnlistedWrapperVisibleAssignmentRefused(t *testing.T) {
	names := map[string]struct{}{"CODEX_HOME": {}, "OPENAI_API_KEY": {}}
	for name := range accountShellStartupNames {
		names[name] = struct{}{}
	}
	for _, command := range []string{
		// An unlisted wrapper with a visible account-environment assignment (a
		// nested env that overrides a denied name) is refused.
		"firejail env CODEX_HOME=/other codex",
		"firejail env OPENAI_API_KEY=sk codex",
		// A strace in an unlisted wrapper's tail carrying a visible -E assignment
		// is refused by the same visible-assignment scan.
		"firejail strace -E CODEX_HOME=/other codex",
		"firejail strace --env=CODEX_HOME=/other codex",
		// A literal --opt=DENIED=value option under an unlisted wrapper is refused.
		"firejail --setenv=CODEX_HOME=/other codex",
	} {
		require.True(t, commandMutatesAccountEnvironment(command, names), "command %q", command)
	}
	for _, command := range []string{
		// An unlisted wrapper without a visible account-environment assignment is
		// admitted, as today.
		"firejail codex",
		"firejail npm run dev",
		"firejail strace codex",
		"firejail echo CODEX_HOME",
		"firejail env PORT=3000 npm start",
		// A plain agent command is unchanged.
		"codex --model x",
		"echo CODEX_HOME=/tmp",
		"rg OPENAI_API_KEY= .",
		"man env",
	} {
		require.False(t, commandMutatesAccountEnvironment(command, names), "command %q", command)
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
