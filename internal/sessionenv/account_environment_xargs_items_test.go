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

// #4977: under -I/-i/--replace, xargs replaces the marker with an input line,
// so a marker in a command slot names a program nobody can see at validation
// time. `xargs -I{} {} CODEX_HOME=/x codex` fed the line `env` runs
// `env CODEX_HOME=/x codex`, and the walk used to read `{}` as an ordinary
// program name and accept it.
func TestValidateAccountEnvironmentCommand_RefusesXargsMarkerInCommandSlot(t *testing.T) {
	account := scopedProcessTabAccount()
	for _, command := range []string{
		// The two shapes from the report.
		"xargs -I{} {} CODEX_HOME=/x codex",
		"xargs -I{} strace {} CODEX_HOME=/x codex",
	} {
		err := ValidateAccountEnvironmentCommand(command, account)
		require.Error(t, err, "command %q must not replace the sibling account environment", command)
		require.Contains(t, err.Error(), "sets an identity or shell-startup variable")
	}
	// A plain substitution into an ordinary argument stays accepted.
	for _, command := range []string{
		"xargs -I{} echo {}",
		"xargs -I{} env codex {}",
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
		// xargs's own command slot, under every replace spelling, and with the
		// marker only part of the word: `/usr/bin/{}` is /usr/bin/env when the
		// line is `env`.
		{"xargs -I{} {} CODEX_HOME=/x codex", true},
		{"xargs -I {} {} CODEX_HOME=/x codex", true},
		{"xargs -i {} CODEX_HOME=/x codex", true},
		{"xargs --replace {} CODEX_HOME=/x codex", true},
		{"xargs --replace=@ @ CODEX_HOME=/x codex", true},
		{"xargs -I{} /usr/bin/{} CODEX_HOME=/x codex", true},
		// A command slot is refused even with nothing mutating after it: the
		// program could be a shell reading its script from stdin or a file.
		{"xargs -I{} {}", true},
		{"xargs -I{} {} /tmp/launch-agent", true},
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
		{"xargs -I{} strace {}", false},
		{"xargs -I{}", false},
		{"xargs -I{} xargs -I[] echo []", false},
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
			if strings.Contains(name, "{}") {
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
