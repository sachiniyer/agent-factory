package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

// The watch-task concurrency limit (#1892) is enforced inside the manager's
// reserveCreate, so these tests drive the REAL manager rather than stubbing
// createSessionForTask — a stub there would bypass the gate under test. Only the
// agent backend is faked (installInstantBackend); the git worktrees are real.

// createForTask issues one task-attributed create against the manager, exactly as
// the watch delivery path does.
func createForTask(m *Manager, repoPath, taskID, base string, limit int) (session.InstanceData, error) {
	return m.CreateSession(context.Background(), CreateSessionRequest{
		TitleBase:         base,
		RepoPath:          repoPath,
		Program:           "claude",
		TaskID:            taskID,
		MaxConcurrentRuns: limit,
	})
}

// settle drives a created session to idle, the transition that releases its
// concurrency slot. It goes through the same ObserveLiveness edge the daemon's
// status poll uses, so the test exercises the real release path rather than
// poking the field.
func settle(t *testing.T, m *Manager, repoID, title string) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	inst := m.instances[daemonInstanceKey(repoID, title)]
	if inst == nil {
		t.Fatalf("no live instance for %q", title)
	}
	if err := inst.Transition(session.ObserveLiveness(session.LiveReady)); err != nil {
		t.Fatalf("transition to ready: %v", err)
	}
}

// TestWatchConcurrencyLimitAdmitsUpToCap is the core contract: a task with a cap
// of K may hold K in-flight sessions, the next delivery is refused (not failed,
// not created), and a session going idle admits the next one.
func TestWatchConcurrencyLimitAdmitsUpToCap(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	const limit = 3
	var titles []string
	for i := 0; i < limit; i++ {
		data, err := createForTask(manager, repoPath, "task1", "dlq-triage", limit)
		if err != nil {
			t.Fatalf("create %d within the cap: %v", i, err)
		}
		if data.TaskID != "task1" {
			t.Fatalf("create %d: task provenance not persisted, got %q", i, data.TaskID)
		}
		titles = append(titles, data.Title)
	}

	// At the cap: the next delivery must be refused with the park sentinel.
	if _, err := createForTask(manager, repoPath, "task1", "dlq-triage", limit); !isAtConcurrencyLimitErr(err) {
		t.Fatalf("create over the cap: want the at-limit refusal, got %v", err)
	}

	// A different task is unaffected — the cap is scoped to the task, not global.
	if _, err := createForTask(manager, repoPath, "task2", "other-task", limit); err != nil {
		t.Fatalf("create for a different task: %v", err)
	}

	// An idle session releases its slot, admitting the next delivery.
	settle(t, manager, repo.ID, titles[0])
	if _, err := createForTask(manager, repoPath, "task1", "dlq-triage", limit); err != nil {
		t.Fatalf("create after a session went idle: %v", err)
	}

	// And the cap binds again immediately.
	if _, err := createForTask(manager, repoPath, "task1", "dlq-triage", limit); !isAtConcurrencyLimitErr(err) {
		t.Fatalf("create back over the cap: want the at-limit refusal, got %v", err)
	}
}

// TestWatchConcurrencyLimitUnlimitedByDefault pins the opt-in default: a task
// that never sets a cap behaves exactly as it did before #1892.
func TestWatchConcurrencyLimitUnlimitedByDefault(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	for i := 0; i < 5; i++ {
		if _, err := createForTask(manager, repoPath, "task1", "uncapped", 0); err != nil {
			t.Fatalf("uncapped create %d: %v", i, err)
		}
	}
}

// TestWatchConcurrencyLimitBurstRace is the acceptance criterion's race
// coverage: a burst of more simultaneous deliveries than the cap must admit at
// most K, with no two creates ever racing the check against each other. Run
// under -race.
//
// This is the test the reporter's userland monitor could not pass: its cap of
// three admitted five, because a session it had already created was invisible to
// a list+title+liveness reconstruction during creation.
func TestWatchConcurrencyLimitBurstRace(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	const (
		limit  = 3
		events = 12
	)

	var (
		mu       sync.Mutex
		live     int // creates currently inside the manager, past admission
		peak     int
		admitted int
		refused  int
	)

	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < events; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release every goroutine at once: this is the burst
			_, err := createForTask(manager, repoPath, "task1", "burst", limit)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if !isAtConcurrencyLimitErr(err) {
					t.Errorf("event %d: unexpected error: %v", i, err)
				}
				refused++
				return
			}
			admitted++
			live++
			if live > peak {
				peak = live
			}
		}(i)
	}
	close(start)
	wg.Wait()

	mu.Lock()
	defer mu.Unlock()
	if peak > limit {
		t.Errorf("burst overshot the cap: peaked at %d in-flight sessions, limit %d", peak, limit)
	}
	if admitted != limit {
		t.Errorf("admitted %d sessions, want exactly the cap %d", admitted, limit)
	}
	if refused != events-limit {
		t.Errorf("refused %d events, want %d", refused, events-limit)
	}
}

