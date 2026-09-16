package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
)

const (
	accountLimitLedgerFileName      = "account-limit-observations.json"
	accountLimitLedgerSchemaVersion = 1
)

// accountLimitLedger is process-wide quota evidence whose lifetime is not tied
// to the session that observed it. It lives at the root of instances/ so a full
// factory reset removes it with session state, while per-repo record deletion
// cannot erase it.
type accountLimitLedger struct {
	SchemaVersion int                                   `json:"schema_version"`
	Observations  []session.AccountLimitObservationData `json:"observations,omitempty"`
}

func accountLimitLedgerPath() (string, error) {
	home, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "instances", accountLimitLedgerFileName), nil
}

func loadAccountLimitLedger() ([]session.AccountLimitObservationData, error) {
	path, err := accountLimitLedgerPath()
	if err != nil {
		return nil, err
	}
	return readAccountLimitLedger(path)
}

type accountLimitEvidenceLoader func() ([]session.AccountLimitObservationData, error)

// loadAccountLimitEvidenceSnapshot combines the two durable evidence sources.
// The ordering is part of the snapshot protocol with session deletion, which
// transfers a row's observations into the ledger before removing that row.
func loadAccountLimitEvidenceSnapshot(
	loadPersisted, loadRetained accountLimitEvidenceLoader,
) ([]session.AccountLimitObservationData, error) {
	persisted, err := loadPersisted()
	if err != nil {
		return nil, fmt.Errorf("load persisted account-limit evidence: %w", err)
	}
	retained, err := loadRetained()
	if err != nil {
		return nil, fmt.Errorf("load retained account-limit evidence: %w", err)
	}
	return append(persisted, retained...), nil
}

// loadPersistedAccountLimitObservations reads quota evidence straight from
// every durable session row. A row that fails FromInstanceData is deliberately
// absent from Manager.instances, but that load failure is not evidence that its
// account became usable. Keep raw-row decoding independent of materialization
// so an unloadable session cannot make a known-limited identity eligible after
// restart. Any unreadable repo fails closed at the automatic-swap decision.
func loadPersistedAccountLimitObservations() ([]session.AccountLimitObservationData, error) {
	allInstances, skipped, err := config.LoadAllRepoInstancesReportingSkips()
	if err != nil {
		return nil, err
	}
	if len(skipped) > 0 {
		return nil, fmt.Errorf("account-limit evidence may be present in unreadable repo state: %s",
			strings.Join(skipped, ", "))
	}
	var observations []session.AccountLimitObservationData
	for repoID, raw := range allInstances {
		if raw == nil || string(raw) == "[]" || string(raw) == "null" {
			continue
		}
		var rows []session.InstanceData
		if err := json.Unmarshal(raw, &rows); err != nil {
			return nil, fmt.Errorf("decode account-limit observations for repo %s: %w", repoID, err)
		}
		for _, row := range rows {
			_, rowObservations := session.AccountLimitEvidenceFromData(row)
			observations = append(observations, rowObservations...)
		}
	}
	return observations, nil
}

func readAccountLimitLedger(path string) ([]session.AccountLimitObservationData, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ledger accountLimitLedger
	if err := json.Unmarshal(raw, &ledger); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if ledger.SchemaVersion != accountLimitLedgerSchemaVersion {
		return nil, fmt.Errorf("%s has schema_version %d, want %d", path,
			ledger.SchemaVersion, accountLimitLedgerSchemaVersion)
	}
	for idx := range ledger.Observations {
		observation := &ledger.Observations[idx]
		observation.Agent = strings.TrimSpace(observation.Agent)
		observation.Account = strings.TrimSpace(observation.Account)
		if observation.Agent == "" || observation.Account == "" {
			return nil, fmt.Errorf("%s contains an account-limit observation with an empty agent or account", path)
		}
	}
	return ledger.Observations, nil
}

