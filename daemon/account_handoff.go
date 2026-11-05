package daemon

// account_handoff.go wires the shadow decision engine (#6776, slice 4) onto
// the daemon: its own ~45-second ticker (never the poll goroutine — a durable
// scan and rollout reads must not suspend RefreshStatuses, the same reason
// the proof reconcile has its own), facts assembled ONLY from the status
// snapshot the poll already produced, and edge-triggered rows to the shadow
// event log. It never moves a session and never opts in unless the resolved
// [account_handoff] mode is "shadow".

import (
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/handoff"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/handoffengine"
	"github.com/sachiniyer/agent-factory/internal/rateindex"
	"github.com/sachiniyer/agent-factory/quota"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// accountHandoffEngineInterval is the design's shadow cadence — the same ~45s
// rhythm the proof reconcile keeps, so a wall's durable sighting, its usage
// evidence, and the shadow verdict stay within one observed minute of each
// other.
var accountHandoffEngineInterval = 45 * time.Second

// startAccountHandoffEngineLoop runs the shadow engine on its own ticker
// goroutine, registered on the shutdown wait group like the proof reconcile's.
func startAccountHandoffEngineLoop(manager *Manager, stopCh <-chan struct{}, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(accountHandoffEngineInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				manager.accountHandoffTick()
			}
		}
	}()
}

// handoffObservation is the poll's own pane knowledge handed to the engine:
// whether the last snapshot captured a frame, when, and its content. The
// engine evaluates this captured text for banner/working/draft — it never
// asks for the pane itself.
type handoffObservation struct {
	captured bool
	at       time.Time
	content  string
}

// noteHandoffObservation records the capture verdict of one status probe for
// the engine's fact assembly. A failed probe is recorded too — "could not
// capture" is itself a fact (composer state unprovable → draft-blocked).
func (m *Manager) noteHandoffObservation(key string, captured bool, content string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.handoffObserved == nil {
		m.handoffObserved = make(map[string]handoffObservation)
	}
	m.handoffObserved[key] = handoffObservation{captured: captured, at: nowFunc(), content: content}
}

// accountHandoffTick is one shadow pass: gather the session inputs from the
// manager's own records, run the engine, warn on failure. Every leg failing
// is a warn-and-retry — the next tick assembles fresh facts from scratch.
func (m *Manager) accountHandoffTick() {
	if err := m.accountHandoffEngine().Tick(m.accountHandoffInputs()); err != nil {
		m.warn().Printf("account-handoff engine tick: %v", err)
	}
}

// accountHandoffEngine lazily builds the engine and its injected deps. Kept
// on the manager for the same reason usageIndexer is: the rateindex memo and
// the edge tracker are stateful across ticks.
func (m *Manager) accountHandoffEngine() *handoffengine.Engine {
	if m.handoffEngine != nil {
		return m.handoffEngine
	}
	m.handoffEngine = handoffengine.New(handoffengine.Deps{
		Now: nowFunc,
		ResolveConfig: func(repoRoot string) (*config.ResolvedConfig, error) {
			return config.ResolveConfigForIdentityDecisionFromGlobal(repoRoot, m.Config())
		},
		LoadState: func() (handoff.State, error) {
			store, err := handoff.DefaultStore()
			if err != nil {
				return handoff.State{}, err
			}
			return store.Load()
		},
		Registered: func(agent string) ([]string, error) {
			home, err := config.GetConfigDir()
			if err != nil {
				return nil, err
			}
			return agentaccount.List(home, agent)
		},
		LimitedAccounts: func(agent string) (map[string]bool, error) {
			names, err := m.limitedAccountsForSwap(agent, loadAccountLimitEvidenceForSwap)
			if err != nil {
				return nil, err
			}
			set := make(map[string]bool, len(names))
			for _, name := range names {
				set[name] = true
			}
			return set, nil
		},
		AccountDir: func(agent, name string) (string, error) {
			home, err := config.GetConfigDir()
			if err != nil {
				return "", err
			}
			return agentaccount.Dir(home, agent, name)
		},
		ReadUsage: func(dir string, carried map[string]int64) handoff.ReadingFacts {
			return readingFactsFrom(m.refreshAccountUsage(dir, carried))
		},
		DriftKey: func(agent, sourceDir, targetDir string) (string, bool) {
			if agent != tmux.ProgramCodex {
				return "", false
			}
			return agentaccount.CodexRuntimeDriftKey(sourceDir, targetDir)
		},
		DetectWall: func(content, agent string) bool {
			hit, _, _, _ := m.limitDetector.Load().Check(content, agent, nowFunc())
			return hit
		},
		WriteRows: func(rows []handoff.EventRow) error {
			log_, err := handoff.DefaultEventLog()
			if err != nil {
				return err
			}
			return log_.Append(rows)
		},
		LogWarn: m.warn().Printf,
		// The reconcile's own skew constants: the resume grace a resets_at
		// gets before it counts as past, and the write margin a calm record
		// must clear of a wall sighting.
		ResetGrace:  limitResumeGrace,
		ProofMargin: accountLimitProofWriteMargin,
	})
	return m.handoffEngine
}

