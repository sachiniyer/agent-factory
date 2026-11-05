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
//
// Retractions is additive under the same schema version: a reader that does
// not know the field sees every wall and no proof, which is the fail-closed
// direction — a downgraded af cannot mistake retracted evidence for a clean
// bill, it can only exclude an account this binary proved free.
type accountLimitLedger struct {
	SchemaVersion int                                   `json:"schema_version"`
	Observations  []session.AccountLimitObservationData `json:"observations,omitempty"`
	Retractions   []session.AccountLimitRetractionData  `json:"retractions,omitempty"`
}

// accountLimitEvidence is the whole durable fact set admission folds: walls
// still believed, and the proofs that may defeat some of them. Both halves
// travel together because the verdict is always the pair — a caller that saw
// only the observations could not distinguish "no proof exists" from "the
// proof source could not be read".
type accountLimitEvidence struct {
	Observations []session.AccountLimitObservationData
	Retractions  []session.AccountLimitRetractionData
}

func accountLimitLedgerPath() (string, error) {
	home, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "instances", accountLimitLedgerFileName), nil
}

func loadAccountLimitLedger() (accountLimitEvidence, error) {
	path, err := accountLimitLedgerPath()
	if err != nil {
		return accountLimitEvidence{}, err
	}
	return readAccountLimitLedger(path)
}

type accountLimitObservationLoader func() ([]session.AccountLimitObservationData, error)
type accountLimitEvidenceLoader func() (accountLimitEvidence, error)

// loadAccountLimitEvidenceSnapshot combines the two durable evidence sources.
// The ordering is part of the snapshot protocol with session deletion, which
// transfers a row's observations into the ledger before removing that row.
// Retractions live only in the ledger — rows hold the walls they observed,
// never proofs about other sessions' accounts.
func loadAccountLimitEvidenceSnapshot(
	loadPersisted accountLimitObservationLoader,
	loadRetained accountLimitEvidenceLoader,
) (accountLimitEvidence, error) {
	persisted, err := loadPersisted()
	if err != nil {
		return accountLimitEvidence{}, fmt.Errorf("load persisted account-limit evidence: %w", err)
	}
	retained, err := loadRetained()
	if err != nil {
		return accountLimitEvidence{}, fmt.Errorf("load retained account-limit evidence: %w", err)
	}
	retained.Observations = append(persisted, retained.Observations...)
	return retained, nil
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

func readAccountLimitLedger(path string) (accountLimitEvidence, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return accountLimitEvidence{}, nil
	}
	if err != nil {
		return accountLimitEvidence{}, err
	}
	var ledger accountLimitLedger
	if err := json.Unmarshal(raw, &ledger); err != nil {
		return accountLimitEvidence{}, fmt.Errorf("decode %s: %w", path, err)
	}
	if ledger.SchemaVersion != accountLimitLedgerSchemaVersion {
		return accountLimitEvidence{}, fmt.Errorf("%s has schema_version %d, want %d", path,
			ledger.SchemaVersion, accountLimitLedgerSchemaVersion)
	}
	for idx := range ledger.Observations {
		observation := &ledger.Observations[idx]
		observation.Agent = strings.TrimSpace(observation.Agent)
		observation.Account = strings.TrimSpace(observation.Account)
		if observation.Agent == "" || observation.Account == "" {
			return accountLimitEvidence{}, fmt.Errorf("%s contains an account-limit observation with an empty agent or account", path)
		}
	}
	// A malformed retraction is treated exactly like a malformed wall: the
	// file's integrity is in doubt, and "the ledger could not be read" must
	// never collapse into "nothing is walled" — fail closed at admission.
	for idx := range ledger.Retractions {
		retraction := &ledger.Retractions[idx]
		retraction.Agent = strings.TrimSpace(retraction.Agent)
		retraction.Account = strings.TrimSpace(retraction.Account)
		if retraction.Agent == "" || retraction.Account == "" {
			return accountLimitEvidence{}, fmt.Errorf("%s contains an account-limit retraction with an empty agent or account", path)
		}
		if retraction.ProvenAt.IsZero() {
			return accountLimitEvidence{}, fmt.Errorf("%s contains an account-limit retraction with no proof time", path)
		}
	}
	return accountLimitEvidence{Observations: ledger.Observations, Retractions: ledger.Retractions}, nil
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
		merged, err := mergeAccountLimitObservations(current.Observations, keyed)
		if err != nil {
			return err
		}
		raw, err := json.MarshalIndent(accountLimitLedger{
			SchemaVersion: accountLimitLedgerSchemaVersion,
			Observations:  merged,
			// The file is rewritten whole: proofs recorded by other writers
			// carry through untouched, and the merge that lands here orders
			// the new walls against them by event time at read, never by
			// which writer happened to lock the file last.
			Retractions: current.Retractions,
		}, "", "  ")
		if err != nil {
			return err
		}
		return config.AtomicWriteFile(path, raw, 0o600)
	})
}

