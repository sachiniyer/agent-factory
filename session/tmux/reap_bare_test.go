package tmux

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// The bare-pane proof differs from an incarnation's in one respect: heritage
// is the marker set itself — AF_SESSION and AF_HOME matching, AF_SESSION_GEN
// present, and AF_INSTANCE_ID absent. The guarded kill, the re-confirmations,
// and the leave-alone-on-doubt posture are reapprovenSession's, shared with
// ReapProvenIncarnation and not re-tested here.
const (
	bareName = "af_af-loginx-codex-776f726b"
	bareHome = "/home/u/.agent-factory"
)

func bareEnv() map[string]string {
	return map[string]string{
		EnvMarkerSession:    bareName,
		EnvMarkerHome:       bareHome,
		EnvMarkerGeneration: "gen-7428",
	}
}

func newBareServer(t *testing.T, env map[string]string) *fakeTmuxServer {
	t.Helper()
	return &fakeTmuxServer{t: t, sessions: []*fakeIncarnation{{
		id: "$9", name: bareName, pid: "5150", created: "1700000100", env: env,
	}}}
}

func bareProof() BareSessionProof {
	return BareSessionProof{SessionName: bareName, Home: bareHome}
}

// M1: a session whose own environment carries this home's marker set — and no
// instance id — is a bare pane this daemon spawned, and is killed by ONE
// guarded server-side command aimed at its probed incarnation id.
func TestReapProvenBareSessionReapsAProvenOrphan(t *testing.T) {
	srv := newBareServer(t, bareEnv())
	rec := stubIncarnationProcesses(t, srv)
	got := ReapProvenBareSession(srv.executor(), "", bareProof())

	require.True(t, got.Stopped, "why: %s", got.Why)
	require.Equal(t, []string{"kill-session -t $9"}, srv.kills(),
		"the kill must target the probed incarnation id, and nothing else")
	guarded := srv.guardedKills()
	require.Len(t, guarded, 1, "the kill must be exactly one guarded server-side command")
	require.Contains(t, guarded[0], "if-shell -F -t $9 #{&&:#{==:#{session_id},$9},#{&&:#{==:#{pid},5150},#{==:#{session_created},1700000100}}}",
		"the guard must compare the session id, server pid and creation stamp in the server")
	require.False(t, srv.has(bareName))
	require.Equal(t, 1, rec.reapCallCount)
	require.Equal(t, 1, rec.killsAtReap, "the captured processes are reaped only after the kill fired")
}

// Every case below leaves the session ALONE, asserted on the recorded argv:
// no kill-session of any target was issued, and no process was signaled.
func TestReapProvenBareSessionLeavesUnprovenSessionsAlone(t *testing.T) {
	without := func(drop string) map[string]string {
		env := bareEnv()
		delete(env, drop)
		return env
	}
	with := func(key, value string) map[string]string {
		env := bareEnv()
		env[key] = value
		return env
	}
	cases := []struct {
		name string
		env  map[string]string
		mod  func(*fakeTmuxServer)
	}{
		// M4: no markers at all — a user's own same-named session, a pre-marker
		// build's pane, or anything tmux < 3.2 could not stamp.
		{name: "no markers at all", env: map[string]string{}},
		// The name stamped into the environment is the identity claim; a
		// mismatch means this session was not spawned under the listed name.
		{name: "a different AF_SESSION", env: with(EnvMarkerSession, "af_other")},
		{name: "no AF_SESSION", env: without(EnvMarkerSession)},
		// M2: a same-named session stamped by ANOTHER agent-factory home.
		{name: "a different install's home", env: with(EnvMarkerHome, "/other/home")},
		{name: "no home marker", env: without(EnvMarkerHome)},
		// Pre-marker-set heritage: the generation stamp is what distinguishes a
		// post-marker binary's pane from a same-named session that predates it.
		{name: "no generation marker", env: without(EnvMarkerGeneration)},
		// M6: an instance create — the pending-creates sweep's class, not this
		// one's. The id is the bare-pane discriminator.
		{name: "carries an instance id", env: with(EnvMarkerInstance, "inst-6258-a")},
		{name: "an environment that cannot be read", env: bareEnv(), mod: func(s *fakeTmuxServer) { s.unreadableEnv = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newBareServer(t, tc.env)
			if tc.mod != nil {
				tc.mod(srv)
			}
			rec := stubIncarnationProcesses(t, srv)
			got := ReapProvenBareSession(srv.executor(), "", bareProof())

			require.False(t, got.Stopped)
			require.NotEmpty(t, got.Why, "a refusal must say which condition kept the session alone")
			require.Empty(t, srv.kills(), "an unproven session must not be killed, by id or by name")
			require.Zero(t, rec.reapCallCount, "nothing captured may be signaled when the session was not reaped")
			require.True(t, srv.has(bareName))
		})
	}
}

