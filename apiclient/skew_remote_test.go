package apiclient

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// W9.4: against a remote target the remedy must be runnable from THIS client and
// must name the daemon's host. "af daemon restart" alone restarts the wrong one.
func TestSkewErrorsNameTheRemoteDaemonAndARemedyRunnableFromHere(t *testing.T) {
	old := FlagDaemonURL
	t.Cleanup(func() { FlagDaemonURL = old })
	t.Setenv("AF_DAEMON_URL", "")
	FlagDaemonURL = "http://box.example:8443"

	skew := (&VersionSkewError{Field: "tab_id", Detail: `json: unknown field "tab_id"`}).Error()
	for _, want := range []string{"the daemon at http://box.example:8443", `"tab_id"`, "ssh box.example", "af daemon restart"} {
		require.Contains(t, skew, want)
	}
	require.NotContains(t, skew, "restart it with `af daemon restart`", "the local remedy must not be offered for a remote daemon")

	missing := (&RouteNotServedError{Route: "/v1/SetConfigValue", Detail: `unknown route "/v1/SetConfigValue"`}).Error()
	for _, want := range []string{"the daemon at http://box.example:8443", "/v1/SetConfigValue", "older than this client", "ssh box.example"} {
		require.Contains(t, missing, want)
	}

	// Control: with no remote target the original local wording stands.
	FlagDaemonURL = ""
	require.Contains(t, (&VersionSkewError{Field: "tab_id", Detail: "x"}).Error(), "restart it with `af daemon restart`")
	require.Equal(t, `daemon does not serve /v1/X (it answered 404: d)`, (&RouteNotServedError{Route: "/v1/X", Detail: "d"}).Error())
}
