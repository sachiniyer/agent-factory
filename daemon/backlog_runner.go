package daemon

import (
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/planusage"
)

const (
	// runnerInterval is the least time between two runner passes.
	runnerInterval = 5 * time.Second
	// runnerBackoffBase and runnerBackoffMax bound how long the runner leaves
	// an item alone after its start was refused.
	runnerBackoffBase = time.Minute
	runnerBackoffMax  = time.Hour
)

// errSlotsFull refuses a capped start when the project's slots are all in use.
var errSlotsFull = errors.New("slots full")

// errRunnerOff refuses an automatic start after the runner switch went off.
var errRunnerOff = errors.New("the runner is off")

// errViewIncomplete refuses an automatic start while the project's session view
// is incomplete: a row that failed to load may be a live run holding a slot.
var errViewIncomplete = errors.New("the project's session view is incomplete")

// errNotRunnable refuses an automatic start of an item that is no longer a
// queued, unblocked leaf outside a paused group. It is backlog.ErrNotRunnable,
// so the admission check here and the check inside the start transaction
// (BeginStart) are one outcome to the runner.
var errNotRunnable = backlog.ErrNotRunnable

// runnerState paces runner passes, holds what the runner remembers between
// passes, and remembers the state each project's runner last published.
type runnerState struct {
	inFlight atomic.Bool
	startMu  sync.Mutex
	started  time.Time

	mu        sync.Mutex
	published map[string]backlog.RunnerEvent
	pacer     backlog.StartPacer
	backoff   map[backoffKey]backoff
	// interrupts tracks the runs af stopped at the cap, by session id.
	interrupts map[string]*capInterrupt
	// ledgerWarned is set while the account limit ledger cannot be read, so
	// the warning is logged once.
	ledgerWarned bool

	// startLocks serializes capped starts per project, so two of them cannot
	// both see the last free slot.
	startLocks sync.Map
}

type backoffKey struct {
	repoID string
	id     int
}

type backoff struct {
	fails int
	until time.Time
}

type capInterrupt struct {
	repoID string
	// gen is the run generation that exceeded the cap: a retry applies only
	// while that run is still the session's latest (an answer that authorized
	// the continuation arms a new one).
	gen     int
	passes  int
	retried bool
}

func (s *runnerState) due(now time.Time) bool {
	s.startMu.Lock()
	defer s.startMu.Unlock()
	if now.Sub(s.started) < runnerInterval {
		return false
	}
	s.started = now
	return true
}

// changed records ev as the project's runner state and reports whether it
// differs from the last one. A project never seen counts as off.
func (s *runnerState) changed(ev backlog.RunnerEvent) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.published == nil {
		s.published = map[string]backlog.RunnerEvent{}
	}
	prev, ok := s.published[ev.RepoID]
	if !ok {
		prev = backlog.RunnerEvent{RepoID: ev.RepoID, State: backlog.RunnerOff, Reason: "Items run only when you start them."}
	}
	s.published[ev.RepoID] = ev
	return prev != ev
}

func (s *runnerState) startLimit(fn func(*backlog.StartPacer) int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(&s.pacer)
}

func (s *runnerState) noteStarted(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pacer.Started(at)
}

// backingOff reports whether the runner leaves item id alone for now.
func (s *runnerState) backingOff(repoID string, now time.Time) func(int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	held := map[int]bool{}
	for k, b := range s.backoff {
		if k.repoID == repoID && now.Before(b.until) {
			held[k.id] = true
		}
	}
	return func(id int) bool { return held[id] }
}

// startFailed backs off from an item whose start was refused: one minute,
// doubling up to an hour.
func (s *runnerState) startFailed(repoID string, id int, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.backoff == nil {
		s.backoff = map[backoffKey]backoff{}
	}
	k := backoffKey{repoID, id}
	b := s.backoff[k]
	wait := runnerBackoffBase << min(b.fails, 6)
	b.fails++
	b.until = now.Add(min(wait, runnerBackoffMax))
	s.backoff[k] = b
}

func (s *runnerState) startOK(repoID string, id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.backoff, backoffKey{repoID, id})
}

// RunBacklogs runs one runner pass on its own goroutine when one is due. The
// poll loop calls it after RefreshStatuses; the pass never blocks the loop.
func (m *Manager) RunBacklogs() {
	st := &m.backlog.runner
	if !st.due(time.Now()) || !st.inFlight.CompareAndSwap(false, true) {
		return
	}
	if !m.launchBackgroundMutation(func(<-chan struct{}) {
		defer st.inFlight.Store(false)
		m.runBacklogPass(time.Now())
	}) {
		st.inFlight.Store(false)
	}
}

func (m *Manager) runBacklogPass(now time.Time) {
	home, err := config.GetConfigDir()
	if err != nil {
		return
	}
	paths, _ := filepath.Glob(filepath.Join(home, "repos", "*", backlog.BacklogFileName))
	for _, p := range paths {
		m.runBacklogProject(filepath.Base(filepath.Dir(p)), now)
	}
}