// TestWatchConcurrencyProvenancePersists covers the first half of surviving a
// daemon restart: the association between a task and the sessions it spawned has
// to be ON DISK. A restarted daemon rebuilds m.instances from these rows
// (refreshDaemonInstances), so a task_id that failed to persist would leave every
// pre-restart session uncountable and the cap wide open.
//
// It also pins that the persisted row classifies as pending — i.e. that the
// rebuilt instance will hold its slot rather than silently freeing it.
func TestWatchConcurrencyProvenancePersists(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	if _, err := createForTask(manager, repoPath, "task1", "persisted", 2); err != nil {
		t.Fatalf("create: %v", err)
	}

	disk, err := loadRepoInstanceData(repo.ID)
	if err != nil {
		t.Fatalf("loadRepoInstanceData: %v", err)
	}
	if len(disk) != 1 {
		t.Fatalf("got %d persisted rows, want 1", len(disk))
	}
	if disk[0].TaskID != "task1" {
		t.Fatalf("persisted task_id = %q, want %q; the cap could not survive a daemon restart", disk[0].TaskID, "task1")
	}
	if activity, _ := session.ClassifyActivity(disk[0]); activity != session.ActivityPending {
		t.Fatalf("persisted row classifies as %v, want pending; a restarted daemon would free the slot", activity)
	}
	// The rebuild of these rows into live instances is covered in the session
	// package (TestInstanceDataCarriesTaskProvenance), which can supply the real
	// worktree that reconstruction requires and the fake backend never persists.
}

// TestWatchConcurrencyCountsAlreadyLiveSessions covers the second half: given
// instances already in the manager — which is exactly what a restarted daemon has
// once refreshDaemonInstances rebuilds them from disk — the cap must bind
// immediately, with no reservation involved. A counter rather than a projection
// would restart at zero here and silently admit K more sessions.
//
// The instances are seeded directly because a literal restore cannot run under
// the fake backend: it persists no worktree, so fromInstanceDataForRefresh
// rejects the row ("worktree path is empty"). The projection over m.instances is
// what this test is about, and it is the same map a real restore fills.
func TestWatchConcurrencyCountsAlreadyLiveSessions(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoPath := setupControlRepo(t)
	repo, err := config.RepoFromPath(repoPath)
	if err != nil {
		t.Fatalf("RepoFromPath: %v", err)
	}

	live, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	const limit = 2
	for i := 0; i < limit; i++ {
		if _, err := createForTask(live, repoPath, "task1", "restart", limit); err != nil {
			t.Fatalf("create %d: %v", i, err)
		}
	}

	restarted, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager (restart): %v", err)
	}
	live.mu.Lock()
	restarted.mu.Lock()
	for key, inst := range live.instances {
		restarted.instances[key] = inst
	}
	restarted.mu.Unlock()
	live.mu.Unlock()

	// No reservation exists on the restarted manager — only the rebuilt instances.
	restarted.mu.Lock()
	err = restarted.admitTaskRunLocked(repo.ID, "task1", limit)
	restarted.mu.Unlock()
	if !errors.Is(err, errAtConcurrencyLimit) {
		t.Fatalf("admit after restart: want the at-limit refusal (the cap must survive a restart), got %v", err)
	}
}

// TestWatchConcurrencyLimitIgnoresOtherRepos pins the repo half of "scoped to the
// task and repository": a same-id task's sessions in another repo must not starve
// this repo's deliveries.
func TestWatchConcurrencyLimitIgnoresOtherRepos(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)
	repoA := setupControlRepo(t)
	repoB := setupControlRepo(t)
	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	const limit = 1
	if _, err := createForTask(manager, repoA, "task1", "scoped", limit); err != nil {
		t.Fatalf("create in repo A: %v", err)
	}
	if _, err := createForTask(manager, repoA, "task1", "scoped", limit); !isAtConcurrencyLimitErr(err) {
		t.Fatalf("second create in repo A: want the at-limit refusal, got %v", err)
	}
	// Repo A is saturated; repo B must still admit.
	if _, err := createForTask(manager, repoB, "task1", "scoped", limit); err != nil {
		t.Fatalf("create in repo B while repo A is at its cap: %v", err)
	}
}

