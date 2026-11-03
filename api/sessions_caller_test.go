package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

func stubCallerSession(t *testing.T, fn func(context.Context) (session.InstanceData, bool, error)) {
	t.Helper()
	prev := callerSessionFn
	callerSessionFn = fn
	t.Cleanup(func() { callerSessionFn = prev })
}

// A credential-bound caller resolves without any tmux context: the narrowed
// sandbox row carries no TmuxName to match (#5571 W3.2).
func TestResolveSelfSessionUsesCredentialBoundCaller(t *testing.T) {
	t.Setenv("AF_DAEMON_TOKEN", "tok")
	stubCallerSession(t, func(context.Context) (session.InstanceData, bool, error) {
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
	stubCallerSession(t, func(context.Context) (session.InstanceData, bool, error) {
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
			stubCallerSession(t, func(context.Context) (session.InstanceData, bool, error) { return row, true, nil })
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
	stubCallerSession(t, func(context.Context) (session.InstanceData, bool, error) {
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
	stubCallerSession(t, func(context.Context) (session.InstanceData, bool, error) {
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
	stubCallerSession(t, func(context.Context) (session.InstanceData, bool, error) {
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
	stubCallerSession(t, func(context.Context) (session.InstanceData, bool, error) {
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

// With AF_DAEMON_TOKEN set but NO remote target, a failed credential-bound
// lookup must leave the tmux/disk resolution unchanged: on the local unix
// socket no credential can bind (the client sends no token and the daemon
// trusts the peer), so the lookup can only ever return ok=false or an error —
// and a stopped, warming or absent local daemon is exactly what the tmux/disk
// fallback exists for. Hard-failing here would regress whoami,
// attention --self, archive --self and the proposal self-fallback to a raw dial
// error whenever the token is exported (the pre-#6586 behavior is restored).
//
// Driven through the REAL client and a REAL disk fixture rather than stubs:
// a temp home has no daemon socket, so the caller lookup fails with the real
// local dial error, and the resolution must come back off instances.json.
func TestResolveSelfSessionLocalCallerFailureKeepsTmuxDiskResolution(t *testing.T) {
	home := testguard.SocketTempDir(t)
	t.Setenv("AGENT_FACTORY_HOME", home)
	t.Setenv("AF_DAEMON_TOKEN", "tok")
	t.Setenv("AF_DAEMON_URL", "")
	remoteFlags(t, "", "")
	raw, err := json.Marshal([]session.InstanceData{{ID: "s1", Title: "mine", TmuxName: "af_op"}})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("repo-x", raw))
	stubCurrentTmuxName(t, func() (string, error) { return "af_op", nil })

	got, err := resolveSelfSession()
	require.NoError(t, err, "a failed local caller lookup must not cost the tmux/disk resolution")
	require.Equal(t, "s1", got.ID)
}

// A non-race error from the credential-bound lookup against a REMOTE daemon
// (transport, auth) surfaces immediately rather than being silently swallowed,
// retried, or masked by a same-machine resolution — there is no local disk to
// fall back to and a bad token's 401 must reach the caller.
func TestResolveSelfSessionRemoteCredentialNonRaceErrorSurfaces(t *testing.T) {
	t.Setenv("AF_DAEMON_TOKEN", "tok")
	remoteTarget(t)
	calls := 0
	stubCallerSession(t, func(context.Context) (session.InstanceData, bool, error) {
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
	stubCallerSession(t, func(context.Context) (session.InstanceData, bool, error) {
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
	stubCallerSession(t, func(context.Context) (session.InstanceData, bool, error) {
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
	stubCallerSession(t, func(context.Context) (session.InstanceData, bool, error) {
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

// --- Fail-closed once a bound caller has been seen (defence in depth) --------

// Once the daemon reports a bound caller, a later "no bound caller" answer
// fails closed instead of dropping to the tmux lookup: the credential did not
// change between polls, so the daemon's answer did — and the tmux path can only
// resolve a DIFFERENT session (the sandbox projection withholds TmuxName, so a
// match is impossible anyway). Pinned on the LOCAL path deliberately: even the
// path that normally falls through must not fall through after a bound report.
func TestResolveSelfSessionBoundCallerVanishedFailsClosed(t *testing.T) {
	t.Setenv("AF_DAEMON_TOKEN", "tok")
	calls := 0
	stubCallerSession(t, func(context.Context) (session.InstanceData, bool, error) {
		calls++
		if calls == 1 {
			return session.InstanceData{}, false, apiclient.ErrCallerRowNotRegistered
		}
		return session.InstanceData{}, false, nil // bound, then unbound
	})
	stubCurrentTmuxName(t, func() (string, error) {
		t.Fatal("a bound caller that vanishes must fail closed, not consult tmux")
		return "", nil
	})
	prev := selfResolveSleep
	selfResolveSleep = func(time.Duration) {}
	t.Cleanup(func() { selfResolveSleep = prev })

	_, err := resolveSelfSession()
	require.ErrorIs(t, err, errBoundCaller)
	require.ErrorContains(t, err, "then reported none")
	require.Equal(t, 2, calls)
}

// resolveCredentialSelf treats the same vanished-answer inconsistency as a
// failure, not as the operator-credential refusal — the credential DID name a
// session when the daemon first answered.
func TestResolveCredentialSelfBoundCallerVanishedFailsClosed(t *testing.T) {
	t.Setenv("AF_DAEMON_TOKEN", "tok")
	remoteTarget(t)
	calls := 0
	stubCallerSession(t, func(context.Context) (session.InstanceData, bool, error) {
		calls++
		if calls == 1 {
			return session.InstanceData{}, false, apiclient.ErrCallerRowNotRegistered
		}
		return session.InstanceData{}, false, nil
	})
	prev := selfResolveSleep
	selfResolveSleep = func(time.Duration) {}
	t.Cleanup(func() { selfResolveSleep = prev })

	_, err := resolveCredentialSelf()
	require.ErrorIs(t, err, errBoundCaller)
	require.NotContains(t, err.Error(), "needs a sandbox credential",
		"a vanished bound caller is not the operator-credential refusal")
}

// --- The poll is bounded in wall time, not only in sleeps --------------------

// A call that HANGS — a wedged daemon that accepts the connection and never
// answers — is cancelled by the deadline across the loop: selfResolveAttempts
// calls at up to the remote request timeout (5m) each would otherwise turn a
// ~1.5s window into a ~50-minute hang (#6698 review).
func TestPollCredentialBoundCallerDeadlineCancelsAHangingCall(t *testing.T) {
	prev := selfResolveDeadline
	selfResolveDeadline = 100 * time.Millisecond
	t.Cleanup(func() { selfResolveDeadline = prev })
	calls := 0
	stubCallerSession(t, func(ctx context.Context) (session.InstanceData, bool, error) {
		calls++
		<-ctx.Done() // a wedged call: returns only when the poll's deadline cancels it
		return session.InstanceData{}, false, ctx.Err()
	})

	started := time.Now()
	_, _, err := pollCredentialBoundCaller()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Equal(t, 1, calls, "the first hung call is cancelled; nothing retries after it")
	require.Less(t, time.Since(started), 10*time.Second)
}

// --- A still-creating row keeps the bounded wait (#6690 review) --------------
//
// The daemon answers the caller's identity from the pending create before the
// session commits, so the row can arrive carrying InFlightOp OpCreating. That
// row is never handed to acting verbs as if it were committed: the poll keeps
// the same bounded wait, resolves once the create commits, and reports "still
// being created" — not the ambiguous gone phrasing — if the window closes
// first.
func TestResolveSelfSessionWaitsForAStillCreatingRowToCommit(t *testing.T) {
	t.Setenv("AF_DAEMON_TOKEN", "tok")
	calls := 0
	stubCallerSession(t, func(context.Context) (session.InstanceData, bool, error) {
		calls++
		if calls == 1 {
			return session.InstanceData{ID: "sess-1", Title: "mine", InFlightOp: session.OpCreating}, true, nil
		}
		return session.InstanceData{ID: "sess-1", Title: "mine"}, true, nil
	})
	stubCurrentTmuxName(t, func() (string, error) {
		t.Fatal("a bound caller must not consult tmux while its create commits")
		return "", nil
	})
	prev := selfResolveSleep
	selfResolveSleep = func(time.Duration) {}
	t.Cleanup(func() { selfResolveSleep = prev })

	got, err := resolveSelfSession()
	require.NoError(t, err)
	require.Equal(t, "sess-1", got.ID)
	require.Empty(t, got.InFlightOp, "the committed row is what acting verbs get")
	require.Equal(t, 2, calls)
}

// A row still creating when the window closes reports exactly that — the
// session is on its way, not gone — and never a tmux fallthrough.
func TestResolveSelfSessionStillCreatingAtWindowCloseFailsClearly(t *testing.T) {
	t.Setenv("AF_DAEMON_TOKEN", "tok")
	stubCallerSession(t, func(context.Context) (session.InstanceData, bool, error) {
		return session.InstanceData{ID: "sess-1", Title: "mine", InFlightOp: session.OpCreating}, true, nil
	})
	stubCurrentTmuxName(t, func() (string, error) {
		t.Fatal("a still-creating bound caller must fail closed, not consult tmux")
		return "", nil
	})
	prev := selfResolveSleep
	selfResolveSleep = func(time.Duration) {}
	t.Cleanup(func() { selfResolveSleep = prev })

	_, err := resolveSelfSession()
	require.ErrorIs(t, err, errBoundCaller)
	require.ErrorContains(t, err, "still being created")
}

// --- CallerGone: the daemon's gone answer fails fast (#6690) -----------------
//
// A bound caller whose session the daemon reports as gone (SnapshotResponse
// .CallerGone — killed, removed, or lost with a create that never committed)
// is not the registration race: the row will never land. The poll fails closed
// on the FIRST answer with the accurate reason — on both targets — instead of
// polling out the window or reaching for a tmux cue. Pinned on the LOCAL path
// deliberately: even the path that normally falls through must not fall
// through a bound answer.
func TestResolveSelfSessionCallerGoneFailsClosed(t *testing.T) {
	t.Setenv("AF_DAEMON_TOKEN", "tok")
	calls := 0
	stubCallerSession(t, func(context.Context) (session.InstanceData, bool, error) {
		calls++
		return session.InstanceData{}, false, apiclient.ErrCallerSessionGone
	})
	stubCurrentTmuxName(t, func() (string, error) {
		t.Fatal("a gone bound caller must fail closed, not consult tmux")
		return "", nil
	})

	_, err := resolveSelfSession()
	require.ErrorIs(t, err, errBoundCaller)
	require.ErrorIs(t, err, apiclient.ErrCallerSessionGone)
	require.Equal(t, 1, calls, "gone is not retried — the row will never land")
}

// resolveCredentialSelf treats the same gone answer as a failure, not as the
// operator-credential refusal — the credential DID name a session; the daemon
// says that session is gone.
func TestResolveCredentialSelfCallerGoneFailsClosed(t *testing.T) {
	t.Setenv("AF_DAEMON_TOKEN", "tok")
	remoteTarget(t)
	calls := 0
	stubCallerSession(t, func(context.Context) (session.InstanceData, bool, error) {
		calls++
		return session.InstanceData{}, false, apiclient.ErrCallerSessionGone
	})

	_, err := resolveCredentialSelf()
	require.ErrorIs(t, err, errBoundCaller)
	require.ErrorIs(t, err, apiclient.ErrCallerSessionGone)
	require.NotContains(t, err.Error(), "needs a sandbox credential",
		"a gone bound caller is not the operator-credential refusal")
	require.Equal(t, 1, calls)
}

// --- credentialBoundCaller (remote create's --repo gate) ---------------------

// The --repo gate rides the same retry: a bound caller mid-registration still
// answers true — a single racy snapshot must not produce a "--repo is required"
// refusal a sandbox credential cannot answer.
func TestCredentialBoundCallerRetriesTheRace(t *testing.T) {
	t.Setenv("AF_DAEMON_TOKEN", "tok")
	remoteTarget(t)
	calls := 0
	stubCallerSession(t, func(context.Context) (session.InstanceData, bool, error) {
		calls++
		if calls < 2 {
			return session.InstanceData{}, false, apiclient.ErrCallerRowNotRegistered
		}
		return session.InstanceData{ID: "s1", Title: "mine"}, true, nil
	})
	prev := selfResolveSleep
	selfResolveSleep = func(time.Duration) {}
	t.Cleanup(func() { selfResolveSleep = prev })

	require.True(t, credentialBoundCaller())
	require.Equal(t, 2, calls, "the gate retried through the registration race")
}

// The --repo gate fails closed exactly as resolveSelfSession does: a poll that
// does not end in a clean bound answer leaves the binding UNPROVEN, so the
// caller keeps its "name the project explicitly" rule rather than act on an
// inconsistent answer — bound-then-none, a window exhausted mid-registration,
// and a session the daemon reports gone all answer false.
func TestCredentialBoundCallerInconsistentAnswersFailClosed(t *testing.T) {
	t.Setenv("AF_DAEMON_TOKEN", "tok")
	remoteTarget(t)
	prev := selfResolveSleep
	selfResolveSleep = func(time.Duration) {}
	t.Cleanup(func() { selfResolveSleep = prev })

	for name, fn := range map[string]func(context.Context) (session.InstanceData, bool, error){
		"window exhausted mid-registration": func(context.Context) (session.InstanceData, bool, error) {
			return session.InstanceData{}, false, apiclient.ErrCallerRowNotRegistered
		},
		"bound, then none": func() func(context.Context) (session.InstanceData, bool, error) {
			calls := 0
			return func(context.Context) (session.InstanceData, bool, error) {
				calls++
				if calls == 1 {
					return session.InstanceData{}, false, apiclient.ErrCallerRowNotRegistered
				}
				return session.InstanceData{}, false, nil
			}
		}(),
		"bound caller gone": func(context.Context) (session.InstanceData, bool, error) {
			return session.InstanceData{}, false, apiclient.ErrCallerSessionGone
		},
	} {
		t.Run(name, func(t *testing.T) {
			stubCallerSession(t, fn)
			require.False(t, credentialBoundCaller())
		})
	}
}

// An operator credential (no bound caller) and a lookup that never got a bound
// answer keep the "name the project explicitly" rule.
func TestCredentialBoundCallerOperatorAndUnreachableStayFalse(t *testing.T) {
	t.Setenv("AF_DAEMON_TOKEN", "tok")
	remoteTarget(t)
	stubCallerSession(t, func(context.Context) (session.InstanceData, bool, error) {
		return session.InstanceData{}, false, nil
	})
	require.False(t, credentialBoundCaller())

	stubCallerSession(t, func(context.Context) (session.InstanceData, bool, error) {
		return session.InstanceData{}, false, errors.New("dial tcp: connection refused")
	})
	require.False(t, credentialBoundCaller(), "a lookup that never answered cannot vouch for the binding")
}

// --- The REAL client against a fake daemon -----------------------------------

// callerRaceServer is a fake remote daemon that answers /v1/Snapshot exactly as
// the real one does during the registration race: the credential is bound from
// the first answer (Caller set) while the row it names registers only later —
// no rows, then a sibling only, then the sibling and the caller together. It
// records the bearer token of every request so the test can prove the same
// credential rode each retry.
type callerRaceServer struct {
	srv    *httptest.Server
	mu     sync.Mutex
	calls  int
	tokens []string
}

func newCallerRaceServer(t *testing.T) *callerRaceServer {
	t.Helper()
	s := &callerRaceServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if r.URL.Path != "/v1/Snapshot" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		s.calls++
		s.tokens = append(s.tokens, agentproto.BearerToken(r.Header.Get(agentproto.AuthHeader)))
		caller := session.InstanceData{ID: "caller-id", Title: "me"}
		sibling := session.InstanceData{ID: "sibling-id", Title: "sib"}
		resp := daemon.SnapshotResponse{Caller: caller.ID}
		switch s.calls {
		case 1: // bound, but the caller's row is not yet registered
		case 2:
			// A sibling's row lands first — the projection still lacks the
			// caller's own row.
			resp.Instances = []session.InstanceData{sibling}
		default:
			resp.Instances = []session.InstanceData{sibling, caller}
		}
		_ = apiproto.WriteEnvelope(w, apiproto.Success(resp))
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// remoteFlags points the real target resolver at srv for the duration of a
// test, so the unstubbed callerSessionFn round-trips a real remote client.
func remoteFlags(t *testing.T, url, token string) {
	t.Helper()
	oldURL, oldTok := apiclient.FlagDaemonURL, apiclient.FlagDaemonToken
	apiclient.FlagDaemonURL, apiclient.FlagDaemonToken = url, token
	t.Cleanup(func() { apiclient.FlagDaemonURL, apiclient.FlagDaemonToken = oldURL, oldTok })
}

// The retry poll through the REAL client (callerSessionFn unstubbed): the
// caller's row lands on the third request of the race sequence, and the same
// token goes out on every request — the wiring the faked callerSessionFn tests
// above cannot cover.
func TestResolveCredentialSelfRealClientRetriesThroughRace(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	fake := newCallerRaceServer(t)
	remoteFlags(t, fake.srv.URL, "tok-abc")
	prev := selfResolveSleep
	selfResolveSleep = func(time.Duration) {}
	t.Cleanup(func() { selfResolveSleep = prev })

	got, err := resolveCredentialSelf()
	require.NoError(t, err)
	require.Equal(t, "caller-id", got.ID)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.Equal(t, 3, fake.calls, "the caller's row appears on the third request")
	require.Equal(t, []string{"tok-abc", "tok-abc", "tok-abc"}, fake.tokens,
		"the same token goes out on every request of the poll")
}

// A REAL 401 from the daemon surfaces through the real client — the credential
// is wrong or revoked, which is neither the registration race nor an operator
// answer, and must reach the caller as the daemon's own refusal after a single
// attempt rather than being retried or misreported.
func TestResolveCredentialSelfReal401Surfaces(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
		_ = apiproto.WriteEnvelope(w, apiproto.Failure("unauthorized"))
	}))
	t.Cleanup(srv.Close)
	remoteFlags(t, srv.URL, "bad-tok")

	_, err := resolveCredentialSelf()
	require.ErrorContains(t, err, "unauthorized")
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, 1, calls, "a 401 is not the registration race and is not retried")
}
