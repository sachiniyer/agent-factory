package quota

import "strings"

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
// order. A prior automatic selection stays first so an interrupted move is
// finished before another identity is chosen.
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
		if candidate == "" || !eligible(candidate) {
			return
		}
		if _, exists := seen[candidate]; exists {
			return
		}
		seen[candidate] = struct{}{}
		selected = append(selected, candidate)
	}
	if current != "" && eligible(current) {
		for _, candidate := range selection.Candidates {
			if strings.TrimSpace(candidate) == current {
				appendEligible(current)
				break
			}
		}
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
