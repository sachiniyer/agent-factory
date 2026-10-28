package session

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestRemoteShScript pins #5571 W2.9: the argument ssh receives is an explicit
// `sh -c '<script>'`, and a POSIX shell reading that argument runs the original
// script byte for byte — hostile quoting, expansions and newlines included.
func TestRemoteShScript(t *testing.T) {
	scripts := []string{
		`echo hello`,
		`echo "it's $((1+2))" && printf '%s\n' "$HOME" >/dev/null`,
		"set -e\nfor x in a b; do echo $x; done",
		`echo '$(touch /tmp/should-not-exist)'`,
	}
	for _, script := range scripts {
		wrapped := remoteShScript(script)
		require.True(t, strings.HasPrefix(wrapped, "sh -c '"), wrapped)

		want, err := exec.Command("sh", "-c", script).CombinedOutput()
		require.NoError(t, err, string(want))
		got, err := exec.Command("sh", "-c", wrapped).CombinedOutput()
		require.NoError(t, err, string(got))
		assert.Equal(t, string(want), string(got), "script %q", script)
	}
}

// TestBuildRunCommandSendsAnExplicitSh: the composed process carries the
// wrapped script as its one "$@" argument.
func TestBuildRunCommandSendsAnExplicitSh(t *testing.T) {
	p := &sandboxProvisioner{sshCmd: "ssh host"}
	cmd := p.buildRunCommand(t.Context(), `mktemp -d "$HOME/x"`, nil)
	last := cmd.Args[len(cmd.Args)-1]
	assert.Equal(t, remoteShScript(`mktemp -d "$HOME/x"`), last)
}
