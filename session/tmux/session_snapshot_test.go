package tmux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/cmd/cmd_test"
	"github.com/sachiniyer/agent-factory/internal/proctree"
)

// #6423: restoring N sessions asked tmux three questions each (has-session,
// display-message for the generation, list-panes for the pane rows). A primed
// SessionSnapshot answers all three for every session it contains from ONE
// `list-panes -a`, so the per-session tmux calls are gone — and only for a
// positive answer: a session the snapshot lacks, or a stale snapshot, still asks.

// countingTmux is an executor that fails every command and counts them, so a
// probe that reaches tmux is both visible and unable to succeed by accident.
func countingTmux(calls *atomic.Int32) cmd_test.MockCmdExec {
	fail := func(c *exec.Cmd) ([]byte, error) {
		calls.Add(1)
		return nil, errors.New("unexpected tmux call: " + fmt.Sprint(c.Args))
	}
	return cmd_test.MockCmdExec{
		RunFunc:    func(c *exec.Cmd) error { _, err := fail(c); return err },
		OutputFunc: fail,
	}
}

// snapshotOutput is what `list-panes -a -F snapshotRowFormat` prints for two
// sessions, one of which has two panes and whose root pid is this test process.
func snapshotOutput(t *testing.T) (out string, pid int, startID uint64) {
	t.Helper()
	pid = os.Getpid()
	proc, err := proctree.Lookup(pid)
	require.NoError(t, err)
	row := func(p int) string { return fmt.Sprintf("%d|0||||/dev/pts/9|1", p) }
	out = "af_one\t$1\t4242\t1700000001\t" + row(pid) + "\n" +
		"af_one\t$1\t4242\t1700000001\t" + row(pid+100000) + "\n" +
		"af_two\t$2\t4242\t1700000002\t" + row(pid+1) + "\n" +
		"malformed line without the tabs\n"
	return out, pid, proc.StartID
}

func TestPrimedSnapshotAnswersTheRestoreProbesWithoutTmuxCalls(t *testing.T) {
	out, pid, startID := snapshotOutput(t)
	release := PrimeSessionSnapshot(parseSessionSnapshot(out, time.Now()))
	defer release()

	var calls atomic.Int32
	ts := NewTmuxSessionFromSanitizedNameWithDeps("af_one", "sh", MakePtyFactory(), countingTmux(&calls))

	exists, answered, err := ts.restoreExistence()
	require.NoError(t, err)
	require.True(t, exists)
	require.True(t, answered, "a present session is a determinate answer, not unknown")

	gen, ok := ts.confirmedGeneration()
	require.True(t, ok)
	require.NotNil(t, gen)
	require.Equal(t, tmuxGeneration{sessionID: "$1", serverPID: "4242", created: "1700000001"}, *gen)

	same, err := ts.PaneRootProcessMatches(pid, startID)
	require.NoError(t, err)
	require.True(t, same, "the recorded root is one of the session's panes")
	same, err = ts.PaneRootProcessMatches(pid+7, startID)
	require.NoError(t, err)
	require.False(t, same, "a pid no pane carries is still a no-match")

	require.Zero(t, calls.Load(), "the per-session has-session / display-message / list-panes calls must be gone")
}

func TestPrimedSnapshotGenerationIsAPrivateCopy(t *testing.T) {
	out, _, _ := snapshotOutput(t)
	release := PrimeSessionSnapshot(parseSessionSnapshot(out, time.Now()))
	defer release()
	ts := NewTmuxSessionFromSanitizedNameWithDeps("af_one", "sh", MakePtyFactory(), countingTmux(new(atomic.Int32)))

	a, _ := ts.confirmedGeneration()
	a.teardownInitiated = true // per-generation mutable state must not leak into the shared snapshot
	b, _ := ts.confirmedGeneration()
	require.False(t, b.teardownInitiated)
}

func TestSnapshotNeverAnswersForASessionItLacksOrOnceStale(t *testing.T) {
	out, _, _ := snapshotOutput(t)

	t.Run("absent session still asks tmux", func(t *testing.T) {
		release := PrimeSessionSnapshot(parseSessionSnapshot(out, time.Now()))
		defer release()
		var calls atomic.Int32
		ts := NewTmuxSessionFromSanitizedNameWithDeps("af_missing", "sh", MakePtyFactory(), countingTmux(&calls))
		_, _, _ = ts.restoreExistence()
		require.NotZero(t, calls.Load(), "absence from the snapshot is not proof the session is gone")
	})
	t.Run("stale snapshot still asks tmux", func(t *testing.T) {
		release := PrimeSessionSnapshot(parseSessionSnapshot(out, time.Now().Add(-2*snapshotMaxAge)))
		defer release()
		var calls atomic.Int32
		ts := NewTmuxSessionFromSanitizedNameWithDeps("af_one", "sh", MakePtyFactory(), countingTmux(&calls))
		_, _, _ = ts.restoreExistence()
		require.NotZero(t, calls.Load())
	})
	t.Run("released snapshot still asks tmux", func(t *testing.T) {
		release := PrimeSessionSnapshot(parseSessionSnapshot(out, time.Now()))
		release()
		var calls atomic.Int32
		ts := NewTmuxSessionFromSanitizedNameWithDeps("af_one", "sh", MakePtyFactory(), countingTmux(&calls))
		_, _, _ = ts.restoreExistence()
		require.NotZero(t, calls.Load())
	})
}

func TestParseSessionSnapshotKeepsEachSessionsPanesAndDropsMalformedLines(t *testing.T) {
	out, pid, _ := snapshotOutput(t)
	s := parseSessionSnapshot(out, time.Now())
	require.Len(t, s.sessions, 2, "the malformed line must not create a session")
	rows, ok := s.paneRows("af_one")
	require.True(t, ok)
	require.Len(t, rows, 2)
	require.Equal(t, fmt.Sprintf("%d|0||||/dev/pts/9|1", pid), rows[0])
	require.False(t, s.has("af_three"))
}
