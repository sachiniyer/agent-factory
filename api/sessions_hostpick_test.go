package api

import (
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/daemon"
)

func stubHostPickSupport(t *testing.T, supported bool, version string, err error) {
	t.Helper()
	stubPing(t, daemon.PingResponse{HostPick: supported, DeliverHostPick: supported, TaskHost: supported, Version: version}, err)
}

func stubPing(t *testing.T, resp daemon.PingResponse, err error) {
	t.Helper()
	prev := pingViaDaemon
	pingViaDaemon = func() (daemon.PingResponse, error) { return resp, err }
	t.Cleanup(func() { pingViaDaemon = prev })
}

// A daemon that does not affirm host picks must never receive one: it would
// create the session elsewhere and the CLI would report success (#5746).
func TestRequireHostPickSupportRefusesAnOlderDaemon(t *testing.T) {
	stubHostPickSupport(t, false, "1.0.280", nil)
	err := requireHostPickSupport("box")
	require.Error(t, err)
	msg := err.Error()
	assert.Contains(t, msg, "daemon (v1.0.280) does not support --host here")
	assert.Contains(t, msg, `"box"`)
	assert.Contains(t, msg, "af daemon restart")
	assert.Contains(t, msg, "nothing was created or changed")

	stubHostPickSupport(t, false, "", nil)
	err = requireHostPickSupport("box")
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "predates version reporting"))
}

func TestRequireHostPickSupportFailsClosedWhenUnverifiable(t *testing.T) {
	stubHostPickSupport(t, false, "", errors.New("connection refused"))
	err := requireHostPickSupport("box")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "was not sent")
	assert.Contains(t, err.Error(), "connection refused")
}

func TestRequireHostPickSupportAllowsACapableDaemon(t *testing.T) {
	stubHostPickSupport(t, true, "1.0.300", nil)
	require.NoError(t, requireHostPickSupport("box"))
}

// Each surface has its own bit: a daemon that honors the create field but
// predates send-prompt's or a task's must still be refused there.
func TestRequireHostPickOnEachSurfaceNeedsItsOwnBit(t *testing.T) {
	createOnly := daemon.PingResponse{HostPick: true, Version: "1.0.310"}
	stubPing(t, createOnly, nil)
	require.NoError(t, requireHostPickOn(hostPickOnCreate, "box"))

	err := requireHostPickOn(hostPickOnDeliver, "box")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "daemon (v1.0.310)")
	assert.Contains(t, err.Error(), "af daemon restart")

	err = requireHostPickOn(hostPickOnTask, "box")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "store the task without a host")

	stubPing(t, daemon.PingResponse{HostPick: true, DeliverHostPick: true, TaskHost: true}, nil)
	require.NoError(t, requireHostPickOn(hostPickOnDeliver, "box"))
	require.NoError(t, requireHostPickOn(hostPickOnTask, "box"))
}

// --host picks where a NEW session is created; without --create it would be
// silently meaningless, so send-prompt refuses it.
func TestSendPromptHostNeedsCreate(t *testing.T) {
	prevHost, prevCreate, prevAll := sendPromptHostFlag, sendPromptCreateFlag, sendPromptAllFlag
	t.Cleanup(func() { sendPromptHostFlag, sendPromptCreateFlag, sendPromptAllFlag = prevHost, prevCreate, prevAll })

	sendPromptAllFlag, sendPromptCreateFlag, sendPromptHostFlag = false, false, "box"
	err := validateSendPromptFlags()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "needs --create")

	sendPromptCreateFlag = true
	require.NoError(t, validateSendPromptFlags())
}
