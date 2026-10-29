package apiclient

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/task"
)

// #5859: a remote client cannot check a path that lives on the daemon's host, so
// every task create and edit it sends asks the daemon to.
func TestRemoteTaskWritesAskTheDaemonToValidateThePath(t *testing.T) {
	bodies := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		bodies[r.URL.Path] = m
		switch r.URL.Path {
		case "/v1/AddTask":
			_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.AddTaskResponse{OK: true}))
		default:
			_ = apiproto.WriteEnvelope(w, apiproto.Success(daemon.UpdateTaskResponse{OK: true}))
		}
	}))
	defer srv.Close()
	c, err := NewRemote(srv.URL, "")
	require.NoError(t, err)

	require.NoError(t, c.AddTask(task.Task{ID: "t1", ProjectPath: "~/alpha"}, task.ActorCLI))
	require.Equal(t, true, bodies["/v1/AddTask"]["validate_path"])
	path := "~/beta"
	_, err = c.UpdateTask("t1", task.TaskUpdate{ProjectPath: &path}, task.ProjectExpectation{}, task.ActorCLI)
	require.NoError(t, err)
	require.Equal(t, true, bodies["/v1/UpdateTask"]["validate_path"])
	require.True(t, c.isRemote())
}
