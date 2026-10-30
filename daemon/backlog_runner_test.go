package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/planusage"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

func ptrTo[T any](v T) *T { return &v }

// runnerTestManager is backlogTestManager with the project registered, as the
// runner needs it to find the project's root and account.
func runnerTestManager(t *testing.T) (*Manager, string) {
	t.Helper()
	m, repoID, repoPath := backlogTestManager(t)
	// The runner gates on the plan budget, which needs [backlog] plan_usage.
	m.Config().Backlog.PlanUsage = true
	if _, err := config.RegisterProject(repoPath); err != nil {
		t.Fatal(err)
	}
	if root, err := registeredProjectRootForRepoID(repoID); err != nil || root == "" {
		t.Fatalf("project %s not found after registering: %q %v", repoID, root, err)
	}
	return m, repoID
}

func addItem(t *testing.T, m *Manager, repoID, title string) {
	t.Helper()
	if _, err := m.backlogService().Add(backlog.AddRequest{RepoID: repoID,
		Items: []backlog.ItemInput{{Key: "k", Title: title}}}); err != nil {
		t.Fatal(err)
	}
}

func queueItem(t *testing.T, m *Manager, repoID string, id int) {
	t.Helper()
	if _, err := m.backlogService().SetItemStatus(backlog.SetItemStatusRequest{RepoID: repoID, ID: id, Status: backlog.StatusQueued}); err != nil {
		t.Fatal(err)
	}
}

func setRunner(t *testing.T, m *Manager, repoID string, on bool, maxRunning int) {
	t.Helper()
	if _, err := m.backlogService().SetSettings(backlog.SetSettingsRequest{RepoID: repoID, Runner: ptrTo(on), MaxRunning: ptrTo(maxRunning)}); err != nil {
		t.Fatal(err)
	}
}

func writeFreshReading(t *testing.T, plan float64) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	now := time.Now().UTC()
	if err := planusage.WriteReading(planusage.DefaultAccount, planusage.Reading{
		FiveHour: &planusage.Limit{UsedPercentage: plan, ResetsAt: now.Add(2 * time.Hour).Unix()}, ReadAt: now,
	}); err != nil {
		t.Fatal(err)
	}
}

func runnerEvents(t *testing.T, m *Manager) func() []backlog.RunnerEvent {
	t.Helper()
	id, ch, _ := m.events.subscribeWithOverflow()
	t.Cleanup(func() { m.events.unsubscribe(id) })
	return func() []backlog.RunnerEvent {
		var out []backlog.RunnerEvent
		for {
			select {
			case ev := <-ch:
				if ev.Type != agentproto.EventBacklogRunner {
					continue
				}
				var p backlog.RunnerEvent
				if err := json.Unmarshal(ev.Data, &p); err != nil {
					t.Fatal(err)
				}
				out = append(out, p)
			default:
				return out
			}
		}
	}
}

func pass(m *Manager, repoID string, at time.Time) {
	m.runBacklogProject(repoID, at)
	m.backgroundMutationWG.Wait()
}

func failCreates(t *testing.T) *int {
	t.Helper()
	var calls int
	var mu sync.Mutex
	prev := createBacklogSession
	createBacklogSession = func(*Manager, context.Context, CreateSessionRequest) (session.InstanceData, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return session.InstanceData{}, errors.New("no worktree")
	}
	t.Cleanup(func() { createBacklogSession = prev })
	return &calls
}

func TestRunnerOffStartsNothing(t *testing.T) {
	m, repoID := runnerTestManager(t)
	writeFreshReading(t, 10)
	queueItem(t, m, repoID, 1)
	events := runnerEvents(t, m)
	pass(m, repoID, time.Now())
	if it := loadItem(t, repoID, 1); it.Status != backlog.StatusQueued || len(it.Runs) != 0 {
		t.Fatalf("item %+v", it)
	}
	if got := events(); len(got) != 0 {
		t.Fatalf("an off runner that stays off publishes nothing: %+v", got)
	}
}

func TestRunnerStartsTopQueuedItem(t *testing.T) {
	m, repoID := runnerTestManager(t)
	writeFreshReading(t, 10)
	queueItem(t, m, repoID, 1)
	setRunner(t, m, repoID, true, 1)
	events := runnerEvents(t, m)
	pass(m, repoID, time.Now())
	it := loadItem(t, repoID, 1)
	if it.Status != backlog.StatusRunning || len(it.Runs) != 1 || it.Runs[0].StartedBy != backlog.StartedByRunner {
		t.Fatalf("item %+v", it)
	}
	got := events()
	if len(got) != 1 || got[0].State != backlog.RunnerRunning || got[0].RepoID != repoID {
		t.Fatalf("events %+v", got)
	}
	pass(m, repoID, time.Now())
	if again := events(); len(again) != 0 {
		t.Fatalf("no event without a change: %+v", again)
	}
}

