package sessionenv

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// #4978: xargs input — an appended item, or the line a -I marker is replaced
// with — must never be able to become a program word, an env operand, or an
// option value that a wrapper feeds to env.
func TestValidateAccountEnvironmentCommand_RefusesXargsInputReachingEnv(t *testing.T) {
	account := scopedProcessTabAccount()
	for _, command := range []string{
		// The two shapes from the report.
		"xargs nohup",
		"xargs -I{} strace --env={} codex",
	} {
		err := ValidateAccountEnvironmentCommand(command, account)
		require.Error(t, err, "command %q must not replace the sibling account environment", command)
		require.Contains(t, err.Error(), "sets an identity or shell-startup variable")
	}
	for _, command := range []string{"xargs env codex", "xargs rm"} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, account), "command %q", command)
	}
}

func TestCommandMutatesAccountEnvironment_XargsInputPositions(t *testing.T) {
	names := accountScopedNames("codex", "CODEX_HOME")
	for name := range accountShellStartupNames {
		names[name] = struct{}{}
	}
	for _, test := range []struct {
		command string
		want    bool
	}{
		// Program word: a chain that ends without naming a command hands
		// appended items the program slot, directly or a link further in.
		{"xargs nohup", true},
		{"xargs nice", true},
		{"xargs nice -n 5", true},
		{"xargs timeout 5", true},
		{"xargs setsid", true},
		{"xargs stdbuf -o0", true},
		{"xargs taskset 1", true},
		{"xargs -n 1 nohup", true},
		{"xargs xargs", true},
		{"xargs env A=1 nohup", true},
		{"xargs strace nohup", true},
		{"xargs nohup strace env A=1 nice", true},
		// strace's option region: appended items land there while strace
		// names no command, and could spell -ECODEX_HOME=1 codex.
		{"xargs strace", true},
		{"xargs strace -f", true},
		{"xargs strace -o", true},
		{"xargs strace -o log", true},
		{"xargs nice -n 5 strace --output", true},
		// An incomplete link behind strace that appended items complete.
		{"xargs strace nice -n", true},
		{"xargs strace -f timeout", true},
		// A marker as -E/--env's value, in an option word whose identity the
		// line decides, or in strace's command slot.
		{"xargs -I{} strace -E {} codex", true},
		{"xargs -I{} strace -E{} codex", true},
		{"xargs -I{} strace -fE{} codex", true},
		{"xargs -I{} strace --env={} codex", true},
		{"xargs -I{} strace --en={} codex", true},
		{"xargs -I{} strace --env {} codex", true},
		{"xargs -I{} strace -{} codex", true},
		{"xargs -I{} strace -f{} codex", true},
		{"xargs -I{} strace --{} codex", true},
		{"xargs -I{} strace {} codex", true},
		{"xargs -I{} strace {}", true},
		{"xargs -I{} strace -E {}=1 codex", true},
		{"xargs -I{} strace --env=x{} codex", true},
		{"xargs -I{} strace -f x{} codex", true},
		{"xargs -I{} env A=1 strace --env={} codex", true},
		// env operand: --unset's value is a name env removes.
		{"xargs -I{} env --unset={} codex", true},
		{"xargs --replace env --unset=x{} codex", true},
		// A nested xargs's -I and --process-slot-var take the outer line.
		{"xargs -I{} xargs --process-slot-var={} codex", true},
		{"xargs -I{} xargs --process-slot-var {} codex", true},
		{"xargs -I{} xargs -I{} strace CODEX_HOME=/x codex", true},
		{"xargs -I{} xargs -I {} strace CODEX_HOME=/x codex", true},
		{"xargs -I{} xargs -i{} strace codex", true},
		// Controls: a named command takes appended items as its arguments,
		// and a marker that only fills a data value stays accepted.
		{"xargs", false},
		{"xargs rm", false},
		{"xargs env codex", false},
		{"xargs nohup rm", false},
		{"xargs nice -n 5 echo", false},
		{"xargs strace -f echo", false},
		{"xargs strace -o log echo", false},
		{"xargs strace -- echo", false},
		{"xargs env A=1 nohup rm", false},
		{"xargs xargs echo", false},
		{"xargs -I{} echo {}", false},
		{"xargs -I{} nohup", false},
		{"xargs -I{} strace -o {} codex", false},
		{"xargs -I{} strace -p {}", false},
		{"xargs -I{} strace -f echo {}", false},
		{"xargs -I{} strace -f echo -E{}", false},
		// -E with no command after it runs no child, and a marker only in
		// the value of a fixed variable name is data.
		{"xargs -I{} strace -E{}", false},
		{"xargs -I{} strace -E A={} codex", false},
		{"xargs -I{} strace -E --chdir={} codex", false},
		{"xargs -I{} strace --env=A={} codex", false},
		// A link the model cannot parse behind strace keeps the walk's
		// verdict: the real nice rejects -E before running anything.
		{"xargs -I{} strace -f nice -E{} codex", false},
		{"xargs strace nice -E x", false},
		{"xargs -I{} env --chdir={} codex", false},
		{"xargs -I{} env PORT={} codex", false},
		{"xargs -I{} xargs -n {} echo", false},
		{"xargs -I{} xargs -I[] echo [] {}", false},
		// No child to run: strace with only options after the marker, and a
		// nested xargs that names no command (it runs echo).
		{"xargs -I{} strace -f -{} --", false},
		{"xargs -I{} xargs -I{} --", false},
		{"xargs -I{} xargs -I {}", false},
		{"xargs -I{} xargs --process-slot-var={}", false},
	} {
		require.Equal(t, test.want, commandMutatesAccountEnvironment(test.command, names),
			"command %q", test.command)
	}
}
