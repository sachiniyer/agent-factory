package app

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/exp/teatest"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ----------------------------------------------------------------------------
// teatest harness for end-to-end UI tests of the async creation flow (#310)
// and lazy PR fetching (#311).
//
// The harness swaps two seams:
//   session.SetBackendFactoryForTest — so NewInstance returns a FakeBackend
//     whose Start blocks until the test signals completion
//   app.SetPRInfoFetcherForTest    — so fetchPRInfoCmd routes to a counter
//     that returns canned PRInfo instead of shelling out to `gh`
//
// Both seams are restored via t.Cleanup, so each test is isolated.
// ----------------------------------------------------------------------------

type e2eHarness struct {
	t    *testing.T
	tm   *teatest.TestModel
	home *home

	bmu      sync.Mutex
	backends []*session.FakeBackend

	fmu         sync.Mutex
	prFetchLog  []prFetchCall
	prFetchResp *git.PRInfo
}

type prFetchCall struct {
	repoPath string
	branch   string
}

// newE2EHarness constructs the home and installs the seams, but does NOT
// start the tea.Program. Tests should preload any instances via
// eh.home.sidebar.AddInstance(...) before calling eh.start() — once the
// Program is running, its goroutine owns the sidebar and direct mutation
// from the test goroutine would race.
func newE2EHarness(t *testing.T) *e2eHarness {
	t.Helper()
	h := newTestHome(t)
	eh := &e2eHarness{t: t, home: h}

	restoreBackend := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, absPath string) (session.Backend, error) {
		fb := session.NewFakeBackend()
		eh.bmu.Lock()
		eh.backends = append(eh.backends, fb)
		eh.bmu.Unlock()
		return fb, nil
	})
	t.Cleanup(restoreBackend)

	restoreFetcher := SetPRInfoFetcherForTest(func(repoPath, branch string) (*git.PRInfo, error) {
		eh.fmu.Lock()
		eh.prFetchLog = append(eh.prFetchLog, prFetchCall{repoPath, branch})
		resp := eh.prFetchResp
		eh.fmu.Unlock()
		return resp, nil
	})
	t.Cleanup(restoreFetcher)

	return eh
}

// start launches the teatest program. Must be called exactly once, after any
// sidebar preloading is complete.
func (eh *e2eHarness) start() {
	eh.t.Helper()
	eh.tm = teatest.NewTestModel(eh.t, eh.home, teatest.WithInitialTermSize(120, 40))
	eh.t.Cleanup(func() {
		// Best-effort shutdown. If the model is already quit Quit returns nil.
		_ = eh.tm.Quit()
	})
}

// addStartedInstance builds an instance already flipped to started+Running
// and adds it to the sidebar. Must be called BEFORE start() — mutates the
// sidebar directly.
func (eh *e2eHarness) addStartedInstance(title string) *session.Instance {
	eh.t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{
		Title:   title,
		Path:    eh.t.TempDir(),
		Program: "claude",
	})
	if err != nil {
		eh.t.Fatal(err)
	}
	inst.SetStartedForTest(true)
	inst.SetStatus(session.Running)
	eh.home.sidebar.AddInstance(inst)
	return inst
}

// addStartedInstanceWithWorktree is addStartedInstance + a fake GitWorktree
// attached, so fetchPRInfoCmd's FetchPRInfoSnapshot guard passes. Use for
// PR-info tests that need the async fetch to actually dispatch.
func (eh *e2eHarness) addStartedInstanceWithWorktree(title, branch string) *session.Instance {
	eh.t.Helper()
	inst := eh.addStartedInstance(title)
	inst.Branch = branch
	gw, err := git.NewGitWorktreeFromStorage(
		eh.t.TempDir(), // repoPath — not read by fake fetcher
		filepath.Join(eh.t.TempDir(), "worktree"), // worktreePath — ditto
		title,
		branch,
		"deadbeef",
		false,
		true,
	)
	if err != nil {
		eh.t.Fatal(err)
	}
	inst.SetGitWorktreeForTest(gw)
	return inst
}

