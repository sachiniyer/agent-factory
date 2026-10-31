package commands

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/apiclient"
)

func TestTargetSetShowClearRoundTrip(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	t.Setenv("AF_DAEMON_URL", "")
	t.Setenv("AF_DAEMON_TOKEN", "")
	oldU, oldT := apiclient.FlagDaemonURL, apiclient.FlagDaemonToken
	apiclient.FlagDaemonURL, apiclient.FlagDaemonToken = "", ""
	t.Cleanup(func() { apiclient.FlagDaemonURL, apiclient.FlagDaemonToken = oldU, oldT })
	t.Cleanup(func() { targetSetTokenStdin = false })

	run := func(c *cobra.Command, in string, args ...string) string {
		var out bytes.Buffer
		c.SetOut(&out)
		c.SetIn(strings.NewReader(in))
		require.NoError(t, c.RunE(c, args))
		return out.String()
	}

	require.Contains(t, run(targetShowCmd, ""), "local daemon")

	targetSetTokenStdin = true
	out := run(targetSetCmd, "s3cret\n", "http://box:8443")
	require.NotContains(t, out, "s3cret", "the token is never echoed")
	path, err := apiclient.SavedTargetPath()
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	show := run(targetShowCmd, "")
	require.Contains(t, show, "http://box:8443")
	require.Contains(t, show, "source: saved")
	require.Contains(t, show, "token: set")
	require.NotContains(t, show, "s3cret")

	apiclient.FlagDaemonURL = "http://flag:1"
	require.Contains(t, run(targetShowCmd, ""), "source: flag")
	apiclient.FlagDaemonURL = ""

	run(targetClearCmd, "")
	require.Contains(t, run(targetShowCmd, ""), "local daemon")

	targetSetTokenStdin = false
	c := targetSetCmd
	require.Error(t, c.RunE(c, []string{"ftp://nope"}))
}
