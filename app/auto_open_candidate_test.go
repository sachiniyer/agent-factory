package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// TestFirstAutoOpenCandidate covers the #1238 relaunch trap: the cold-start
// auto-open must prefer a non-reserved instance so a relaunch (e.g. right after
// an auto-update) doesn't greet the user with root front-and-center — root pins
// first in the store order, so instances[0] is always root when present.
func TestFirstAutoOpenCandidate(t *testing.T) {
	mk := func(title string) *session.Instance {
		restore := session.SetBackendFactoryForTest(func(_ session.InstanceOptions, _ string) (session.Backend, error) {
			return session.NewFakeBackend(), nil
		})
		defer restore()
		inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: t.TempDir(), Program: "claude"})
		require.NoError(t, err)
		return inst
	}

	t.Run("empty list returns nil", func(t *testing.T) {
		assert.Nil(t, firstAutoOpenCandidate(nil))
		assert.Nil(t, firstAutoOpenCandidate([]*session.Instance{}))
	})

	t.Run("root is the sole session falls back to root", func(t *testing.T) {
		root := mk(session.RootSessionTitle)
		assert.Same(t, root, firstAutoOpenCandidate([]*session.Instance{root}))
	})

	t.Run("prefers the first non-reserved instance over a top-pinned root", func(t *testing.T) {
		// Store order pins root first (ui/store/order.go); the candidate must
		// skip it for the first real session.
		root := mk(session.RootSessionTitle)
		work := mk("feature-x")
		other := mk("feature-y")
		got := firstAutoOpenCandidate([]*session.Instance{root, work, other})
		assert.Same(t, work, got)
	})

	t.Run("no root present returns the first instance", func(t *testing.T) {
		a := mk("feature-x")
		b := mk("feature-y")
		assert.Same(t, a, firstAutoOpenCandidate([]*session.Instance{a, b}))
	})
}