// latestBackend returns the most recently created FakeBackend, blocking up
// to d for the factory to fire at least once. Useful when a test triggers
// instance creation via keystrokes and needs the backend handle to unblock
// Start.
func (eh *e2eHarness) latestBackend(d time.Duration) *session.FakeBackend {
	eh.t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		eh.bmu.Lock()
		n := len(eh.backends)
		eh.bmu.Unlock()
		if n > 0 {
			eh.bmu.Lock()
			fb := eh.backends[len(eh.backends)-1]
			eh.bmu.Unlock()
			return fb
		}
		time.Sleep(5 * time.Millisecond)
	}
	eh.t.Fatalf("no FakeBackend created within %s", d)
	return nil
}

// waitForStart blocks until the given FakeBackend's Start has been invoked.
// Fails the test if the call hasn't fired within d.
func (eh *e2eHarness) waitForStart(fb *session.FakeBackend, d time.Duration) {
	eh.t.Helper()
	select {
	case <-fb.StartCalled():
	case <-time.After(d):
		eh.t.Fatalf("FakeBackend.Start was not invoked within %s", d)
	}
}

// prFetchCount returns how many times the fake PR fetcher has been called.
func (eh *e2eHarness) prFetchCount() int {
	eh.fmu.Lock()
	defer eh.fmu.Unlock()
	return len(eh.prFetchLog)
}

// prFetchBranches returns the branches requested so far (for assertions
// that the right instance's PR was fetched).
func (eh *e2eHarness) prFetchBranches() []string {
	eh.fmu.Lock()
	defer eh.fmu.Unlock()
	out := make([]string, 0, len(eh.prFetchLog))
	for _, c := range eh.prFetchLog {
		out = append(out, c.branch)
	}
	return out
}

// waitUntil polls fn every 10ms until it returns true or d elapses, then
// fails the test with msg. Used to synchronise assertions on Update
// side-effects that happen asynchronously under the tea.Program goroutine.
func (eh *e2eHarness) waitUntil(d time.Duration, msg string, fn func() bool) {
	eh.t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	eh.t.Fatalf("timeout after %s waiting for: %s", d, msg)
}

// TestE2E_HarnessBoots is a smoke test: constructing the harness should
// not panic and the tea.Program should start without immediately bailing.
func TestE2E_HarnessBoots(t *testing.T) {
	eh := newE2EHarness(t)
	eh.start()
	// Give the program a moment to process its initial commands (Init
	// returns a batch of tickers).
	time.Sleep(100 * time.Millisecond)
	// Sidebar should be empty (no preloaded instances).
	if got := len(eh.home.sidebar.GetInstances()); got != 0 {
		t.Fatalf("expected empty sidebar, got %d instances", got)
	}
}

// query runs f on the tea.Program goroutine and blocks until f returns.
// All sidebar and state reads from e2e tests go through this helper — the
// sidebar is single-goroutine-owned in production, so direct access from
// the test goroutine races with Update handlers.
//
// See runOnEventLoopMsg in app.go for the message type that plumbs this.
func (eh *e2eHarness) query(f func(h *home)) {
	eh.t.Helper()
	done := make(chan struct{})
	eh.tm.Send(runOnEventLoopMsg{fn: f, done: done})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		eh.t.Fatal("timed out waiting for e2e query to run on tea goroutine")
	}
}

// findInstance returns the sidebar instance with the given title, or nil.
// Routes the read through the tea goroutine to avoid racing Update handlers.
func (eh *e2eHarness) findInstance(title string) *session.Instance {
	var found *session.Instance
	eh.query(func(h *home) {
		for _, inst := range h.sidebar.GetInstances() {
			if inst.Title == title {
				found = inst
				return
			}
		}
	})
	return found
}

