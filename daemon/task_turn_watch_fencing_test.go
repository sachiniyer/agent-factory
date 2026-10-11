package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// A frame captured BEFORE a redelivery's boundary must not reach the new
// window's chrome watcher (#5221 review): the helper reads the boundary at
// feed time, so a send landing between SnapshotAgent and the feed would
// otherwise arm a fresh watcher off a pane that never saw the new prompt —
// for scoped agents an indicator-absent capture primes the absent→present
// contract, after which unrelated pre-boundary chrome can pass as the
// redelivery's turn and release the held idle edge early.
func TestNoteTaskRunTurnEvidenceDropsPreBoundaryCapture(t *testing.T) {
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "turn-fence", Path: t.TempDir(), Program: "codex", TaskID: "task-1",
	})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)
	require.True(t, inst.TaskRunActive(), "precondition: a task-spawned session starts with its run in flight")

	boundary := time.Now()
	require.True(t, inst.RecordPromptAttempt(session.PromptSentUnverified, boundary))
	_, epoch := inst.InFlightOpAndEpoch()

	m := &Manager{taskRunTurnWatches: map[string]*taskRunTurnWatchEntry{}}
	const key = "turn-fence"

	// The capture started before the attempt: whatever its content, it is not
	// this window's evidence and must not create or mutate a watcher.
	quiet := "\x1b[2J\x1b[H› \r\n\r\n"
	require.False(t, m.noteTaskRunTurnEvidence(key, inst, quiet, boundary.Add(-time.Millisecond), epoch))
	_, watched := m.taskRunTurnWatches[key]
	require.False(t, watched, "a pre-boundary capture must not arm the new window's watcher")

	// A post-boundary capture feeds normally — the quiet frame arms the
	// scoped agent's absent→present contract.
	require.False(t, m.noteTaskRunTurnEvidence(key, inst, quiet, boundary.Add(time.Millisecond), epoch))
	_, watched = m.taskRunTurnWatches[key]
	require.True(t, watched, "a post-boundary capture feeds the watcher")
}

// A successful but EMPTY capture is a real frame for the scoped agents — the
// definitive indicator-absent observation that arms the absent→present
// contract (#5221 review). Dropping it at the helper's entrance would leave
// the watch blind across a repaint.
func TestNoteTaskRunTurnEvidenceFeedsEmptyFrame(t *testing.T) {
	inst, err := session.NewInstance(session.InstanceOptions{
		Title: "turn-empty", Path: t.TempDir(), Program: "codex", TaskID: "task-1",
	})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	inst.SetStartedForTest(true)

	boundary := time.Now()
	require.True(t, inst.RecordPromptAttempt(session.PromptSentUnverified, boundary))
	_, epoch := inst.InFlightOpAndEpoch()

	m := &Manager{taskRunTurnWatches: map[string]*taskRunTurnWatchEntry{}}
	require.False(t, m.noteTaskRunTurnEvidence("turn-empty", inst, "", boundary.Add(time.Millisecond), epoch))
	_, watched := m.taskRunTurnWatches["turn-empty"]
	require.True(t, watched, "an empty successful capture still arms the scoped watcher's absence")
}