// retainAccountLimitObservations durably merges evidence before its session
// record is deleted. Unknown reset times dominate (they intentionally exclude
// indefinitely); otherwise the later reset is the safer expiry boundary.
func retainAccountLimitObservations(observations []session.AccountLimitObservationData) error {
	// A torn-down session can contain legacy or malformed observations that have
	// no identity key. They cannot exclude any candidate, so do not let them keep
	// an otherwise completed deletion tombstoned forever. Ledger reads and merges
	// remain strict: corrupt evidence that was already persisted still fails closed.
	keyed := make([]session.AccountLimitObservationData, 0, len(observations))
	for _, observation := range observations {
		observation.Agent = strings.TrimSpace(observation.Agent)
		observation.Account = strings.TrimSpace(observation.Account)
		if observation.Agent == "" || observation.Account == "" {
			continue
		}
		keyed = append(keyed, observation)
	}
	if len(keyed) == 0 {
		return nil
	}
	path, err := accountLimitLedgerPath()
	if err != nil {
		return err
	}
	return config.WithFileLockTimeout(path, config.RepoInstancesLockTimeout, func() error {
		current, err := readAccountLimitLedger(path)
		if err != nil {
			return err
		}
		merged, err := mergeAccountLimitObservations(current, keyed)
		if err != nil {
			return err
		}
		raw, err := json.MarshalIndent(accountLimitLedger{
			SchemaVersion: accountLimitLedgerSchemaVersion,
			Observations:  merged,
		}, "", "  ")
		if err != nil {
			return err
		}
		return config.AtomicWriteFile(path, raw, 0o600)
	})
}

// retractAccountLimitObservation removes one {agent, account}'s retained
// evidence — the refutation half of the ledger (#4404). A session that answered
// under an account has proven the observation stale, and an entry left behind
// by a deleted session would otherwise keep excluding that account until its
// recorded reset passed.
func retractAccountLimitObservation(agent, account string) error {
	agent = strings.TrimSpace(agent)
	account = strings.TrimSpace(account)
	if agent == "" || account == "" {
		return nil
	}
	path, err := accountLimitLedgerPath()
	if err != nil {
		return err
	}
	return config.WithFileLockTimeout(path, config.RepoInstancesLockTimeout, func() error {
		current, err := readAccountLimitLedger(path)
		if err != nil {
			return err
		}
		kept := make([]session.AccountLimitObservationData, 0, len(current))
		removed := false
		for _, observation := range current {
			if observation.Agent == agent && observation.Account == account {
				removed = true
				continue
			}
			kept = append(kept, observation)
		}
		if !removed {
			return nil
		}
		raw, err := json.MarshalIndent(accountLimitLedger{
			SchemaVersion: accountLimitLedgerSchemaVersion,
			Observations:  kept,
		}, "", "  ")
		if err != nil {
			return err
		}
		return config.AtomicWriteFile(path, raw, 0o600)
	})
}

