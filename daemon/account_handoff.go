package daemon

// account_handoff.go wires the shadow decision engine (#6776, slice 4) onto
// the daemon: its own ~45-second ticker (never the poll goroutine — a durable
// scan and rollout reads must not suspend RefreshStatuses, the same reason
// the proof reconcile has its own), facts assembled ONLY from the status
// snapshot the poll already produced, and edge-triggered rows to the shadow
// event log. It never moves a session and never opts in unless the resolved
// [account_handoff] mode is "shadow".

import (
	"errors"
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
		manager.accountHandoffDiscover()
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

// accountHandoffDiscover recomputes "is shadow opted in anywhere" from the
// already-loaded global config plus each live session's resolved project
// config — the ONLY inputs an opt-in can arrive through (#6817 round-2
// review). It runs at daemon start and on config apply, so a hand-edited
// personal-project file takes effect on the next `af config` write or
// daemon restart — documented, never polled.
func (m *Manager) accountHandoffDiscover() {
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
	governed, err := m.accountHandoffEngine().Tick(m.accountHandoffInputs())
	if err != nil {
		m.warn().Printf("account-handoff engine tick: %v", err)
	}
	m.handoffActive.Store(m.handoffActive.Load() || governed > 0)
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
		DeliverReport: func(spec string, recipient handoffengine.SessionInput, rows []handoff.EventRow) {
			m.enqueueHandoffReport(spec, recipient.Key, rows)
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
				Key:            sessionKey,
				Title:          inst.Title,
				RepoID:         repoID,
				RepoRoot:       repoRoot,
				Agent:          agent,
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
				CarryPossible:  session.CarrySupportedAgent(agent) && !isRemoteWorkspace(inst),
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
		if d.in.Agent == tmux.ProgramCodex && d.in.Started && d.in.Live &&
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

// handoffReportJob is one resolved report_to delivery: the spec the engine
// used, the recipient's stable session key, and the rows emitted for that
// spec this tick.
type handoffReportJob struct {
	spec string
	key  string
	rows []handoff.EventRow
}

// reportFlushInterval caps the digest cadence (R15): at most one digest a
// minute per recipient; halted and move-failed jump the window.
var reportFlushInterval = time.Minute

// reportImmediateTTL bounds how long a once-only immediate signature stays
// spent (#6818 review): a halt/move-failure incident is a fresh one after
// this long, and its report must fire again.
var reportImmediateTTL = 24 * time.Hour

// reportPendingCap bounds rows held for one recipient while delivery waits —
// a recipient attached for hours cannot grow the queue without bound.
const reportPendingCap = 512

// reportSendOutcome tells the worker what one send attempt did:
// delivered/dropped retire the rows; held keeps them for the next flush —
// a held delivery waits rather than retrying (#6818 review).
type reportSendOutcome int

const (
	reportSent reportSendOutcome = iota
	reportHeld
	reportDropped
)

// reportPending is one recipient's digest backlog plus the spec that
// produced it — a config change to mode or report_to invalidates it.
type reportPending struct {
	spec string
	rows []handoff.EventRow
}

// startHandoffReportLoop runs the report_to delivery worker. It owns the
// prompt sends so the decision ticker never blocks on a remote transport:
// the ticker only ever does a non-blocking channel write.
func startHandoffReportLoop(manager *Manager, stopCh <-chan struct{}, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		flush := time.NewTicker(reportFlushInterval)
		defer flush.Stop()
		pending := map[string]*reportPending{}  // recipient key → digest backlog
		sentImmediate := map[string]time.Time{} // signature → when delivered
		for {
			select {
			case <-stopCh:
				return
			case job := <-manager.handoffReportCh:
				for _, row := range job.rows {
					if handoff.ImmediateReport(row) {
						sig := handoff.EventRowSignature(row)
						if _, sent := sentImmediate[sig]; sent {
							continue
						}
						switch manager.deliverHandoffReport(job.key, handoff.ReportLine(row)) {
						case reportSent, reportDropped:
							// A dropped immediate is spent like a sent one —
							// once-only means never retried, not retried
							// forever.
							sentImmediate[sig] = nowFunc()
						case reportHeld:
							// Attached recipient: the row waits inside the
							// digest backlog and still delivers exactly once.
							manager.queueReportRow(pending, job, row)
						}
						continue
					}
					manager.queueReportRow(pending, job, row)
				}
			case <-flush.C:
				for sig, at := range sentImmediate {
					if nowFunc().Sub(at) >= reportImmediateTTL {
						delete(sentImmediate, sig)
					}
				}
				for key, entry := range pending {
					rows := manager.liveReportRows(entry)
					if len(rows) == 0 {
						delete(pending, key)
						continue
					}
					switch manager.deliverHandoffReport(key, handoff.ReportDigest(rows)) {
					case reportHeld:
						entry.rows = rows
					default:
						delete(pending, key)
					}
				}
			}
		}
	}()
}

// queueReportRow appends one row to the recipient's backlog under its cap.
// A job whose spec differs from the backlog's replaces it — the older rows
// were emitted under a report_to or mode the config no longer has.
func (m *Manager) queueReportRow(pending map[string]*reportPending, job handoffReportJob, row handoff.EventRow) {
	entry := pending[job.key]
	if entry == nil || entry.spec != job.spec {
		entry = &reportPending{spec: job.spec}
		pending[job.key] = entry
	}
	if len(entry.rows) >= reportPendingCap {
		return
	}
	entry.rows = append(entry.rows, row)
}

// liveReportRows filters a digest backlog down to rows whose emitting
// session still wants this spec: the emitter must resolve to a config still
// in shadow mode whose report_to is the spec the rows were emitted under
// (#6818 review — pending digests die when mode or report_to changes). A
// row whose emitter is gone drops with it.
func (m *Manager) liveReportRows(entry *reportPending) []handoff.EventRow {
	m.mu.Lock()
	roots := make(map[string]string, len(entry.rows))
	for _, row := range entry.rows {
		if _, ok := roots[row.SessionKey]; ok {
			continue
		}
		for k, inst := range m.instances {
			repoID, _ := splitDaemonInstanceKey(k)
			if inst != nil && stableSessionKey(repoID, inst) == row.SessionKey {
				roots[row.SessionKey] = inst.GetRepoPath()
				if roots[row.SessionKey] == "" {
					roots[row.SessionKey] = inst.Path
				}
				break
			}
		}
	}
	m.mu.Unlock()
	// Resolve once per project root per flush (#6818 round-2 review): a
	// backlog holds many rows from few emitters, and resolution walks the
	// project's config file — per-row resolution is a disk read per line.
	resolved := map[string]*config.ResolvedConfig{}
	var out []handoff.EventRow
	for _, row := range entry.rows {
		root, ok := roots[row.SessionKey]
		if !ok || root == "" {
			continue
		}
		cfg, seen := resolved[root]
		if !seen {
			var err error
			cfg, err = config.ResolveConfigForIdentityDecisionFromGlobal(root, m.Config())
			if err != nil {
				cfg = nil
			}
			resolved[root] = cfg
		}
		if cfg == nil ||
			cfg.AccountHandoff.Mode != config.AccountHandoffModeShadow ||
			cfg.AccountHandoff.ReportTo != entry.spec {
			continue
		}
		out = append(out, row)
	}
	return out
}

// enqueueHandoffReport hands the worker one job. The send is non-blocking by
// contract: a saturated queue drops the rows with a warn rather than stall
// the engine tick behind an operator's recipient.
func (m *Manager) enqueueHandoffReport(spec, key string, rows []handoff.EventRow) {
	select {
	case m.handoffReportCh <- handoffReportJob{spec: spec, key: key, rows: rows}:
	default:
		m.warn().Printf("account-handoff report queue full — dropped %d row(s) for %s", len(rows), key)
	}
}

// deliverHandoffReport sends one fixed-template prompt to the resolved
// recipient session through the SAME guarded path any automated delivery
// takes (#6818 review): the attach lease defers while a TUI is typing in the
// pane, and sendPromptGuarded carries the operation lock, teardown and
// pending-swap checks, the automated liveness gate — which parks a
// limit-reached recipient instead of answering its picker — and the
// send-confirmation record. A vanished recipient drops; a held one reports
// reportHeld so the worker waits for detach instead of retrying into a
// different session.
func (m *Manager) deliverHandoffReport(key, text string) reportSendOutcome {
	m.mu.Lock()
	var target *session.Instance
	var repoID string
	for k, inst := range m.instances {
		rid, _ := splitDaemonInstanceKey(k)
		if inst != nil && stableSessionKey(rid, inst) == key {
			target, repoID = inst, rid
			break
		}
	}
	m.mu.Unlock()
	if target == nil {
		m.warn().Printf("account-handoff report recipient %s is gone — dropped", key)
		return reportDropped
	}
	// The attach/composer revalidation runs INSIDE sendPromptGuarded's
	// guard — i.e. under the recipient's operation lock, immediately
	// before the liveness gate and the submit (#6818 round-3 review): a
	// user who attaches and starts typing while the request waits on
	// that lock cannot be pasted over. The cheap pre-checks still run
	// first so a recipient that never frees up returns held without
	// queueing on the lock at all.
	if m.isPollPaused(repoID, target.Title, target.ID) {
		return reportHeld
	}
	guard := func(inst *session.Instance, rid string) error {
		if m.isPollPaused(rid, inst.Title, inst.ID) {
			return errReportHeld
		}
		if !inst.Started() {
			return errReportHeld // no runtime ⇒ no provable-empty composer
		}
		if !reportComposerProvableEmpty(inst) {
			return errReportHeld
		}
		return nil
	}
	status, err := m.sendPromptGuarded(SendPromptRequest{
		ID: target.ID, Title: target.Title, RepoID: repoID,
		Prompt: text, TaskOrigin: true,
	}, guard)
	if err != nil {
		if errors.Is(err, errReportHeld) {
			return reportHeld
		}
		m.warn().Printf("account-handoff report to %q: %v", target.Title, err)
		return reportDropped
	}
	if status != session.PromptDelivered {
		m.warn().Printf("account-handoff report to %q: delivery status %s", target.Title, status)
	}
	return reportSent
}

// errReportHeld marks a guard veto that means "wait", not "drop" — the
// worker keeps the rows for the next flush.
var errReportHeld = errors.New("account-handoff report held")

// reportComposerProvableEmpty reads the recipient's composer through a
// NON-consuming preview (#6818 round-3 review): SnapshotAgent would
// swallow the status poll's change notification, so the poll's churn
// accounting and first-update detection must not ride on this read. A
// capture failure, a missing agent tab, or a non-empty OR unprovable
// composer all answer false — the report's C-u would otherwise erase a
// draft the attach lease could not see.
func reportComposerProvableEmpty(inst *session.Instance) bool {
	tabID := ""
	for _, t := range inst.GetTabs() {
		if t.Kind == session.TabKindAgent {
			tabID = t.ID
			break
		}
	}
	if tabID == "" {
		return false
	}
	snap, err := inst.AgentServer().PreviewByID(tabID, false)
	if err != nil {
		return false
	}
	draft, provable := tmux.ComposerDraftPresent(snap.Content)
	return !draft && provable
}
