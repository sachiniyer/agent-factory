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
	if len(observations) == 0 {
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
		merged, err := mergeAccountLimitObservations(current, observations)
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
		if !exists || (!prior.ResetAt.IsZero() &&
			(observation.ResetAt.IsZero() || observation.ResetAt.After(prior.ResetAt))) {
			merged[key] = observation
		}
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