// #6846's second round, enforced on the bare-pane membership read: tmux copies
// the first starter's environment into the server's GLOBAL environment, so an
// unrelated session can appear to carry another home's markers when a read is
// not session-scoped. A session whose markers exist ONLY in the global
// environment must be left alone, and no command may ask for `-g`.
func TestReapProvenBareSessionIgnoresTheGlobalEnvironment(t *testing.T) {
	srv := newBareServer(t, map[string]string{})
	srv.globalEnv = bareEnv()
	rec := stubIncarnationProcesses(t, srv)
	got := ReapProvenBareSession(srv.executor(), "", bareProof())

	require.False(t, got.Stopped)
	require.Empty(t, srv.kills(), "global markers are no one's membership proof")
	require.Zero(t, rec.reapCallCount)
	for _, issued := range srv.issued() {
		require.NotContains(t, issued, "-g", "every marker read must be session-scoped: %q", issued)
	}
}

// The same property the incarnation tests enforce: when the SERVER evaluates
// the guarded kill, the target must still be the verified incarnation. A
// server replaced between the client's last check and the kill — reissuing
// "$9" — is not killed, and nothing captured is reaped.
func TestReapProvenBareSessionDoesNothingWhenTheServerChangesBeforeTheKill(t *testing.T) {
	replaceServer := func(s *fakeTmuxServer) {
		s.sessions[0].pid = "9999"
		s.sessions[0].created = "1700000555"
	}
	cases := map[string]string{
		// After the client's re-confirmation: only the guard can catch it.
		"after the re-confirmation": "display-message -p -t $9 #{session_id} #{pid} #{session_created}",
		// After the pane capture, immediately before the kill.
		"after the pane capture": "list-panes -s -t $9",
	}
	for name, trigger := range cases {
		t.Run(name, func(t *testing.T) {
			srv := newBareServer(t, bareEnv())
			rec := stubIncarnationProcesses(t, srv)
			srv.after = map[string]func(*fakeTmuxServer){trigger: replaceServer}
			got := ReapProvenBareSession(srv.executor(), "", bareProof())

			require.False(t, got.Stopped)
			require.Contains(t, got.Why, "replaced")
			require.Empty(t, srv.kills(), "the guard must stop the kill on a replaced server")
			require.True(t, srv.has(bareName), "the session at the reissued id survives")
			require.Zero(t, rec.reapCallCount, "nothing captured may be reaped when the kill did not fire")
			require.Len(t, srv.guardedKills(), 1, "the guard itself was evaluated by the server")
		})
	}
}

func TestReapProvenBareSessionRequiresEveryProofField(t *testing.T) {
	cases := map[string]BareSessionProof{
		"no session name": {Home: bareHome},
		"no home":         {SessionName: bareName},
		"neither":         {},
	}
	for name, proof := range cases {
		t.Run(name, func(t *testing.T) {
			srv := newBareServer(t, bareEnv())
			rec := stubIncarnationProcesses(t, srv)
			got := ReapProvenBareSession(srv.executor(), "", proof)

			require.False(t, got.Stopped)
			require.NotEmpty(t, got.Why)
			require.Empty(t, srv.issued(), "an unproven proof must not reach the server at all")
			require.Zero(t, rec.reapCallCount)
			require.True(t, srv.has(bareName))
		})
	}
}

// A session that does not answer under the listed name leaves nothing to
// prove — and nothing to kill.
func TestReapProvenBareSessionLeavesAVanishedSessionAlone(t *testing.T) {
	srv := newBareServer(t, bareEnv())
	srv.sessions[0].name = "af_other"
	rec := stubIncarnationProcesses(t, srv)
	got := ReapProvenBareSession(srv.executor(), "", bareProof())

	require.False(t, got.Stopped)
	require.NotEmpty(t, got.Why)
	require.Empty(t, srv.kills())
	require.Zero(t, rec.reapCallCount)
}
