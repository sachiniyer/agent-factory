package daemon

import (
	"fmt"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/handoff"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/rateindex"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// The refutation half of refutable limit evidence (#4404, epic #6776 slice
// 1d). A wall observation is durable, but it is only ever a claim about the
// past: the account's own usage record can prove it ended. Proof is a usage
// READING the merged indexer (internal/rateindex, #6794) reads out of the
// account's own rollout store — the structural fact the agent wrote when its
// server answered — never an inference from what a pane happens to show. Two
// review rounds of pane-derived proof broke in new ways (a busy frame is a
// turn in flight, not an accepted request; a retrying-after-429 spinner still
// looks busy), so the pane is out of the proof path entirely: this is the
// design's §2 source and the only proof that counts.
//
// An agent without a verified usage record — claude, devin, everything but
// codex today — keeps the pre-slice behavior: its walls end at the claimed
// reset, or never when the banner named none.
//
// The reconcile owns the heavy work — the durable evidence scan (~12 MB of
// instance rows on a busy box) plus one bounded indexer refresh per walled
// account — so it runs on its own ticker, never the 1 Hz poll goroutine.

// accountLimitProofReconcileInterval is the cadence of the dedicated
// reconcile goroutine — the durable scan it owns is far too heavy for the
// poll loop, so the poll never waits on it. Loose enough that a day-long wall
// does not notice the wait. A package var so tests can compress it.
var accountLimitProofReconcileInterval = 45 * time.Second

// startAccountLimitReconcileLoop runs the proof reconcile on its own ticker
// goroutine, registered on the daemon's shutdown wait group like the orphan
// sweep's deferred worker — its durable scan and rollout reads must never
// suspend RefreshStatuses, lost-session recovery, or limit resumes (#6802
// review).
func startAccountLimitReconcileLoop(manager *Manager, stopCh <-chan struct{}, wg *sync.WaitGroup) {
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(accountLimitProofReconcileInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stopCh:
				return
			case <-ticker.C:
				manager.reconcileAccountLimitProofs()
			}
		}
	}()
}

// accountLimitIdentity names one provider-scoped identity the ledger keys on.
type accountLimitIdentity struct {
	agent   string
	account string
}

// accountLimitProofInput gathers what a usage reading is judged against:
// the resolved wall threshold, the carry watermarks that quarantine rollout
// prefixes copied from a source account, and the AF home the account
// directories sit under. Any leg being unreadable fails the whole pass
// closed: a guessed threshold or an unknown watermark could free an account
// on a poisoned reading — one inside a carried prefix holds the SOURCE
// account's numbers wearing the target's name (#6794).
type accountLimitProofInput struct {
	home            string
	wallPercent     int
	carriedPrefixes map[string]int64
}

func loadAccountLimitProofInputs() (accountLimitProofInput, error) {
	home, err := config.GetConfigDir()
	if err != nil {
		return accountLimitProofInput{}, err
	}
	// The retraction is ungated, so the threshold is the RESOLVED leaf: the
	// compiled default when no [account_handoff] table exists — absent ≡
	// off ≡ the design's defaults, never a guessed number. An unreadable
	// config fails the pass: the operator's own wall line is unknowable.
	wallPercent := config.DefaultConfig().AccountHandoff.WallPercent
	load, err := config.LoadConfigReadOnly()
	if err != nil {
		return accountLimitProofInput{}, fmt.Errorf("wall threshold: %w", err)
	}
	if load.Config != nil {
		wallPercent = load.Config.AccountHandoff.WallPercent
	}
	var prefixes map[string]int64
	store, err := handoff.DefaultStore()
	if err != nil {
		return accountLimitProofInput{}, err
	}
	state, err := store.Load()
	if err != nil {
		// A missing state file is an empty map — an af home that never
		// carried has no watermarks and every byte is eligible. A CORRUPT
		// one fails the pass: an observed reading might sit inside a carried
		// prefix, and the carried bytes are the other account's evidence.
		return accountLimitProofInput{}, fmt.Errorf("carried-prefix watermarks: %w", err)
	}
	prefixes = state.CarriedPrefixes
	return accountLimitProofInput{
		home:            home,
		wallPercent:     wallPercent,
		carriedPrefixes: prefixes,
	}, nil
}

// accountUsageReadingSource reports whether the agent persists a usage record
// the indexer can read. Only codex writes the rate_limits rollout record
// (#6794); every other account-capable agent keeps today's behavior — its
// walls end at the claimed reset, or never (#6802 review).
func accountUsageReadingSource(agent string) bool {
	return agent == tmux.ProgramCodex
}

// usageReadingProof decides whether one indexer refresh proves the account
// was serving at a specific instant. The newest claim must be OBSERVED —
// "none" is no evidence, "unknown" is a claim that cannot be trusted, and
// neither frees a wall — timestamped no later than now (a future-dated record
// is as untrustworthy as a future-dated retraction), and must carry at least
// one window with EVERY reported window below the wall threshold: "no
// windows" is not a zero reading, and one hot window means the account is
// still at its wall by its own newest word. The proof instant is the record's
// own timestamp — ordering runs on when the provider made the claim, never
// on when af happened to read it.
func usageReadingProof(reading rateindex.AccountReading, wallPercent int, now time.Time) (time.Time, bool) {
	if reading.Status != rateindex.StatusObserved {
		return time.Time{}, false
	}
	recorded := reading.Reading.RecordedAt
	if recorded.IsZero() || recorded.After(now) {
		return time.Time{}, false
	}
	if len(reading.Reading.Windows) == 0 {
		return time.Time{}, false
	}
	for _, window := range reading.Reading.Windows {
		if window.UsedPercent >= float64(wallPercent) {
			return time.Time{}, false
		}
	}
	return recorded, true
}

