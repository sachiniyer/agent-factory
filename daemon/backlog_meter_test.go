package daemon

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/planusage"

	"github.com/stretchr/testify/require"
)

func TestGetBudgetMetersAndPublishesSpend(t *testing.T) {
	cs, _ := newBacklogTestServer(t)
	cs.manager.cfg = &config.Config{Backlog: config.BacklogConfig{PlanUsage: true}}
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

// Plan usage is opt-in ([backlog] plan_usage, off by default): with it off, a
// metering pass opens no transcript and loads no reading, GetBudget says
// disabled, and nothing is written to the meter (owner decision on #6284).
func TestPlanUsageOffReadsNothing(t *testing.T) {
	cs, _ := newBacklogTestServer(t) // no config: plan_usage is off
	claudeDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	now := time.Now().UTC()
	line := `{"type":"assistant","timestamp":"` + now.Add(-time.Minute).Format(time.RFC3339) +
		`","message":{"id":"m1","model":"claude-opus-5-5","usage":{"input_tokens":1000000,"output_tokens":0}}}` + "\n"
	transcript := filepath.Join(claudeDir, "projects", "-home-me-x", "s.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(transcript), 0o755))
	require.NoError(t, os.WriteFile(transcript, []byte(line), 0o644))

	cs.manager.MeterBacklog()
	st := &cs.manager.backlog.meter
	st.mu.Lock()
	cs.manager.meterPassLocked(time.Now())
	st.mu.Unlock()

	code, env := postProposalRoute(t, cs, "GetBudget", GetBudgetRequest{})
	require.Equal(t, http.StatusOK, code, env)
	data := env["data"].(map[string]any)
	require.Equal(t, true, data["disabled"])
	require.Nil(t, data["metering"], "a disabled meter is not 'still metering'")
	require.Nil(t, data["accounts"])

	home, err := config.GetConfigDir()
	require.NoError(t, err)
	_, statErr := os.Stat(filepath.Join(home, "plan"))
	require.True(t, os.IsNotExist(statErr), "nothing is stored under $AF_HOME/plan while plan usage is off")
}

// When the daemon's CLAUDE_CONFIG_DIR is a registered account's directory, its
// transcripts are scanned under that account, not the ambient identity (Codex P2
// on #6299): the tap files the plan reading under the same account.
func TestScanSourcesFilesAnAmbientDirUnderItsRegisteredAccount(t *testing.T) {
	home := "/af"
	work := "/af/accounts/claude/work"
	got := scanSources(home, map[string]string{"work": work}, work, nil)
	if len(got["work"]) != 1 || got["work"][0] != work {
		t.Fatalf("work sources %v: the account's own directory, once", got["work"])
	}
	if len(got[planusage.DefaultAccount]) != 0 {
		t.Fatalf("the ambient identity also scans %v: its costs would count there first and the account would be left with none", got[planusage.DefaultAccount])
	}
	got = scanSources(home, map[string]string{"work": work}, "/home/me/.claude", map[string][]string{"work": {"/custom"}})
	if len(got[planusage.DefaultAccount]) != 1 || len(got["work"]) != 2 {
		t.Fatalf("a plain ambient dir stays ambient, a run's effective dir joins its account: %v", got)
	}
}

func TestTranscriptConfigRoot(t *testing.T) {
	for file, want := range map[string]string{
		"/h/.claude/projects/-w-a/s.jsonl":                         "/h/.claude",
		"/af/accounts/claude/work/projects/-w/s/subagents/x.jsonl": "/af/accounts/claude/work",
		"/home/u/projects/x/.claude/projects/-w/s.jsonl":           "/home/u/projects/x/.claude",
		"/no/marker/s.jsonl":                                       "",
	} {
		if got := transcriptConfigRoot(file); got != want {
			t.Errorf("transcriptConfigRoot(%q) = %q, want %q", file, got, want)
		}
	}
}
