package daemon

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/handoff"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/internal/rateindex"
	"github.com/sachiniyer/agent-factory/quota"
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

// accountLimitProofWriteMargin is the ordering slack a usage record must
// clear past a wall's last sighting before it may count as proof. The
// record's timestamp is when codex WROTE the line — a token_count snapshot
// lands as a model response completes — so a request accepted before the
// wall can carry a later timestamp: pre-wall service wearing a post-wall
// date. The record shape carries no request-start field, so the supported
// bound is a margin after the sighting (#6802 review). Thirty minutes
// generously bounds one response's write lag while staying nothing against
// the hours these walls run.
var accountLimitProofWriteMargin = 30 * time.Minute

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
	store, err := handoff.DefaultStore()
	if err != nil {
		return accountLimitProofInput{}, err
	}
	railsStore, err := handoff.DefaultRailsStore()
	if err != nil {
		return accountLimitProofInput{}, err
	}
	// One consistent pair of reads: LoadSnapshot double-reads each file
	// and merges worst-case so a carry committing mid-load can only
	// over-exclude claims, never let a superseded watermark stand in for
	// a retired quarantine (#6776 review).
	snap, stateErr, railsErr := handoff.LoadSnapshot(store, railsStore)
	if stateErr != nil {
		// A missing state file is an empty map — an af home that never
		// carried has no watermarks and every byte is eligible. A CORRUPT
		// one fails the pass: an observed reading might sit inside a carried
		// prefix, and the carried bytes are the other account's evidence.
		return accountLimitProofInput{}, fmt.Errorf("carried-prefix watermarks: %w", stateErr)
	}
	if railsErr != nil {
		// Same closed posture for the rails file: its unrecorded-carries
		// quarantine is the record that a carried rollout's claims cannot
		// be trusted AT ALL — a corrupt file hides that record, so the
		// pass cannot know which readings are poison (#6776 review).
		return accountLimitProofInput{}, fmt.Errorf("handoff rails: %w", railsErr)
	}
	return accountLimitProofInput{
		home:            home,
		wallPercent:     wallPercent,
		carriedPrefixes: snap.EffectiveCarriedPrefixes(),
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
// is as untrustworthy as a future-dated retraction), and must report EVERY
// window the account is subject to, each below the wall threshold. "No
// windows" is not a zero reading, one hot window means the account is still
// at its wall by its own newest word, and a snapshot missing a slot is
// silent about that window — "secondary": null beside a calm primary is not
// evidence the weekly limit is below the wall (#6802 review). The proof
// instant is the record's own timestamp — ordering runs on when the
// provider made the claim, never on when af happened to read it.
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
	reported := make(map[string]struct{}, len(reading.Reading.Windows))
	for _, window := range reading.Reading.Windows {
		if window.UsedPercent >= float64(wallPercent) {
			return time.Time{}, false
		}
		reported[window.Name] = struct{}{}
	}
	for _, name := range quota.CodexWindowSlotNames() {
		if _, ok := reported[name]; !ok {
			return time.Time{}, false
		}
	}
	return recorded, true
}

// usageIndexer lazily builds the rollout indexer the reconcile refreshes
// through. It is kept on the manager because its per-file memoization is the
// point: an unchanged rollout costs zero bytes on the next pass, which is
// what makes a 45-second refresh cadence cheap. The caller must hold
// rateIndexerMu — use refreshAccountUsage, which takes it.
func (m *Manager) usageIndexer() *rateindex.Indexer {
	if m.rateIndexer == nil {
		m.rateIndexer = rateindex.New()
	}
	return m.rateIndexer
}

// refreshAccountUsage runs one bounded index pass under rateIndexerMu: the
// reconcile and the shadow-engine ticker are two goroutines sharing this one
// memo, and the Indexer's maps are not internally synchronized.
func (m *Manager) refreshAccountUsage(dir string, carried map[string]int64) rateindex.AccountReading {
	m.rateIndexerMu.Lock()
	defer m.rateIndexerMu.Unlock()
	return m.usageIndexer().Refresh(dir, rateindex.Options{CarriedPrefixes: carried})
}

