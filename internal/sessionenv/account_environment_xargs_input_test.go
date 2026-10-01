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
		// Codex on #4980: a marker at a chain head is the program even after
		// modeled wrappers peel off.
		{"xargs -I{} nohup {}", true},
		{"xargs -I{} nice -n 5 {}", true},
		{"xargs -I{} strace nohup {}", true},
		// getopt_long abbreviations, measured on strace 6.8 and GNU xargs
		// 4.9. --fol is --follow-forks (no argument), so {} is strace's
		// program; --proc/--p is --process-slot-var; --re= is --replace.
		{"xargs -I{} strace --fol {} codex", true},
		{"xargs -I{} strace --follow-forks xargs --proc={} codex", true},
		{"xargs -I{} xargs --proc={} codex", true},
		{"xargs -I{} xargs --p {} codex", true},
		{"xargs -I{} xargs --re={} codex", true},
		{"xargs -I{} strace --en={} codex", true},
		// An option the tables cannot resolve may or may not take the next
		// word, so a marker there may be the program.
		{"xargs -I{} strace --zz {} codex", true},
		{"xargs -I{} strace --outp {} codex", true},
		// A link the walk cannot parse behind strace still reaches env when
		// input completes it: nice's --adj is --adjustment.
		{"xargs strace nice --adj", true},
		{"xargs -I{} strace nice --adj={} codex", true},
		// The same rule refuses a marker in any link the walk cannot parse,
		// which is master's verdict for that link in command position
		// (`nice -Ex codex` is unsafe there): the model cannot tell an
		// option the binary rejects from one it accepts under a prefix.
		{"xargs -I{} strace -f nice -E{} codex", true},
		{"xargs strace nice -E x", true},
		// Codex on #4980, round 2. A separate option value of strace or a
		// nested xargs is a possible program: a shadowed binary doing
		// `shift; exec "$@"` runs it, as the modeled wrappers' consumed
		// operands are judged.
		{"xargs -I{} strace -o {} codex", true},
		{"xargs -I{} strace -p {}", true},
		{"xargs -I{} strace -E A={} codex", true},
		{"xargs -I{} strace -E --chdir={} codex", true},
		{"xargs -I{} strace --decode-pids {} codex", true},
		{"xargs -I{} strace --output {} codex", true},
		{"xargs -I{} strace --string-l {} codex", true},
		{"xargs -I{} xargs --max-args {} echo", true},
		{"xargs -I{} xargs -n {}", true},
		{"xargs -I{} xargs -n {} echo", true},
		{"xargs -I{} xargs -I {}", true},
		// After "--" a dash-spelled word is still the program, so a nested
		// xargs's --process-slot-var reaches it.
		{"xargs -I{} xargs --process-slot-var={} -- -dir/codex", true},
		{"xargs -I{} strace -f -{} -- -dir/codex", true},
		// A marker containing '=' is replaced wherever it occurs.
		{"xargs -I= nohup =", true},
		// -I then -n2/-L cancels replace mode, so input is appended.
		{"xargs -I{} -n2 nohup", true},
		{"xargs -I{} -L1 strace", true},
		{"xargs -I{} -n ' 2' nohup", true},
		// Codex on #4980, round 3. A '='-bearing marker reaches -E's name
		// through the separator; counts past Go's int range and a nested
		// count the outer line spells still cancel -I.
		{"xargs -I= strace -E= codex", true},
		{"xargs -I= strace --env== codex", true},
		{"xargs -I{} -n9223372036854775808 nohup", true},
		{"xargs -I{} -L99999999999999999999 nohup", true},
		{"xargs -a /tmp/outer -I{} xargs -I[] -n{} nohup", true},
		{"xargs -I{} xargs -I[] --max-args={} nohup", true},
		// BSD xargs (macOS) keeps -I in force alongside -n/-L, so both
		// readings are judged: the marker is still substituted there.
		{"xargs -I{} -n2 nohup {} CODEX_HOME=/x codex", true},
		{"xargs -Icat -n2 strace /bin/cat codex", true},
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
		{"xargs -I{} strace -f echo {}", false},
		{"xargs -I{} strace -f echo -E{}", false},
		// -E with no command after it runs no child, and a marker only in
		// the value of a fixed variable name is data.
		{"xargs -I{} strace -E{}", false},
		{"xargs -I{} strace --env=A={} codex", false},

		{"xargs -I{} env --chdir={} codex", false},
		{"xargs -I{} env PORT={} codex", false},
		// Both markers are substituted, so echo gets two unknown words, which
		// the walk refuses as it refuses `echo "$y" "$y"` (#4977's "$y"
		// parity).
		{"xargs -I{} xargs -I[] echo [] {}", true},
		// The outer marker reaches the nested -I, so the nested marker is an
		// unknown word, refused as `xargs -I"$y" --` is (#4977).
		{"xargs -I{} xargs -I{} --", true},
		// xargs's own COMMAND word is never substituted (GNU xargs 4.9 runs a
		// literal `{}`), so a marker there is a fixed program name.
		{"xargs -I{} {}", false},
		{"xargs -I{} {} codex", false},
		// An attached value is part of one argv word; a shadowed strace
		// cannot exec it on its own.
		{"xargs -I{} strace -o{} codex", false},
		{"xargs -I{} strace --output={} codex", false},
		{"xargs -I{} strace --env=A={} codex", false},
		// With nothing after "--" there is still no program.
		{"xargs -I{} strace -f -{} --", false},
		// -n1 keeps replace mode in GNU's count grammar.
		{"xargs -I{} -n1 echo {}", false},
		{"xargs -I{} -n ' 1' echo {}", false},
		// Abbreviated and hidden options consume no marker here.
		{"xargs -I{} strace --fol echo {}", false},
		// An unparseable link no input reaches keeps the walk's verdict.
		{"xargs -I{} strace nice -E x", false},
		// No child to run: strace with only options after the marker, and a
		// nested xargs that names no command (it runs echo).
		{"xargs -I{} strace -f -{} --", false},
		{"xargs -I{} xargs --process-slot-var={}", false},
	} {
		require.Equal(t, test.want, commandMutatesAccountEnvironment(test.command, names),
			"command %q", test.command)
	}
}