// runBacklogProject is one pass for one project: stop runs over the cap per
// run, then, with the runner on and its gates open, start queued items into
// the free slots.
func (m *Manager) runBacklogProject(repoID string, now time.Time) {
	b, err := backlog.Load(repoID)
	if err != nil {
		m.warn().Printf("backlog runner for project %s: %v", repoID, err)
		return
	}
	budget, err := backlog.LoadBudget()
	if err != nil {
		m.warn().Printf("backlog runner: %v", err)
		return
	}
	rs := &m.backlog.runner
	states := b.DeriveAll(backlogHost{m: m}.Facts(repoID))
	usage := m.backlog.meter.current()
	if !m.planUsageOn() {
		usage = disabledUsage() // the runner then reads the closed plan_usage_off gate
	} else if b.Settings.Runner {
		usage = m.meterUsageWithin(meterInterval)
	}
	m.capBacklogRuns(repoID, b, states, usage, budget.MaxPerRun)
	m.retryCapInterrupts(repoID)

	choose := func(usage planusage.Usage) backlog.RunnerPlan {
		in := backlog.RunnerInput{States: states, Used: m.backlogSlotsUsed(repoID), Limit: -1,
			Skip: rs.backingOff(repoID, now)}
		if b.Settings.Runner {
			gate, account := m.backlogGate(repoID, *budget, usage, now)
			in.Gate = gate
			in.Limit = rs.startLimit(func(p *backlog.StartPacer) int { return p.Limit(usageFor(usage, account), now) })
		}
		p := b.PlanRunner(in)
		p.Start = m.selectStarts(repoID, b, states, p, *budget, usage, now)
		return p
	}
	p := pickStartsFresh(usage, func() planusage.Usage { return m.meterUsageWithin(meterFreshFor) }, choose)
	if m.backlogViewIncomplete(repoID) {
		p.Start = nil // never admit automatic work against a session count that may be short
	}
	for _, id := range p.Start {
		_, err := m.startBacklogItemCapped(backlog.StartRequest{RepoID: repoID, ID: id}, backlog.StartedByRunner)
		if errors.Is(err, errSlotsFull) || errors.Is(err, errRunnerOff) || errors.Is(err, errViewIncomplete) {
			break
		}
		if errors.Is(err, errNotRunnable) {
			continue // the user changed it since the pass chose it; not a failed start
		}
		if err != nil {
			rs.startFailed(repoID, id, now)
			m.warn().Printf("backlog runner: start %s in project %s: %v", backlog.FormatID(id), repoID, err)
			continue
		}
		rs.startOK(repoID, id)
		rs.noteStarted(now)
	}
	ev := backlog.RunnerEvent{RepoID: repoID, State: p.View.State, Reason: p.View.Reason, Gate: p.View.Gate}
	if rs.changed(ev) {
		m.publishEvent(agentproto.EventBacklogRunner, ev)
	}
}

// startBacklogItemCapped starts an item only while the project has a free
// slot. The check and the slot the start takes happen under the project's
// start lock, so concurrent runner and run starts cannot pass max_running.
func (m *Manager) startBacklogItemCapped(req backlog.StartRequest, by string) (backlog.RunRef, error) {
	ref, err := m.backlogService().Resolve(req.RepoID, req.RepoPath)
	if err != nil {
		return backlog.RunRef{}, err
	}
	lock, _ := m.backlog.runner.startLocks.LoadOrStore(ref.ID, &sync.Mutex{})
	mu := lock.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()
	b, err := backlog.Load(ref.ID)
	if err != nil {
		return backlog.RunRef{}, err
	}
	// The pass chose this item before waiting for the lock; the user may have
	// turned the runner off since. Explicit user starts bypass the switch on
	// purpose, an automatic one must honour it at the admission boundary.
	if by == backlog.StartedByRunner && !b.Settings.Runner {
		return backlog.RunRef{}, errRunnerOff
	}
	// Likewise the item itself: the user may have drafted it, added a
	// dependency or paused its group while this start waited for the lock;
	// explicit starts ignore all three on purpose, an automatic one must not
	// override them.
	if by == backlog.StartedByRunner && !b.RunnerEligible(req.ID, b.DeriveAll(backlogHost{m: m}.Facts(ref.ID))) {
		return backlog.RunRef{}, errNotRunnable
	}
	if by == backlog.StartedByRunner && m.startUnmeterable(ref.ID, b, req.ID) {
		return backlog.RunRef{}, errNotRunnable
	}
	if by == backlog.StartedByRunner && m.backlogViewIncomplete(ref.ID) {
		return backlog.RunRef{}, errViewIncomplete
	}
	if m.backlogSlotsUsed(ref.ID) >= b.Settings.MaxRunning {
		return backlog.RunRef{}, errSlotsFull
	}
	return m.StartBacklogItem(req, by)
}