func TestRunnerWaitsForAReading(t *testing.T) {
	m, repoID := runnerTestManager(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	queueItem(t, m, repoID, 1)
	setRunner(t, m, repoID, true, 1)
	events := runnerEvents(t, m)
	pass(m, repoID, time.Now())
	if it := loadItem(t, repoID, 1); it.Status != backlog.StatusQueued {
		t.Fatalf("item %+v", it)
	}
	got := events()
	if len(got) != 1 || got[0].State != backlog.RunnerWaiting || got[0].Gate != backlog.GateNoReading {
		t.Fatalf("events %+v", got)
	}
	v, err := m.backlogService().Get(backlog.GetRequest{RepoID: repoID})
	if err != nil {
		t.Fatal(err)
	}
	if v.Runner.State != backlog.RunnerWaiting || !strings.HasPrefix(v.Runner.Reason, "No plan usage seen yet") {
		t.Fatalf("runner %+v", v.Runner)
	}
}

func TestGetBacklogShowsTheReadingAfterTheFirstPass(t *testing.T) {
	m, repoID := runnerTestManager(t)
	writeFreshReading(t, 95)
	setRunner(t, m, repoID, true, 1)
	v, err := m.backlogService().Get(backlog.GetRequest{RepoID: repoID})
	if err != nil {
		t.Fatal(err)
	}
	if v.Runner.State != backlog.RunnerWaiting || v.Runner.Gate != backlog.GateMetering {
		t.Fatalf("before the first pass: %+v", v.Runner)
	}
	m.backgroundMutationWG.Wait()
	if v, err = m.backlogService().Get(backlog.GetRequest{RepoID: repoID}); err != nil {
		t.Fatal(err)
	}
	if v.Runner.Gate != backlog.GateStopAt {
		t.Fatalf("the read started a pass; the next read shows the reading: %+v", v.Runner)
	}
}

func TestRunBacklogIgnoresSwitchAndGates(t *testing.T) {
	m, repoID := runnerTestManager(t)
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	queueItem(t, m, repoID, 1)
	out, err := m.RunBacklog(backlog.RunRequest{RepoID: repoID})
	if err != nil {
		t.Fatal(err)
	}
	m.backgroundMutationWG.Wait()
	if len(out.Started) != 1 || out.Started[0] != 1 {
		t.Fatalf("%+v", out)
	}
	if it := loadItem(t, repoID, 1); it.Status != backlog.StatusRunning || it.Runs[0].StartedBy != backlog.StartedByUser {
		t.Fatalf("item %+v", it)
	}
	out, err = m.RunBacklog(backlog.RunRequest{RepoID: repoID, Count: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Started) != 0 || !strings.HasPrefix(out.Reason, "all 1 slots are in use") {
		t.Fatalf("%+v", out)
	}
}

func TestConcurrentStartsKeepMaxRunning(t *testing.T) {
	m, repoID := runnerTestManager(t)
	writeFreshReading(t, 10)
	for _, title := range []string{"b", "c", "d"} {
		addItem(t, m, repoID, title)
	}
	for id := 1; id <= 4; id++ {
		queueItem(t, m, repoID, id)
	}
	setRunner(t, m, repoID, true, 2)
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); _, _ = m.RunBacklog(backlog.RunRequest{RepoID: repoID, Count: 2}) }()
		go func() { defer wg.Done(); m.runBacklogProject(repoID, time.Now()) }()
	}
	wg.Wait()
	m.backgroundMutationWG.Wait()
	running := 0
	for id := 1; id <= 4; id++ {
		if loadItem(t, repoID, id).Status == backlog.StatusRunning {
			running++
		}
	}
	if running != 2 {
		t.Fatalf("%d items running, max 2", running)
	}
}

func TestRunnerHoldsAnItemWhoseCreateFailed(t *testing.T) {
	m, repoID := runnerTestManager(t)
	writeFreshReading(t, 10)
	addItem(t, m, repoID, "second")
	queueItem(t, m, repoID, 1)
	queueItem(t, m, repoID, 2)
	setRunner(t, m, repoID, true, 1)
	calls := failCreates(t)
	now := time.Now()
	for i := 0; i < 4; i++ {
		pass(m, repoID, now.Add(time.Duration(i)*runnerInterval))
	}
	if it := loadItem(t, repoID, 1); it.Status != backlog.StatusQueued || len(it.Runs) != 1 {
		t.Fatalf("item 1 retried: %+v", it.Runs)
	}
	if it := loadItem(t, repoID, 2); len(it.Runs) != 1 {
		t.Fatalf("item 2 should get its turn once: %+v", it.Runs)
	}
	if *calls != 2 {
		t.Fatalf("%d creates, want 2", *calls)
	}
}

