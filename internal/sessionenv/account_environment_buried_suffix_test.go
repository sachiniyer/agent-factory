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
// buried payload.
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
		// Controls: an ordinary tail, and a buried name that is not the account's.
		{"echo a a a", false},
		{"codex --model x", false},
		{"echo xargs --process-slot-var PORT codex", false},
		{"echo unset PORT", false},
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
