package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// The teardown fence (#3865).
//
// #2953 spent four rounds trying to DETECT that a user had adopted a finished
// task session before its declared on_complete verb landed on it, and each
// round's check ran outside the fence the destructive call later took. These
// tests are about the fence rather than the check: they drive the real teardown
// (unstubbed ArchiveSession/KillSession) and pin both orderings the fence admits
// and the absence of a third.

// startTeardownForTest dispatches the teardown exactly as
// applyTaskSessionLifecycleOnRunEnd does — the same arguments, including the
// adoption baseline the completion transition pinned, and the real
// archive/kill underneath. Only the `go` statement and the hook channel are the
// test's: an instance built by the fixtures has no hook run in flight, so there
// is no production seam for holding the wait open.
func startTeardownForTest(
	t *testing.T,
	m *Manager,
	repoID string,
	inst *session.Instance,
	taskID, verb string,
	hooks <-chan struct{},
) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	adoptedAt := inst.AdoptionDeliveriesAtRunEnd()
	go func() {
		defer close(done)
		m.runTaskSessionLifecycle(repoID, inst.ID, inst.Title, taskID, verb, hooks, adoptedAt)
	}()
	return done
}

// browserTerminalInput drives the exact binding daemon/ws_pty.go builds for a
// ?tab_id= connection — idTabBinding, whose input() is `as.InputTab(tabID, p)`
// and which takes no manager lock at all. That absence is why constraint 6 of
// #3865 exists, and why this path needs its own serialization.
//
// The write itself fails against these fixtures (a FakeBackend session has no
// tmux behind the tab), and that is deliberate rather than a gap: the adoption
// mark is taken BEFORE the write, so a delivery whose outcome is unknown counts
// as one. A send that landed with its reply lost must not leave the session
// eligible for a teardown. That the count moves regardless of the write's fate
// is asserted directly in session.TestEveryAgentServerWritePathIsAdoption.
func browserTerminalInput(t *testing.T, inst *session.Instance, keys string) error {
	t.Helper()
	ta, ok := inst.AgentServer().(session.TabAddressableServer)
	require.True(t, ok, "the local runtime must expose the id-native plane the browser binds to")
	return idTabBinding{as: ta, tabID: "tab-agent"}.input([]byte(keys))
}

// assertStillRegistered is the outcome a stand-down must produce: the session is
// left exactly where it was, which is recoverable, and is what the hook-wait
// timeout already does.
func assertStillRegistered(t *testing.T, m *Manager, repoID, title string) {
	t.Helper()
	m.mu.Lock()
	inst, present := m.instances[daemonInstanceKey(repoID, title)]
	m.mu.Unlock()
	require.True(t, present, "an adopted session must be left in place")
	require.NotEqual(t, session.LiveArchived, inst.GetLiveness(), "nor archived out from under the user")
}

// TestTaskSessionLifecycle_APromptDuringTheHookWaitStandsTheTeardownDown is
// #3865's first case. The teardown decides at the completion edge and acts up to
// ten minutes later, after post_worktree_commands finish; a user who prompts the
// session in that window has adopted it.
//
// The state asserted below is the one no earlier attempt could act on: the
// session still reads LiveReady and its task run is still over, so every
// LEVEL says nothing has happened. Only the delivery count — an event — differs,
// and the teardown reads it under the fence.
func TestTaskSessionLifecycle_APromptDuringTheHookWaitStandsTheTeardownDown(t *testing.T) {
	manager, logs, repoID, repoPath := newStatusTestManagerCapturingLogs(t)
	inst := registerTaskSpawnedSession(t, manager, repoID, repoPath, "nightly", "task-kill")
	stubTaskLifecycle(t, "task-kill", task.OnCompleteKill)
	endRunOnIdleEdge(t, inst)

	hooks := make(chan struct{})
	done := startTeardownForTest(t, manager, repoID, inst, "task-kill", task.OnCompleteKill, hooks)

	// The user picks the finished run up while its hooks are still running.
	require.NoError(t, manager.SendPrompt(SendPromptRequest{
		ID: inst.ID, Title: "nightly", RepoID: repoID, Prompt: "actually, also update the changelog",
	}))
	require.Equal(t, session.LiveReady, inst.GetLiveness(),
		"precondition: the poll has not observed the new turn, so liveness still reads idle")
	require.False(t, inst.TaskRunActive(),
		"precondition: the task's own run is permanently over, so that marker cannot tell this apart either")
	require.Greater(t, inst.AdoptionDeliveries(), inst.AdoptionDeliveriesAtRunEnd(),
		"precondition: the only signal that moved is the delivery count")

	close(hooks)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the teardown never finished")
	}

	assertStillRegistered(t, manager, repoID, "nightly")
	assert.Contains(t, logs.info.String(), "not applying on_complete=kill",
		"a stand-down says why, once")

	// And the session is usable again: a stand-down must REOPEN the fence, or the
	// guard would leave a live session permanently unable to accept a keystroke.
	require.NotErrorIs(t, browserTerminalInput(t, inst, "\r"), session.ErrAdoptionFenced,
		"a stand-down must readmit input")
}

