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

// legacyCommittedTaskRPC is a pre-#3036 daemon: it reports a committed outcome
// by returning an ERROR carrying the shared prefix, with no envelope at all.
type legacyCommittedTaskRPC struct{}

func (legacyCommittedTaskRPC) AddTask(_ AddTaskRequest, _ *AddTaskResponse) error {
	return errors.New(taskAddCommittedErrorPrefix + " simulated reload failure")
}

// TestControlClientPreservesMutationCommittedOutcome crosses a real isolated
// net/rpc socket. net/rpc normally flattens the server error to rpc.ServerError;
// the client must restore the definite committed outcome without classifying
// unrelated transport or application failures as committed.
// serveControlRPC points the control client at a private socket under its own
// AGENT_FACTORY_HOME and serves handler on it. The accept loop is a LOOP on
// purpose: a single-shot Accept leaves the socket dialable but unattended, so a
// second call blocks on read until the package-wide test timeout instead of
// failing — which is exactly how the first version of this test hung CI.
func serveControlRPC(t *testing.T, handler any) {
	t.Helper()
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	socketPath, err := DaemonSocketPath()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(socketPath), 0o755))

	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })

	server := rpc.NewServer()
	require.NoError(t, server.RegisterName(controlServiceName, handler))
	go func() {
		for {
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			go server.ServeConn(conn)
		}
	}()
}

func TestControlClientPreservesMutationCommittedOutcome(t *testing.T) {
	serveControlRPC(t, committedTaskRPC{})

	err := callDaemonNoEnsure("AddTask", AddTaskRequest{}, &AddTaskResponse{})
	require.Error(t, err)
	type committedOutcome interface{ MutationCommitted() bool }
	var outcome committedOutcome
	require.True(t, errors.As(err, &outcome) && outcome.MutationCommitted(),
		"the control client must preserve the server's definite committed outcome: %T: %v", err, err)
}

// TestControlClientDoesNotInventCommittedOutcome is the negative control, and it
// runs as its OWN test rather than a subtest: it needs a different
// AGENT_FACTORY_HOME, and the earlier version set an env var
// (`AF_DAEMON_SOCKET`) that NOTHING in the tree reads, so it silently dialled the
// other test's socket and hung rather than asserting anything.
func TestControlClientDoesNotInventCommittedOutcome(t *testing.T) {
	serveControlRPC(t, cleanTaskRPC{})

	cleanErr := callDaemonNoEnsure("AddTask", AddTaskRequest{}, &AddTaskResponse{})
	require.Error(t, cleanErr)
	require.Contains(t, cleanErr.Error(), "task add failed before commit",
		"the test must reach the clean-failure handler, not some other socket")
	type committedOutcome interface{ MutationCommitted() bool }
	var promoted committedOutcome
	assert.False(t, errors.As(cleanErr, &promoted),
		"an outcome with nothing committed must not be promoted: %T: %v", cleanErr, cleanErr)
}

// TestControlClientClassifiesLegacyCommittedRPCError pins the skew path: the
// task handlers answer with an ERROR, so net/rpc sends no body and an OLDER
// daemon's committed marker survives only as a flattened rpc.ServerError.
func TestControlClientClassifiesLegacyCommittedRPCError(t *testing.T) {
	serveControlRPC(t, legacyCommittedTaskRPC{})

	err := callDaemonNoEnsure("AddTask", AddTaskRequest{}, &AddTaskResponse{})
	require.Error(t, err)
	require.True(t, isMutationCommitted(err),
		"an older daemon's committed task failure read as an ordinary one; a retry would duplicate the task: %T: %v", err, err)
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
