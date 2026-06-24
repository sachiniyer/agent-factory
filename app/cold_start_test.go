package app

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
)

// errDaemonStarting mimics the daemon's typed "up but still restoring sessions"
// error (#829) so the cold-start warm-up retry is exercised without a real
// daemon. The literal must contain the substring daemon.IsDaemonStartingErr
// matches on; the daemon package's own warmup_test guards the constant itself.
func errDaemonStarting() error {
	return errors.New("agent-factory daemon is starting (restoring sessions); retry shortly")
}

func init() {
	// Sanity: our fake must actually be recognized as a warming-daemon error,
	// otherwise the retry tests would silently treat it as a hard failure.
	if !daemon.IsDaemonStartingErr(errDaemonStarting()) {
		panic("errDaemonStarting() not recognized by daemon.IsDaemonStartingErr")
	}
}

// TestColdStartFromSnapshot_PopulatesSidebar proves the TUI builds its sidebar
// from the daemon's Snapshot at startup (#960 PR 6) — the instances.json disk
// read is gone, the daemon is the source of truth.
func TestColdStartFromSnapshot_PopulatesSidebar(t *testing.T) {
	h := newTestHome(t)
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return newSnapshotTestInstance(t, d.Title), nil
	}))

	h.snapshotFetcher = func(repoID string) ([]session.InstanceData, error) {
		require.Equal(t, h.repoID, repoID, "cold start must fetch the TUI's repo scope")
		return []session.InstanceData{{Title: "alpha"}, {Title: "beta"}}, nil
	}

	require.NoError(t, h.coldStartFromSnapshot())
	require.NotNil(t, findSidebarInstance(h, "alpha"), "snapshot session must be in the sidebar")
	require.NotNil(t, findSidebarInstance(h, "beta"), "snapshot session must be in the sidebar")
}

// TestColdStartFromSnapshot_WaitsOutWarmingDaemon proves the warm-up retry path:
// while the daemon reports "still restoring" (#829) the cold start retries rather
// than rendering an empty sidebar (which looked like a fresh install, #766/#868),
// then populates once the daemon answers.
func TestColdStartFromSnapshot_WaitsOutWarmingDaemon(t *testing.T) {
	h := newTestHome(t)
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return newSnapshotTestInstance(t, d.Title), nil
	}))

	// No real sleeps between retries.
	prevPoll := coldStartWarmupPoll
	coldStartWarmupPoll = 0
	defer func() { coldStartWarmupPoll = prevPoll }()

	calls := 0
	h.snapshotFetcher = func(string) ([]session.InstanceData, error) {
		calls++
		if calls < 3 {
			return nil, errDaemonStarting()
		}
		return []session.InstanceData{{Title: "restored"}}, nil
	}

	require.NoError(t, h.coldStartFromSnapshot())
	require.Equal(t, 3, calls, "cold start must retry while the daemon is warming")
	require.NotNil(t, findSidebarInstance(h, "restored"),
		"the session must appear once the warming daemon answers")
}

// TestColdStartFromSnapshot_HardErrorAborts proves a non-warming daemon failure
// is surfaced (newHome exits on it) rather than swallowed — there is no
// standalone disk-read fallback anymore (#960 PR 6 dropped no-daemon mode).
func TestColdStartFromSnapshot_HardErrorAborts(t *testing.T) {
	h := newTestHome(t)

	h.snapshotFetcher = func(string) ([]session.InstanceData, error) {
		return nil, errors.New("connection refused")
	}

	err := h.coldStartFromSnapshot()
	require.Error(t, err, "a hard daemon failure must abort cold start, not fall back to disk")
	require.Empty(t, h.sidebar.GetInstances(), "no sidebar rows on a failed cold start")
}
