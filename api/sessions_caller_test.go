package api

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/session"
)

func stubCallerSession(t *testing.T, fn func() (session.InstanceData, bool, error)) {
	t.Helper()
	prev := callerSessionFn
	callerSessionFn = fn
	t.Cleanup(func() { callerSessionFn = prev })
}

// A credential-bound caller resolves without any tmux context: the narrowed
// sandbox row carries no TmuxName to match (#5571 W3.2).
func TestResolveSelfSessionUsesCredentialBoundCaller(t *testing.T) {
	t.Setenv("AF_DAEMON_TOKEN", "tok")
	stubCallerSession(t, func() (session.InstanceData, bool, error) {
		return session.InstanceData{ID: "sess-1", Title: "mine"}, true, nil
	})
	stubCurrentTmuxName(t, func() (string, error) { return "", errors.New("must not consult tmux") })

	got, err := resolveSelfSession()
	require.NoError(t, err)
	require.Equal(t, "sess-1", got.ID)
}

// With no token (the local path) the credential lookup is never attempted.
func TestResolveSelfSessionLocalPathSkipsCallerLookup(t *testing.T) {
	t.Setenv("AF_DAEMON_TOKEN", "")
	stubCallerSession(t, func() (session.InstanceData, bool, error) {
		t.Fatal("caller lookup must not run without a daemon token")
		return session.InstanceData{}, false, nil
	})
	stubCurrentTmuxName(t, func() (string, error) { return "", errors.New("no tmux") })

	_, err := resolveSelfSession()
	require.ErrorContains(t, err, "no tmux")
}

// The credential can arrive by flag or by env; both take the credential-bound
// path, and the flag wins when both are set (#5747).
func TestResolveSelfSessionHonoursTokenFlagAndEnv(t *testing.T) {
	row := session.InstanceData{ID: "sess-1", Title: "mine"}
	for name, setup := range map[string]func(t *testing.T){
		"flag only": func(t *testing.T) {
			t.Setenv("AF_DAEMON_TOKEN", "")
			prev := apiclient.FlagDaemonToken
			apiclient.FlagDaemonToken = "flag-tok"
			t.Cleanup(func() { apiclient.FlagDaemonToken = prev })
		},
		"env only": func(t *testing.T) { t.Setenv("AF_DAEMON_TOKEN", "env-tok") },
		"both": func(t *testing.T) {
			t.Setenv("AF_DAEMON_TOKEN", "env-tok")
			prev := apiclient.FlagDaemonToken
			apiclient.FlagDaemonToken = "flag-tok"
			t.Cleanup(func() { apiclient.FlagDaemonToken = prev })
		},
	} {
		t.Run(name, func(t *testing.T) {
			setup(t)
			stubCallerSession(t, func() (session.InstanceData, bool, error) { return row, true, nil })
			stubCurrentTmuxName(t, func() (string, error) { return "", errors.New("must not consult tmux") })
			got, err := resolveSelfSession()
			require.NoError(t, err)
			require.Equal(t, "sess-1", got.ID)
		})
	}
}

func TestHasDaemonTokenPrefersFlag(t *testing.T) {
	t.Setenv("AF_DAEMON_TOKEN", "")
	prev := apiclient.FlagDaemonToken
	t.Cleanup(func() { apiclient.FlagDaemonToken = prev })
	apiclient.FlagDaemonToken = ""
	require.False(t, apiclient.HasDaemonToken())
	apiclient.FlagDaemonToken = "flag-tok"
	require.True(t, apiclient.HasDaemonToken())
}
