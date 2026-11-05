package daemon

// account_handoff.go wires the shadow decision engine (#6776, slice 4) onto
// the daemon: its own ~45-second ticker (never the poll goroutine — a durable
// scan and rollout reads must not suspend RefreshStatuses, the same reason
// the proof reconcile has its own), facts assembled ONLY from the status
// snapshot the poll already produced, and edge-triggered rows to the shadow
// event log. It never moves a session and never opts in unless the resolved
// [account_handoff] mode is "shadow".

import (
	"strings"
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
// Discovery — "is shadow opted in anywhere" — runs ONCE here at start and on
// every config apply, never on a timer (#6817 round-2 review): an opted-out
// box must not pay a periodic instance walk, lstat chain, or config resolve
// for machinery it never asked for.
func startAccountHandoffEngineLoop(manager *Manager, stopCh <-chan struct{}, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		accountHandoffDiscovery(manager)
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
// capture" is itself a fact (composer state unprovable → draft-blocked). The
// handoffActive gate is the "off is off" half of the contract: with no
// opted-in config anywhere, a status probe must not even take m.mu on the
// engine's behalf.
func (m *Manager) noteHandoffObservation(key string, captured bool, content string) {
	if !m.handoffActive.Load() {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.handoffObserved == nil {
		m.handoffObserved = make(map[string]handoffObservation)
	}
	m.handoffObserved[key] = handoffObservation{captured: captured, at: nowFunc(), content: content}
}

// accountHandoffDiscovery is the indirection a test uses to count discovery
// invocations (#6817 final review): session create and materialise must
// never trigger one — opt-in arrives only through `af config` writes, which
// run discovery on apply.
var accountHandoffDiscovery = (*Manager).accountHandoffDiscover

// accountHandoffDiscover recomputes "is shadow opted in anywhere" from the
// already-loaded global config plus each live session's resolved project
// config — the ONLY inputs an opt-in can arrive through (#6817 round-2
// review). It runs at daemon start and on config apply, so a hand-edited
// personal-project file takes effect on the next `af config` write or
// daemon restart — documented, never polled.
func (m *Manager) accountHandoffDiscover() {
	// Compute AND store under the one lock (#6817 final review): two
	// discoveries queue in arrival order and each resolves config at its
	// own lock time, so an older pass can never overwrite a newer apply's
	// answer with a stale snapshot.
	m.handoffDiscoverMu.Lock()
	defer m.handoffDiscoverMu.Unlock()
	active := false
	if cfg := m.Config(); cfg != nil && cfg.AccountHandoffEnabled() {
		active = true
	}
	if !active {
		m.mu.Lock()
		roots := make([]string, 0, len(m.instances))
		for _, inst := range m.instances {
			if inst == nil {
				continue
			}
			root := inst.GetRepoPath()
			if root == "" {
				root = inst.Path
			}
			if root != "" {
				roots = append(roots, root)
			}
		}
		m.mu.Unlock()
		seen := map[string]bool{}
		for _, root := range roots {
			if seen[root] {
				continue
			}
			seen[root] = true
			cfg, err := config.ResolveConfigForIdentityDecisionFromGlobal(root, m.Config())
			if err == nil && cfg != nil && cfg.AccountHandoff.Mode == config.AccountHandoffModeShadow {
				active = true
				break
			}
		}
	}
	m.handoffActive.Store(active)
}

// accountHandoffTick is one shadow pass: gather the session inputs from the
// manager's own records, run the engine, warn on failure. Every leg failing
// is a warn-and-retry — the next tick assembles fresh facts from scratch.
//
// The gate is the review's "off isn't off" property: inactive ticks do
// literally nothing — no walk, no resolution — because discovery at start
// and on apply is the only way active turns on. A tick right after
// activation also waits for the poll's first observations, so a walled
// session's first verdict never logs a false "draft unprovable".
func (m *Manager) accountHandoffTick() {
	if !m.handoffActive.Load() {
		return
	}
	m.mu.Lock()
	observed := len(m.handoffObserved) > 0
	m.mu.Unlock()
	if !observed {
		return
	}
	// The tick NEVER stores handoffActive (#6817 round-3 review): discovery
	// — start, config apply, session registration — is the only writer, so
	// the most recent config decision always wins. A tick that began while
	// active and finishes after ApplyConfig turned the last opt-in off
	// must not re-activate from its stale governed count; it just returns,
	// and the next tick's gate observes the deactivation.
	if _, err := m.accountHandoffEngine().Tick(m.accountHandoffInputs()); err != nil {
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

// handoffInputDraft is the under-lock half of accountHandoffInputs: the
// input populated from in-memory instance state, plus the instance pointer
// the second phase still needs.
type handoffInputDraft struct {
	in   handoffengine.SessionInput
	inst *session.Instance
}

// accountHandoffInputs gathers one SessionInput per instance from the
// manager's records — the same sources accountSwapOpportunity consults — plus
// the pane observation the status poll last recorded.
//
// m.mu is held for the map walk ONLY (#6817 review): every getter it calls is
// in-memory instance state. SessionCodexHomeDir — the lstat, and for a legacy
// row the RepoFromPath→untimed-git chain — runs AFTER the lock is released,
// and only for the sessions that can consume it: live, started codex seats.
// An archived row's home resolve is skipped entirely.
func (m *Manager) accountHandoffInputs() []handoffengine.SessionInput {
	m.mu.Lock()
	drafts := make([]handoffInputDraft, 0, len(m.instances))
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
		agent := accountSwapAgent(inst)
		drafts = append(drafts, handoffInputDraft{
			in: handoffengine.SessionInput{
				Key:         sessionKey,
				Title:       inst.Title,
				RepoID:      repoID,
				RepoRoot:    repoRoot,
				Agent:       agent,
				Account:     account,
				Auto:        auto,
				Started:     inst.Started(),
				Live:        liveness == session.LiveRunning || liveness == session.LiveReady || parked,
				Working:     liveness == session.LiveRunning,
				Parked:      parked,
				Attached:    m.isPollPaused(repoID, inst.Title, inst.ID),
				VSCodeTab:   instanceHasVSCodeTab(inst),
				PendingOp:   inst.GetInFlightOp() != session.OpNone || pendingSwap || manual,
				SwapCapable: inst.SupportsAutomaticAccountSwap(),
				// CarryPossible is admission's own feasibility predicate
				// (#6817 review): a carry-supporting provider BY LAUNCH
				// COMMAND plus a recorded conversation that belongs to it —
				// a session with no conversation id can never carry.
				CarryPossible:  inst.AccountSwapCarryPossible() && !isRemoteWorkspace(inst),
				LimitAccount:   limitAccountFor(inst),
				WallObservedAt: wallObservedAtFor(inst),
				Captured:       obs.captured,
				CapturedAt:     obs.at,
				Content:        obs.content,
			},
			inst: inst,
		})
	}
	// Prune observations for sessions that are gone — same rule the poll's
	// other keyed maps sweep on (#1794 discipline).
	for key := range m.handoffObserved {
		if !liveKeys[key] {
			delete(m.handoffObserved, key)
		}
	}
	m.mu.Unlock()
	inputs := make([]handoffengine.SessionInput, 0, len(drafts))
	for _, d := range drafts {
		// RuntimeProgram evidence is the gate, not just the binding (#6817
		// round-2 review): an empty runtime program means the row predates
		// launch evidence, and SessionCodexHomeDir's fallback for that case
		// is resolveLaunchProgramForInstance → RepoFromPath → UNTIMED GIT —
		// which must never run on this ticker. A legacy row resolves "" and
		// its drift fact reports unverifiable — the safe direction.
		// The codex-home judgment follows the provider the LAUNCH COMMAND
		// resolves to (#6817 review, the #6811 resolver), not the settled
		// swap enum: a program_overrides session running codex under
		// another agent name still writes a codex home worth reading.
		if d.inst.ResolvedAgent() == tmux.ProgramCodex && d.in.Started && d.in.Live &&
			strings.TrimSpace(d.inst.RuntimeProgram()) != "" {
			d.in.HomeDir = d.inst.SessionCodexHomeDir()
		}
		inputs = append(inputs, d.in)
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
