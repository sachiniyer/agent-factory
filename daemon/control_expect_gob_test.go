package daemon

import (
	"bytes"
	"encoding/gob"
	"errors"
	"net"
	"net/rpc"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
	"github.com/sachiniyer/agent-factory/task"
)

type committedTaskRPC struct{}

func (committedTaskRPC) AddTask(_ AddTaskRequest, resp *AddTaskResponse) error {
	resp.OK = true
	// Mirrors the real handler since #3036: a committed mutation answers OK and
	// reports itself in the response envelope, because net/rpc would strip a
	// concrete error down to its string.
	resp.record(&mutationCommittedError{err: errors.New(taskAddCommittedErrorPrefix + " simulated")})
	return nil
}

// cleanTaskRPC is the negative control: a server that fails with NOTHING
// committed. The client must not promote it.
type cleanTaskRPC struct{}

func (cleanTaskRPC) AddTask(_ AddTaskRequest, _ *AddTaskResponse) error {
	return errors.New("task add failed before commit")
}

// TestControlClientPreservesMutationCommittedOutcome crosses a real isolated
// net/rpc socket. net/rpc normally flattens the server error to rpc.ServerError;
// the client must restore the definite committed outcome without classifying
// unrelated transport or application failures as committed.
func TestControlClientPreservesMutationCommittedOutcome(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	socketPath, err := DaemonSocketPath()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(socketPath), 0o755))

	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	server := rpc.NewServer()
	require.NoError(t, server.RegisterName(controlServiceName, committedTaskRPC{}))
	go func() {
		conn, acceptErr := listener.Accept()
		if acceptErr == nil {
			server.ServeConn(conn)
		}
	}()

	err = callDaemonNoEnsure("AddTask", AddTaskRequest{}, &AddTaskResponse{})
	require.Error(t, err)
	type committedOutcome interface{ MutationCommitted() bool }
	var outcome committedOutcome
	require.True(t, errors.As(err, &outcome) && outcome.MutationCommitted(),
		"the control client must preserve the server's definite committed outcome: %T: %v", err, err)

	// The other half, and the reason the old prefix-matching classifier existed:
	// the client must not INVENT a committed outcome. Under the envelope that is
	// no longer a string-matching question — a server that fails with nothing
	// committed leaves Committed false, and its error must stay an ordinary one.
	t.Run("a failure with nothing committed is not promoted", func(t *testing.T) {
		listener2, lerr := net.Listen("unix", filepath.Join(testguard.SocketTempDir(t), "clean.sock"))
		require.NoError(t, lerr)
		defer listener2.Close()
		t.Setenv("AF_DAEMON_SOCKET", listener2.Addr().String())

		server2 := rpc.NewServer()
		require.NoError(t, server2.RegisterName(controlServiceName, cleanTaskRPC{}))
		go func() {
			conn, acceptErr := listener2.Accept()
			if acceptErr == nil {
				server2.ServeConn(conn)
			}
		}()

		cleanErr := callDaemonNoEnsure("AddTask", AddTaskRequest{}, &AddTaskResponse{})
		require.Error(t, cleanErr)
		var promoted committedOutcome
		assert.False(t, errors.As(cleanErr, &promoted),
			"an outcome with nothing committed must not be promoted: %T: %v", cleanErr, cleanErr)
	})
}

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
