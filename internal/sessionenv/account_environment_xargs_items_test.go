package sessionenv

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func xargsItemTestNames() map[string]struct{} {
	codex := accountScopedNames("codex", "CODEX_HOME")
	for name := range accountShellStartupNames {
		codex[name] = struct{}{}
	}
	return codex
}

// #4977: under -I/-i/--replace, xargs replaces the marker in its initial
// arguments with an input line, so a marker in a command slot among them
// names a program nobody can see at validation time: `xargs -I{} strace {}
// CODEX_HOME=/x codex` fed the line `env` runs `strace env CODEX_HOME=/x
// codex`, and the walk used to read `{}` as an ordinary program name.
func TestValidateAccountEnvironmentCommand_RefusesXargsMarkerInCommandSlot(t *testing.T) {
	account := scopedProcessTabAccount()
	for _, command := range []string{
		"xargs -I{} strace {} CODEX_HOME=/x codex",
		"xargs -I{} nohup {} CODEX_HOME=/x codex",
	} {
		err := ValidateAccountEnvironmentCommand(command, account)
		require.Error(t, err, "command %q must not replace the sibling account environment", command)
		require.Contains(t, err.Error(), "sets an identity or shell-startup variable")
	}
	// A plain substitution into an ordinary argument stays accepted. So does
	// the report's first shape: xargs never substitutes its own COMMAND word
	// (GNU xargs 4.9: `printf 'env\n' | xargs -I{} {} A` tries to execute a
	// literal `{}`), so `{}` there is a fixed program name like any other.
	for _, command := range []string{
		"xargs -I{} echo {}",
		"xargs -I{} env codex {}",
		"xargs -I{} {} CODEX_HOME=/x codex",
	} {
		require.NoError(t, ValidateAccountEnvironmentCommand(command, account), "command %q", command)
	}
}

func TestCommandMutatesAccountEnvironment_XargsMarkerPositions(t *testing.T) {
	names := xargsItemTestNames()
	for _, test := range []struct {
		command string
		want    bool
	}{
		// An initial argument in a command slot, under every replace
		// spelling, and with the marker only part of the word: `/usr/bin/{}`
		// is /usr/bin/env when the line is `env`.
		{"xargs -I {} nohup {} CODEX_HOME=/x codex", true},
		{"xargs -i nohup {} CODEX_HOME=/x codex", true},
		{"xargs --replace nohup {} CODEX_HOME=/x codex", true},
		{"xargs --replace=@ nohup @ CODEX_HOME=/x codex", true},
		{"xargs -I{} nohup /usr/bin/{} CODEX_HOME=/x codex", true},
		// A command slot among the initial arguments is refused even with
		// nothing mutating after it: the program could be a shell reading its
		// script from stdin or a file.
		{"xargs -I{} nohup {}", true},
		{"xargs -I{} setsid {} /tmp/launch-agent", true},
		// A marker that contains '=' is replaced wherever it occurs (GNU
		// xargs 4.9 turns `A=b` into `AXXb` under -I=), so the assignment
		// split does not make it data. Codex on #4979.
		{"xargs -I= strace = -u CODEX_HOME codex", true},
		{"xargs -I= env = codex", true},
		{"xargs -I=x nohup A=x codex", true},
		// -I then -n2/-L/-l cancels replace mode (the last one wins), so
		// input is appended: `env` with no command gets it as operands.
		// Codex on #4979; -n1 is GNU's exception and keeps replace.
		{"xargs -I{} -n2 env", true},
		{"xargs -I{} -L1 env", true},
		{"xargs -I{} -l env", true},
		{"xargs -I{} --max-args=2 env", true},
		// GNU parses counts with leading whitespace. Codex on #4979.
		{"xargs -I{} -n ' 2' env", true},
		{"xargs -I{} -L ' 2' env", true},
		// BSD xargs (macOS) keeps -I in force alongside -n/-L, so the marker
		// is still substituted there. Codex on #4979. That makes `cat` a
		// marker in `/bin/cat` on macOS even though GNU drops it.
		{"xargs -I{} -n2 nohup {} CODEX_HOME=/x codex", true},
		{"xargs -I{} -L1 nohup {}", true},
		{"xargs -Icat -n2 strace /bin/cat -u CODEX_HOME codex", true},
		// A modeled wrapper's command slot.
		{"xargs -I{} nohup {} CODEX_HOME=/x codex", true},
		{"xargs -I{} nice -n 5 {}", true},
		{"xargs -I{} timeout 5 {} /tmp/launch-agent", true},
		// An unmodeled wrapper's argv, where the marker may be what it execs.
		{"xargs -I{} strace {} CODEX_HOME=/x codex", true},
		{"xargs -I{} strace -f {} -u CODEX_HOME codex", true},
		{"xargs -I{} strace {} sh -c 'unset CODEX_HOME; codex'", true},
		// The outer marker reaches the inner -I argument, so the inner marker
		// is whatever the outer line says — possibly the word `strace`.
		{"xargs -I{} xargs -I{} strace CODEX_HOME=/x codex", true},
		{"xargs -I{} xargs -I {} strace CODEX_HOME=/x codex", true},
		// A dynamic marker can match any word, including the command.
		{`xargs -I "$M" echo hi`, true},
		// Controls: the marker as an ordinary argument, as an assignment's
		// value, and after a literal env's command slot.
		{"xargs -I{} echo {}", false},
		{"xargs -I{} cp {} /tmp", false},
		{"xargs -I{} nice -n 5 echo {}", false},
		{"xargs -I{} strace -f echo {}", false},
		{"xargs -I{} env codex {}", false},
		{"xargs -I{} env PORT={} codex", false},
		{"xargs -I{} echo PORT={}", false},
		// strace is modeled (#4978), so {} here is its program.
		{"xargs -I{} strace {}", true},
		{"xargs -I{}", false},
		{"xargs -I{} xargs -I[] echo []", false},
		// xargs's own COMMAND word is never substituted, so the marker there
		// is a literal program name. Codex on #4979.
		{"xargs -I{} {} CODEX_HOME=/x codex", false},
		{"xargs -I{} {}", false},
		{"xargs -i {} echo hi", false},
		{"xargs -I{} /usr/bin/{} echo", false},
		// -n1 keeps replace mode in GNU's count grammar (` 1`, `+1`, `01`).
		{"xargs -I{} -n1 echo {}", false},
		{"xargs -I{} -n ' 1' echo {}", false},
		{"xargs -I{} -n +1 echo {}", false},
		{"xargs -I{} -n 01 env codex", false},
		// A later literal -I supersedes an earlier marker. Codex on #4979.
		{"xargs -I M -I{} echo hi M", false},
		{"xargs -I{} -i echo {} hi", false},
		// A marker only in an assignment's value stays data.
		{"xargs -I{} nohup env A={} codex", false},
	} {
		require.Equal(t, test.want, commandMutatesAccountEnvironment(test.command, names),
			"command %q", test.command)
	}
}