// retractAccountLimitObservations records positive proof that accounts were
// serving at ProvenAt. It writes tombstones, never deletes walls: the defeat
// is applied at read time by event-time ordering (see
// session.AccountLimitObservationRetracted), so a fresh wall sighted after the
// proof always outranks it no matter which lands on disk last. A retraction
// with no proof time or no identity is refused outright — an untimed proof
// cannot be ordered and an unnamed one cannot be scoped.
func retractAccountLimitObservations(retractions []session.AccountLimitRetractionData) error {
	keyed := make([]session.AccountLimitRetractionData, 0, len(retractions))
	for _, retraction := range retractions {
		retraction.Agent = strings.TrimSpace(retraction.Agent)
		retraction.Account = strings.TrimSpace(retraction.Account)
		if retraction.Agent == "" || retraction.Account == "" || retraction.ProvenAt.IsZero() {
			return fmt.Errorf("account-limit retraction needs a named identity and a proof time")
		}
		keyed = append(keyed, retraction)
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
		merged, err := mergeAccountLimitRetractions(current.Retractions, keyed)
		if err != nil {
			return err
		}
		raw, err := json.MarshalIndent(accountLimitLedger{
			SchemaVersion: accountLimitLedgerSchemaVersion,
			Observations:  current.Observations,
			Retractions:   merged,
		}, "", "  ")
		if err != nil {
			return err
		}
		return config.AtomicWriteFile(path, raw, 0o600)
	})
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
			// The wall's event time is its last sighting: keep the newest
			// across every copy of the fact, so ordering a proof against the
			// merged record cannot be beaten by a stale duplicate (#6776 1d).
			if prior.ObservedAt.After(observation.ObservedAt) {
				observation.ObservedAt = prior.ObservedAt
			}
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

// mergeAccountLimitRetractions folds retraction facts per identity, keeping
// only the newest proof each account earned: an older proof is a strict subset
// of the ordering power of a newer one and adds nothing.
func mergeAccountLimitRetractions(current, added []session.AccountLimitRetractionData) ([]session.AccountLimitRetractionData, error) {
	merged := make(map[string]session.AccountLimitRetractionData)
	merge := func(retraction session.AccountLimitRetractionData) error {
		retraction.Agent = strings.TrimSpace(retraction.Agent)
		retraction.Account = strings.TrimSpace(retraction.Account)
		if retraction.Agent == "" || retraction.Account == "" {
			return fmt.Errorf("account-limit retraction has an empty agent or account")
		}
		if retraction.ProvenAt.IsZero() {
			return fmt.Errorf("account-limit retraction has no proof time")
		}
		key := retraction.Agent + "\x00" + retraction.Account
		if prior, exists := merged[key]; !exists || prior.ProvenAt.Before(retraction.ProvenAt) {
			merged[key] = retraction
		}
		return nil
	}
	for _, retraction := range current {
		if err := merge(retraction); err != nil {
			return nil, err
		}
	}
	for _, retraction := range added {
		if err := merge(retraction); err != nil {
			return nil, err
		}
	}
	result := make([]session.AccountLimitRetractionData, 0, len(merged))
	for _, retraction := range merged {
		result = append(result, retraction)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Agent != result[j].Agent {
			return result[i].Agent < result[j].Agent
		}
		return result[i].Account < result[j].Account
	})
	return result, nil
}