// accountLimitProofWorthScanning is the cheap gate that keeps the 45-second
// durable scan off users it can never help (#6802 review). The full pass
// parses every repo's instance rows — megabytes of JSON on a busy box — so
// before paying it, the gate asks the question the pass itself would: does
// an UNEXPIRED codex wall observation exist anywhere a wall can live? The
// sources are checked cheapest first:
//   - no registered codex account directories means no rollout store exists
//     for any identity, so no reading could ever count as proof;
//   - the process-wide ledger is one small structured file — its own
//     observations and retractions are decoded, never byte-matched;
//   - live instances carry their observation lists in memory; and
//   - durable rows pay a field-name PREFILTER, never a verdict: a row that
//     serializes no limit field cannot hold a wall, so only a row carrying
//     one is decoded and checked for a real codex observation that still
//     excludes. A codex session's program text or prompt prose is not a wall.
//
// Every answer short of a definite no runs the full pass — a false positive
// costs one scan; a false negative would leave a wall excluded forever. A
// package var so tests can drive the reconcile without standing up the
// gate's real inputs.
var accountLimitProofWorthScanning = func(m *Manager) bool {
	home, err := config.GetConfigDir()
	if err != nil {
		return true
	}
	if accounts, err := agentaccount.List(home, tmux.ProgramCodex); err == nil && len(accounts) == 0 {
		return false
	}
	now := nowFunc()
	// The ledger read is cheap (one file) and feeds every "still excluding"
	// check below, so a wall already retracted frees no scan either.
	evidence, err := loadAccountLimitLedger()
	if err != nil {
		return true // an unreadable ledger is for the full pass to surface
	}
	stillWalls := func(observation session.AccountLimitObservationData) bool {
		return accountUsageReadingSource(observation.Agent) &&
			accountLimitObservationExcludes(observation, observation.Agent, evidence.Retractions, now)
	}
	for _, observation := range evidence.Observations {
		if stillWalls(observation) {
			return true
		}
	}
	m.mu.Lock()
	instances := make([]*session.Instance, 0, len(m.instances))
	for _, inst := range m.instances {
		instances = append(instances, inst)
	}
	m.mu.Unlock()
	for _, inst := range instances {
		if inst == nil {
			continue
		}
		for _, observation := range inst.AccountLimitObservations() {
			if stillWalls(observation) {
				return true
			}
		}
	}
	rows, skipped, err := config.LoadAllRepoInstancesReportingSkips()
	if err != nil || len(skipped) > 0 {
		return true
	}
	for _, raw := range rows {
		// Wall evidence serializes only under limit_* field names or a
		// parked liveness — "limit-reached" covers the derived
		// liveness_name every SaveInstances row carries, and the
		// liveness-integer spellings cover rows written before the derived
		// names existed (the enum is append-only, so 6 is stable). A row
		// matching none carries no observation to parse.
		if !accountLimitRowMightWall(raw) {
			continue
		}
		var data []session.InstanceData
		if err := json.Unmarshal(raw, &data); err != nil {
			return true // an unreadable row may hold the wall — the pass surfaces it
		}
		for _, row := range data {
			_, observations := session.AccountLimitEvidenceFromData(row)
			for _, observation := range observations {
				if stillWalls(observation) {
					return true
				}
			}
		}
	}
	return false
}

// accountLimitWallFieldNeedles is the prefilter for durable rows: every
// serialized shape of wall evidence contains at least one. It is RECALL,
// never verdict — a matching row is decoded for real, so a false match costs
// one row's parse while a missed row would hide a wall.
var accountLimitWallFieldNeedles = [][]byte{
	[]byte("limit_"),        // account_limit_observations and every limit_* field
	[]byte("limit-reached"), // liveness_name on every row modern af writes parked
	[]byte(`"liveness":6`),  // the parked int itself — compact- and
	[]byte(`"liveness": 6`), // indent-encoded rows written before the names existed
}

func accountLimitRowMightWall(raw json.RawMessage) bool {
	for _, needle := range accountLimitWallFieldNeedles {
		if bytes.Contains(raw, needle) {
			return true
		}
	}
	return false
}

// reconcileAccountLimitProofs reads every account still excluded by stored
// evidence and asks each one's own usage record whether the wall ended. The
// durable scan plus the rollout refreshes are the expensive part, so this
// runs on the dedicated ticker, never the poll — and only after the cheap
// gate above proves a codex wall could even exist. Stored evidence is the
// whole domain a retraction can act on — durable rows, the process-wide
// ledger, and live rows' own observation lists; a live session parked at its
// wall contributes a live wall that admission keeps regardless, so
// retraction here never un-parks it (#6776 1d property). Every failure
// aborts the pass — nothing is half-decided on a partial picture, and the
// next tick retries from scratch.
func (m *Manager) reconcileAccountLimitProofs() {
	if !accountLimitProofWorthScanning(m) {
		return
	}
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
			m.refreshAccountUsage(dir, inputs.carriedPrefixes),
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
//
// The proof must additionally clear the write margin of EVERY timed wall
// still excluding, not merely defeat one of them: the predicate asks only
// for strictly-after, so a record written inside the margin would beat a
// wall whose sighting it cannot honestly order itself past — a request
// accepted before the wall whose response landed after it. One unsafe
// comparison refuses the whole tombstone.
func accountLimitProofDefeatsAStoredWall(candidate session.AccountLimitRetractionData, observations []session.AccountLimitObservationData, retractions []session.AccountLimitRetractionData, now time.Time) bool {
	defeats := false
	for _, observation := range observations {
		if observation.Agent != candidate.Agent || observation.Account != candidate.Account {
			continue
		}
		if !accountLimitObservationExcludes(observation, candidate.Agent, retractions, now) {
			continue
		}
		if !observation.ObservedAt.IsZero() &&
			!candidate.ProvenAt.After(observation.ObservedAt.Add(accountLimitProofWriteMargin)) {
			return false
		}
		if session.AccountLimitObservationRetracted(observation, candidate) {
			defeats = true
		}
	}
	return defeats
}
