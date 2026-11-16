package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// registerStartedAgent mirrors registerStarted for a session whose pane does
// not run claude: registerStarted hardcodes Program "claude", and the program
// is what ResolvedAgent falls back to when no pane command is recorded —
// which is exactly the axis #7434 splits on (PublishesWorkingContent or not).
func registerStartedAgent(t *testing.T, m *Manager, repoID, repoPath, title, program string) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: repoPath, Program: program})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	inst.SetStatusForTest(session.Running)
	seedDiskInstance(t, repoID, title, repoPath)
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, title)] = inst
	m.mu.Unlock()
	return inst
}

// A viewer's resize reflows the pane; the poll reads the changed capture as
// churn and, for an agent with no working indicator, that churn was the ONLY
// work evidence — so the Running edge answered and cleared a live attention
// request before the user ever saw it (#7434). The reflow mark must hold the
// repaint out of the answer while still reporting Running to every other
// consumer.
func TestAttentionSurvivesViewerReflowForNoIndicatorAgents(t *testing.T) {
	for _, tc := range []struct {
		name    string
		program string
	}{
		// codex and aider are the agents the issue names; the custom program
		// is every other no-indicator pane — ResolvedAgent "" publishes no
		// working content either.
		{"codex", "codex"},
		{"aider", "aider"},
		{"a custom program", "my-custom-agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, repoID, repoPath := newStatusTestManager(t)
			inst := registerStartedAgent(t, manager, repoID, repoPath, "asker", tc.program)

			inst.SetStatusForTest(session.Ready)
			_, err := inst.SetAttention("pick A or B", time.Unix(100, 0))
			require.NoError(t, err)
			require.True(t, inst.Attention().SeenIdle,
				"premise: at Ready the request inherits the satisfied idle spell, so the next Running edge would answer it")

			_, epoch := inst.InFlightOpAndEpoch()
			manager.settleChurnedLiveness(inst, true, "the pane repainted at the viewer's new width", epoch, true)
			require.Equal(t, session.LiveRunning, inst.GetLiveness(),
				"reflow churn still reports Running — no status or readiness detector may read the agent idle")
			require.True(t, inst.HasAttention(),
				"a repaint caused by viewing must not answer the request (#7434)")
		})
	}
}

// The control half: the same changed capture without the reflow mark is the
// agent's real output, and a Running edge after the idle spell remains the
// request's answer exactly as before — the mechanism must not buy the
// protection by never clearing.
func TestRealChurnStillAnswersAttentionForNoIndicatorAgents(t *testing.T) {
	for _, tc := range []struct {
		name    string
		program string
	}{
		{"codex", "codex"},
		{"aider", "aider"},
		{"a custom program", "my-custom-agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, repoID, repoPath := newStatusTestManager(t)
			inst := registerStartedAgent(t, manager, repoID, repoPath, "asker", tc.program)

			inst.SetStatusForTest(session.Ready)
			_, err := inst.SetAttention("pick A or B", time.Unix(100, 0))
			require.NoError(t, err)

			_, epoch := inst.InFlightOpAndEpoch()
			manager.settleChurnedLiveness(inst, true, "real agent output", epoch, false)
			require.False(t, inst.HasAttention(),
				"genuine output after the idle spell still answers the request")
		})
	}
}

// The #7444 gap #7434's protection opened: a reflow moves liveness Ready →
// Running while the request stands, and the agent's REAL output arriving while
// liveness is already Running is a Running → Running "transition" — no liveness
// edge, so the request is never evaluated and stays displayed through a real
// work turn. Genuine churn must answer through the explicit work-evidence path
// even when there is no transition to carry it.
func TestGenuineChurnAfterReflowAnswersAttentionWhileRunning(t *testing.T) {
	for _, tc := range []struct {
		name    string
		program string
	}{
		{"codex", "codex"},
		{"aider", "aider"},
		{"a custom program", "my-custom-agent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			manager, repoID, repoPath := newStatusTestManager(t)
			inst := registerStartedAgent(t, manager, repoID, repoPath, "asker", tc.program)

			var pending []func()
			prev := runAttentionClear
			runAttentionClear = func(clear func()) { pending = append(pending, clear) }
			t.Cleanup(func() { runAttentionClear = prev })

			inst.SetStatusForTest(session.Ready)
			_, err := inst.SetAttention("pick A or B", time.Unix(100, 0))
			require.NoError(t, err)

			_, epoch := inst.InFlightOpAndEpoch()
			manager.settleChurnedLiveness(inst, true, "the pane repainted at the viewer's new width", epoch, true)
			require.Equal(t, session.LiveRunning, inst.GetLiveness())
			require.True(t, inst.HasAttention(),
				"premise: the reflow kept the request while liveness moved to Running (#7434)")

			// Real output with no idle poll in between — the Running → Running
			// tick that used to be a dead end for the request.
			_, epoch = inst.InFlightOpAndEpoch()
			manager.settleChurnedLiveness(inst, true, "real agent output", epoch, false)
			require.Len(t, pending, 1,
				"genuine output must answer a qualifying request whatever the current liveness (#7444)")
			pending[0]()
			require.False(t, inst.HasAttention())
		})
	}
}

// An agent WITH a working indicator changes nothing: the reflow mark does not
// touch its path — a non-working frame still survives by the composer rule,
// and a frame carrying the in-turn indicator still answers it, resize or not.
func TestViewerReflowDoesNotChangeIndicatorAgentAnswer(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := startedLocalTabInstance(t, manager, repoID, repoPath, "asker", "af_attention_reflow_indicator")
	require.Equal(t, "claude", inst.ResolvedAgent(), "premise: the session runs an indicator agent")

	var pending []func()
	prev := runAttentionClear
	runAttentionClear = func(clear func()) { pending = append(pending, clear) }
	t.Cleanup(func() { runAttentionClear = prev })

	inst.SetStatusForTest(session.Ready)
	_, err := inst.SetAttention("pick A or B", time.Unix(100, 0))
	require.NoError(t, err)

	_, epoch := inst.InFlightOpAndEpoch()
	manager.settleChurnedLiveness(inst, true, fixturePane(t, "claude_idle_draft.ansi"), epoch, true)
	require.True(t, inst.HasAttention(),
		"a reflow-marked non-working frame is as silent as any other churn for an indicator agent")
	require.Empty(t, pending)

	inst.SetStatusForTest(session.Ready)
	_, epoch = inst.InFlightOpAndEpoch()
	manager.settleChurnedLiveness(inst, true, fixturePane(t, "claude_working_tool.ansi"), epoch, true)
	require.Len(t, pending, 1,
		"the reflow mark must not suppress the agent's own in-turn indicator — positive work evidence still answers")
	pending[0]()
	require.False(t, inst.HasAttention())
}