// accountHandoffInputs gathers one SessionInput per instance from the
// manager's records — the same sources accountSwapOpportunity consults — plus
// the pane observation the status poll last recorded.
func (m *Manager) accountHandoffInputs() []handoffengine.SessionInput {
	m.mu.Lock()
	defer m.mu.Unlock()
	inputs := make([]handoffengine.SessionInput, 0, len(m.instances))
	liveKeys := make(map[string]bool, len(m.instances))
	for key, inst := range m.instances {
		if inst == nil {
			continue
		}
		repoID, _ := splitDaemonInstanceKey(key)
		sessionKey := stableSessionKey(repoID, inst)
		liveKeys[sessionKey] = true
		liveness := inst.GetLiveness()
		parked := liveness == session.LiveLimitReached
		account, auto := inst.AccountSelection()
		_, _, pendingSwap := inst.PendingAccountSwap()
		manual, _ := inst.PendingManualAccountSwap()
		repoRoot := inst.GetRepoPath()
		if repoRoot == "" {
			repoRoot = inst.Path
		}
		obs := m.handoffObserved[sessionKey]
		inputs = append(inputs, handoffengine.SessionInput{
			Key:            sessionKey,
			Title:          inst.Title,
			RepoID:         repoID,
			RepoRoot:       repoRoot,
			Agent:          accountSwapAgent(inst),
			Account:        account,
			Auto:           auto,
			Started:        inst.Started(),
			Live:           liveness == session.LiveRunning || liveness == session.LiveReady || parked,
			Working:        liveness == session.LiveRunning,
			Parked:         parked,
			Attached:       m.isPollPaused(repoID, inst.Title, inst.ID),
			VSCodeTab:      instanceHasVSCodeTab(inst),
			PendingOp:      inst.GetInFlightOp() != session.OpNone || pendingSwap || manual,
			SwapCapable:    inst.SupportsAutomaticAccountSwap(),
			CarryPossible:  session.CarrySupportedAgent(accountSwapAgent(inst)) && !isRemoteWorkspace(inst),
			HomeDir:        inst.SessionCodexHomeDir(),
			LimitAccount:   limitAccountFor(inst),
			WallObservedAt: wallObservedAtFor(inst),
			Captured:       obs.captured,
			CapturedAt:     obs.at,
			Content:        obs.content,
		})
	}
	// Prune observations for sessions that are gone — same rule the poll's
	// other keyed maps sweep on (#1794 discipline).
	for key := range m.handoffObserved {
		if !liveKeys[key] {
			delete(m.handoffObserved, key)
		}
	}
	return inputs
}

// limitAccountFor returns the account this session's durable wall is
// attributed to ("" when ambient or none recorded).
func limitAccountFor(inst *session.Instance) string {
	_, account, ok := inst.LimitIdentity()
	if !ok {
		return ""
	}
	return account
}

// wallObservedAtFor returns the newest durable sighting of this session's
// live wall — the observation's own event time, which calm-reading refutation
// orders itself against. Zero when nothing is durably observed.
func wallObservedAtFor(inst *session.Instance) time.Time {
	agent, account, ok := inst.LimitIdentity()
	if !ok {
		return time.Time{}
	}
	var newest time.Time
	for _, observation := range inst.AccountLimitObservations() {
		if observation.Agent != agent || observation.Account != account {
			continue
		}
		if observation.ObservedAt.After(newest) {
			newest = observation.ObservedAt
		}
	}
	return newest
}

// readingFactsFrom converts the indexer's answer into the leaf's reading
// shape. Complete means every codex window slot reported — the same rule the
// proof reconcile's usageReadingProof applies.
func readingFactsFrom(reading rateindex.AccountReading) handoff.ReadingFacts {
	switch reading.Status {
	case rateindex.StatusObserved:
		facts := handoff.ReadingFacts{
			Status:     handoff.ReadingObserved,
			RecordedAt: reading.Reading.RecordedAt,
			Complete:   true,
		}
		reported := make(map[string]bool, len(reading.Reading.Windows))
		for _, window := range reading.Reading.Windows {
			facts.Windows = append(facts.Windows, handoff.WindowFact{
				Name: window.Name, UsedPercent: window.UsedPercent, ResetsAt: window.ResetsAt,
			})
			reported[window.Name] = true
		}
		for _, name := range quota.CodexWindowSlotNames() {
			if !reported[name] {
				facts.Complete = false
				break
			}
		}
		return facts
	case rateindex.StatusUnknown:
		return handoff.ReadingFacts{Status: handoff.ReadingUnknown}
	default:
		return handoff.ReadingFacts{Status: handoff.ReadingNone}
	}
}
