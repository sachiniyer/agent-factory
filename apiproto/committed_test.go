package apiproto

import (
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// committedStub is a carrier defined outside the packages that raise the marker
// in production, which is the point: IsMutationCommitted must classify ANY type
// carrying the method, because the daemon, its control client and the HTTP
// client each own a different one.
type committedStub struct{ err error }

var _ MutationCommittedError = (*committedStub)(nil)

func (e *committedStub) Error() string           { return e.err.Error() }
func (e *committedStub) Unwrap() error           { return e.err }
func (e *committedStub) MutationCommitted() bool { return true }

// notCommittedStub carries the method and answers false. It pins the second half
// of the predicate: presence in the chain is not enough, the answer is read.
type notCommittedStub struct{ err error }

func (e *notCommittedStub) Error() string           { return e.err.Error() }
func (e *notCommittedStub) MutationCommitted() bool { return false }

func TestIsMutationCommitted(t *testing.T) {
	committed := &committedStub{err: errors.New("hook failed after the archive landed")}

	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil is not committed", nil, false},
		{"a plain error is not committed", errors.New("refused"), false},
		{"the marker itself", committed, true},
		// The wrapped cases are the reason the rule is one function rather than
		// a type switch at each call site: a mutation's error acquires context
		// on its way up, and the classification has to survive that.
		{"wrapped with %w", fmt.Errorf("archive alpha: %w", committed), true},
		{"wrapped twice", fmt.Errorf("outer: %w", fmt.Errorf("inner: %w", committed)), true},
		{"joined with another error", errors.Join(errors.New("first"), committed), true},
		{"carrier answering false", &notCommittedStub{err: errors.New("refused")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsMutationCommitted(tc.err))
		})
	}
}

// TestMutationCommittedErrorMatchesUnwrap keeps the interface honest about the
// shape the production carriers use: they wrap an inner error and expose it, so
// a caller can still read the underlying failure it must surface.
func TestMutationCommittedErrorMatchesUnwrap(t *testing.T) {
	inner := errors.New("post-commit hook failed")
	err := error(&committedStub{err: inner})

	require.True(t, IsMutationCommitted(err))
	assert.ErrorIs(t, err, inner, "the committed marker must not hide why the follow-up failed")
}
