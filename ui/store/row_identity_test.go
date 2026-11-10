package store

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

// namingProjection holds a session titled "alpha" and a naming row whose draft
// spells the same title, the #7060 collision.
func namingProjection(t *testing.T) (p *Projection, existing, naming *session.Instance) {
	t.Helper()
	p = NewProjection()
	existing = newUnstartedProjectionInstance(t, "alpha")
	naming = newUnstartedProjectionInstance(t, "alpha")
	p.AddInstance(existing)
	p.AddInstance(naming)
	p.SetNaming(naming)
	return p, existing, naming
}

// TestSameRowNeverMatchesTheNamingRowByTitle is the seam #7060 moved every
// selection rule onto: a same-title replacement is the same row, but a session
// the naming row's draft merely spells is not.
func TestSameRowNeverMatchesTheNamingRowByTitle(t *testing.T) {
	p, existing, naming := namingProjection(t)

	assert.True(t, p.SameRow(naming, naming), "a row is always itself")
	assert.True(t, p.SameRow(existing, existing))
	assert.False(t, p.SameRow(naming, existing), "a draft title is not an identity")
	assert.False(t, p.SameRow(existing, naming), "in either argument order")
	assert.False(t, p.SameRow(nil, existing), "nil matches nothing")
	assert.False(t, p.SameRow(existing, nil))
	assert.False(t, p.SameRow(nil, nil))

	// A #765 kill+recreate: a new instance carrying the old row's title is the
	// same row, so the selection, fold and tab memo follow it.
	replacement := newUnstartedProjectionInstance(t, "alpha")
	assert.True(t, p.SameRow(existing, replacement), "a same-title replacement is the same row")

	// Once naming ends the title is committed, and it is an identity again.
	p.SetNaming(nil)
	assert.True(t, p.SameRow(naming, existing))
}

// TestTitleResolversSkipTheNamingRow pins the by-title lookups: whatever order
// the rows sort in, a draft title never resolves to the naming row, so a
// replace or remove by title cannot hit it either.
func TestTitleResolversSkipTheNamingRow(t *testing.T) {
	p, existing, naming := namingProjection(t)
	assert.Same(t, existing, p.GetInstanceByTitle("alpha"))

	// Whichever row is the naming one is skipped, wherever it sorts (it is
	// normally the newest, so last).
	p.SetNaming(nil)
	p.SetNaming(existing)
	assert.Same(t, naming, p.GetInstanceByTitle("alpha"))
	p.SetNaming(naming)

	replacement := newUnstartedProjectionInstance(t, "alpha")
	require.True(t, p.ReplaceInstanceByTitle("alpha", replacement))
	assert.True(t, p.ContainsInstance(naming), "the naming row is never the row replaced by title")
	assert.False(t, p.ContainsInstance(existing))

	require.True(t, p.RemoveInstanceByTitle("alpha"))
	assert.True(t, p.ContainsInstance(naming), "the naming row is never the row removed by title")
	assert.False(t, p.ContainsInstance(replacement))
	assert.False(t, p.RemoveInstanceByTitle("alpha"), "only the naming row is left, so nothing answers")

	// Not the naming row: a submitted create is found by its title again.
	p.SetNaming(nil)
	assert.Same(t, naming, p.GetInstanceByTitle("alpha"))
}

// TestResolvePrefersThePointerThenTheTitle pins the reconcile's re-pin: a held
// pointer wins over any same-title row, and the title only answers for a
// pointer a swap dropped. The naming row resolves only to itself.
func TestResolvePrefersThePointerThenTheTitle(t *testing.T) {
	p, existing, naming := namingProjection(t)

	assert.Same(t, naming, p.Resolve(naming), "the naming row keeps its own pointer")
	assert.Same(t, existing, p.Resolve(existing))
	assert.Nil(t, p.Resolve(nil))

	replacement := newUnstartedProjectionInstance(t, "alpha")
	require.True(t, p.ReplaceInstance(existing, replacement))
	assert.Same(t, replacement, p.Resolve(existing),
		"a swapped-out row resolves to its same-title replacement, never to the naming row")
	assert.Same(t, naming, p.Resolve(naming), "and the naming row still resolves to itself")
}

// TestSetNamingBumpsTheVersion keeps derived views honest: the rail derives its
// rows from SameRow, so marking or clearing the naming row must invalidate them.
func TestSetNamingBumpsTheVersion(t *testing.T) {
	p := NewProjection()
	inst := newUnstartedProjectionInstance(t, "draft")
	p.AddInstance(inst)

	v := p.Version()
	p.SetNaming(inst)
	assert.Greater(t, p.Version(), v)
	assert.Same(t, inst, p.Naming())

	v = p.Version()
	p.SetNaming(inst)
	assert.Equal(t, v, p.Version(), "an unchanged marker is not a mutation")

	p.SetNaming(nil)
	assert.Greater(t, p.Version(), v)
	assert.Nil(t, p.Naming())
}
