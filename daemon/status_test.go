package daemon

import (
	"encoding/json"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// deadTmuxBackend is a FakeBackend whose IsAlive reports false, modelling a
// tmux/remote session that vanished out from under the daemon — the #935
// scenario. HasUpdated inherits FakeBackend's (false,false), the same value a
// healthy idle session returns, so the daemon-side status pass must resolve the
// ambiguity with a liveness probe rather than repainting it Ready.
type deadTmuxBackend struct {
	*session.FakeBackend
}

func (deadTmuxBackend) IsAlive(*session.Instance) bool { return false }

// promptTapBackend is a FakeBackend that always reports a waiting prompt and
// counts TapEnter calls, so the AutoYes path the status pass now subsumes can be
// asserted (#960 PR 5 folded the old AutoYes-only loop into RefreshStatuses).
type promptTapBackend struct {
	*session.FakeBackend
	tapped int
}

func (b *promptTapBackend) HasUpdated(*session.Instance) (bool, bool) { return false, true }
func (b *promptTapBackend) TapEnter(*session.Instance)                { b.tapped++ }

// bothFlagsBackend reports (updated, hasPrompt) == (true, true) — the state a
// freshly-appeared prompt commonly produces, because the prompt text is itself
// new output — and counts TapEnter calls. It guards the #992 regression: the
// pre-#965 AutoYes loop tapped Enter whenever hasPrompt was true regardless of
// updated, but the merged switch matched `case updated` first and swallowed the
// tap on that first tick.
type bothFlagsBackend struct {
	*session.FakeBackend
	tapped int
}

func (b *bothFlagsBackend) HasUpdated(*session.Instance) (bool, bool) { return true, true }
func (b *bothFlagsBackend) TapEnter(*session.Instance)                { b.tapped++ }

// updatedOnlyBackend reports (updated, hasPrompt) == (true, false): live output
// churn with no waiting prompt. It counts TapEnter so the status-only path can
// be asserted to transition to Running WITHOUT tapping Enter.
type updatedOnlyBackend struct {
	*session.FakeBackend
	tapped int
}

func (b *updatedOnlyBackend) HasUpdated(*session.Instance) (bool, bool) { return true, false }
func (b *updatedOnlyBackend) TapEnter(*session.Instance)                { b.tapped++ }

// registerStarted seeds a single on-disk record and registers a live in-memory
// instance with the supplied backend and starting status under the daemon's key.
// One instance per repo: seedDiskInstance overwrites the repo file, so callers
// that need several use a fresh manager/repo each.
func registerStarted(t *testing.T, m *Manager, repoID, repoPath, title string, backend session.Backend, started bool, status session.Status) *session.Instance {
	t.Helper()
	inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: repoPath, Program: "claude"})
	if err != nil {
		t.Fatalf("NewInstance: %v", err)
	}
	inst.SetBackend(backend)
	inst.SetStartedForTest(started)
	inst.SetStatus(status)
	seedDiskInstance(t, repoID, title, repoPath)
	m.mu.Lock()
	m.instances[daemonInstanceKey(repoID, title)] = inst
	m.mu.Unlock()
	return inst
}

func newStatusTestManager(t *testing.T) (*Manager, string, string) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return manager, repo.ID, repoPath
}

func persistedStatus(t *testing.T, repoID, title string) session.Status {
	t.Helper()
	raw, err := config.LoadRepoInstances(repoID)
	if err != nil {
		t.Fatalf("LoadRepoInstances: %v", err)
	}
	var data []session.InstanceData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatalf("unmarshal instances: %v", err)
	}
	for _, d := range data {
		if d.Title == title {
			return d.Status
		}
	}
	t.Fatalf("instance %q not found in persisted store", title)
	return session.Status(0)
}

// TestRefreshStatuses_LiveIdleSessionBecomesReady is the control for #935 ported
// daemon-side: a live, idle session (IsAlive true, HasUpdated false,false) must
// be marked Ready, and the daemon — the sole writer now — must persist that
// transition through the targeted writer.
func TestRefreshStatuses_LiveIdleSessionBecomesReady(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerStarted(t, manager, repoID, repoPath, "idle", session.NewFakeBackend(), true, session.Running)

	manager.RefreshStatuses()

	inst := manager.instances[daemonInstanceKey(repoID, "idle")]
	if got := inst.GetStatus(); got != session.Ready {
		t.Fatalf("in-memory status = %v, want Ready", got)
	}
	if got := persistedStatus(t, repoID, "idle"); got != session.Ready {
		t.Fatalf("persisted status = %v, want Ready (daemon must persist the transition)", got)
	}
}

// TestRefreshStatuses_DeadSessionMarkedDeadNotReady is the status half of #935:
// the daemon must not repaint a session whose backing tmux/remote session has
// vanished as Ready. HasUpdated latches (false,false), indistinguishable from a
// healthy idle one, so the daemon probes liveness and marks it Dead — and
// persists it so the hollow dead-dot survives a reload.
func TestRefreshStatuses_DeadSessionMarkedDeadNotReady(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	// Start from Running to prove the pass actively transitions it to Dead
	// rather than merely leaving a pre-set status untouched.
	registerStarted(t, manager, repoID, repoPath, "gone", deadTmuxBackend{session.NewFakeBackend()}, true, session.Running)

	manager.RefreshStatuses()

	inst := manager.instances[daemonInstanceKey(repoID, "gone")]
	if got := inst.GetStatus(); got != session.Dead {
		t.Fatalf("in-memory status = %v, want Dead (a dead session must never be Ready)", got)
	}
	if got := persistedStatus(t, repoID, "gone"); got != session.Dead {
		t.Fatalf("persisted status = %v, want Dead", got)
	}
}

