package daemon

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// Manager owns the daemon's authoritative session mutations.
type Manager struct {
	cfg *config.Config

	// limitDetector is the resolved usage-limit matcher set (#1146), built once
	// from cfg.LimitPatterns at construction (it compiles the override regexes)
	// and reused across poll ticks. Immutable; read lock-free by the poll loop.
	limitDetector task.LimitDetector

	// ready is closed once the initial instance restore has completed. Until
	// then the daemon is "warming up": the control socket is already bound
	// (#829) but state-dependent RPCs return errDaemonStarting.
	ready     chan struct{}
	readyOnce sync.Once

	mu                  sync.Mutex
	storage             *session.Storage
	instances           map[string]*session.Instance
	reservedTitles      map[string]struct{}
	reservedRemoteNames map[string]struct{}
	repoStartLocks      map[string]*sync.Mutex
	// targetLocks serializes DeliverPrompt per (repo, title) so concurrent
	// deliveries to the same shared target session create it once and deliver
	// the rest in arrival order instead of racing creation and dropping the
	// losers' prompts (#865). Lazily populated like repoStartLocks.
	targetLocks map[string]*sync.Mutex
	// rootEnsureStates tracks per-configured-repo retry state for the
	// root-agent ensure loop (#1106), keyed by the root_agents config key
	// (the repo path as written in config.json).
	rootEnsureStates map[string]*rootEnsureState
	// rootKilledAt records repos (by repo ID) whose root agent was explicitly
	// killed, and WHEN. The ensure loop honors the kill only for
	// rootKillHealDelay, then self-heals a still-configured root (#1223): config
	// (root_agents), not a runtime kill, decides whether an always-on root runs.
	rootKilledAt map[string]time.Time
	// killsInFlight marks sessions (by daemon instance key) whose KillSession
	// teardown is currently running, so the status poll's finish-kill pass for
	// tombstoned records (#1108) never runs a second concurrent teardown of
	// the same session, and a duplicate KillSession RPC is rejected instead of
	// double-killing.
	killsInFlight map[string]struct{}
	// lostRestoreStates tracks per-session retry state for the Lost-session
	// restore loop (#1108 PR 2), keyed by daemon instance key — the general
	// sibling of rootEnsureStates.
	lostRestoreStates map[string]*lostRestoreState
	// limitResumeStates tracks per-session retry state for the usage-limit
	// auto-resume scheduler (#1146 PR3), keyed by daemon instance key — the
	// opt-in sibling of lostRestoreStates. Guarded by m.mu.
	limitResumeStates map[string]*limitResumeState
	// instanceOpLocks serializes the mutually-exclusive per-session
	// operations — kill teardown and Lost-recovery — by daemon instance key.
	// killsInFlight alone is a point-in-time signal; this lock is what makes
	// a KillSession arriving mid-Recover WAIT for the recover attempt and
	// then tear the restored session down, instead of interleaving a teardown
	// with a re-spawn. The recover side only TryLocks (the poll goroutine
	// must never stall behind a slow teardown). Lazily populated like
	// repoStartLocks; entries are never removed (a few bytes per session ever
	// touched).
	instanceOpLocks map[string]*sync.Mutex

	// pausedPolls records sessions whose daemon capture-pane liveness poll is
	// paused while a TUI is attached full-screen to them (#1160), keyed by
	// daemon instance key → lease expiry. Guarded by pausedMu, a DEDICATED
	// mutex (NOT m.mu): refreshInstanceStatus deliberately snapshots under m.mu
	// and then runs each slow tmux probe with m.mu RELEASED so a hung probe
	// can't block unrelated RPCs — the pause check runs inside that lock-free
	// window, so reusing m.mu would reintroduce exactly the contention the
	// release avoids. Each entry is lease-bounded (statusPollLease): a crashed
	// TUI that never sends Resume auto-resumes within one lease, so the pause
	// can never permanently blind the daemon.
	pausedMu    sync.Mutex
	pausedPolls map[string]time.Time

	// events is the WS events-plane fan-out (#1592 Phase 2 PR5): every session/
	// task mutation the daemon owns publishes here, and GET /v1/events streams it
	// to clients. On the Manager (not a controlServer) because both transports
	// mutate through this one Manager, so a single hub captures every change.
	// Immutable after construction; the hub is internally synchronized.
	events *eventsHub
}

// NewManager constructs a manager and synchronously restores all persisted
// instances into it, returning only once the manager is ready. RunDaemon
// deliberately does NOT use this: it builds the shell with newManagerShell,
// binds the control socket, and only then runs RestoreInstances — the restore
// can take minutes on remote-hook repos and must not delay the bind (#829).
func NewManager(cfg *config.Config) (*Manager, error) {
	manager, err := newManagerShell(cfg)
	if err != nil {
		return nil, err
	}
	if err := manager.RestoreInstances(); err != nil {
		return nil, err
	}
	return manager, nil
}

