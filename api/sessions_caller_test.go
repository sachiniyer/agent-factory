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

// --- Credential-bound retry through the registration race (#6586) -------------
//
// The retry loop added for #6587 was wired only around the tmux-name path
// (whoamiSession, which matches rows by TmuxName). A sandbox credential's row
// carries no TmuxName (the daemon's sandbox projection strips it), so the
// tmux-name retry can never rescue a sandbox credential — the credential-bound
// lookup (callerSessionFn, the only path that can resolve one) was single-shot.
// These tests pin the fix: the credential-bound lookup now retries on
// ErrCallerRowNotRegistered (the daemon reports a bound caller but the row is
// not yet in the snapshot) within the same selfResolveAttempts/selfResolvePoll
// window, and falls through to the tmux path only for an operator credential
// (Caller empty).

// A sandbox program whose first --self call lands in the registration window
// (no tmux context — the normal off-box case) retries the credential-bound
// lookup and succeeds once the row registers, never consulting tmux.
func TestResolveSelfSessionRetriesCredentialBoundCallerThroughRace(t *testing.T) {
	t.Setenv("AF_DAEMON_TOKEN", "tok")
	calls := 0
	stubCallerSession(t, func() (session.InstanceData, bool, error) {
		calls++
		if calls < 3 {
			// The daemon reports a bound caller but the row is not yet
			// registered in the snapshot — the race.
			return session.InstanceData{}, false, apiclient.ErrCallerRowNotRegistered
		}
		return session.InstanceData{ID: "sess-1", Title: "mine"}, true, nil
	})
	// TMUX is unset inside a sandbox — currentTmuxName must never be reached.
	stubCurrentTmuxName(t, func() (string, error) {
		t.Fatal("credential-bound retry must not consult tmux when the daemon reports a bound caller")
		return "", nil
	})
	prev := selfResolveSleep
	selfResolveSleep = func(time.Duration) {}
	t.Cleanup(func() { selfResolveSleep = prev })

	got, err := resolveSelfSession()
	require.NoError(t, err)
	require.Equal(t, "sess-1", got.ID)
	require.GreaterOrEqual(t, calls, 2, "the row appears on a retry, not the first look")
}

// Even with an af_-prefixed tmux name (the retry loop's trigger), the
// credential-bound retry handles the race and the tmux-name path is never
// reached — the sandbox projection strips TmuxName, so whoamiSession can never
// match a sandbox credential's row.
func TestResolveSelfSessionCredentialRaceDoesNotReachTmuxPath(t *testing.T) {
	t.Setenv("AF_DAEMON_TOKEN", "tok")
	callerCalls := 0
	stubCallerSession(t, func() (session.InstanceData, bool, error) {
		callerCalls++
		if callerCalls < 2 {
			return session.InstanceData{}, false, apiclient.ErrCallerRowNotRegistered
		}
		return session.InstanceData{ID: "sess-1", Title: "mine"}, true, nil
	})
	// The snapshot stub serves the row the way the daemon's sandbox projection
	// does — TmuxName stripped — so a tmux-name match can never succeed. If the
	// tmux path were reached it would burn all selfResolveAttempts polls and
	// fail; the test proves the credential-bound retry handles the race instead.
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		t.Fatal("tmux-name path (whoamiSession) must not be reached when the credential-bound lookup retries the race")
		return nil, nil
	})
	stubCurrentTmuxName(t, func() (string, error) { return "af_prog", nil })
	prev := selfResolveSleep
	selfResolveSleep = func(time.Duration) {}
	t.Cleanup(func() { selfResolveSleep = prev })

	got, err := resolveSelfSession()
	require.NoError(t, err)
	require.Equal(t, "sess-1", got.ID)
	require.Equal(t, 2, callerCalls, "callerSessionFn retried the race; the tmux path was not reached")
}

// The credential-bound retry window is bounded: a row that never registers
// exhausts selfResolveAttempts and returns the sentinel, never consulting tmux.
func TestResolveSelfSessionCredentialRaceBoundedGiveUp(t *testing.T) {
	t.Setenv("AF_DAEMON_TOKEN", "tok")
	calls := 0
	stubCallerSession(t, func() (session.InstanceData, bool, error) {
		calls++
		return session.InstanceData{}, false, apiclient.ErrCallerRowNotRegistered
	})
	stubCurrentTmuxName(t, func() (string, error) {
		t.Fatal("must not consult tmux while the credential-bound race window is open")
		return "", nil
	})
	prev := selfResolveSleep
	selfResolveSleep = func(time.Duration) {}
	t.Cleanup(func() { selfResolveSleep = prev })

	_, err := resolveSelfSession()
	require.ErrorIs(t, err, apiclient.ErrCallerRowNotRegistered)
	require.Equal(t, selfResolveAttempts, calls, "the retry window is bounded by selfResolveAttempts")
}