func TestRunnerBacksOffARefusedStart(t *testing.T) {
	m, repoID := runnerTestManager(t)
	writeFreshReading(t, 10)
	addItem(t, m, repoID, "second")
	if _, err := m.backlogService().UpdateItem(backlog.UpdateItemRequest{RepoID: repoID, ID: 1, Program: ptrTo("no-such-program")}); err != nil {
		t.Fatal(err)
	}
	queueItem(t, m, repoID, 1)
	queueItem(t, m, repoID, 2)
	setRunner(t, m, repoID, true, 1)
	now := time.Now()
	pass(m, repoID, now)
	if it := loadItem(t, repoID, 1); len(it.Runs) != 0 || it.Status != backlog.StatusQueued {
		t.Fatalf("a refused start changes nothing: %+v", it)
	}
	pass(m, repoID, now.Add(runnerInterval))
	if it := loadItem(t, repoID, 2); it.Status != backlog.StatusRunning {
		t.Fatalf("the runner should back off from B-1 and start B-2: %+v", it)
	}
}

func capUsage(t *testing.T, m *Manager, sessionID string, cost float64) {
	t.Helper()
	// The cap needs the plan budget, which needs plan usage on.
	m.Config().Backlog.PlanUsage = true
	reset := time.Now().Add(time.Hour)
	m.backlog.meter.usageMu.Lock()
	w := planusage.Windows{FiveHour: planusage.PlanWindow{Plan: 40, ResetsAt: &reset, All: 10}}
	m.backlog.meter.usage = planusage.Usage{Runs: map[string]float64{sessionID: cost}, Windows: w,
		Accounts: map[string]planusage.AccountUsage{planusage.DefaultAccount: {Windows: w}}}
	m.backlog.meter.finished = time.Now()
	m.backlog.meter.usageMu.Unlock()
}

func stubInterrupt(t *testing.T) *[]string {
	t.Helper()
	var got []string
	prev := interruptBacklogSession
	interruptBacklogSession = func(i *session.Instance) error {
		got = append(got, i.ID)
		return nil
	}
	t.Cleanup(func() { interruptBacklogSession = prev })
	return &got
}

func TestRunnerInterruptsRunOverCap(t *testing.T) {
	m, repoID, inst := startedItem(t)
	interrupted := stubInterrupt(t)
	capUsage(t, m, inst.ID, 3)

	now := time.Now()
	pass(m, repoID, now)
	if len(*interrupted) != 1 || (*interrupted)[0] != inst.ID {
		t.Fatalf("interrupted %v", *interrupted)
	}
	it := loadItem(t, repoID, 1)
	q := it.OpenQuestion()
	if q == nil || q.Author != backlog.AuthorAF || !strings.HasPrefix(q.Text, "This run used 12% of your 5-hour window, over the 10% cap per run.") {
		t.Fatalf("thread %+v", it.Thread)
	}
	if it.Runs[0].CapBase != 3 {
		t.Fatalf("run %+v", it.Runs[0])
	}
	for i := 1; i <= capInterruptRetryPasses+2; i++ {
		pass(m, repoID, now.Add(time.Duration(i)*runnerInterval))
	}
	if len(*interrupted) != 2 {
		t.Fatalf("a run still active gets one more Escape, not more: %v", *interrupted)
	}
	if n := len(loadItem(t, repoID, 1).Thread); n != 1 {
		t.Fatalf("asked again: %d messages", n)
	}
}

func TestRunnerCapAsksOnlyWhenAttached(t *testing.T) {
	m, repoID, inst := startedItem(t)
	interrupted := stubInterrupt(t)
	capUsage(t, m, inst.ID, 3)
	m.PauseStatusPoll(repoID, inst.Title, inst.ID)
	pass(m, repoID, time.Now())
	if len(*interrupted) != 0 {
		t.Fatalf("Escape sent to an attached session: %v", *interrupted)
	}
	if it := loadItem(t, repoID, 1); it.OpenQuestion() == nil {
		t.Fatal("no question")
	}
}

