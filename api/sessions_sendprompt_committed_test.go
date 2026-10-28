package api

import (
	"encoding/json"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/session"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type committedAutoCreateTestError struct{}

func (committedAutoCreateTestError) Error() string {
	return "session created, but its start could not be confirmed"
}
func (committedAutoCreateTestError) MutationCommitted() bool { return true }

func stubSendPromptCreate(t *testing.T, status session.PromptDeliveryStatus, deliverErr error) {
	t.Helper()
	setupRepoForCmd(t)
	prevCreate, prevPreflight, prevDeliver := sendPromptCreateFlag, preflightLocalSession, deliverPromptViaDaemon
	sendPromptCreateFlag = true
	preflightLocalSession = func(*config.Config, string) error { return nil }
	deliverPromptViaDaemon = func(daemon.DeliverPromptRequest) (string, session.PromptDeliveryStatus, error) {
		return "started", status, deliverErr
	}
	t.Cleanup(func() {
		sendPromptCreateFlag, preflightLocalSession, deliverPromptViaDaemon = prevCreate, prevPreflight, prevDeliver
	})
}

// A committed auto-create made the session durable, so send-prompt --create
// exits 0 with a warning (as kill/archive/restore do) instead of a failure a
// script would retry, sending the prompt twice (#5761).
func TestSendPromptCreateReportsCommittedAutoCreateAsSuccess(t *testing.T) {
	stubSendPromptCreate(t, "", committedAutoCreateTestError{})

	out, err := runCmdCaptureStdout(t, sessionsSendPromptCmd, []string{"worker", "hi"})
	require.NoError(t, err, "a committed auto-create must exit 0")

	var parsed map[string]any
	require.NoError(t, json.Unmarshal(out, &parsed))
	assert.Equal(t, true, parsed["ok"])
	assert.Equal(t, string(session.PromptCouldNotConfirm), parsed["status"],
		"no delivery proof: the status must not claim delivery")
	assert.Contains(t, parsed["warning"], "could not be confirmed")
}

func TestSendPromptCreateStillHardFailsOnCleanError(t *testing.T) {
	stubSendPromptCreate(t, "", &plainKillError{msg: "daemon refused"})

	_, err := runCmdCaptureStdout(t, sessionsSendPromptCmd, []string{"worker", "hi"})
	require.Error(t, err)
}
