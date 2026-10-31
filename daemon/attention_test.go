package daemon

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/session"
)

func TestIsTypedInputIgnoresTerminalReplies(t *testing.T) {
	cases := map[string]bool{
		"a":                        true,
		"\r":                       true,
		"\x03":                     true,
		"\x1b[I":                   false,
		"\x1b[O":                   false,
		"\x1b[12;1R":               false,
		"":                         false,
		"\x1b[200~yes\x1b[201~":    true,
		"\x1b[200~line one\nline ": true,
		"\x1b[200~":                false,
		"\x1b[200~\x1b[201~":       false,
	}
	for in, want := range cases {
		if got := isTypedInput([]byte(in)); got != want {
			t.Errorf("isTypedInput(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestClearAttentionOnInputDoesNotWaitForTheRepoStartLock(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	inst := startedLocalTabInstance(t, manager, repo.ID, repoPath, "asker", "af_attention_input")
	_, err = inst.SetAttention("pick one", time.Unix(100, 0))
	require.NoError(t, err)

	var pending []func()
	prev := runAttentionClear
	runAttentionClear = func(clear func()) { pending = append(pending, clear) }
	t.Cleanup(func() { runAttentionClear = prev })

	lock := manager.startLockForRepo(repo.ID)
	lock.Lock()
	manager.clearAttentionOnInput(inst)
	manager.clearAttentionOnInput(inst)
	lock.Unlock()
	require.Len(t, pending, 1, "a burst of keys starts one clear")

	_, err = inst.SetAttention("a newer question", time.Unix(200, 0))
	require.NoError(t, err)
	pending[0]()
	require.True(t, inst.HasAttention(), "a request set after the keystroke stays")

	manager.clearAttentionOnInput(inst)
	require.Len(t, pending, 2, "the finished clear no longer blocks the next one")
	pending[1]()
	require.False(t, inst.HasAttention())

	_, err = inst.SetAttention("C", time.Unix(300, 0))
	require.NoError(t, err)
	manager.clearAttentionOnInput(inst)
	_, err = inst.SetAttention("D", time.Unix(400, 0))
	require.NoError(t, err)
	manager.clearAttentionOnInput(inst)
	require.Len(t, pending, 3, "the key for D joins the clear already in flight")
	pending[2]()
	require.False(t, inst.HasAttention(), "the in-flight clear also clears the request the later key saw")
}

func TestIsSubmittedInputNeedsAnEnterOutsideAPaste(t *testing.T) {
	cases := map[string]bool{
		"h":                                    false,
		"\x7f":                                 false,
		"\x03":                                 false,
		"\x1b[I":                               false,
		"\r":                                   true,
		"\n":                                   true,
		"yes\r":                                true,
		"\x1b[200~line one\nline two\x1b[201~": false,
		"\x1b[200~line\x1b[201~\r":             true,
		"\x1b[200~unterminated\n":              false,
		"":                                     false,
	}
	for in, want := range cases {
		if got := isSubmittedInput([]byte(in)); got != want {
			t.Errorf("isSubmittedInput(%q) = %v, want %v", in, got, want)
		}
	}
}

// A bracketed paste can span input frames (the CLI attach reads stdin 32 bytes at
// a time), and its markers can be split across them: a newline inside the paste
// is not a submission (Codex P2 on #6300).
func TestSubmitDetectorFollowsAPasteAcrossFrames(t *testing.T) {
	frames := func(fs ...string) (got []bool) {
		d := &submitDetector{}
		for _, f := range fs {
			got = append(got, d.Submitted([]byte(f)))
		}
		return got
	}
	eq := func(got, want []bool) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range got {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}
	for name, tc := range map[string]struct {
		in   []string
		want []bool
	}{
		"paste start, then a continuation with a newline": {[]string{"\x1b[200~line one", "\nline two"}, []bool{false, false}},
		"the paste ends in a later frame, then Enter":     {[]string{"\x1b[200~a", "b\n", "c\x1b[201~", "\r"}, []bool{false, false, false, true}},
		"the end marker is split across frames":           {[]string{"\x1b[200~a\n", "\x1b[20", "1~", "\n"}, []bool{false, false, false, true}},
		"the start marker is split across frames":         {[]string{"\x1b[20", "0~x\ny", "\x1b[201~"}, []bool{false, false, false}},
		"typed text then Enter is a submission":           {[]string{"hello", "\r"}, []bool{false, true}},
		"a terminal reply is not a submission":            {[]string{"\x1b[I", "\r"}, []bool{false, true}},
	} {
		if got := frames(tc.in...); !eq(got, tc.want) {
			t.Errorf("%s: %q -> %v, want %v", name, tc.in, got, tc.want)
		}
	}
}

func fixturePane(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("../task/testdata/" + name)
	require.NoError(t, err)
	return string(raw)
}

// The end-to-end shape of the rule, on real Claude Code captures. A session that
// asked for the user sits idle; the user types a draft, which repaints the
// composer. The poll reads that as churn and moves the session to Running, but
// the request must survive it: only Claude's own in-turn indicator answers it.
func TestAttentionSurvivesTypingADraftIntoClaudesComposer(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	inst := startedLocalTabInstance(t, manager, repo.ID, repoPath, "asker", "af_attention_draft")
	require.Equal(t, "claude", inst.ResolvedAgent(), "premise: the session runs Claude")

	var pending []func()
	prev := runAttentionClear
	runAttentionClear = func(clear func()) { pending = append(pending, clear) }
	t.Cleanup(func() { runAttentionClear = prev })

	// Waiting at Ready when it asks: the next Running edge would be its answer.
	inst.SetStatusForTest(session.Ready)
	_, err = inst.SetAttention("pick A or B", time.Unix(100, 0))
	require.NoError(t, err)
	require.True(t, inst.Attention().SeenIdle, "premise: the old churn rule would clear on the next edge")

	settle := func(frame string) {
		_, epoch := inst.InFlightOpAndEpoch()
		manager.settleChurnedLiveness(inst, true, frame, epoch)
	}

	settle(fixturePane(t, "claude_idle_draft.ansi"))
	require.Equal(t, session.LiveRunning, inst.GetLiveness(), "the typed draft churns the pane, so the poll reads Running")
	require.True(t, inst.HasAttention(), "typing a draft must not answer the request")
	require.Empty(t, pending, "and nothing is queued to clear it")

	// Control: Claude actually working on a turn is the answer.
	inst.SetStatusForTest(session.Ready)
	settle(fixturePane(t, "claude_working_tool.ansi"))
	require.Len(t, pending, 1, "the agent's own indicator queues the keyed clear")
	pending[0]()
	require.False(t, inst.HasAttention())
}

// A poll captures a working frame and decides at an epoch; the agent raises a
// request before the poll settles it. The frame predates the request, so it must
// not queue a clear for it, even though a request raised at Ready inherits a
// satisfied idle spell. The settle runs the real call site (the transition
// rejects the stale observation and the attention check must too).
func TestStaleWorkingFrameCannotClearARequestRaisedAfterTheCapture(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	require.NoError(t, err)
	manager, err := NewManager(config.DefaultConfig())
	require.NoError(t, err)
	inst := startedLocalTabInstance(t, manager, repo.ID, repoPath, "asker", "af_attention_stale")
	require.Equal(t, "claude", inst.ResolvedAgent(), "premise: the session runs Claude")

	var pending []func()
	prev := runAttentionClear
	runAttentionClear = func(clear func()) { pending = append(pending, clear) }
	t.Cleanup(func() { runAttentionClear = prev })

	inst.SetStatusForTest(session.Ready)
	_, captureEpoch := inst.InFlightOpAndEpoch() // the poll captures the working frame here

	_, err = inst.SetAttention("pick A or B", time.Unix(100, 0)) // the agent asks after the capture
	require.NoError(t, err)
	require.True(t, inst.Attention().SeenIdle, "premise: the request inherits a satisfied idle spell")

	manager.settleChurnedLiveness(inst, true, fixturePane(t, "claude_working_tool.ansi"), captureEpoch)
	require.Empty(t, pending, "a frame from before the request queues no clear for it")
	require.True(t, inst.HasAttention())

	// Control: the same frame decided after the request answers it.
	inst.SetStatusForTest(session.Ready)
	_, epoch := inst.InFlightOpAndEpoch()
	manager.settleChurnedLiveness(inst, true, fixturePane(t, "claude_working_tool.ansi"), epoch)
	require.Len(t, pending, 1, "a frame observed after the request answers it")
	pending[0]()
	require.False(t, inst.HasAttention())
}