// selectedInstance returns h.sidebar.GetSelectedInstance() via the tea
// goroutine so it doesn't race with concurrent sidebar mutations.
func (eh *e2eHarness) selectedInstance() *session.Instance {
	var sel *session.Instance
	eh.query(func(h *home) {
		sel = h.sidebar.GetSelectedInstance()
	})
	return sel
}

// homeState returns h.state via the tea goroutine.
func (eh *e2eHarness) homeState() state {
	var s state
	eh.query(func(h *home) { s = h.state })
	return s
}

// homeTextOverlayNil returns (h.textOverlay == nil) via the tea goroutine.
func (eh *e2eHarness) homeTextOverlayNil() bool {
	var nilOverlay bool
	eh.query(func(h *home) { nilOverlay = h.textOverlay == nil })
	return nilOverlay
}

// namingTitle returns h.namingInstance.Title via the tea goroutine, or ""
// when no naming is in progress.
func (eh *e2eHarness) namingTitle() string {
	var title string
	eh.query(func(h *home) {
		if h.namingInstance != nil {
			title = h.namingInstance.Title
		}
	})
	return title
}

// instanceStatus reads an instance's Status via the tea goroutine to avoid
// racing SetStatus writes from Update handlers.
func (eh *e2eHarness) instanceStatus(inst *session.Instance) session.Status {
	var s session.Status
	eh.query(func(*home) { s = inst.Status })
	return s
}

// ----------------------------------------------------------------------------
// #310 — instance creation must not interfere with switching.
//
// End-to-end drive of the real tea.Program:
//   preload 'other' (already Running)
//   press 'n', type 'first', press Enter
//   wait for FakeBackend.Start to fire (creation is now in flight)
//   press ↑ to navigate back to 'other'
//   complete Start
//   assert: no help modal, selection still on 'other', 'first' is Running
// ----------------------------------------------------------------------------

func TestE2E_310_Success_NavigateAwayDuringCreation(t *testing.T) {
	eh := newE2EHarness(t)
	other := eh.addStartedInstance("other")
	eh.home.sidebar.SetSelectedInstance(0) // selects 'other'
	eh.start()

	// Settle initial Init ticks.
	time.Sleep(100 * time.Millisecond)

	// Press 'n' to begin creating a new instance. The key flows through
	// handleMenuHighlighting (which re-emits it once for highlighting
	// bookkeeping), so this lands in stateNew after the second Update cycle.
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	eh.waitUntil(time.Second, "state transitions to stateNew", func() bool {
		return eh.homeState() == stateNew
	})

	// Type the name. Characters chosen to avoid GlobalKeyStringsMap
	// entries ({k,j,o,n,q,s,a,r,p,h,l}) — handleMenuHighlighting re-emits
	// any mapped key, which reorders typing in subtle ways.
	eh.tm.Type("fig")
	eh.waitUntil(2*time.Second, "title 'fig' fully typed", func() bool {
		return eh.namingTitle() == "fig"
	})

	// Commit with Enter. This fires startCmd (which blocks on FakeBackend).
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	eh.waitUntil(time.Second, "state returns to stateDefault after Enter", func() bool {
		return eh.homeState() == stateDefault
	})

	// FakeBackend was created by the factory during NewInstance; wait for
	// Start to be invoked by the creation goroutine.
	fb := eh.latestBackend(time.Second)
	eh.waitForStart(fb, time.Second)

	// Creation is now in flight. Grab the creating instance for later
	// assertions (pointer is stable, so no race on reading .Status).
	fig := eh.findInstance("fig")
	require.NotNil(t, fig, "'fig' should be in sidebar while creating")
	require.Equal(t, session.Loading, eh.instanceStatus(fig),
		"status should be Loading during creation")

	// Navigate back to 'other' while 'fig' is still Loading. The creating
	// instance was just added and auto-selected (see startNewInstance), so
	// a single Up moves us back.
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyUp})
	eh.waitUntil(time.Second, "selection moves back to 'other'", func() bool {
		return eh.selectedInstance() == other
	})

	// Unblock Start — creation completes successfully.
	fb.CompleteStart()

	// Wait for the async instanceStartedMsg handler to flip status. Read
	// via instanceStatus so the test goroutine doesn't race SetStatus.
	eh.waitUntil(time.Second, "'fig' status flips to Running", func() bool {
		return eh.instanceStatus(fig) == session.Running
	})

	// Core assertions for #310:
	assert.Equal(t, stateDefault, eh.homeState(),
		"no help modal should pop when the user has navigated away")
	assert.True(t, eh.homeTextOverlayNil(),
		"no help overlay should be set")
	assert.Same(t, other, eh.selectedInstance(),
		"user's selection on 'other' must be preserved across creation completion")
	assert.Equal(t, session.Running, eh.instanceStatus(fig),
		"'fig' must still flip to Running even though no modal was shown")
}

