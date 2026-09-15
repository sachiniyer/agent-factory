package quota

import (
	"fmt"
	"strings"
	"time"
)

// AccountSelection is the process-wide usage-limit evidence used to decide
// whether an account-scoped retry is allowed.
type AccountSelection struct {
	CurrentAccount      string
	CurrentAutoSelected bool
	Candidates          []string
	Registered          []string
	Limited             []string
}

// SelectAccountCandidates returns the explicitly configured, registered
// accounts that are not currently observed at a usage limit, in selection
// order. The current identity is never a replacement candidate: interrupted
// committed moves are resumed by the daemon's separate pending-swap path.
func SelectAccountCandidates(selection AccountSelection) []string {
	current := strings.TrimSpace(selection.CurrentAccount)
	if current != "" && !selection.CurrentAutoSelected {
		return nil
	}

	registered := make(map[string]struct{}, len(selection.Registered))
	for _, name := range selection.Registered {
		if name = strings.TrimSpace(name); name != "" {
			registered[name] = struct{}{}
		}
	}
	limited := make(map[string]struct{}, len(selection.Limited))
	for _, name := range selection.Limited {
		if name = strings.TrimSpace(name); name != "" {
			limited[name] = struct{}{}
		}
	}
	eligible := func(name string) bool {
		_, exists := registered[name]
		_, blocked := limited[name]
		return exists && !blocked
	}

	// A prior attempt may have durably selected an account after stopping the
	// limited runtime but failed before its replacement came up. Finish that
	// already-declared move before choosing a second identity.
	selected := make([]string, 0, len(selection.Candidates))
	seen := make(map[string]struct{}, len(selection.Candidates))
	appendEligible := func(candidate string) {
		candidate = strings.TrimSpace(candidate)
		if candidate == "" || candidate == current || !eligible(candidate) {
			return
		}
		if _, exists := seen[candidate]; exists {
			return
		}
		seen[candidate] = struct{}{}
		selected = append(selected, candidate)
	}
	for _, candidate := range selection.Candidates {
		appendEligible(candidate)
	}
	return selected
}

// SelectAccountCandidate chooses the first eligible account.
func SelectAccountCandidate(selection AccountSelection) (string, bool) {
	candidates := SelectAccountCandidates(selection)
	if len(candidates) == 0 {
		return "", false
	}
	return candidates[0], true
}

// AllAccountsWalledError reports a create-time pool route with no healthy
// candidate (#4404): every registered account for the agent carries unexpired
// usage-limit evidence, so starting a session would launch it straight into a
// wall. Earliest is the soonest known reset across the pool — the moment a retry
// could work — and EarliestOn names the account holding it; both are empty when
// no observation carried a reset time at all.
type AllAccountsWalledError struct {
	Agent      string
	Accounts   []string
	Earliest   time.Time
	EarliestOn string
}

func (e *AllAccountsWalledError) Error() string {
	base := fmt.Sprintf(
		"every registered %s account (%s) has unexpired usage-limit evidence, so the session was not created — "+
			"launching into a known wall is worse than refusing; pass --account to pin one anyway, or wait",
		e.Agent, strings.Join(e.Accounts, ", "))
	if e.Earliest.IsZero() {
		return base + ": no account reports a reset time"
	}
	return fmt.Sprintf("%s: the earliest reset is %s on %q",
		base, e.Earliest.UTC().Format(time.RFC3339), e.EarliestOn)
}

// RouteAccountPool picks the registered account a new session launches under
// when the client named none (#4404). limited maps a currently-walled account
// name to the merged reset boundary its evidence claims (zero when unknown);
// entries naming accounts that are not registered are ignored. preferred is the
// configured default_accounts answer — honored while it is healthy, skipped
// like any other walled account when it is not. loads counts live sessions per
// account so a pool with no preference spreads work; ties resolve in
// registered order.
func RouteAccountPool(agent string, registered []string, limited map[string]time.Time, preferred string, loads map[string]int) (string, error) {
	preferred = strings.TrimSpace(preferred)
	healthy := make([]string, 0, len(registered))
	for _, name := range registered {
		if _, walled := limited[name]; !walled {
			healthy = append(healthy, name)
		}
	}
	if len(healthy) == 0 {
		err := &AllAccountsWalledError{Agent: agent, Accounts: registered}
		for _, name := range registered {
			reset, ok := limited[name]
			if !ok || reset.IsZero() {
				continue
			}
			if err.Earliest.IsZero() || reset.Before(err.Earliest) {
				err.Earliest, err.EarliestOn = reset, name
			}
		}
		return "", err
	}
	if preferred != "" {
		for _, name := range healthy {
			if name == preferred {
				return preferred, nil
			}
		}
	}
	best := healthy[0]
	for _, name := range healthy[1:] {
		if loads[name] < loads[best] {
			best = name
		}
	}
	return best, nil
}
