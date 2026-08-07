package sessionenv

import "fmt"

// AccountLookup resolves an account NAME to its scope, and is installed by the
// binary rather than called directly, because internal/agentaccount imports this
// package and the dependency cannot run both ways.
//
// It is deliberately a hook with no default. A nil lookup REFUSES an
// account-scoped launch instead of falling through unscoped: a build that forgot
// to wire it would otherwise run every scoped session on the ambient identity
// while reporting the selected account, which is the silent wrong-account
// outcome this feature exists to prevent (#3051).
var AccountLookup func(agent, name string) (Account, error)

// applyAccountScope resolves the named account and applies its boundary.
//
// Every failure refuses. There is no path here that returns the unscoped
// environment: an account that cannot be resolved, a lookup that was never
// installed, or a boundary that rejects the command all mean af cannot prove the
// session will use the requested identity — and an unprovable launch is not
// evidence of a correct one.
func applyAccountScope(environ []string, agent, account, command string) ([]string, error) {
	if AccountLookup == nil {
		return nil, fmt.Errorf("account-scoped launch requested but no account lookup is installed")
	}
	scope, err := AccountLookup(agent, account)
	if err != nil {
		return nil, fmt.Errorf("resolve account %q for %s: %w", account, agent, err)
	}
	scoped, err := ApplyAccount(environ, command, scope)
	if err != nil {
		return nil, fmt.Errorf("scope session to account %q: %w", account, err)
	}
	return scoped, nil
}
