package daemon

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
// Cleared sibling rows are persisted through the settlement path so a restart
// cannot resurrect them.
//
// accountLimitMu makes the whole retirement one publication against automatic
// account-swap admission: a swap reads either all of the identity's evidence or
// none of it, never a half-retracted mix.
//
// Returns whether the session's own row changed, so the caller can checkpoint
// the cleared evidence through the settlement write path.
func (m *Manager) refuteAccountLimitEvidence(instance *session.Instance, epoch uint64) bool {
	agent, account, changed := instance.RefuteAccountLimitObservationAtEpoch(epoch)
	if agent == "" || account == "" {
		return false
	}
	key := agent + "\x00" + account
	type clearedRow struct {
		repoID   string
		key      string
		instance *session.Instance
	}
	var cleared []clearedRow
	m.accountLimitMu.Lock()
	m.mu.Lock()
	_, checked := m.refutedLedgerAccounts[key]
	if !checked {
		if m.refutedLedgerAccounts == nil {
			m.refutedLedgerAccounts = make(map[string]struct{})
		}
		m.refutedLedgerAccounts[key] = struct{}{}
	}
	for instanceKey, other := range m.instances {
		if other == nil || other == instance {
			continue
		}
		if !other.RetractAccountLimitObservation(agent, account) {
			continue
		}
		repoID, _ := splitDaemonInstanceKey(instanceKey)
		cleared = append(cleared, clearedRow{repoID: repoID, key: instanceKey, instance: other})
	}
	m.mu.Unlock()
	if !checked {
		if err := retractAccountLimitObservation(agent, account); err != nil {
			m.warn().Printf("could not retract the retained limit evidence for %s account %q — it may keep excluding the account until restart: %v",
				agent, account, err)
		}
	}
	m.accountLimitMu.Unlock()
	// A cleared row is durable evidence retired in memory; the settlement write
	// makes the retirement durable too, and its retry obligation survives a
	// failed write rather than resurrecting the wall on the next load.
	for _, row := range cleared {
		if err := m.persistSettlement(row.repoID, row.key, row.instance); err != nil {
			m.warn().Printf("session %q: refuted limit evidence for %s account %q cleared in memory but not yet on disk: %v",
				row.instance.Title, agent, account, err)
		}
	}
	return changed
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
