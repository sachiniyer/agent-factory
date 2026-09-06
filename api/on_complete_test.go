package api

import (
	"encoding/json"
	"testing"

	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/task"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAPICatalogListsOnComplete(t *testing.T) {
	assert.Contains(t, runAPICmd(t, true), "/v1/ListOnComplete")
	values := task.OnCompleteValues()
	response := apiproto.ListOnCompleteResponse{}
	for _, value := range values {
		response.Values = append(response.Values, apiproto.OnCompleteOption{Value: value, Hint: task.OnCompleteHint(value)})
	}
	raw, err := json.Marshal(apiproto.Success(response))
	require.NoError(t, err)
	var wire struct {
		Data struct {
			Values []struct {
				Value string `json:"value"`
				Hint  string `json:"hint"`
			} `json:"values"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(raw, &wire))
	require.Len(t, wire.Data.Values, len(values))
	for i, option := range wire.Data.Values {
		assert.Equal(t, values[i], option.Value)
		assert.Equal(t, task.OnCompleteHint(values[i]), option.Hint)
	}
}

// Both API verbs must leave normalization to the daemon's real store path.
func TestTasksKeepCanonicalizesOnCreateAndUpdate(t *testing.T) {
	useTempConfig(t)
	resetAddFlags(t)
	resetUpdateFlags(t)
	calls := stubDaemon(t)
	setupAddRepo(t)
	t.Cleanup(func() {
		taskAddOnCompleteFlag = ""
		taskUpdateOnCompleteFlag = ""
		tasksUpdateCmd.Flags().Lookup("on-complete").Changed = false
	})
	taskAddNameFlag = "keep-policy"
	taskAddPromptFlag = "work"
	taskAddCronFlag = "0 3 * * *"
	taskAddProgramFlag = "claude"
	taskAddOnCompleteFlag = "keep"
	sentAdd := daemonAddTask
	daemonAddTask = func(tk task.Task, actor task.Actor) error {
		assert.Equal(t, "keep", tk.OnComplete, "create sends the selected verb")
		return sentAdd(tk, actor)
	}
	require.NoError(t, tasksAddCmd.RunE(tasksAddCmd, nil))
	stored, err := task.LoadTasks()
	require.NoError(t, err)
	require.Len(t, stored, 1)
	assert.Empty(t, stored[0].OnComplete, "create stores keep as empty")
	archive := task.OnCompleteArchive
	_, err = task.UpdateTask(stored[0].ID, task.TaskUpdate{OnComplete: &archive}, task.ProjectExpectation{})
	require.NoError(t, err)
	require.NoError(t, tasksUpdateCmd.Flags().Set("on-complete", "keep"))
	require.NoError(t, tasksUpdateCmd.RunE(tasksUpdateCmd, []string{stored[0].ID}))
	require.NotNil(t, calls.lastUpdate.OnComplete)
	assert.Equal(t, "keep", *calls.lastUpdate.OnComplete, "update sends the selected verb")
	got, err := task.GetTask(stored[0].ID)
	require.NoError(t, err)
	assert.Empty(t, got.OnComplete, "update stores keep as empty")
}