// TestE2E_310_Success_UserStillOnInstance verifies the counterpart: when
// the user DOES stay on the creating instance, the attach-help modal fires
// as before (regression for #310 over-correction).
func TestE2E_310_Success_UserStillOnInstance(t *testing.T) {
	eh := newE2EHarness(t)
	eh.start()

	time.Sleep(100 * time.Millisecond)

	eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	eh.waitUntil(time.Second, "stateNew", func() bool { return eh.homeState() == stateNew })
	eh.tm.Type("bee")
	eh.waitUntil(time.Second, "title typed", func() bool {
		return eh.namingTitle() == "bee"
	})
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	eh.waitUntil(time.Second, "stateDefault", func() bool { return eh.homeState() == stateDefault })

	fb := eh.latestBackend(time.Second)
	eh.waitForStart(fb, time.Second)

	// User does NOT navigate away — they stay on 'bee'.
	fb.CompleteStart()

	// The attach-help modal should fire: state flips to stateHelp.
	eh.waitUntil(time.Second, "state flips to stateHelp", func() bool {
		return eh.homeState() == stateHelp
	})
	assert.False(t, eh.homeTextOverlayNil(), "help overlay must be installed")

	bee := eh.findInstance("bee")
	require.NotNil(t, bee)
	assert.Equal(t, session.Running, eh.instanceStatus(bee))
}

// ----------------------------------------------------------------------------
// #310 failure — the instance that failed must be removed, and the user's
// current selection on an unrelated instance must be preserved.
// ----------------------------------------------------------------------------

func TestE2E_310_Failure_DoesNotTouchOtherInstance(t *testing.T) {
	eh := newE2EHarness(t)
	other := eh.addStartedInstance("other")
	eh.home.sidebar.SetSelectedInstance(0)
	eh.start()

	time.Sleep(100 * time.Millisecond)

	// Kick off creation of 'bug'.
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'n'}})
	eh.waitUntil(time.Second, "stateNew", func() bool { return eh.homeState() == stateNew })
	eh.tm.Type("bug")
	eh.waitUntil(time.Second, "title typed", func() bool {
		return eh.namingTitle() == "bug"
	})
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyEnter})
	eh.waitUntil(time.Second, "stateDefault", func() bool { return eh.homeState() == stateDefault })

	fb := eh.latestBackend(time.Second)
	eh.waitForStart(fb, time.Second)

	// User navigates back to 'other' while 'bug' is mid-creation.
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyUp})
	eh.waitUntil(time.Second, "selection back on 'other'", func() bool {
		return eh.selectedInstance() == other
	})

	// Creation fails.
	fb.FailStart(errors.New("simulated launch failure"))

	// Wait for the handler to run RemoveInstanceByTitle.
	eh.waitUntil(2*time.Second, "'bug' is removed from sidebar", func() bool {
		return eh.findInstance("bug") == nil
	})

	// Core #310 failure assertions:
	assert.NotNil(t, eh.findInstance("other"),
		"unrelated 'other' instance must NOT be killed when 'bug' dies")
	assert.Same(t, other, eh.selectedInstance(),
		"user's selection must remain on 'other'")
	assert.Equal(t, stateDefault, eh.homeState(),
		"failure should not put the app into a modal state")
}

