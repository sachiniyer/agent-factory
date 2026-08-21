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
func applyAccountScope(environ []string, agent, account, command string, proof AccountLaunchProof) ([]string, error) {
	return applyNamedAccountScope(environ, agent, account, command, proof, true)
}

func applyAccountEnvironmentScope(environ []string, agent, account, command string) ([]string, error) {
	return applyNamedAccountScope(environ, agent, account, command, AccountLaunchProof{}, false)
}

// ResolveAccountEnvironment returns the non-secret environment entries that
// identify a selected account. Launchers use them for inherited process
// boundaries such as tmux-created windows; the child exec shim still repeats
// the lookup and applies the complete boundary before the initial pane starts.
func ResolveAccountEnvironment(agent, account string) ([]string, error) {
	scope, err := lookupNamedAccount(agent, account)
	if err != nil {
		return nil, err
	}
	return ApplyAccountEnvironment(nil, "", scope)
}

func applyNamedAccountScope(environ []string, agent, account, command string, proof AccountLaunchProof, validateCommand bool) ([]string, error) {
	scope, err := lookupNamedAccount(agent, account)
	if err != nil {
		return nil, err
	}
	// The launcher's claim about its OWN executable/arguments, attached to the scope the
	// lookup resolved. AccountLookup answers "which directory", never "which
	// command" — those facts travel with the launch, not with the registry (#3083, #3108).
	scope.TrustedExecutable = proof.TrustedExecutable
	scope.GeneratedArgs = proof.GeneratedArgs
	var scoped []string
	if validateCommand {
		scoped, err = ApplyAccount(environ, command, scope)
	} else {
		scoped, err = ApplyAccountEnvironment(environ, command, scope)
	}
	if err != nil {
		return nil, fmt.Errorf("scope session to account %q: %w", account, err)
	}
	return scoped, nil
}

func lookupNamedAccount(agent, account string) (Account, error) {
	if AccountLookup == nil {
		return Account{}, fmt.Errorf("account-scoped launch requested but no account lookup is installed")
	}
	scope, err := AccountLookup(agent, account)
	if err != nil {
		return Account{}, fmt.Errorf("resolve account %q for %s: %w", account, agent, err)
	}
	return scope, nil
}