// TestWatchConcurrencyReservationReleasedOnFailedCreate guards the wedge: a
// create that fails must refund its reserved slot, or a task would leak its way
// to a permanent standstill.
func TestWatchConcurrencyReservationReleasedOnFailedCreate(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	repoPath := setupControlRepo(t)

	// Fail the create AFTER admission has already reserved a slot: the reservation
	// happens in reserveCreate, the backend is provisioned after it.
	failCreate := true
	restore := session.SetBackendFactoryForTest(func(opts session.InstanceOptions, absPath string) (session.Backend, error) {
		if failCreate {
			return nil, fmt.Errorf("backend provisioning blew up")
		}
		backend := session.NewFakeBackend()
		backend.CompleteStart()
		return readyFakeBackend{backend}, nil
	})
	t.Cleanup(restore)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	_, err = manager.CreateSession(context.Background(), CreateSessionRequest{
		TitleBase:         "doomed",
		RepoPath:          repoPath,
		Program:           "claude",
		TaskID:            "task1",
		MaxConcurrentRuns: 1,
	})
	if err == nil {
		t.Fatal("create with a failing backend: want an error")
	}
	if isAtConcurrencyLimitErr(err) {
		t.Fatalf("create with a failing backend: want the backend error, got the at-limit refusal: %v", err)
	}

	manager.mu.Lock()
	leaked := manager.reservedTaskRuns["task1"]
	manager.mu.Unlock()
	if leaked != 0 {
		t.Fatalf("failed create leaked %d reserved slot(s); the task would wedge at its cap forever", leaked)
	}

	// Proof the leak would actually bite: with a cap of 1, a refunded slot must
	// still admit the next real delivery.
	failCreate = false
	if _, err := createForTask(manager, repoPath, "task1", "after-failure", 1); err != nil {
		t.Fatalf("create after a failed create: %v", err)
	}
}

// TestClassifyActivityHoldsSlotWhileCreating pins the acceptance criterion that a
// new session counts against the cap immediately — before any liveness exists and
// while its asynchronous post-worktree hooks still run. This is the exact window
// the reporter's title+liveness monitor was blind to.
func TestClassifyActivityHoldsSlotWhileCreating(t *testing.T) {
	creating := session.InstanceData{InFlightOp: session.OpCreating}
	if activity, _ := session.ClassifyActivity(creating); activity != session.ActivityPending {
		t.Fatalf("a mid-create session must hold a slot, got %v", activity)
	}

	// A legacy record predating the liveness axis (#1195) resolves through the
	// composed Status. Loading must still hold a slot: LivenessForStatus maps it
	// to LiveReady, so a naive liveness-only read would free the slot mid-create.
	legacyLoading := session.InstanceData{Status: session.Loading}
	if activity, _ := session.ClassifyActivity(legacyLoading); activity != session.ActivityPending {
		t.Fatalf("a legacy mid-create record must hold a slot, got %v", activity)
	}

	for _, tc := range []struct {
		name string
		data session.InstanceData
		want session.Activity
	}{
		{"running holds", session.InstanceData{Liveness: session.LiveRunning}, session.ActivityPending},
		{"usage-limit parked holds", session.InstanceData{Liveness: session.LiveLimitReached}, session.ActivityPending},
		{"idle releases", session.InstanceData{Liveness: session.LiveReady}, session.ActivityIdle},
		{"archived releases", session.InstanceData{Liveness: session.LiveArchived}, session.ActivityTerminal},
		{"lost releases", session.InstanceData{Liveness: session.LiveLost}, session.ActivityTerminal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if activity, _ := session.ClassifyActivity(tc.data); activity != tc.want {
				t.Fatalf("ClassifyActivity = %v, want %v", activity, tc.want)
			}
		})
	}
}

// TestIsAtConcurrencyLimitErrSurvivesRPCFlattening guards the wire contract: the
// create reaches the manager over net/rpc, which flattens a sentinel error into a
// plain string. If this ever stops matching, deliveries at the cap would be
// treated as delivery FAILURES — logged as errors, counted against the
// delivery-failure alarm, and retried on a growing backoff instead of parked.
func TestIsAtConcurrencyLimitErrSurvivesRPCFlattening(t *testing.T) {
	flattened := fmt.Errorf("%s", errAtConcurrencyLimit.Error())
	if !isAtConcurrencyLimitErr(flattened) {
		t.Fatal("the at-limit refusal must still be recognizable after net/rpc flattens it to a string")
	}
	if isAtConcurrencyLimitErr(fmt.Errorf("some unrelated failure")) {
		t.Fatal("an unrelated error must not be mistaken for the at-limit refusal")
	}
}