// ----------------------------------------------------------------------------
// #311 — PR info should be async and lazy.
//
// Scenario 3: selecting an instance with stale PR info triggers exactly one
// fake-fetcher call. A second selection-change within the freshness window
// must be debounced (no second call).
// ----------------------------------------------------------------------------

func TestE2E_311_LazyDebounce_OnSelectionChange(t *testing.T) {
	eh := newE2EHarness(t)
	a := eh.addStartedInstanceWithWorktree("egg", "branch-a")
	b := eh.addStartedInstanceWithWorktree("fig", "branch-b")
	eh.home.sidebar.SetSelectedInstance(0) // start on A
	eh.start()

	// Wait for the initial selection-triggered fetch. Freshness age is
	// sentinel-infinite on process start, so the first selection dispatches.
	eh.waitUntil(2*time.Second, "first fetch (for 'egg') dispatches", func() bool {
		return eh.prFetchCount() >= 1
	})
	require.Equal(t, []string{"branch-a"}, eh.prFetchBranches(),
		"first fetch must target the selected instance's branch")

	// Navigate down to B — triggers a second selectionChanged and fetch.
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyDown})
	eh.waitUntil(2*time.Second, "second fetch (for 'fig') dispatches", func() bool {
		return eh.prFetchCount() >= 2
	})
	assert.Equal(t, []string{"branch-a", "branch-b"}, eh.prFetchBranches())

	// Navigate back to A — freshness window is 60s, so no new fetch should
	// fire. Give the event loop a chance to do the wrong thing if it's going to.
	eh.tm.Send(tea.KeyMsg{Type: tea.KeyUp})
	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, 2, eh.prFetchCount(),
		"debounce must suppress a re-fetch for an instance whose PR info is still fresh")

	assert.NotNil(t, eh.findInstance("egg"))
	assert.NotNil(t, eh.findInstance("fig"))
	_ = a
	_ = b
}

// ----------------------------------------------------------------------------
// Scenario 4: the PR info tick refreshes the selected instance ONLY,
// not every instance in the sidebar (the pre-#311 behavior).
// ----------------------------------------------------------------------------

func TestE2E_311_TickRefresh_OnlySelectedInstance(t *testing.T) {
	eh := newE2EHarness(t)
	eh.addStartedInstanceWithWorktree("egg", "branch-a")
	b := eh.addStartedInstanceWithWorktree("fig", "branch-b")
	eh.home.sidebar.SetSelectedInstance(1) // select B
	eh.start()

	// Let the initial selection-triggered fetch complete.
	eh.waitUntil(2*time.Second, "initial fetch for selected instance", func() bool {
		return eh.prFetchCount() >= 1
	})
	initial := eh.prFetchCount()
	require.Equal(t, []string{"branch-b"}, eh.prFetchBranches(),
		"initial fetch must be for the selected instance only")

	// Send the PR info tick message synthetically — the real ticker waits
	// 60s, too long for a unit test. The handler forces a fetch for the
	// selected instance regardless of freshness.
	eh.tm.Send(tickUpdatePRInfoMessage{})
	eh.waitUntil(2*time.Second, "tick triggers another fetch", func() bool {
		return eh.prFetchCount() > initial
	})

	// Every fetch should have targeted B's branch — the tick must not
	// iterate over every instance the way the pre-#311 code did.
	branches := eh.prFetchBranches()
	for _, br := range branches {
		assert.Equal(t, "branch-b", br,
			"every fetch must target the selected instance's branch; got %v", branches)
	}
	_ = b
}
