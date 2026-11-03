package api

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
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

// A program in a just-spawned af session can run `af sessions attention --self`
// before its session row is scannable (#6586): the pane execs the program while
// registration is still landing. Inside an af_-named tmux session a not-found
// whoami is that race,
// so the lookup polls for a bounded window rather than reporting the absence
// at once.
func TestResolveSelfSessionRetriesWhileTheRowRegisters(t *testing.T) {
	t.Setenv("AF_DAEMON_TOKEN", "")
	calls := 0
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		calls++
		if calls < 3 {
			return nil, nil // the row is not scannable yet
		}
		return []session.InstanceData{{ID: "s1", Title: "racer", TmuxName: "af_racer_agent"}}, nil
	})
	stubCurrentTmuxName(t, func() (string, error) { return "af_racer_agent", nil })

	got, err := resolveSelfSession()
	require.NoError(t, err)
	require.Equal(t, "s1", got.ID)
	require.GreaterOrEqual(t, calls, 2, "the row appears on a retry, not the first look")
}

// Outside an af_-named session the miss is real — no row is coming — so the
// lookup must not stall waiting for one.
func TestResolveSelfSessionDoesNotWaitOutsideAfSession(t *testing.T) {
	t.Setenv("AF_DAEMON_TOKEN", "")
	calls := 0
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		calls++
		return nil, nil
	})
	stubCurrentTmuxName(t, func() (string, error) { return "main", nil })

	_, err := resolveSelfSession()
	require.ErrorContains(t, err, "no Agent Factory session found")
	require.Equal(t, 1, calls)
}

// A miss that is not the absence claim — a remote daemon's transport error —
// is not the registration race and never retries. (A local snapshot error falls
// back to disk, whose own absence answer is the same retriable claim.)
func TestResolveSelfSessionDoesNotRetryOtherErrors(t *testing.T) {
	t.Setenv("AF_DAEMON_TOKEN", "")
	remoteTarget(t)
	calls := 0
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		calls++
		return nil, errors.New("401 from the remote daemon")
	})
	stubCurrentTmuxName(t, func() (string, error) { return "af_racer_agent", nil })

	_, err := resolveSelfSession()
	require.ErrorContains(t, err, "401")
	require.Equal(t, 1, calls)
}

// The window is bounded: a row that never registers still reports the miss —
// an af_-named session whose row was deleted underneath it cannot hang.
func TestResolveSelfSessionGivesUpOnARowThatNeverRegisters(t *testing.T) {
	t.Setenv("AF_DAEMON_TOKEN", "")
	calls := 0
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		calls++
		return nil, nil
	})
	stubCurrentTmuxName(t, func() (string, error) { return "af_gone_agent", nil })
	prev := selfResolveSleep
	selfResolveSleep = func(time.Duration) {}
	t.Cleanup(func() { selfResolveSleep = prev })

	_, err := resolveSelfSession()
	require.ErrorContains(t, err, "no Agent Factory session found")
	require.Equal(t, selfResolveAttempts, calls)
}
