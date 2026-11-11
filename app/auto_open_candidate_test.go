package app

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// TestFirstAutoOpenCandidate pins the cold-start auto-open choice: the root
// session wherever it sits in the list, else the first instance, else nil.
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

	t.Run("root as the sole session returns root", func(t *testing.T) {
		root := mk(session.RootSessionTitle)
		assert.Same(t, root, firstAutoOpenCandidate([]*session.Instance{root}))
	})

	t.Run("root listed first among others returns root", func(t *testing.T) {
		root := mk(session.RootSessionTitle)
		work := mk("feature-x")
		other := mk("feature-y")
		assert.Same(t, root, firstAutoOpenCandidate([]*session.Instance{root, work, other}))
	})

	t.Run("root not listed first still returns root", func(t *testing.T) {
		// Store order is not relied on: root is found wherever it sits.
		work := mk("feature-x")
		root := mk(session.RootSessionTitle)
		other := mk("feature-y")
		assert.Same(t, root, firstAutoOpenCandidate([]*session.Instance{work, other, root}))
		assert.Same(t, root, firstAutoOpenCandidate([]*session.Instance{work, root, other}))
	})

	t.Run("no root present returns the first instance", func(t *testing.T) {
		a := mk("feature-x")
		b := mk("feature-y")
		assert.Same(t, a, firstAutoOpenCandidate([]*session.Instance{a, b}))
	})
}