func TestRunnerFailsClosedOnAnUnreadableLedger(t *testing.T) {
	m, repoID := runnerTestManager(t)
	writeFreshReading(t, 10)
	path, err := accountLimitLedgerPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	queueItem(t, m, repoID, 1)
	setRunner(t, m, repoID, true, 1)
	pass(m, repoID, time.Now())
	if it := loadItem(t, repoID, 1); it.Status != backlog.StatusQueued {
		t.Fatalf("started with an unreadable ledger: %+v", it)
	}
	v, err := m.backlogService().Get(backlog.GetRequest{RepoID: repoID})
	if err != nil {
		t.Fatal(err)
	}
	if v.Runner.Gate != backlog.GateUsageLimit || !strings.Contains(v.Runner.Reason, "fix or remove it") {
		t.Fatalf("runner %+v", v.Runner)
	}
}

func TestRunnerStateChangedOnReason(t *testing.T) {
	var s runnerState
	ev := backlog.RunnerEvent{RepoID: "r", State: backlog.RunnerStopped, Gate: backlog.GateStopAt, Reason: "Plan at 91%"}
	if !s.changed(ev) {
		t.Fatal("first stop")
	}
	if s.changed(ev) {
		t.Fatal("same event")
	}
	ev.Reason = "Plan at 92%"
	if !s.changed(ev) {
		t.Fatal("reason changed")
	}
	if s.changed(backlog.RunnerEvent{RepoID: "x", State: backlog.RunnerOff, Reason: "Items run only when you start them."}) {
		t.Fatal("a project that stays off")
	}
}

func TestActiveLimit(t *testing.T) {
	now := time.Now()
	later := now.Add(time.Hour)
	obs := []session.AccountLimitObservationData{
		{Agent: tmux.ProgramClaude, Account: "", ResetAt: later},
		{Agent: tmux.ProgramClaude, Account: "work", ResetAt: now.Add(-time.Hour)},
		{Agent: "codex", Account: "work", ResetAt: later},
	}
	if ok, until := activeLimit(obs, tmux.ProgramClaude, planAccount(""), now); !ok || !until.Equal(later) {
		t.Fatal(ok, until)
	}
	if ok, _ := activeLimit(obs, tmux.ProgramClaude, "work", now); ok {
		t.Fatal("a reset limit and another agent's limit do not count")
	}
	unknown := []session.AccountLimitObservationData{{Agent: tmux.ProgramClaude, Account: "work"}}
	if ok, until := activeLimit(unknown, tmux.ProgramClaude, "work", now); !ok || !until.IsZero() {
		t.Fatal(ok, until)
	}
}

func TestUsageForAccount(t *testing.T) {
	at := time.Now()
	work := planusage.Windows{FiveHour: planusage.PlanWindow{Plan: 40, ResetsAt: &at}}
	u := planusage.Usage{Account: "work", ReadingAt: &at, Windows: work,
		Accounts: map[string]planusage.AccountUsage{"work": {Windows: work, ReadingAt: &at}, planusage.DefaultAccount: {}}}
	if got := usageFor(u, "work"); got.ReadingAt == nil || got.Windows.FiveHour.Plan != 40 {
		t.Fatalf("%+v", got)
	}
	if got := usageFor(u, planusage.DefaultAccount); got.ReadingAt != nil || got.Windows.FiveHour.Valid() || got.Account != planusage.DefaultAccount {
		t.Fatalf("another account's reading is not this one's: %+v", got)
	}
	if got := usageFor(u, "unknown"); got.ReadingAt != nil {
		t.Fatalf("an account af does not meter has no reading: %+v", got)
	}
}

// With plan usage off (the default) the runner starts nothing, whatever is
// queued and however fresh a reading on disk is, and says why.
func TestRunnerStaysClosedWhenPlanUsageIsOff(t *testing.T) {
	m, repoID := runnerTestManager(t)
	m.Config().Backlog.PlanUsage = false
	writeFreshReading(t, 10)
	queueItem(t, m, repoID, 1)
	setRunner(t, m, repoID, true, 1)
	pass(m, repoID, time.Now())
	if it := loadItem(t, repoID, 1); it.Status != backlog.StatusQueued || len(it.Runs) != 0 {
		t.Fatalf("the runner started an item with plan usage off: %+v", it)
	}
	v, err := m.backlogService().Get(backlog.GetRequest{RepoID: repoID})
	if err != nil {
		t.Fatal(err)
	}
	if v.Runner.Gate != backlog.GatePlanUsageOff || !strings.Contains(v.Runner.Reason, "backlog.plan_usage") {
		t.Fatalf("runner view %+v", v.Runner)
	}
}

