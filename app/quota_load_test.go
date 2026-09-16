package app

import (
	"errors"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/quota"
	"github.com/stretchr/testify/require"
)

// The Usage section's load path (#4361). These pin the same contract the
// Accounts section's tests do: a remote read never runs on the UI loop, never
// mutates the pane from its own goroutine, and a stale completion can never
// overwrite a newer opening — because the silent version of each is a pane
// showing the wrong host's evidence as if it were fresh.

func usageReportWith(agent, detail string) daemon.QuotaReportResponse {
	return daemon.QuotaReportResponse{
		Rows: []quota.Row{{Agent: agent, Quota: "not reported", Observed: "limit reached", Detail: detail}},
		Note: quota.ReportNote,
	}
}

func TestUsageLoadRemoteDefersCompletionToUILoop(t *testing.T) {
	h := remoteAccountLoadHome(t)
	calls := 0
	t.Cleanup(SetUsageSeamForTest(func(daemon.QuotaReportRequest) (daemon.QuotaReportResponse, error) {
		calls++
		return usageReportWith("stubagent", "the remote wall"), nil
	}))
	_, cmd := h.showConfigEditor()
	require.Zero(t, calls, "opening the remote pane must not run QuotaReport on the UI loop")
	require.NotNil(t, cmd)
	sizeConfigPane(h)
	require.Contains(t, h.configPane.String(), "Loading usage…")
	msgs := sectionsMsgs(t, cmd)
	require.Equal(t, 1, calls)
	updateAll(h, msgs)
	require.Contains(t, h.configPane.String(), "the remote wall")
	require.NotContains(t, h.configPane.String(), "Loading usage…")
	require.True(t, h.configPane.HasFocus())
	require.Equal(t, stateConfigEditor, h.state)
}

func TestUsageLoadRemoteErrorStaysInSection(t *testing.T) {
	h := remoteAccountLoadHome(t)
	t.Cleanup(SetUsageSeamForTest(func(daemon.QuotaReportRequest) (daemon.QuotaReportResponse, error) {
		return daemon.QuotaReportResponse{}, errors.New("remote usage report unavailable")
	}))
	_, cmd := h.showConfigEditor()
	sizeConfigPane(h)
	require.NotContains(t, h.configPane.String(), "remote usage report unavailable")
	require.NotNil(t, cmd)
	updateAll(h, sectionsMsgs(t, cmd))
	require.Contains(t, h.configPane.String(), "remote usage report unavailable")
	require.NotContains(t, h.configPane.String(), "Loading usage…")
}

func TestUsageLoadRemoteIgnoresCompletionAfterReopen(t *testing.T) {
	h := remoteAccountLoadHome(t)
	calls := 0
	t.Cleanup(SetUsageSeamForTest(func(daemon.QuotaReportRequest) (daemon.QuotaReportResponse, error) {
		calls++
		detail := "old-opening"
		if calls > 1 {
			detail = "current-opening"
		}
		return usageReportWith("stubagent", detail), nil
	}))
	_, first := h.showConfigEditor()
	require.NotNil(t, first)
	stale := sectionsMsgs(t, first)
	h.Update(tea.KeyMsg{Type: tea.KeyEsc})
	_, second := h.showConfigEditor()
	require.NotNil(t, second)
	sizeConfigPane(h)
	pending := h.configPane.String()
	updateAll(h, stale)
	require.Equal(t, pending, h.configPane.String(), "an earlier opening's report must not replace the pending load")
	updateAll(h, sectionsMsgs(t, second))
	current := h.configPane.String()
	require.Contains(t, current, "current-opening")
	updateAll(h, stale)
	require.Equal(t, current, h.configPane.String(), "an earlier opening's report must not replace the current one")
}

// The local read is in-process through quotahost: it applies during open with
// no command, and — the property that matters — it can never reach a daemon.
func TestUsageLoadLocalAppliesInline(t *testing.T) {
	h := newTestHome(t)
	calls := 0
	t.Cleanup(SetUsageSeamForTest(func(daemon.QuotaReportRequest) (daemon.QuotaReportResponse, error) {
		calls++
		return usageReportWith("localagent", "the local wall"), nil
	}))
	_, cmd := h.showConfigEditor()
	require.Nil(t, cmd, "a local open owes no remote command")
	require.Equal(t, 1, calls, "the local usage read is inline, like the local accounts read")
	sizeConfigPane(h)
	require.Contains(t, h.configPane.String(), "the local wall")
}