// TestTaskSessionLifecycle_BrowserTerminalInputDuringTheHookWaitStandsItDown is
// constraint 4 and constraint 6 together: browser input never touches
// Manager.SendPrompt, and it takes no manager lock, so a fence that only
// serialized against SendPrompt would inherit the same hole in a new place.
func TestTaskSessionLifecycle_BrowserTerminalInputDuringTheHookWaitStandsItDown(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerTaskSpawnedSession(t, manager, repoID, repoPath, "nightly", "task-archive")
	stubTaskLifecycle(t, "task-archive", task.OnCompleteArchive)
	endRunOnIdleEdge(t, inst)

	hooks := make(chan struct{})
	done := startTeardownForTest(t, manager, repoID, inst, "task-archive", task.OnCompleteArchive, hooks)

	// A user types into the web terminal. Nothing about the session's lifecycle
	// state changes — no transition, no prompt-evidence record, no liveness edge.
	_ = browserTerminalInput(t, inst, "make test\r")
	require.Equal(t, session.LiveReady, inst.GetLiveness())
	require.False(t, inst.TaskRunActive())
	require.Greater(t, inst.AdoptionDeliveries(), inst.AdoptionDeliveriesAtRunEnd())

	close(hooks)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the teardown never finished")
	}
	assertStillRegistered(t, manager, repoID, "nightly")
}

// TestTaskSessionLifecycle_AnUntouchedRunIsStillReapedAfterTheHookWait is the
// anti-vacuity case, and it is the one that says the guard is a guard and not an
// off switch. #2595's whole point is that these sessions stop accumulating.
func TestTaskSessionLifecycle_AnUntouchedRunIsStillReapedAfterTheHookWait(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerTaskSpawnedSession(t, manager, repoID, repoPath, "nightly", "task-kill")
	stubTaskLifecycle(t, "task-kill", task.OnCompleteKill)
	endRunOnIdleEdge(t, inst)

	hooks := make(chan struct{})
	done := startTeardownForTest(t, manager, repoID, inst, "task-kill", task.OnCompleteKill, hooks)
	close(hooks)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the teardown never finished")
	}

	manager.mu.Lock()
	_, present := manager.instances[daemonInstanceKey(repoID, "nightly")]
	manager.mu.Unlock()
	assert.False(t, present, "a finished run nobody touched must still be reaped")
}

