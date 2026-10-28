package sessionenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// #5034: ionice and taskset scan their child tail for a buried suffix that a
// PATH-shadowed wrapper (`shift N; exec "$@"`) would promote to the executed
// command; the five structurally identical wrappers returned the tail
// unscanned. Every wrapper must give the verdict ionice gives for the same
// buried payload.
func TestWrapperChildTailBuriedSuffixMatchesIonice(t *testing.T) {
	names := map[string]struct{}{"CODEX_HOME": {}, "OPENAI_API_KEY": {}}
	for name := range accountShellStartupNames {
		names[name] = struct{}{}
	}
	wrappers := []string{"ionice", "taskset 0x1", "nice", "nohup", "timeout 5", "setsid", "stdbuf -o0"}
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
	} {
		require.True(t, commandMutatesAccountEnvironment(command, names), "command %q", command)
	}
}