// TestRefreshStatuses_SkipsTransientAndUnstarted mirrors the old metadata tick's
// guards: a Loading session (tmux may not exist yet), a Deleting one (mid-
// teardown — clobbering it would re-enable kill/attach, #844), and an unstarted
// one must all keep their status untouched. Each gets a fresh manager because
// seedDiskInstance overwrites the per-repo file.
func TestRefreshStatuses_SkipsTransientAndUnstarted(t *testing.T) {
	cases := []struct {
		name    string
		started bool
		status  session.Status
	}{
		{"loading", true, session.Loading},
		{"deleting", true, session.Deleting},
		{"unstarted", false, session.Running},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			manager, repoID, repoPath := newStatusTestManager(t)
			// A dead backend would flip a probed instance to Dead; using it proves
			// the skip happens BEFORE any liveness probe.
			registerStarted(t, manager, repoID, repoPath, tc.name, deadTmuxBackend{session.NewFakeBackend()}, tc.started, tc.status)

			manager.RefreshStatuses()

			inst := manager.instances[daemonInstanceKey(repoID, tc.name)]
			if got := inst.GetStatus(); got != tc.status {
				t.Fatalf("status = %v, want %v (skipped instance must not be re-derived)", got, tc.status)
			}
		})
	}
}

// TestRefreshStatuses_TapsEnterOnWaitingPrompt guards that the AutoYes path the
// status pass subsumed still fires: a session reporting a waiting prompt gets
// TapEnter, and its status is left for the next tick to resolve (Running here).
func TestRefreshStatuses_TapsEnterOnWaitingPrompt(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &promptTapBackend{FakeBackend: session.NewFakeBackend()}
	registerStarted(t, manager, repoID, repoPath, "prompted", backend, true, session.Running)

	manager.RefreshStatuses()

	if backend.tapped == 0 {
		t.Fatal("AutoYes regressed: TapEnter was not called for a session with a waiting prompt")
	}
	inst := manager.instances[daemonInstanceKey(repoID, "prompted")]
	if got := inst.GetStatus(); got != session.Running {
		t.Fatalf("status = %v, want Running (a waiting-prompt session keeps its status)", got)
	}
}

// TestRefreshStatuses_TapsEnterWhenUpdatedAndPrompt is the #992 regression guard:
// when HasUpdated reports (true, true) — the usual shape on the first tick a
// prompt appears, since the prompt text is fresh output — AutoYes must still tap
// Enter on that tick, not one poll interval later. The merged switch matched
// `case updated` before `case hasPrompt`, so the tap was swallowed until the
// next poll once output stabilized. Status is still set Running by the updated
// branch (left as-is from before this fix).
func TestRefreshStatuses_TapsEnterWhenUpdatedAndPrompt(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &bothFlagsBackend{FakeBackend: session.NewFakeBackend()}
	registerStarted(t, manager, repoID, repoPath, "both-flags", backend, true, session.Running)

	manager.RefreshStatuses()

	if backend.tapped == 0 {
		t.Fatal("BUG (#992): TapEnter was not called when HasUpdated returned (true, true)")
	}
	inst := manager.instances[daemonInstanceKey(repoID, "both-flags")]
	if got := inst.GetStatus(); got != session.Running {
		t.Fatalf("status = %v, want Running (the updated branch still sets Running)", got)
	}
}

// TestRefreshStatuses_UpdatedWithoutPromptDoesNotTap is the complement: pure
// output churn (true, false) must transition the session to Running WITHOUT
// tapping Enter — only a waiting prompt drives AutoYes.
func TestRefreshStatuses_UpdatedWithoutPromptDoesNotTap(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	backend := &updatedOnlyBackend{FakeBackend: session.NewFakeBackend()}
	registerStarted(t, manager, repoID, repoPath, "updated-only", backend, true, session.Ready)

	manager.RefreshStatuses()

	if backend.tapped != 0 {
		t.Fatalf("TapEnter called %d times for (updated, hasPrompt)==(true,false); AutoYes must only tap on a waiting prompt", backend.tapped)
	}
	inst := manager.instances[daemonInstanceKey(repoID, "updated-only")]
	if got := inst.GetStatus(); got != session.Running {
		t.Fatalf("status = %v, want Running (updated output drives Running)", got)
	}
}

// TestRefreshStatuses_NoTransitionDoesNotRepersist proves the disk-churn guard:
// an already-Ready idle session stays Ready and is not re-written, so an idle
// fleet does not rewrite instances.json every poll.
func TestRefreshStatuses_NoTransitionDoesNotRepersist(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerStarted(t, manager, repoID, repoPath, "steady", session.NewFakeBackend(), true, session.Ready)

	// Corrupt the on-disk record's status to a sentinel the pass would only
	// overwrite if it (wrongly) persisted a non-transition.
	mark := []session.InstanceData{{Title: "steady", Path: repoPath, Status: session.Loading}}
	raw, err := json.Marshal(mark)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := config.LoadState().SaveInstances(repoID, raw); err != nil {
		t.Fatalf("seed sentinel: %v", err)
	}

	manager.RefreshStatuses()

	if got := persistedStatus(t, repoID, "steady"); got != session.Loading {
		t.Fatalf("persisted status = %v, want Loading untouched (an unchanged status must not be re-persisted)", got)
	}
}