// refuteAccountLimitEvidence retires durable negative-quota evidence for the
// account a session is demonstrably running under — the poll calls it on the
// same affirmative-work evidence that settles a session Running (#4404). It
// removes the session's own stored observation and, once per identity per
// daemon, retracts the retained-ledger copy a deleted session may have left.
// The ledger check is bounded rather than per-tick because it is a locked file
// read on a path the poll reaches every time a pane produces output.
//
// The refute retires the identity's evidence EVERYWHERE it is stored, not just
// on the reporting session's row: a sibling session that hit the same wall
// earlier keeps contributing its observation to accountLimitEvidenceForSwap,
// so clearing one row left the account walled by evidence the refute had just
// disproven (#4404 review). What is deliberately NOT retired is live state — a
// sibling still parked LiveLimitReached under the identity keeps claiming its
// wall, the conservative direction, and clears it on its own affirmative work.
// Cleared rows are persisted INSIDE the same fence (persistRefutedRow): the
// durable copy is what accountLimitEvidenceForSwap merges back in, so retiring
// the in-memory observation and releasing the fence before the durable write
// landed left a window where a create read stale rows the refute had already
// disproven and kept excluding the account anyway (#4404 review).
//
// accountLimitMu makes the whole retirement one publication against automatic
// account-swap admission: a swap reads either all of the identity's evidence or
// none of it, never a half-retracted mix.
//
// Returns whether the session's own row changed, so the caller can checkpoint
// the cleared evidence through the settlement write path.
func (m *Manager) refuteAccountLimitEvidence(instance *session.Instance, epoch uint64) bool {
	m.accountLimitMu.Lock()
	defer m.accountLimitMu.Unlock()
	// The reporting session's epoch-gated refute runs inside the fence too —
	// it is part of the same publication.
	agent, account, changed := instance.RefuteAccountLimitObservationAtEpoch(epoch)
	if agent == "" || account == "" {
		return false
	}
	key := agent + "\x00" + account
	m.mu.Lock()
	reportKey := ""
	rows := make(map[string]*session.Instance, len(m.instances))
	for instanceKey, other := range m.instances {
		if other == nil {
			continue
		}
		rows[instanceKey] = other
		if other == instance {
			reportKey = instanceKey
		}
	}
	_, checked := m.refutedLedgerAccounts[key]
	m.mu.Unlock()
	if changed && reportKey != "" {
		m.persistRefutedRow(reportKey, instance)
	}
	for instanceKey, other := range rows {
		if other == instance {
			continue
		}
		if !other.RetractAccountLimitObservation(agent, account) {
			continue
		}
		m.persistRefutedRow(instanceKey, other)
	}
	if !checked {
		if err := retractAccountLimitObservation(agent, account); err != nil {
			m.warn().Printf("could not retract the retained limit evidence for %s account %q — it may keep excluding the account until restart: %v",
				agent, account, err)
		} else {
			// The mark means "the durable row is already retracted" — so it
			// may only be written AFTER a successful retraction. Marking it
			// before the fallible file op and then failing would poison the
			// identity for the daemon's lifetime: every later refutation
			// would see the mark and skip the retry the evidence still
			// needs (#4404 review).
			m.mu.Lock()
			if m.refutedLedgerAccounts == nil {
				m.refutedLedgerAccounts = make(map[string]struct{})
			}
			m.refutedLedgerAccounts[key] = struct{}{}
			m.mu.Unlock()
		}
	}
	return changed
}

// errRefutedRowBusy is the sentinel recordSettlementWrite receives for a
// cleared row that could not be written inside the refutation fence because the
// session is mid-operation — the write is owed to the settlement retry, which
// holds the op lock this path cannot take and revalidates under it.
var errRefutedRowBusy = errors.New("session is mid-operation; the cleared row is owed to the settlement retry")