// A pass that chose an item before the user turned the runner off must not start
// it after the start lock frees (Codex P2 on #6300); an explicit user start still
// ignores the switch.
func TestAutomaticStartHonoursTheRunnerSwitchAtAdmission(t *testing.T) {
	m, repoID := runnerTestManager(t)
	queueItem(t, m, repoID, 1)
	setRunner(t, m, repoID, false, 1)
	if _, err := m.startBacklogItemCapped(backlog.StartRequest{RepoID: repoID, ID: 1}, backlog.StartedByRunner); !errors.Is(err, errRunnerOff) {
		t.Fatalf("an automatic start with the runner off: %v", err)
	}
	if it := loadItem(t, repoID, 1); len(it.Runs) != 0 {
		t.Fatalf("a session was started: %+v", it)
	}
	if _, err := m.startBacklogItemCapped(backlog.StartRequest{RepoID: repoID, ID: 1}, backlog.StartedByUser); err != nil {
		t.Fatalf("an explicit start must ignore the switch: %v", err)
	}
}

// A retry of the cap interrupt belongs to the run that exceeded the cap: a
// continuation the user authorized re-arms the session under a new generation.
func TestCapInterruptRetryStopsAfterTheRunIsRearmed(t *testing.T) {
	m, repoID, inst := startedItem(t)
	m.Config().Backlog.PlanUsage = true
	interrupted := stubInterrupt(t)
	rs := &m.backlog.runner
	rs.mu.Lock()
	rs.interrupts = map[string]*capInterrupt{inst.ID: {repoID: repoID, gen: inst.BacklogRunGeneration() - 1, passes: capInterruptRetryPasses}}
	rs.mu.Unlock()
	m.retryCapInterrupts(repoID)
	if len(*interrupted) != 0 {
		t.Fatalf("an old run's cap interrupt was retried on the new run: %v", *interrupted)
	}
	rs.mu.Lock()
	_, kept := rs.interrupts[inst.ID]
	rs.mu.Unlock()
	if kept {
		t.Fatal("the stale interrupt entry was kept")
	}
}

// A registered account named "default" is its own identity: the ambient identity
// has a key no account name can equal (Codex P2 on #6300). The account comes from
// the global config on DISK, exactly as a session create resolves it
// (config.DefaultAccountLayersFor treats the repo resolution as authoritative), so
// the fixture writes config.toml rather than mutating the in-memory config.
func TestItemAccountKeepsARegisteredDefaultSeparateFromTheAmbientIdentity(t *testing.T) {
	m, repoID := runnerTestManager(t)
	home, err := config.GetConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(home, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("[default_accounts]\nclaude = \"default\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if acct, err := m.itemAccount(repoID); err != nil || acct != "default" || acct == planusage.DefaultAccount {
		t.Fatalf("a registered account named default: %q %v", acct, err)
	}
	if err := os.Remove(cfgPath); err != nil {
		t.Fatal(err)
	}
	if acct, err := m.itemAccount(repoID); err != nil || acct != planusage.DefaultAccount {
		t.Fatalf("the ambient identity: %q %v", acct, err)
	}
}

// An item the user drafted after the pass chose it is not started by the runner
// (Codex P2 on #6300); the explicit start of the same item still works.
func TestAutomaticStartRevalidatesTheItemAtAdmission(t *testing.T) {
	m, repoID := runnerTestManager(t)
	queueItem(t, m, repoID, 1)
	setRunner(t, m, repoID, true, 1)
	if _, err := m.backlogService().SetItemStatus(backlog.SetItemStatusRequest{RepoID: repoID, ID: 1, Status: backlog.StatusDraft}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.startBacklogItemCapped(backlog.StartRequest{RepoID: repoID, ID: 1}, backlog.StartedByRunner); !errors.Is(err, errNotRunnable) {
		t.Fatalf("an automatic start of a drafted item: %v", err)
	}
	if it := loadItem(t, repoID, 1); len(it.Runs) != 0 {
		t.Fatalf("the drafted item was started: %+v", it)
	}
}

// A continuation spends its own session's account, not the project default's
// (Codex P2 on #6300).
func TestContinuationAccountIsTheLiveSessionsAccount(t *testing.T) {
	m, repoID, inst := startedItem(t)
	b, err := backlog.Load(repoID)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.continuationAccount(repoID, b, 1); got != planusage.DefaultAccount {
		t.Fatalf("an ambient session continues on the ambient key, got %q", got)
	}
	inst.Account = "alt"
	if got := m.continuationAccount(repoID, b, 1); got != "alt" {
		t.Fatalf("got %q, want the session's own account", got)
	}
	if got := m.continuationAccount(repoID, b, 99); got != "" {
		t.Fatalf("an item with no run starts a new session on the default: %q", got)
	}
}
