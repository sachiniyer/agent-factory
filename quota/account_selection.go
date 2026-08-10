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

// SelectAccountCandidate chooses the first explicitly configured, registered
// account that is not currently observed at a usage limit.
func SelectAccountCandidate(selection AccountSelection) (string, bool) {
	current := strings.TrimSpace(selection.CurrentAccount)
	if current != "" && !selection.CurrentAutoSelected {
		return "", false
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
	if current != "" && eligible(current) {
		for _, candidate := range selection.Candidates {
			if strings.TrimSpace(candidate) == current {
				return current, true
			}
		}
	}
	for _, candidate := range selection.Candidates {
		candidate = strings.TrimSpace(candidate)
		if candidate != "" && eligible(candidate) {
			return candidate, true
		}
	}
	return "", false
}