// TestTaskSessionLifecycle_ADeliveryRacingTheDecisionIsRefused is constraint 5,
// and the case no previous attempt could express.
//
// The teardown is held at the exact point that used to be a gap: it has claimed
// killsInFlight, holds the per-session op-lock, has shut the adoption fence and
// read the count under it, and has not yet destroyed anything. Both delivery
// paths then try to land there.
//
// Neither may be accepted, and the reason differs per path, which is the point:
// Manager.SendPrompt contends for the manager's fence and is refused by the
// claim; browser PTY input takes no manager lock and is refused by the adoption
// fence. Together with the two tests above — where a delivery that lands BEFORE
// the guard stands the teardown down — there is no third ordering.
func TestTaskSessionLifecycle_ADeliveryRacingTheDecisionIsRefused(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerTaskSpawnedSession(t, manager, repoID, repoPath, "nightly", "task-kill")
	stubTaskLifecycle(t, "task-kill", task.OnCompleteKill)
	endRunOnIdleEdge(t, inst)

	inWindow := make(chan struct{})
	release := make(chan struct{})
	prev := testHookTaskLifecycleGuardPassed
	testHookTaskLifecycleGuardPassed = func() {
		close(inWindow)
		<-release
	}
	t.Cleanup(func() { testHookTaskLifecycleGuardPassed = prev })

	done := startTeardownForTest(t, manager, repoID, inst, "task-kill", task.OnCompleteKill, nil)
	select {
	case <-inWindow:
	case <-time.After(30 * time.Second):
		t.Fatal("the teardown never reached its decision")
	}

	promptErr := manager.SendPrompt(SendPromptRequest{
		ID: inst.ID, Title: "nightly", RepoID: repoID, Prompt: "one more thing",
	})
	ptyErr := browserTerminalInput(t, inst, "one more thing\r")

	close(release)
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the teardown never finished")
	}

	require.Error(t, promptErr, "a prompt racing the decision must be refused, not delivered")
	assert.Contains(t, promptErr.Error(), "being deleted")
	require.ErrorIs(t, ptyErr, session.ErrAdoptionFenced,
		"browser input racing the decision must be refused by the adoption fence")
	assert.Equal(t, inst.AdoptionDeliveriesAtRunEnd(), inst.AdoptionDeliveries(),
		"a refused delivery must not be counted — the teardown acted on a count nothing changed")

	manager.mu.Lock()
	_, present := manager.instances[daemonInstanceKey(repoID, "nightly")]
	manager.mu.Unlock()
	assert.False(t, present, "nothing adopted this session, so the reap was correct")
}

// TestTaskSessionLifecycle_DeferredDrainUsesTheSameRevalidation keeps the two
// routes into a teardown consistent (#3865 item 5). A run that finished while a
// TUI was attached parks its intent; the drain must ask the same question the
// hook-wait teardown asks, not a weaker one.
func TestTaskSessionLifecycle_DeferredDrainUsesTheSameRevalidation(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerTaskSpawnedSession(t, manager, repoID, repoPath, "nightly", "task-kill")
	stubTaskLifecycle(t, "task-kill", task.OnCompleteKill)

	was := endRunOnIdleEdge(t, inst)
	manager.deferTaskSessionLifecycleWhilePaused(repoID, inst, was)

	// A delivery during the attach, with the session settling straight back to
	// idle: liveness and the run marker both read exactly as they did at
	// completion, so the drain's pre-existing checks see nothing.
	require.NoError(t, manager.SendPrompt(SendPromptRequest{
		ID: inst.ID, Title: "nightly", RepoID: repoID, Prompt: "mine now",
	}))
	require.Equal(t, session.LiveReady, inst.GetLiveness())
	require.False(t, inst.TaskRunActive())

	manager.applyDeferredTaskSessionLifecycle(repoID, inst)
	time.Sleep(200 * time.Millisecond)
	assertStillRegistered(t, manager, repoID, "nightly")
}

// TestTaskLifecycleStillOwed covers the shared re-validation directly, including
// the two ways it must refuse and the anti-vacuity case.
func TestTaskLifecycleStillOwed(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerTaskSpawnedSession(t, manager, repoID, repoPath, "nightly", "task-x")
	endRunOnIdleEdge(t, inst)
	baseline := inst.AdoptionDeliveriesAtRunEnd()

	assert.NoError(t, taskLifecycleStillOwed(inst, inst.ID, baseline, baseline),
		"an untouched finished run is still owed its verb")
	assert.Error(t, taskLifecycleStillOwed(nil, inst.ID, baseline, baseline),
		"a session that is no longer registered is owed nothing")
	assert.Error(t, taskLifecycleStillOwed(inst, "some-other-id", baseline, baseline),
		"a replacement session must not inherit the original's owed teardown")
	assert.Error(t, taskLifecycleStillOwed(inst, inst.ID, baseline, baseline+1),
		"a delivery since the run ended makes the work the user's")
}
