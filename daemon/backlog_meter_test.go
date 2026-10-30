package daemon

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/planusage"

	"github.com/stretchr/testify/require"
)

func TestGetBudgetMetersAndPublishesSpend(t *testing.T) {
	cs, _ := newBacklogTestServer(t)
	claudeDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	now := time.Now().UTC()
	line := `{"type":"assistant","timestamp":"` + now.Add(-time.Minute).Format(time.RFC3339) +
		`","message":{"id":"m1","model":"claude-opus-5-5","usage":{"input_tokens":1000000,"output_tokens":0}}}` + "\n"
	transcript := filepath.Join(claudeDir, "projects", "-home-me-x", "s.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(transcript), 0o755))
	require.NoError(t, os.WriteFile(transcript, []byte(line+line), 0o644))
	reset := now.Add(2 * time.Hour).Truncate(time.Hour)
	require.NoError(t, planusage.WriteReading(planusage.DefaultAccount, planusage.Reading{
		FiveHour: &planusage.Limit{UsedPercentage: 30, ResetsAt: reset.Unix()}, ReadAt: now,
	}))
	id, ch, _ := cs.manager.events.subscribeWithOverflow()
	defer cs.manager.events.unsubscribe(id)

	code, env := postProposalRoute(t, cs, "GetBudget", GetBudgetRequest{})
	require.Equal(t, http.StatusOK, code, env)
	require.Equal(t, true, env["data"].(map[string]any)["metering"], "the first read is served before any pass")
	st := &cs.manager.backlog.meter
	st.mu.Lock()
	cs.manager.meterPassLocked(time.Now())
	st.mu.Unlock()

	code, env = postProposalRoute(t, cs, "GetBudget", GetBudgetRequest{})
	require.Equal(t, http.StatusOK, code, env)
	data := env["data"].(map[string]any)
	require.Nil(t, data["metering"])
	require.Contains(t, data["accounts"], planusage.DefaultAccount)
	require.Equal(t, float64(50), data["budget"].(map[string]any)["share"])
	five := data["windows"].(map[string]any)["five_hour"].(map[string]any)
	require.Equal(t, float64(30), five["plan"])
	require.InDelta(t, 4.0, five["all_cost"], 1e-9)
	require.Contains(t, data, "run_window")
	require.Equal(t, float64(0), five["backlog"], "no item runs: nothing is the backlog's")
	require.NotNil(t, data["reading_at"])
	require.Nil(t, data["windows"].(map[string]any)["seven_day"].(map[string]any)["resets_at"])

	state, err := planusage.LoadMeter()
	require.NoError(t, err)
	total := 0.0
	for _, b := range state.Accounts[planusage.DefaultAccount].Buckets {
		total += b.All
	}
	require.InDelta(t, 4.0, total, 1e-9, "the duplicate line is counted once")

	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type != agentproto.EventBacklogSpend {
				continue
			}
			var payload map[string]any
			require.NoError(t, json.Unmarshal(ev.Data, &payload))
			require.Contains(t, payload, "five_hour")
			require.Contains(t, payload, "runs")
			require.Contains(t, payload, "run_window")
			require.Contains(t, payload, "unmeterable")
			return
		case <-deadline:
			t.Fatal("no backlog.spend event")
		}
	}
}
