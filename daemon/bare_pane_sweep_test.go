package daemon

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/accountlogin"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The sweep decides which LISTED sessions are ever asked for their markers;
// the marker set itself is proven inside tmux.ReapProvenBareSession, whose own
// tests (session/tmux/reap_bare_test.go) cover every accept and refusal. These
// tests pin the sweep-side membership rules: a candidate is reaped, and a
// claimed, foreign-shaped, or unanswerable listing never reaches the reap.
type bareSweepSpy struct {
	reaped  []tmux.BareSessionProof
	reapWhy map[string]string
}

// stubBareSweep replaces the sweep's three seams: the listing answers names,
// the journal reads empty, and the reap records the proofs it was handed and
// reports success (or the per-name forced Why). Restored on cleanup.
func stubBareSweep(t *testing.T, names []string) *bareSweepSpy {
	t.Helper()
	spy := &bareSweepSpy{reapWhy: map[string]string{}}
	prevList, prevReap, prevMarkers := barePaneListSessions, barePaneReap, barePanePendingMarkers
	barePaneListSessions = func(cmd.Executor) ([]string, error) { return names, nil }
	barePaneReap = func(_ cmd.Executor, _ string, proof tmux.BareSessionProof) tmux.IncarnationReap {
		spy.reaped = append(spy.reaped, proof)
		if why := spy.reapWhy[proof.SessionName]; why != "" {
			return tmux.IncarnationReap{Why: why}
		}
		return tmux.IncarnationReap{Stopped: true}
	}
	barePanePendingMarkers = func() ([]pendingCreateMarker, error) { return nil, nil }
	t.Cleanup(func() {
		barePaneListSessions, barePaneReap, barePanePendingMarkers = prevList, prevReap, prevMarkers
	})
	return spy
}

func newBareSweepManager(t *testing.T) *Manager {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	m, err := newManagerShell(config.DefaultConfig())
	require.NoError(t, err)
	return m
}

// M1: every name shaped like a bare pane this daemon could have spawned —
// a config agent and both login formats — reaches the reap with this home's
// proof, and only those names do.
func TestSweepOrphanBarePanesReapsOrphanedBarePanes(t *testing.T) {
	m := newBareSweepManager(t)
	spy := stubBareSweep(t, []string{
		"af_af-config-3",
		"af_af-loginx-codex-776f726b",
		"af_af-login-claude-work",
		// Not candidates: wrong shapes, other namespaces, a user's session.
		"af_af-config-x", "af_af-config-", "af_af-loginx-codex-", "af_af-loginx-codex-zz!",
		"af_af-login-codex", "af_af-login-", "af_af-loginxx-a-b", "af_repo_a1b2c3_mine", "af_", "bash",
	})
	m.sweepOrphanBarePanes()

	want := []string{"af_af-config-3", "af_af-loginx-codex-776f726b", "af_af-login-claude-work"}
	var got []string
	home, err := config.GetConfigDir()
	require.NoError(t, err)
	for _, proof := range spy.reaped {
		got = append(got, proof.SessionName)
		require.Equal(t, home, proof.Home)
	}
	require.Equal(t, want, got, "exactly the bare-pane-shaped names must reach the reap, in listing order")
}

// M3 (config agents): a name this daemon's own config-agent supervisor tracks
// is live-owned, and the sweep must never ask for it.
func TestSweepOrphanBarePanesSkipsDaemonOwnedConfigAgent(t *testing.T) {
	m := newBareSweepManager(t)
	m.configAgents.sessions["af_af-config-7"] = tmux.NewTmuxSessionFromSanitizedName("af_af-config-7", "")
	spy := stubBareSweep(t, []string{"af_af-config-7", "af_af-config-8"})
	m.sweepOrphanBarePanes()

	require.Len(t, spy.reaped, 1)
	require.Equal(t, "af_af-config-8", spy.reaped[0].SessionName)
}

// M3 (account logins): the same check consults the account-login supervisor,
// which keys its panes by account identity rather than tmux name.
func TestSweepOrphanBarePanesSkipsDaemonOwnedAccountLogin(t *testing.T) {
	m := newBareSweepManager(t)
	prev := barePaneAccountLoginOwns
	barePaneAccountLoginOwns = func(*accountlogin.Supervisor, string) bool { return true }
	t.Cleanup(func() { barePaneAccountLoginOwns = prev })
	spy := stubBareSweep(t, []string{"af_af-loginx-codex-776f726b"})
	m.sweepOrphanBarePanes()

	require.Empty(t, spy.reaped, "a pane this daemon's login supervisor tracks is never a sweep candidate")
}