// persistRefutedRow durably writes one cleared row while the caller still
// holds the refutation fence — the half of the retirement the in-memory
// Retract cannot provide on its own, because accountLimitEvidenceForSwap folds
// the durable row straight back into the evidence a router or swap reads
// (#4404 review).
//
// The per-session op lock IS taken — by TryLock, never Lock. opLock →
// accountLimitMu is the established order (a kill or swap holds its op lock
// across the fence), and the caller holds accountLimitMu, so blocking here
// would deadlock; a TryLock that never waits cannot. A held op lock means an
// operation is mid-flight, which is the same answer the in-flight check gives:
// the write is owed to the settlement retry, which revalidates under the op
// lock after the op releases it. The check must run UNDER that lock rather than
// before it — an op that raises its fence between a bare GetInFlightOp read and
// the ToInstanceData snapshot would let this write persist a half-built row
// with the transient operation scrubbed by ForStorage (#4404 review).
//
// repoStartLock is absent: it is held above accountLimitMu on the create path.
// The write is a stable-id-keyed update, and the repo file lock
// persistInstanceData takes is all the ordering it needs.
func (m *Manager) persistRefutedRow(key string, instance *session.Instance) {
	repoID, _ := splitDaemonInstanceKey(key)
	m.mu.Lock()
	registered := m.instances[key] == instance
	m.mu.Unlock()
	if !registered {
		// A deleted row has no repo record left to retire — the delete moved its
		// evidence to the ledger under this same fence, and the retraction
		// retires that copy. Persisting it would only publish a stale
		// session.updated for a session that is gone (#4404 review).
		//
		// An ARCHIVED row is not the same shape: it still sits in the repo's
		// instances file, and loadPersistedAccountLimitObservations reads every
		// durable row without an archived exclusion — so skipping the write
		// leaves the cleared observation on disk, and the next routing scan
		// folds the stale wall straight back in (#4404 review).
		return
	}
	opLock := m.opLockFor(key)
	if !opLock.TryLock() {
		m.recordSettlementWrite(repoID, key, instance, errRefutedRowBusy)
		return
	}
	defer opLock.Unlock()
	// Re-verify under the lock. Registration can change while it is acquired,
	// and an op raised outside it must not be flattened by this write.
	m.mu.Lock()
	registered = m.instances[key] == instance
	m.mu.Unlock()
	if !registered {
		return
	}
	if instance.GetInFlightOp() != session.OpNone {
		m.recordSettlementWrite(repoID, key, instance, errRefutedRowBusy)
		return
	}
	data := instance.ToInstanceData()
	err := persistInstanceData(repoID, data)
	if err != nil {
		m.warn().Printf("session %q: refuted account-limit evidence cleared in memory but not yet on disk (the poll retries it): %v",
			instance.Title, err)
	}
	m.publishEvent(agentproto.EventSessionUpdated, data)
	m.recordSettlementWrite(repoID, key, instance, err)
}

// unrefuteRetainedAccountLimits drops the daemon-lifetime "ledger already
// checked" mark for identities whose evidence was just re-retained. The mark
// bounds locked file reads; it is not a truth cache — new retained evidence
// for {agent, account} means the next successful session under it is a fresh
// refutation that must retract the ledger row again, not a skipped one (#4404
// review). Called after every successful retain, with the same observations
// the retain just wrote.
func (m *Manager) unrefuteRetainedAccountLimits(observations []session.AccountLimitObservationData) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, observation := range observations {
		agent := strings.TrimSpace(observation.Agent)
		account := strings.TrimSpace(observation.Account)
		if agent == "" || account == "" {
			continue
		}
		delete(m.refutedLedgerAccounts, agent+"\x00"+account)
	}
}

func mergeAccountLimitObservations(current, added []session.AccountLimitObservationData) ([]session.AccountLimitObservationData, error) {
	merged := make(map[string]session.AccountLimitObservationData)
	merge := func(observation session.AccountLimitObservationData) error {
		observation.Agent = strings.TrimSpace(observation.Agent)
		observation.Account = strings.TrimSpace(observation.Account)
		if observation.Agent == "" || observation.Account == "" {
			return fmt.Errorf("account-limit observation has an empty agent or account")
		}
		key := observation.Agent + "\x00" + observation.Account
		prior, exists := merged[key]
		if exists {
			observation.ResetAt = session.RetainedAccountLimitReset(prior.ResetAt, observation.ResetAt)
		}
		merged[key] = observation
		return nil
	}
	for _, observation := range current {
		if err := merge(observation); err != nil {
			return nil, err
		}
	}
	for _, observation := range added {
		if err := merge(observation); err != nil {
			return nil, err
		}
	}
	result := make([]session.AccountLimitObservationData, 0, len(merged))
	for _, observation := range merged {
		result = append(result, observation)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Agent != result[j].Agent {
			return result[i].Agent < result[j].Agent
		}
		return result[i].Account < result[j].Account
	})
	return result, nil
}
