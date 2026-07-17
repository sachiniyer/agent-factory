package daemon

import (
	"bytes"
	"encoding/gob"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/task"
)

// The control socket is net/rpc with gob encoding, and gob ELIDES zero-valued
// fields. That is what made *T optional fields silently arrive as nil in #1700,
// so any new optional RPC field has to be shown surviving the round trip rather
// than assumed to.
//
// The load-bearing case is {Enforce: true, ProjectPath: ""} — "I authorized this
// task while it was unbound". ProjectPath is the zero string and gets elided on
// the wire, so the receiver reconstructs it from ITS zero value. That is only
// safe because the field is a plain string whose zero value is the value sent;
// a *string would have arrived nil and turned the check off silently.
func TestProjectExpectationSurvivesGob(t *testing.T) {
	for _, tc := range []struct {
		name   string
		expect task.ProjectExpectation
	}{
		{"no expectation", task.ProjectExpectation{}},
		{"bound project", task.ProjectExpectation{Enforce: true, ProjectPath: "/repos/alpha"}},
		{"enforced but unbound", task.ProjectExpectation{Enforce: true, ProjectPath: ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			require.NoError(t, gob.NewEncoder(&buf).Encode(RemoveTaskRequest{ID: "a1b2c3d4", Expect: tc.expect}))

			var got RemoveTaskRequest
			require.NoError(t, gob.NewDecoder(&buf).Decode(&got))
			assert.Equal(t, tc.expect, got.Expect, "the expectation must survive the control socket intact")
		})
	}
}

// TestProjectExpectationGobSkewDefaultsToNoCheck pins the version-skew shape: a
// NEW client sending Expect to an OLD daemon that has no such field, and an OLD
// client sending none to a new daemon. The latter must decode as "no
// expectation" rather than as an enforced empty one, which would refuse every
// bound task.
func TestProjectExpectationGobSkewDefaultsToNoCheck(t *testing.T) {
	type oldRemoveTaskRequest struct{ ID string }

	var buf bytes.Buffer
	require.NoError(t, gob.NewEncoder(&buf).Encode(oldRemoveTaskRequest{ID: "a1b2c3d4"}))

	var got RemoveTaskRequest
	require.NoError(t, gob.NewDecoder(&buf).Decode(&got))
	assert.Equal(t, "a1b2c3d4", got.ID)
	assert.False(t, got.Expect.Enforce, "an older client's request must decode as no expectation, not an enforced empty one")
	require.NoError(t, got.Expect.Verify(task.Task{ID: "a1b2c3d4", ProjectPath: "/repos/alpha"}),
		"no expectation must not refuse a bound task")
}
