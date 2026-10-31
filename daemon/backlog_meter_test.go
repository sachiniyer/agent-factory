package daemon

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/agentaccount"
	"github.com/sachiniyer/agent-factory/planusage"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"

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

// A start that finds a metering pass running leaves its run directories pending
// rather than dropping them, and the pass that follows saves them: the session
// can be killed before then, and its spend would stay unmetered (Codex P2 on #6299).
func TestRunDirsRecordedDuringAPassAreNotDropped(t *testing.T) {
	cs, _ := newBacklogTestServer(t)
	m := cs.manager
	m.cfg = &config.Config{Backlog: config.BacklogConfig{PlanUsage: true}}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	st := &m.backlog.meter

	st.mu.Lock() // a pass is running
	m.rememberRunDirs("s-1", []string{"/x/projects/-w-1"})
	st.mu.Unlock()
	state, err := planusage.LoadMeter()
	require.NoError(t, err)
	require.Empty(t, state.RunDirs["s-1"], "nothing is saved while the pass holds the meter")

	st.mu.Lock()
	m.meterPassLocked(time.Now())
	st.mu.Unlock()
	state, err = planusage.LoadMeter()
	require.NoError(t, err)
	require.Equal(t, []string{"/x/projects/-w-1"}, state.RunDirs["s-1"], "the next pass saves the pending record")
	require.Empty(t, st.pendingDirs, "a saved record is no longer pending")

	m.rememberRunDirs("s-2", []string{"/x/projects/-w-2"})
	state, err = planusage.LoadMeter()
	require.NoError(t, err)
	require.Equal(t, []string{"/x/projects/-w-2"}, state.RunDirs["s-2"], "with no pass running it saves at once")
}

// meterUsage returns the last metering result a pass published. The meter's
// usage is written under its own lock; this helper reads it safely.
func meterUsage(t *testing.T, st *meterState) planusage.Usage {
	t.Helper()
	st.usageMu.Lock()
	defer st.usageMu.Unlock()
	return st.usage
}