// usageIndexer lazily builds the rollout indexer the reconcile refreshes
// through. It is kept on the manager because its per-file memoization is the
// point: an unchanged rollout costs zero bytes on the next pass, which is
// what makes a 45-second refresh cadence cheap. Called only from the
// reconcile goroutine (and tests driving it directly).
func (m *Manager) usageIndexer() *rateindex.Indexer {
	if m.rateIndexer == nil {
		m.rateIndexer = rateindex.New()
	}
	return m.rateIndexer
}

// reconcileAccountLimitProofs reads every account still excluded by stored
// evidence and asks each one's own usage record whether the wall ended. The
// durable scan plus the rollout refreshes are the expensive part, so this
// runs on the dedicated ticker, never the poll. Stored evidence is the whole
// domain a retraction can act on — durable rows, the process-wide ledger, and
// live rows' own observation lists; a live session parked at its wall
// contributes a live wall that admission keeps regardless, so retraction here
// never un-parks it (#6776 1d property). Every failure aborts the pass —
// nothing is half-decided on a partial picture, and the next tick retries
// from scratch.
func (m *Manager) reconcileAccountLimitProofs() {
	evidence, err := loadAccountLimitEvidenceForSwap()
	if err != nil {
		m.warn().Printf("account-limit proof reconcile could not load durable evidence: %v", err)
		return
	}
	m.mu.Lock()
	instances := make([]*session.Instance, 0, len(m.instances))
	for _, inst := range m.instances {
		instances = append(instances, inst)
	}
	m.mu.Unlock()
	observations := append([]session.AccountLimitObservationData(nil), evidence.Observations...)
	for _, inst := range instances {
		if inst != nil {
			observations = append(observations, inst.AccountLimitObservations()...)
		}
	}
	now := nowFunc()
	// The only identities a reading could free: those with a stored wall
	// still excluding. Accounts with no wall — or whose wall a recorded
	// retraction already defeats — earn no read of their rollout store.
	walled := make(map[accountLimitIdentity]struct{})
	for _, observation := range observations {
		if accountLimitObservationExcludes(observation, observation.Agent, evidence.Retractions, now) {
			walled[accountLimitIdentity{agent: observation.Agent, account: observation.Account}] = struct{}{}
		}
	}
	readable := false
	for identity := range walled {
		if accountUsageReadingSource(identity.agent) {
			readable = true
			break
		}
	}
	if !readable {
		return
	}
	inputs, err := loadAccountLimitProofInputs()
	if err != nil {
		m.warn().Printf("account-limit proof reconcile could not read proof inputs: %v", err)
		return
	}
	var retractions []session.AccountLimitRetractionData
	for identity := range walled {
		if !accountUsageReadingSource(identity.agent) {
			continue
		}
		dir, err := agentaccount.Dir(inputs.home, identity.agent, identity.account)
		if err != nil {
			// A walled identity with no readable account directory has no
			// store to scan — no evidence, so nothing proves it.
			continue
		}
		provenAt, ok := usageReadingProof(
			m.usageIndexer().Refresh(dir, rateindex.Options{CarriedPrefixes: inputs.carriedPrefixes}),
			inputs.wallPercent, now)
		if !ok {
			continue
		}
		candidate := session.AccountLimitRetractionData{
			Agent:    identity.agent,
			Account:  identity.account,
			ProvenAt: provenAt,
		}
		if accountLimitProofDefeatsAStoredWall(candidate, observations, evidence.Retractions, now) {
			retractions = append(retractions, candidate)
		}
	}
	if len(retractions) == 0 {
		return
	}
	if err := retractAccountLimitObservations(retractions); err != nil {
		m.warn().Printf("account-limit proof reconcile could not write retractions: %v", err)
		return
	}
	for _, retraction := range retractions {
		m.info().Printf("retracted stale account-limit evidence for %s account %q: its own usage record shows service below the wall at %s",
			retraction.Agent, retraction.Account, retraction.ProvenAt.UTC().Format(time.RFC3339))
	}
}

// accountLimitProofDefeatsAStoredWall reports whether persisting candidate
// flips at least one still-excluding observation: the wall must currently
// exclude under the retractions already recorded AND be defeated by this
// proof's own instant. The exclusion half is the reconcile-side sibling of
// limitedAccountsForSwap's stored-evidence loops, sharing
// accountLimitObservationExcludes so the write path can never believe
// something admission would not; the defeat half stops a reading that
// predates the wall — the older fact — from littering the ledger with a
// tombstone admission never honors (#6802 review).
func accountLimitProofDefeatsAStoredWall(candidate session.AccountLimitRetractionData, observations []session.AccountLimitObservationData, retractions []session.AccountLimitRetractionData, now time.Time) bool {
	for _, observation := range observations {
		if observation.Agent != candidate.Agent || observation.Account != candidate.Account {
			continue
		}
		if accountLimitObservationExcludes(observation, candidate.Agent, retractions, now) &&
			session.AccountLimitObservationRetracted(observation, candidate) {
			return true
		}
	}
	return false
}