// An operator credential (no bound caller, ok=false, nil error) falls through to
// the tmux-name path — the credential-bound retry only applies when the daemon
// reports a bound caller.
func TestResolveSelfSessionOperatorCredentialFallsThroughToTmux(t *testing.T) {
	t.Setenv("AF_DAEMON_TOKEN", "tok")
	callerCalls := 0
	stubCallerSession(t, func() (session.InstanceData, bool, error) {
		callerCalls++
		// Operator credential: the daemon reports no bound caller.
		return session.InstanceData{}, false, nil
	})
	stubSnapshot(t, func(daemon.SnapshotRequest) ([]session.InstanceData, error) {
		return []session.InstanceData{{ID: "s1", Title: "op", TmuxName: "af_op"}}, nil
	})
	stubCurrentTmuxName(t, func() (string, error) { return "af_op", nil })

	got, err := resolveSelfSession()
	require.NoError(t, err)
	require.Equal(t, "s1", got.ID)
	require.Equal(t, 1, callerCalls, "operator credential: single-shot, then fall through to tmux")
}

// A non-race error from the credential-bound lookup (transport, auth) surfaces
// immediately rather than being silently swallowed and retried — it is not the
// registration race and the caller should see the real failure.
func TestResolveSelfSessionCredentialNonRaceErrorSurfaces(t *testing.T) {
	t.Setenv("AF_DAEMON_TOKEN", "tok")
	calls := 0
	stubCallerSession(t, func() (session.InstanceData, bool, error) {
		calls++
		return session.InstanceData{}, false, errors.New("401 from the remote daemon")
	})
	stubCurrentTmuxName(t, func() (string, error) {
		t.Fatal("a non-race credential error must surface, not fall through to tmux")
		return "", nil
	})

	_, err := resolveSelfSession()
	require.ErrorContains(t, err, "401")
	require.Equal(t, 1, calls, "a non-race error is not retried")
}

// --- resolveCredentialSelf (remote --self) retry through the race -------------

// resolveCredentialSelf (the remote --self path) retries the credential-bound
// lookup through the same registration race, rather than refusing on the first
// ok=false while the row is still registering.
func TestResolveCredentialSelfRetriesThroughRace(t *testing.T) {
	t.Setenv("AF_DAEMON_TOKEN", "tok")
	remoteTarget(t)
	calls := 0
	stubCallerSession(t, func() (session.InstanceData, bool, error) {
		calls++
		if calls < 2 {
			return session.InstanceData{}, false, apiclient.ErrCallerRowNotRegistered
		}
		return session.InstanceData{ID: "sess-1", Title: "mine"}, true, nil
	})
	prev := selfResolveSleep
	selfResolveSleep = func(time.Duration) {}
	t.Cleanup(func() { selfResolveSleep = prev })

	got, err := resolveCredentialSelf()
	require.NoError(t, err)
	require.Equal(t, "sess-1", got.ID)
	require.Equal(t, 2, calls, "the row appears on a retry, not the first look")
}

// resolveCredentialSelf still refuses (does not retry) for an operator credential
// — no bound caller means no row to wait for.
func TestResolveCredentialSelfRefusesOperatorCredential(t *testing.T) {
	t.Setenv("AF_DAEMON_TOKEN", "tok")
	remoteTarget(t)
	stubCallerSession(t, func() (session.InstanceData, bool, error) {
		return session.InstanceData{}, false, nil
	})

	_, err := resolveCredentialSelf()
	require.ErrorContains(t, err, "needs a sandbox credential that names the calling session")
}

// resolveCredentialSelf's retry window is bounded: a row that never registers
// exhausts selfResolveAttempts and returns the sentinel, not the refusal.
func TestResolveCredentialSelfCredentialRaceBoundedGiveUp(t *testing.T) {
	t.Setenv("AF_DAEMON_TOKEN", "tok")
	remoteTarget(t)
	calls := 0
	stubCallerSession(t, func() (session.InstanceData, bool, error) {
		calls++
		return session.InstanceData{}, false, apiclient.ErrCallerRowNotRegistered
	})
	prev := selfResolveSleep
	selfResolveSleep = func(time.Duration) {}
	t.Cleanup(func() { selfResolveSleep = prev })

	_, err := resolveCredentialSelf()
	require.ErrorIs(t, err, apiclient.ErrCallerRowNotRegistered)
	require.Equal(t, selfResolveAttempts, calls)
}