// newManagerShell constructs a Manager with no instances loaded. The manager
// reports !Ready() until RestoreInstances completes.
func newManagerShell(cfg *config.Config) (*Manager, error) {
	state := config.LoadState()
	storage, err := session.NewStorage(state, "")
	if err != nil {
		return nil, fmt.Errorf("failed to initialize storage: %w", err)
	}
	return &Manager{
		cfg:                 cfg,
		limitDetector:       task.NewLimitDetector(cfg.LimitPatterns),
		ready:               make(chan struct{}),
		storage:             storage,
		instances:           make(map[string]*session.Instance),
		reservedTitles:      make(map[string]struct{}),
		reservedRemoteNames: make(map[string]struct{}),
		repoStartLocks:      make(map[string]*sync.Mutex),
		targetLocks:         make(map[string]*sync.Mutex),
		rootEnsureStates:    make(map[string]*rootEnsureState),
		rootKilledAt:        make(map[string]time.Time),
		killsInFlight:       make(map[string]struct{}),
		lostRestoreStates:   make(map[string]*lostRestoreState),
		limitResumeStates:   make(map[string]*limitResumeState),
		instanceOpLocks:     make(map[string]*sync.Mutex),
		pausedPolls:         make(map[string]time.Time),
		events:              newEventsHub(),
	}, nil
}

// RestoreInstances loads every repo's persisted instances into the manager
// and marks it ready. This is the slow part of daemon startup — restoring a
// remote-hook session shells out to the repo's list_cmd (often ssh) per
// session — which is why RunDaemon runs it only after the control socket is
// bound (#829). Replacing the instance map wholesale is safe: every RPC that
// mutates it is gated on Ready, and the refresh poll loop starts after the
// restore completes.
func (m *Manager) RestoreInstances() error {
	instances, err := refreshDaemonInstances(nil)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.instances = instances
	m.mu.Unlock()
	m.readyOnce.Do(func() { close(m.ready) })
	return nil
}

// Ready reports whether the initial instance restore has completed.
func (m *Manager) Ready() bool {
	select {
	case <-m.ready:
		return true
	default:
		return false
	}
}

func (m *Manager) RefreshInstances() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.refreshLocked()
}

func (m *Manager) InstancesSnapshot() []*session.Instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	return daemonInstances(m.instances)
}

func (m *Manager) SaveInstances() error {
	return m.storage.SaveInstances(m.InstancesSnapshot())
}

// Snapshot returns the authoritative InstanceData for every session the manager
// owns, scoped to repoID (all repos when repoID is empty). It is the read side
// of the single-writer model (#960 PR 3): the manager's in-memory instance map
// IS the source of truth, so the TUI mirrors this projection instead of
// re-reading instances.json. Pure read — it copies the instance pointers under
// m.mu, then serializes each via ToInstanceData (which takes the instance's own
// lock) OUTSIDE m.mu so a slow serialize never blocks a concurrent mutation.
// Results are ordered by (repo, title) key for a stable diff, so the TUI
// reconcile does not repaint on map-iteration jitter.
func (m *Manager) Snapshot(repoID string) []session.InstanceData {
	m.mu.Lock()
	keys := make([]string, 0, len(m.instances))
	for key := range m.instances {
		if repoID != "" {
			rid, _ := splitDaemonInstanceKey(key)
			if rid != repoID {
				continue
			}
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	insts := make([]*session.Instance, 0, len(keys))
	for _, key := range keys {
		if inst := m.instances[key]; inst != nil {
			insts = append(insts, inst)
		}
	}
	m.mu.Unlock()

	data := make([]session.InstanceData, 0, len(insts))
	for _, inst := range insts {
		data = append(data, inst.ToInstanceData())
	}
	return data
}

func (m *Manager) refreshLocked() error {
	refreshed, err := refreshDaemonInstances(m.instances)
	if err != nil {
		return err
	}
	m.instances = refreshed
	return nil
}

func (m *Manager) startLockForRepo(repoID string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()

	lock := m.repoStartLocks[repoID]
	if lock == nil {
		lock = &sync.Mutex{}
		m.repoStartLocks[repoID] = lock
	}
	return lock
}

// opLockFor returns the per-session operation lock serializing kill teardown
// against Lost-recovery and prompt writes for one daemon instance key (#1108 PR
// 2, #1473).
func (m *Manager) opLockFor(key string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()

	lock := m.instanceOpLocks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		m.instanceOpLocks[key] = lock
	}
	return lock
}