// meterRepoWithItem writes a single running item to a fresh project's backlog
// and returns the item used, so each metering test starts from the same shape.
func meterRepoWithItem(t *testing.T, repoID string, item backlog.Item) {
	t.Helper()
	if _, err := backlog.Update(repoID, func(b *backlog.Backlog) error {
		b.Items = []backlog.Item{item}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// A historical (non-latest) run of a running item keeps RunStarted forever:
// backlog.RunEnded advances only the item's status, not older RunRefs, so a
// restarted item accumulates RunStarted runs. An archived non-Claude session
// also stays registered in manager.instances (no path removes it). The meter
// must flag only the item's CURRENT run as an active-run metering failure: a
// historical run's non-Claude session must not poison usage.Unmeterable with
// advice aimed at an item whose current run meters fine (Codex P2 on #6300).
func TestMeterPassHistoricalNonClaudeRunNotUnmeterable(t *testing.T) {
	cs, _ := newBacklogTestServer(t)
	m := cs.manager
	m.cfg = &config.Config{Backlog: config.BacklogConfig{PlanUsage: true}}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Now().UTC()
	meterRepoWithItem(t, "meter-repo", backlog.Item{
		ID: 1, Title: "Item one", Status: backlog.StatusRunning,
		Runs: []backlog.RunRef{
			{SessionID: "sess-old", State: backlog.RunStarted, StartedAt: now.Add(-time.Hour)},
			{SessionID: "sess-new", State: backlog.RunStarted, StartedAt: now},
		},
	})
	// Both sessions are still registered: the historical one is codex, the
	// current (latest) one is claude and meters clean.
	m.mu.Lock()
	m.instances = map[string]*session.Instance{
		"sess-old": {ID: "sess-old", BacklogItem: 1, Program: "codex"},
		"sess-new": {ID: "sess-new", BacklogItem: 1, Program: "claude"},
	}
	m.mu.Unlock()

	st := &m.backlog.meter
	st.mu.Lock()
	m.meterPassLocked(now)
	st.mu.Unlock()

	got := meterUsage(t, st)
	require.NotContains(t, got.Unmeterable, "sess-old",
		"a historical run's non-Claude session must not be unmeterable when the current run meters fine")
	require.NotContains(t, got.Unmeterable, "sess-new", "the current Claude run meters clean")
	require.Empty(t, got.Unmeterable, "no run of this running item is unmeterable")
	// Historical runs are still metered for their cost: both runs appear.
	require.Contains(t, got.Runs, "sess-old")
	require.Contains(t, got.Runs, "sess-new")
}

// The fix must not over-narrow: a running item whose CURRENT (latest) run is a
// non-Claude session is still an active-run metering failure and stays
// unmeterable. (Positive control for the first Unmeterable producer.)
func TestMeterPassCurrentNonClaudeRunStillUnmeterable(t *testing.T) {
	cs, _ := newBacklogTestServer(t)
	m := cs.manager
	m.cfg = &config.Config{Backlog: config.BacklogConfig{PlanUsage: true}}
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Now().UTC()
	meterRepoWithItem(t, "meter-repo", backlog.Item{
		ID: 1, Title: "Item one", Status: backlog.StatusRunning,
		Runs: []backlog.RunRef{{SessionID: "sess", State: backlog.RunStarted, StartedAt: now}},
	})
	m.mu.Lock()
	m.instances = map[string]*session.Instance{
		"sess": {ID: "sess", BacklogItem: 1, Program: "codex"},
	}
	m.mu.Unlock()

	st := &m.backlog.meter
	st.mu.Lock()
	m.meterPassLocked(now)
	st.mu.Unlock()

	got := meterUsage(t, st)
	require.Equal(t, backlog.UnmeterableProgram(1, "codex"), got.Unmeterable["sess"],
		"the current run of a non-Claude session is still an active-run metering failure")
}

// A metered run whose current config dir cannot be resolved — a session killed
// between passes (no live instance), after a daemon restart, or handed to another
// agent and metered from remembered transcripts — is still in usage.Runs, so it
// must also appear in usage.RunWindow (the documented contract: "0 without a
// valid window", never a missing key). A started run with no transcripts at all
// likewise gets a 0 entry, so the runs and run_window key sets never diverge
// (Codex P2 on #6299: the runs/run_window key-set divergence was the bug).
func TestMeterPassRunWindowCoversRunsMeteredFromRememberedDirs(t *testing.T) {
	cs, _ := newBacklogTestServer(t)
	cs.manager.cfg = &config.Config{Backlog: config.BacklogConfig{PlanUsage: true}}
	claudeDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	now := time.Now().UTC()
	// One assistant message of 1M input tokens at the opus price = $4 of usage.
	line := `{"type":"assistant","timestamp":"` + now.Add(-time.Minute).Format(time.RFC3339) +
		`","message":{"id":"m1","model":"claude-opus-5-5","usage":{"input_tokens":1000000,"output_tokens":0}}}` + "\n"
	projectDir := filepath.Join(claudeDir, "projects", "-home-me-x")
	transcript := filepath.Join(projectDir, "s-killed.jsonl")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	require.NoError(t, os.WriteFile(transcript, []byte(line), 0o644))
	reset := now.Add(2 * time.Hour).Truncate(time.Hour)
	require.NoError(t, planusage.WriteReading(planusage.DefaultAccount, planusage.Reading{
		FiveHour: &planusage.Limit{UsedPercentage: 30, ResetsAt: reset.Unix()}, ReadAt: now,
	}))
	// A started run whose session is gone (killed) but whose transcript dirs were
	// remembered, and a second started run with no transcripts at all: both stay in
	// usage.Runs; the metering pass reaches MeterRun only for the remembered one.
	const repoID = "meter-test-repo"
	_, err := backlog.Update(repoID, func(b *backlog.Backlog) error {
		b.Items = append(b.Items, backlog.Item{ID: 1, Title: "t", Status: backlog.StatusRunning})
		b.Item(1).Runs = []backlog.RunRef{
			{State: backlog.RunStarted, SessionID: "s-killed", StartedAt: now.Add(-2 * time.Minute)},
			{State: backlog.RunStarted, SessionID: "s-nodirs", StartedAt: now.Add(-time.Minute)},
		}
		return nil
	})
	require.NoError(t, err)
	// No instance for either session: simulate killed sessions. Remember the
	// transcript's project dir for the killed run so the pass meters it.
	cs.manager.rememberRunDirs("s-killed", []string{projectDir})

	id, ch, _ := cs.manager.events.subscribeWithOverflow()
	defer cs.manager.events.unsubscribe(id)
	st := &cs.manager.backlog.meter
	st.mu.Lock()
	cs.manager.meterPassLocked(now)
	st.mu.Unlock()

	// The run_window entry exists and equals the metered cost; the run with no
	// transcripts gets a 0 entry, so runs and run_window have the same keys.
	st.usageMu.Lock()
	usage := st.usage
	st.usageMu.Unlock()
	require.InDelta(t, 4.0, usage.Runs["s-killed"], 1e-9, "the killed run is metered from its remembered transcripts")
	require.Contains(t, usage.RunWindow, "s-killed", "a metered killed session must have a run_window entry, not a missing key")
	require.InDelta(t, 4.0, usage.RunWindow["s-killed"], 1e-9, "the killed run's window cost is its metered cost")
	require.Contains(t, usage.RunWindow, "s-nodirs", "a started run with no transcripts still has a run_window entry (0)")
	require.InDelta(t, 0.0, usage.RunWindow["s-nodirs"], 1e-9)
	require.Len(t, usage.RunWindow, len(usage.Runs), "run_window must have one entry per run")
	for k := range usage.Runs {
		require.Contains(t, usage.RunWindow, k, "run %q missing from run_window", k)
	}

	// The same shape reaches a JSON consumer of /v1/GetBudget: run_window[s-killed]
	// is a number, not a missing key.
	code, env := postProposalRoute(t, cs, "GetBudget", GetBudgetRequest{})
	require.Equal(t, http.StatusOK, code, env)
	data := env["data"].(map[string]any)
	runs := data["runs"].(map[string]any)
	runWindow := data["run_window"].(map[string]any)
	require.Contains(t, runWindow, "s-killed", "JSON run_window must include the metered killed session")
	require.Equal(t, runs["s-killed"], runWindow["s-killed"])
	require.Contains(t, runWindow, "s-nodirs")
	for k := range runs {
		require.Contains(t, runWindow, k, "JSON runs key %q missing from run_window", k)
	}

	// The backlog.spend event carries the same runs/run_window key set.
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type != agentproto.EventBacklogSpend {
				continue
			}
			var payload map[string]any
			require.NoError(t, json.Unmarshal(ev.Data, &payload))
			er := payload["runs"].(map[string]any)
			ew := payload["run_window"].(map[string]any)
			require.Contains(t, ew, "s-killed", "spend event run_window must include the metered killed session")
			require.Equal(t, er["s-killed"], ew["s-killed"])
			require.Contains(t, ew, "s-nodirs")
			for k := range er {
				require.Contains(t, ew, k, "spend event runs key %q missing from run_window", k)
			}
			return
		case <-deadline:
			t.Fatal("no backlog.spend event")
		}
	}
}

// A run metered this pass under the same account it always ran under keeps its
// run_window from the current config dir (no regression for the live-session
// path): the window cost comes out of the buckets AddRunEntries filed, summed
// across the accounts state.RunWindowAccounts reports.
func TestMeterPassRunWindowCoversLiveRunFromCurrentConfigDir(t *testing.T) {
	cs, _ := newBacklogTestServer(t)
	cs.manager.cfg = &config.Config{Backlog: config.BacklogConfig{PlanUsage: true}}
	claudeDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	now := time.Now().UTC()
	line := `{"type":"assistant","timestamp":"` + now.Add(-time.Minute).Format(time.RFC3339) +
		`","message":{"id":"m1","model":"claude-opus-5-5","usage":{"input_tokens":1000000,"output_tokens":0}}}` + "\n"
	projectDir := filepath.Join(claudeDir, "projects", "-home-me-x")
	transcript := filepath.Join(projectDir, "s-live.jsonl")
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	require.NoError(t, os.WriteFile(transcript, []byte(line), 0o644))
	reset := now.Add(2 * time.Hour).Truncate(time.Hour)
	require.NoError(t, planusage.WriteReading(planusage.DefaultAccount, planusage.Reading{
		FiveHour: &planusage.Limit{UsedPercentage: 30, ResetsAt: reset.Unix()}, ReadAt: now,
	}))
	const repoID = "meter-live-repo"
	_, err := backlog.Update(repoID, func(b *backlog.Backlog) error {
		b.Items = append(b.Items, backlog.Item{ID: 1, Title: "t", Status: backlog.StatusRunning})
		b.Item(1).Runs = []backlog.RunRef{{State: backlog.RunStarted, SessionID: "s-live", StartedAt: now.Add(-2 * time.Minute)}}
		return nil
	})
	require.NoError(t, err)
	// No instance here either: the transcript dirs are remembered, so the pass
	// meters from them. With a live instance the config dir would resolve to the
	// same account, so the window cost this computes is identical.
	cs.manager.rememberRunDirs("s-live", []string{projectDir})

	st := &cs.manager.backlog.meter
	st.mu.Lock()
	cs.manager.meterPassLocked(now)
	st.mu.Unlock()

	st.usageMu.Lock()
	usage := st.usage
	st.usageMu.Unlock()
	require.InDelta(t, 4.0, usage.Runs["s-live"], 1e-9)
	require.InDelta(t, 4.0, usage.RunWindow["s-live"], 1e-9, "the live run's window cost is its metered cost")
	require.Len(t, usage.RunWindow, len(usage.Runs), "run_window must have one entry per run")
	for k := range usage.Runs {
		require.Contains(t, usage.RunWindow, k, "run %q missing from run_window", k)
	}
}

// A live run whose session switched accounts still holds pre-swap buckets under
// the old account, but its resolved current config dir scopes run_window to the
// current account — the documented "cost within its account's current 5-hour
// window". Summing every bucket would report $7 for a run that spent $5 before
// the swap and $2 after, and the per-run percentage compares run_window against
// the CURRENT account's all_cost (Codex P2 on #6328). The all-bucket sum stays
// only as the fallback for a run whose current config dir cannot be resolved.
func TestMeterPassRunWindowScopesSwappedRunToCurrentAccount(t *testing.T) {
	cs, repoPath := newBacklogTestServer(t)
	cs.manager.cfg = &config.Config{Backlog: config.BacklogConfig{PlanUsage: true}}
	home, err := config.GetConfigDir()
	require.NoError(t, err)
	claudeDir := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", claudeDir)
	now := time.Now().UTC()
	// The account the session swapped TO.
	accDir, err := agentaccount.Register(home, tmux.ProgramClaude, "work")
	require.NoError(t, err)
	line := func(id string, input int) string {
		return `{"type":"assistant","timestamp":"` + now.Add(-time.Minute).Format(time.RFC3339) +
			`","message":{"id":"` + id + `","model":"claude-opus-5-5","usage":{"input_tokens":` +
			strconv.Itoa(input) + `,"output_tokens":0}}}` + "\n"
	}
	// $5 of pre-swap history filed under the default account's config root, $2
	// under the swapped-to account's. Both dirs are remembered, so the pass
	// meters both into session|account buckets regardless of the live dir.
	defaultProj := filepath.Join(claudeDir, "projects", "-home-me-x")
	swappedProj := filepath.Join(accDir, "projects", "-home-me-x")
	require.NoError(t, os.MkdirAll(defaultProj, 0o755))
	require.NoError(t, os.MkdirAll(swappedProj, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(defaultProj, "s-old.jsonl"), []byte(line("m-old", 1250000)), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(swappedProj, "s-new.jsonl"), []byte(line("m-new", 500000)), 0o644))
	reset := now.Add(2 * time.Hour).Truncate(time.Hour)
	for _, account := range []string{planusage.DefaultAccount, "work"} {
		require.NoError(t, planusage.WriteReading(account, planusage.Reading{
			FiveHour: &planusage.Limit{UsedPercentage: 30, ResetsAt: reset.Unix()}, ReadAt: now,
		}))
	}
	const repoID = "meter-swap-repo"
	const title = "swapped"
	_, err = backlog.Update(repoID, func(b *backlog.Backlog) error {
		b.Items = append(b.Items, backlog.Item{ID: 1, Title: "t", Status: backlog.StatusRunning})
		b.Item(1).Runs = []backlog.RunRef{{State: backlog.RunStarted, SessionID: "s-swapped", StartedAt: now.Add(-3 * time.Minute)}}
		return nil
	})
	require.NoError(t, err)
	// A live instance on the swapped-to account: its resolved config dir scopes
	// the run's window to that account.
	inst, err := session.NewInstance(session.InstanceOptions{ID: "s-swapped", Title: title, Path: repoPath, Program: "claude"})
	require.NoError(t, err)
	inst.SetBackend(session.NewFakeBackend())
	gw, _, err := sessiongit.NewGitWorktreeInPlace(repoPath)
	require.NoError(t, err)
	inst.SetGitWorktreeForTest(gw)
	inst.BacklogItem = 1
	inst.Account = "work"
	cs.manager.instances = make(map[string]*session.Instance)
	cs.manager.instances[daemonInstanceKey(repoID, title)] = inst
	cs.manager.rememberRunDirs("s-swapped", []string{defaultProj, swappedProj})

	st := &cs.manager.backlog.meter
	st.mu.Lock()
	cs.manager.meterPassLocked(now)
	st.mu.Unlock()

	st.usageMu.Lock()
	usage := st.usage
	st.usageMu.Unlock()
	require.InDelta(t, 7.0, usage.Runs["s-swapped"], 1e-9, "the run's total cost spans both accounts' transcripts")
	require.InDelta(t, 2.0, usage.RunWindow["s-swapped"], 1e-9,
		"a live run's window is scoped to its current account, not the sum of every bucket")
}
