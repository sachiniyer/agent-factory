package sessionenv

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func withLookup(t *testing.T, fn func(agent, name string) (Account, error)) {
	t.Helper()
	previous := AccountLookup
	AccountLookup = fn
	t.Cleanup(func() { AccountLookup = previous })
}

// Two concurrent sessions on different accounts must see DIFFERENT credential
// roots, and neither may see the other's. Presence of the right value is not the
// property that matters — exclusivity is (#3051).
func TestApplyAccountScope_TwoAccountsSeeDifferentRootsAndNotEachOther(t *testing.T) {
	withLookup(t, func(agent, name string) (Account, error) {
		return Account{Agent: agent, Name: name, Dir: "/home/op/.af/accounts/codex/" + name}, nil
	})
	ambient := []string{"PATH=/usr/bin", "OPENAI_API_KEY=sk-ambient", "CODEX_HOME=/home/op/.codex"}

	a, err := applyAccountScope(ambient, "codex", "alpha", "codex", AccountLaunchProof{})
	require.NoError(t, err)
	b, err := applyAccountScope(ambient, "codex", "beta", "codex", AccountLaunchProof{})
	require.NoError(t, err)

	joinedA, joinedB := strings.Join(a, "\n"), strings.Join(b, "\n")
	require.Contains(t, joinedA, "CODEX_HOME=/home/op/.af/accounts/codex/alpha")
	require.Contains(t, joinedB, "CODEX_HOME=/home/op/.af/accounts/codex/beta")

	// Exclusivity, both directions.
	require.NotContains(t, joinedA, "accounts/codex/beta", "session alpha must not see beta's directory")
	require.NotContains(t, joinedB, "accounts/codex/alpha", "session beta must not see alpha's directory")

	// SUBTRACTION: the ambient key outranks the config dir, so a session that
	// still carried it would authenticate as whoever owns that key while
	// reporting the selected account.
	require.NotContains(t, joinedA, "OPENAI_API_KEY", "an ambient API key must not survive into a scoped session")
	require.NotContains(t, joinedB, "OPENAI_API_KEY")

	// And the ambient CODEX_HOME must be replaced, not merely appended after.
	require.Equal(t, 1, strings.Count(joinedA, "CODEX_HOME="),
		"exactly one CODEX_HOME must reach the agent")
}

// Every failure path must REFUSE, never return an unscoped environment. A
// fallback here is the silent wrong-account launch the feature exists to stop.
func TestApplyAccountScope_RefusesRatherThanFallingBack(t *testing.T) {
	ambient := []string{"OPENAI_API_KEY=sk-ambient"}

	// No lookup installed at all.
	withLookup(t, nil)
	_, err := applyAccountScope(ambient, "codex", "alpha", "codex", AccountLaunchProof{})
	require.Error(t, err, "a build with no lookup installed must refuse, not run unscoped")

	// Lookup fails (unregistered account).
	withLookup(t, func(string, string) (Account, error) {
		return Account{}, errNotRegistered
	})
	_, err = applyAccountScope(ambient, "codex", "ghost", "codex", AccountLaunchProof{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "ghost")

	// Boundary rejects the command: an unprovable program is not evidence the
	// account would be used.
	withLookup(t, func(agent, name string) (Account, error) {
		return Account{Agent: agent, Name: name, Dir: "/home/op/.af/accounts/codex/" + name}, nil
	})
	_, err = applyAccountScope(ambient, "codex", "alpha", "sh -c 'codex'", AccountLaunchProof{})
	require.Error(t, err, "an unprovable program must refuse rather than launch scoped-in-name-only")
}

var errNotRegistered = errNotRegisteredType{}

type errNotRegisteredType struct{}

func (errNotRegisteredType) Error() string { return "account is not registered" }
