package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/require"
)

// TestQuotaReport_ServesTheHostReport proves the RPC hands callers the host's
// own usage report (#4361): rows for the agents whose sessions it can see,
// each parked session's sighting time carried through, and the standing note —
// the same cells a daemonless `af quota` prints, so a remote caller reads the
// answering host's evidence worded identically. The handler has no manager
// dependency: the records live on disk and are read fresh per call.
func TestQuotaReport_ServesTheHostReport(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	observed := time.Unix(1_690_000_000, 0).UTC()
	reset := observed.Add(6 * 24 * time.Hour)
	raw, err := json.Marshal([]session.InstanceData{
		{Title: "parked", Program: "codex", Liveness: session.LiveLimitReached,
			LimitResetAt: reset, LimitObservedAt: observed},
		{Title: "live", Program: "claude", Liveness: session.LiveRunning},
	})
	require.NoError(t, err)
	require.NoError(t, config.SaveRepoInstances("repo", raw))

	cs := &controlServer{}
	var resp QuotaReportResponse
	require.NoError(t, cs.QuotaReport(QuotaReportRequest{}, &resp))

	require.NotEmpty(t, resp.Rows, "the report must carry one row per agent")
	require.NotEmpty(t, resp.Note, "the report must carry its framing note for surfaces to render")
	var foundCodex bool
	for _, row := range resp.Rows {
		if row.Agent != "codex" {
			continue
		}
		foundCodex = true
		require.Equal(t, "limit reached", row.Observed)
		require.Contains(t, row.Detail, observed.Format(time.RFC3339),
			"the parked row must name when af saw the wall (#4361)")
	}
	require.True(t, foundCodex, "rows = %+v, want a codex row", resp.Rows)
}