// RunBacklog is one runner pass now for a user act: it ignores the runner
// switch and the budget gates but not the slots, and starts up to req.Count
// items (default 1).
func (m *Manager) RunBacklog(req backlog.RunRequest) (backlog.RunResponse, error) {
	svc := m.backlogService()
	ref, err := svc.Resolve(req.RepoID, req.RepoPath)
	if err != nil {
		return backlog.RunResponse{}, err
	}
	b, err := backlog.Load(ref.ID)
	if err != nil {
		return backlog.RunResponse{}, err
	}
	states := b.DeriveAll(backlogHost{m: m}.Facts(ref.ID))
	pick, reason := b.RunNow(states, m.backlogSlotsUsed(ref.ID), req.Count)
	out := backlog.RunResponse{Started: []int{}, Reason: reason}
	for _, id := range pick {
		start := backlog.StartRequest{RepoID: ref.ID, ID: id}
		if req.RepoPath != "" {
			start = backlog.StartRequest{RepoPath: req.RepoPath, ID: id}
		}
		// A pass start, not a start by hand: its transaction honours a pause
		// that lands after RunNow chose the batch.
		_, err := m.startBacklogItemCapped(start, backlog.StartedByRunPass)
		if errors.Is(err, errSlotsFull) {
			if len(out.Started) == 0 {
				out.Reason = backlog.SlotsFull(b.Settings.MaxRunning)
			}
			break
		}
		if errors.Is(err, errNotRunnable) {
			continue // its group was paused since the pass chose it; not a failed start
		}
		if err != nil {
			if len(out.Started) == 0 {
				return out, err
			}
			m.warn().Printf("backlog run: start %s: %v", backlog.FormatID(id), err)
			continue
		}
		out.Started = append(out.Started, id)
	}
	return out, nil
}

func (s *controlServer) RunBacklog(req RunBacklogRequest, resp *RunBacklogResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	out, err := s.manager.RunBacklog(req)
	if err != nil {
		return err
	}
	*resp = out
	return nil
}

// selectStarts picks the items this pass starts, each judged against the account
// it would actually spend: a continuation spends its live session's account, a new
// session the project's default. The runner plan above gates and paces on the
// default alone, so its choice is only used when a gate holds regardless of the
// account (plan usage off, no meter); otherwise the whole queue is scanned, so an
// item whose account is closed or stale never hides one whose account is open
// (Codex P2s on #6300). Pacing is per account and counts this pass's picks.
func (m *Manager) selectStarts(repoID string, b *backlog.Backlog, states map[int]backlog.Derived, p backlog.RunnerPlan, budget backlog.Budget, usage planusage.Usage, now time.Time) []int {
	if !b.Settings.Runner || p.View.Gate == backlog.GatePlanUsageOff || p.View.Gate == backlog.GateMeterUnavailable {
		return p.Start
	}
	free := backlog.FreeSlots(b.Settings.MaxRunning, m.backlogSlotsUsed(repoID))
	if free <= 0 {
		return nil
	}
	defAcct, defErr := m.itemAccount(repoID)
	rs := &m.backlog.runner
	var out []int
	picked := map[string]int{}
	gates := map[string]backlog.Gate{}
	for _, id := range b.Runnable(states, len(b.Items), rs.backingOff(repoID, now)) {
		if len(out) >= free {
			break
		}
		if m.startUnmeterable(repoID, b, id) {
			continue
		}
		acct := m.continuationAccount(repoID, b, id)
		if acct == "" {
			if defErr != nil {
				continue // a new session needs a resolvable account
			}
			acct = defAcct
		}
		g, ok := gates[acct]
		if !ok {
			g = m.backlogGateFor(acct, budget, usage, now)
			gates[acct] = g
		}
		if !g.Open() {
			continue
		}
		limit := rs.startLimit(func(pc *backlog.StartPacer) int { return pc.Limit(usageFor(usage, acct), now) })
		if limit >= 0 && picked[acct] >= limit {
			continue
		}
		picked[acct]++
		out = append(out, id)
	}
	return out
}

// pickStartsFresh chooses starts on the reading it has and, when that picks any,
// takes a fresh reading and chooses again, returning only the second choice. The
// default account's plan can be empty while a continuation on another account is
// picked, so the refresh follows the final choice and not the default plan: a
// start is never admitted on a cached reading that a fresh one would close (Codex
// P2 on #6300).
func pickStartsFresh(usage planusage.Usage, refresh func() planusage.Usage, choose func(planusage.Usage) backlog.RunnerPlan) backlog.RunnerPlan {
	p := choose(usage)
	if len(p.Start) > 0 {
		p = choose(refresh())
	}
	return p
}