// A substituted marker is one argv word of unknown content, so the walk judges
// it as it judges a double-quoted "$y" in the same place. This pins both
// directions over a generated corpus: the marker is never refused where "$y"
// would be accepted, and it is never accepted where "$y" would be refused,
// except after a literal env's command slot, which envCallArgvParse admits.
func TestCommandMutatesAccountEnvironment_XargsMarkerMatchesDynamicWord(t *testing.T) {
	names := xargsItemTestNames()
	heads := []string{"xargs -I{}", "xargs -i", "xargs --replace"}
	tokens := []string{
		"{}", "x{}", "{}=1", "echo", "env", "strace", "nice", "sh", "-c", "-u",
		"CODEX_HOME=/x", "CODEX_HOME", "codex", "/tmp",
	}
	spell := func(head string, words []string) (string, string) {
		dynamic := make([]string, len(words))
		for i, word := range words {
			name, _, _ := strings.Cut(word, "=")
			dynamic[i] = word
			// xargs's own COMMAND word is never substituted.
			if i > 0 && strings.Contains(name, "{}") {
				dynamic[i] = `"$y"`
			}
		}
		return head + " " + strings.Join(words, " "), head + " " + strings.Join(dynamic, " ")
	}
	checked := 0
	for _, head := range heads {
		for _, a := range tokens {
			for _, b := range tokens {
				for _, c := range tokens {
					words := []string{a, b, c}
					marker, dynamic := spell(head, words)
					got := commandMutatesAccountEnvironment(marker, names)
					want := commandMutatesAccountEnvironment(dynamic, names)
					if got && !want {
						t.Errorf("%q is refused but %q is accepted", marker, dynamic)
					}
					if !got && want && !strings.Contains(" "+strings.Join(words, " ")+" ", " env ") {
						t.Errorf("%q is accepted but %q is refused", marker, dynamic)
					}
					checked++
				}
			}
		}
	}
	require.Equal(t, len(heads)*len(tokens)*len(tokens)*len(tokens), checked)
}
