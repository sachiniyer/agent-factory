package daemon

import (
	"strings"
	"time"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// The turn-evidence half of refutable limit evidence (#4404, epic #6776 slice
// 1d). A wall observation is durable, but it is only ever a claim about the
// past: the same daemon that parks sessions also watches every pane, and a
// pane that demonstrably runs a turn under a named account is positive proof
// the account was serving at that instant. This file collects those proofs
// off the poll's own captures and folds them, on a bounded cadence, into the
// durable ledger as retractions — never deleting a wall, always letting
// event-time ordering decide which fact is newer.
//
// Proof standard: the agent's own in-turn chrome, the exact contract
// task.TurnWatch already enforces for run boundaries — a ticking timed row for
// claude/devin, a scoped absent→present edge for codex/amp/opencode. Anything
// less is not proof: pane churn can be a typed draft, a delivery verdict can
// be unverified, and absence of evidence, a timeout, or an unknown reading
// retract nothing ever.
//
// The reconcile is deliberately rare: it owns the expensive durable scan, and
// the accounts it acts on were walled for far longer than the interval costs.

// accountLimitProofReconcileInterval bounds how often pending turn proofs are
// folded against durable evidence — never inside the 1 Hz per-session work,
// and loose enough that a day-long wall does not notice the wait. A package
// var so tests can compress it.
var accountLimitProofReconcileInterval = 45 * time.Second

// accountLimitProofKey names one provider-scoped identity a turn proved.
type accountLimitProofKey struct {
	agent   string
	account string
}

// accountTurnWatchEntry is one session's chrome watch: rebuilt whenever the
// (agent, account) signature changes, so a turn begun under one identity can
// never be attributed to another.
type accountTurnWatchEntry struct {
	sig   string
	watch *task.TurnWatch
}

// noteAccountTurnEvidence feeds this tick's confirmed pane capture through the
// session's turn watch and, when the agent's own in-turn chrome is
// demonstrated, records the frame instant as positive proof for the account
// the pane runs under. The proof is a fact about the account, kept even if
// the session later dies; reconcileAccountLimitProofs decides what it
// retracts. Called beside noteTaskRunTurnEvidence on both poll paths — it
// needs every confirmed frame, including chrome-absent ones, because the
// scoped agents' watch arms on absence.
func (m *Manager) noteAccountTurnEvidence(key string, instance *session.Instance, obs session.Observation, capturedAt time.Time) {
	if !obs.Captured {
		return
	}
	account, _ := instance.AccountSelection()
	account = strings.TrimSpace(account)
	if account == "" {
		// The ambient identity is never named-account evidence (#6764), so a
		// turn under it proves nothing the ledger keys on.
		return
	}
	agent := instance.AccountAgent()
	if agent == "" {
		return
	}
	// A frame the limit detector calls a wall for this agent is ambiguous —
	// chrome and a live banner can share a capture — and positive proof must
	// be unambiguous. Skip it whole rather than let a walled pane arm or fire
	// the watch.
	if hit, _, _, _ := m.limitDetector.Load().Check(obs.Content, agent, capturedAt); hit {
		return
	}
	sig := agent + "\x00" + account
	m.pausedMu.Lock()
	entry := m.accountTurnWatches[key]
	if entry == nil || entry.sig != sig {
		entry = &accountTurnWatchEntry{sig: sig, watch: task.NewTurnWatch(agent, capturedAt)}
		m.accountTurnWatches[key] = entry
	}
	turning := entry.watch.Observe(obs.Content, capturedAt)
	m.pausedMu.Unlock()
	// The scoped agents' watch reports "turn in flight" on every
	// indicator-present frame after it has armed — not only on the edge —
	// so an unchanged pane left showing busy chrome would otherwise mint a
	// newer proof on every capture and let a frozen frame out-rank a wall
	// sighted after it. Only fresh pane bytes carry fresh turn evidence:
	// a capture identical to the last proves nothing new (#6802 review).
	if !turning || !obs.Updated {
		return
	}
	m.pausedMu.Lock()
	proofKey := accountLimitProofKey{agent: agent, account: account}
	if prior := m.accountLimitProofs[proofKey]; capturedAt.After(prior) {
		m.accountLimitProofs[proofKey] = capturedAt
	}
	m.pausedMu.Unlock()
}

// accountTurnProofObservable reports whether a paused instance is worth the
// bounded backstop purely for proof collection: it runs a named account on an
// agent whose chrome can confirm a turn — the same account/agent checks
// noteAccountTurnEvidence applies to every fed frame. Without it an attached
// interactive session's successful turns produce no proof and a stale stored
// wall on its account can never be refuted while the attach lasts, because
// detaching after the turn finishes cannot reconstruct the missed chrome
// evidence (#6802 review).
func accountTurnProofObservable(instance *session.Instance) bool {
	account, _ := instance.AccountSelection()
	if strings.TrimSpace(account) == "" {
		return false
	}
	return task.TurnSignaturedAgent(instance.AccountAgent())
}

// sweepAccountTurnWatches drops chrome watchers for sessions that are gone or
// replaced — keyed by stableSessionKey, so a same-title successor never
// inherits a predecessor's armed state. Proofs already recorded are facts
// about the account and stay; only the per-session watch is reclaimed. Called
// once per poll beside sweepTaskRunTurnWatches.
func (m *Manager) sweepAccountTurnWatches() {
	m.pausedMu.Lock()
	size := len(m.accountTurnWatches)
	m.pausedMu.Unlock()
	if size == 0 {
		return
	}
	m.mu.Lock()
	live := make(map[string]struct{}, size)
	for key, inst := range m.instances {
		repoID, _ := splitDaemonInstanceKey(key)
		live[stableSessionKey(repoID, inst)] = struct{}{}
	}
	m.mu.Unlock()
	m.pausedMu.Lock()
	defer m.pausedMu.Unlock()
	for key := range m.accountTurnWatches {
		if _, ok := live[key]; !ok {
			delete(m.accountTurnWatches, key)
		}
	}
}

// reconcileAccountLimitProofs folds the turn proofs collected since the last
// pass against durable stored evidence and writes a retraction for every
// account the stored record still excludes. The durable scan is the expensive
// part, so it runs at most once per interval and only while proofs are
// pending. Stored evidence is the whole domain a retraction can act on —
// durable rows, the process-wide ledger, and live rows' own observation lists.
// A live session parked at its wall contributes a live wall that admission
// keeps regardless; retraction here never un-parks it (#6776 1d property).
func (m *Manager) reconcileAccountLimitProofs() {
	now := nowFunc()
	m.pausedMu.Lock()
	if len(m.accountLimitProofs) == 0 || now.Before(m.accountLimitProofReconcileAt) {
		m.pausedMu.Unlock()
		return
	}
	m.accountLimitProofReconcileAt = now.Add(accountLimitProofReconcileInterval)
	proofs := m.accountLimitProofs
	m.accountLimitProofs = make(map[accountLimitProofKey]time.Time)
	m.pausedMu.Unlock()

	evidence, err := loadAccountLimitEvidenceForSwap()
	if err != nil {
		// Unreadable evidence is fail-closed territory: retain the proofs for
		// the next pass rather than retraction-guessing on a partial record.
		m.warn().Printf("account-limit turn-proof reconcile could not load durable evidence; retaining %d pending proof(s): %v", len(proofs), err)
		m.requeueAccountLimitProofs(proofs)
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
	var retractions []session.AccountLimitRetractionData
	for key, provenAt := range proofs {
		candidate := session.AccountLimitRetractionData{
			Agent:    key.agent,
			Account:  key.account,
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
		m.warn().Printf("account-limit turn-proof reconcile could not write retractions; retaining %d pending proof(s): %v", len(proofs), err)
		m.requeueAccountLimitProofs(proofs)
		return
	}
	for _, retraction := range retractions {
		m.info().Printf("retracted stale account-limit evidence for %s account %q: a live turn proved it serving at %s",
			retraction.Agent, retraction.Account, retraction.ProvenAt.UTC().Format(time.RFC3339))
	}
}

// requeueAccountLimitProofs returns proofs a failed reconcile could not act
// on, keeping the newest instant per key so a retry orders against fresh
// evidence, not a stale proof.
func (m *Manager) requeueAccountLimitProofs(proofs map[accountLimitProofKey]time.Time) {
	m.pausedMu.Lock()
	defer m.pausedMu.Unlock()
	for key, provenAt := range proofs {
		if prior := m.accountLimitProofs[key]; provenAt.After(prior) {
			m.accountLimitProofs[key] = provenAt
		}
	}
}

// accountLimitProofDefeatsAStoredWall reports whether persisting candidate
// flips at least one still-excluding observation: the wall must currently
// exclude under the retractions already recorded AND be defeated by this
// proof's own instant. The exclusion half is the reconcile-side sibling of
// limitedAccountsForSwap's stored-evidence loops, sharing
// accountLimitObservationExcludes so the write path can never believe
// something admission would not; the defeat half stops a proof that predates
// the wall — the older fact — from littering the ledger with a tombstone
// admission never honors (#6802 review).
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
