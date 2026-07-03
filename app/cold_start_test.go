package app

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/ui"
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

// TestColdStartFromSnapshot_LaunchSelectionParity pins the launch selection
// state after a cold start, byte-for-byte with the pre-store TUI (#1024 PR 2
// review follow-up). The TUI has NEVER auto-selected the first restored
// instance at launch: the sidebar cursor starts on the Instances HEADER
// (ui/sidebar_test.go's TestSidebarInitialState pins the same on the pre-store
// Sidebar, and newHome issues no SetSelectedInstance/SelectInstance at
// startup), so no instance is bound to the workspace panes — the pre-store
// TabbedWindow.instance also started nil — and the active tab starts at 0.
// The panes bind on the first cursor move: Down lands on the first restored
// instance and selectionChanged binds it, exactly the old
// selectionChanged→TabbedWindow.SetInstance first-keypress behavior.
func TestColdStartFromSnapshot_LaunchSelectionParity(t *testing.T) {
	h := newTestHome(t)
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return newSnapshotTestInstance(t, d.Title), nil
	}))
	h.snapshotFetcher = func(string) ([]session.InstanceData, error) {
		return []session.InstanceData{{Title: "first"}, {Title: "second"}, {Title: "third"}}, nil
	}

	require.NoError(t, h.coldStartFromSnapshot())
	// The launch paint path must run cleanly with nothing bound yet.
	_ = h.selectionChanged()

	sel := h.sidebar.GetSelection()
	require.True(t, sel.IsHeader, "launch cursor rests on the Instances header, as before the store")
	require.Equal(t, ui.SectionInstances, sel.Kind)
	require.Nil(t, h.sidebar.GetSelectedInstance(), "no cursor-selected instance at launch")
	require.Nil(t, h.store.GetSelectedInstance(),
		"no instance bound to the workspace panes at launch (TabbedWindow.instance started nil pre-store)")
	require.Equal(t, 0, h.store.ActiveTab(), "active tab starts on the agent tab")

	// First keypress: Down moves onto the first restored instance and binds it.
	h.sidebar.Down()
	_ = h.selectionChanged()
	first := h.store.GetInstances()[0]
	require.Equal(t, "first", first.Title)
	require.Same(t, first, h.sidebar.GetSelectedInstance(),
		"the first Down must land on the first restored instance")
	require.Same(t, first, h.store.GetSelectedInstance(),
		"the store must bind the first instance to the panes, as SetInstance did pre-store")
	require.Equal(t, 0, h.store.ActiveTab())
}

// TestColdStartFromSnapshot_AutoOpensFirstInstancePane pins the startup
// auto-open on a cold start with restored sessions (#1088, #1099 play-test):
// the launch cursor rests on the Instances header (selection parity above), so
// the auto-open must fire from the header branch of selectionChanged — falling
// back to the first restored instance — not wait for the first cursor move.
// The selection itself stays untouched: the pane opens, the cursor stays on
// the header, nothing is bound to the pane verbs.
func TestColdStartFromSnapshot_AutoOpensFirstInstancePane(t *testing.T) {
	h := newTestHome(t)
	h.initialPaneOpened = false // newTestHome latches it off; this test IS the auto-open
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		return newSnapshotTestInstance(t, d.Title), nil
	}))
	h.snapshotFetcher = func(string) ([]session.InstanceData, error) {
		return []session.InstanceData{{Title: "first"}, {Title: "second"}}, nil
	}

	require.NoError(t, h.coldStartFromSnapshot())
	// The launch paint / preview tick runs selectionChanged with the cursor on
	// the Instances header.
	_ = h.selectionChanged()

	require.Equal(t, 1, h.store.NumOpenPanes(), "cold start with restored sessions must auto-open a pane")
	p := h.store.OpenPanes()[0]
	require.Equal(t, "first", p.Instance().Title, "the first restored instance's pane opens")
	require.Equal(t, 0, p.Tab(), "the agent tab opens")
	require.True(t, h.initialPaneOpened, "the once-per-run latch must be set")

	// Launch selection parity is preserved: the pane opened WITHOUT selecting.
	sel := h.sidebar.GetSelection()
	require.True(t, sel.IsHeader, "the cursor stays on the Instances header")
	require.Nil(t, h.store.GetSelectedInstance(), "no instance is bound to the pane verbs")

	// Re-entering (the 100ms preview tick) must not open more panes.
	_ = h.selectionChanged()
	require.Equal(t, 1, h.store.NumOpenPanes())
}

// TestColdStartFromSnapshot_AutoOpenWaitsOutTransientStatus pins the re-fire
// half of the cold-start auto-open: a restored instance still in a transient
// status is not opened, and the auto-open fires — without any cursor move —
// on a later selectionChanged (the preview tick re-enters every 100ms) once
// the instance leaves it.
func TestColdStartFromSnapshot_AutoOpenWaitsOutTransientStatus(t *testing.T) {
	h := newTestHome(t)
	h.initialPaneOpened = false
	t.Cleanup(SetInstanceBuilderForTest(func(d session.InstanceData) (*session.Instance, error) {
		inst := newSnapshotTestInstance(t, d.Title)
		inst.SetStatus(session.Loading)
		return inst, nil
	}))
	h.snapshotFetcher = func(string) ([]session.InstanceData, error) {
		return []session.InstanceData{{Title: "restoring"}}, nil
	}

	require.NoError(t, h.coldStartFromSnapshot())
	_ = h.selectionChanged()
	require.Equal(t, 0, h.store.NumOpenPanes(), "a Loading instance must not auto-open")
	require.False(t, h.initialPaneOpened, "the latch must stay unset so the auto-open can re-fire")

	// The instance finishes restoring; the next tick's selectionChanged opens it.
	h.store.GetInstances()[0].SetStatus(session.Running)
	_ = h.selectionChanged()
	require.Equal(t, 1, h.store.NumOpenPanes(), "the pane must auto-open once the instance leaves Loading")
	require.Equal(t, "restoring", h.store.OpenPanes()[0].Instance().Title)
}

// TestColdStartFromSnapshot_EmptySnapshotNoSelection pins the empty cold
// start: no instances → no selection anywhere and the empty content mode,
// with the launch paint path running clean — matching the pre-store TUI.
func TestColdStartFromSnapshot_EmptySnapshotNoSelection(t *testing.T) {
	h := newTestHome(t)
	h.snapshotFetcher = func(string) ([]session.InstanceData, error) {
		return nil, nil
	}

	require.NoError(t, h.coldStartFromSnapshot())
	_ = h.selectionChanged()

	require.Equal(t, 0, h.store.NumInstances())
	require.Nil(t, h.sidebar.GetSelectedInstance())
	require.Nil(t, h.store.GetSelectedInstance())
	require.Equal(t, 0, h.store.ActiveTab())
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
	require.Empty(t, h.store.GetInstances(), "no sidebar rows on a failed cold start")
}
