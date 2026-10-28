package config

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/agentaccount"
)

// normalizeLimitAccountCandidates validates the ordered account names and
// removes duplicate entries without changing which account wins first.
func normalizeLimitAccountCandidates(values []string) ([]string, error) {
	seen := make(map[string]struct{}, len(values))
	var out []string
	for _, value := range values {
		name := strings.TrimSpace(value)
		if err := agentaccount.ValidateName(name); err != nil {
			return nil, fmt.Errorf("limit_account_candidates entry: %w", err)
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out, nil
}

func validateLimitAccountCandidatesValue(_, value string) error {
	_, err := normalizeLimitAccountCandidates(splitListValue(value))
	return err
}