// M5: a bare-shaped name a restored instance row claims is never touched —
// the user session titled like a login pane, or any row carrying the name.
func TestSweepOrphanBarePanesSkipsInstanceRowClaims(t *testing.T) {
	m := newBareSweepManager(t)
	// A user session titled like a login pane produces the same tmux name. The
	// restored row claims it through the unscoped title derivation, so the
	// sweep may not even ask for its markers.
	m.instances["repo1"] = &session.Instance{Title: "af-login-codex-work"}
	// A row whose tmux fields name the session outright.
	m.instances["repo2"] = &session.Instance{Title: "other"}
	// repo2 has no tmux fields and a non-matching title, so it claims nothing;
	// the direct claimReason test below covers TmuxName/Tabs-bearing rows.
	spy := stubBareSweep(t, []string{"af_af-login-codex-work", "af_af-loginx-codex-776f726b"})
	m.sweepOrphanBarePanes()

	require.Len(t, spy.reaped, 1)
	require.Equal(t, "af_af-loginx-codex-776f726b", spy.reaped[0].SessionName)

	// The row forms taskFireRowOwnsTmuxName covers — direct and tab-carried.
	for _, rows := range [][]session.InstanceData{
		{{TmuxName: "af_af-loginx-a-b1"}},
		{{Tabs: []session.TabData{{TmuxName: "af_af-loginx-a-b1"}}}},
	} {
		require.NotEmpty(t, m.barePaneClaimReason("af_af-loginx-a-b1", rows, nil),
			"a restored row carrying the name must claim it")
	}
	require.Empty(t, m.barePaneClaimReason("af_af-loginx-a-b1", nil, nil))
}

// A session the pending-create journal still holds is the unrecorded-create
// sweep's case, not this one's — even when its name is bare-shaped.
func TestSweepOrphanBarePanesSkipsPendingCreateClaims(t *testing.T) {
	m := newBareSweepManager(t)
	spy := stubBareSweep(t, []string{"af_af-config-2", "af_af-config-3"})
	prev := barePanePendingMarkers
	barePanePendingMarkers = func() ([]pendingCreateMarker, error) {
		return []pendingCreateMarker{{SessionName: "af_af-config-2"}}, nil
	}
	t.Cleanup(func() { barePanePendingMarkers = prev })
	m.sweepOrphanBarePanes()

	require.Len(t, spy.reaped, 1)
	require.Equal(t, "af_af-config-3", spy.reaped[0].SessionName)
}

// M7: a listing that does not answer leaves EVERYTHING untouched — "unknown"
// is never "ours".
func TestSweepOrphanBarePanesAbortsOnUnanswerableList(t *testing.T) {
	m := newBareSweepManager(t)
	spy := stubBareSweep(t, nil)
	prev := barePaneListSessions
	barePaneListSessions = func(cmd.Executor) ([]string, error) { return nil, errors.New("tmux wedged") }
	t.Cleanup(func() { barePaneListSessions = prev })
	m.sweepOrphanBarePanes()

	require.Empty(t, spy.reaped)
}

// A candidate the guarded reap cannot prove is left alone and logged, and the
// sweep continues to the next name.
func TestSweepOrphanBarePanesLeavesUnprovenCandidates(t *testing.T) {
	m := newBareSweepManager(t)
	spy := stubBareSweep(t, []string{"af_af-config-1", "af_af-config-2"})
	spy.reapWhy["af_af-config-1"] = "session carries a different AF_HOME"
	m.sweepOrphanBarePanes()

	require.Len(t, spy.reaped, 2, "a refusal on one name must not stop the sweep")
}

func TestBarePaneCandidateName(t *testing.T) {
	candidates := []string{
		"af_af-config-1", "af_af-config-42",
		"af_af-loginx-codex-776f726b", "af_af-loginx-claude-a1", "af_af-loginx-a-00",
		"af_af-login-codex-work", "af_af-login-a-b-c", "af_af-login-x-y_1",
	}
	for _, name := range candidates {
		require.True(t, barePaneCandidateName(name), "%q is a bare-pane shape", name)
	}
	notCandidates := []string{
		// Malformed bare shapes.
		"af_af-config-", "af_af-config-x", "af_af-config-1a",
		"af_af-loginx-", "af_af-loginx-codex", "af_af-loginx-codex-", "af_af-loginx-codex-zz!",
		"af_af-login-", "af_af-login-codex",
		// Other namespaces: instance sessions, user sessions, non-af names.
		"af_af-loginxx-a-b", "af_af-loginx", "af_af-login",
		"af_repo_a1b2c3_mine", "af_mysession", "af_", "bash", "tmux",
	}
	for _, name := range notCandidates {
		require.False(t, barePaneCandidateName(name), "%q is not a bare-pane shape", name)
	}
}