// An explicitly empty replace marker (--replace= or -I "") used to leak into the
// xargs-input scans, where Go's strings.Contains(s, "")/strings.HasPrefix(s, "")
// are true for every word and collapsed the walk to "every word carries
// substituted input -> refuse." The empty marker now defaults to {} (like a
// bare --replace), so a replace-cancelling -L/-l/-n no longer refuses commands
// GNU xargs runs, and the no-canceller case stays fail-safe (#4980).
func TestCommandMutatesAccountEnvironment_XargsEmptyReplaceMarker(t *testing.T) {
	names := accountScopedNames("codex", "CODEX_HOME")
	for name := range accountShellStartupNames {
		names[name] = struct{}{}
	}
	for _, test := range []struct {
		command string
		want    bool
	}{
		// Empty marker + a replace-canceller: GNU xargs 4.9 appends input
		// (the marker is ignored) and runs the command, so it must not be
		// refused. The default-marker control already passed.
		{"xargs --replace= -L1 strace -f echo", false},
		{"xargs --replace= -L1 nohup echo", false},
		{"xargs --replace= -l strace -f echo", false},
		{"xargs --replace= -n2 strace -f echo", false},
		{"xargs --replace= --max-args=2 strace -f echo", false},
		{`xargs -I "" -L1 strace -f echo`, false},
		{`xargs -I "" -n2 nohup echo`, false},
		// No canceller: the empty marker defaults to {} too. GNU rejects the
		// command with a usage error, but a usage error cannot mutate the
		// environment, so the security answer is accepted.
		{"xargs --replace= strace -f echo", false},
		{"xargs --replace= echo", false},
		{`xargs -I "" echo`, false},
		// When the defaulted {} actually reaches a dangerous spot the
		// command is still refused, exactly as an explicit -I{} would be.
		{"xargs --replace= strace --env={} codex", true},
		{"xargs --replace= nohup {} CODEX_HOME=/x codex", true},
		{"xargs --replace= env --unset={} codex", true},
		{`xargs -I "" strace {} codex`, true},
		// Appended input still reaches env's operand region with the
		// canceller, with or without the empty marker.
		{"xargs --replace= -L1 env", true},
		{"xargs --replace= -n2 env", true},
		{`xargs -I "" -L1 env`, true},
		// Default-marker controls are unchanged.
		{"xargs --replace -L1 strace -f echo", false},
		{"xargs --replace -L1 env", true},
		{"xargs -I{} -n2 nohup", true},
		{"xargs -I{} -L1 strace", true},
	} {
		require.Equal(t, test.want, commandMutatesAccountEnvironment(test.command, names),
			"command %q", test.command)
	}
}
