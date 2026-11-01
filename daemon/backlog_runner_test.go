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
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
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
	m.PauseStatusPollFor(repoID, inst.Title, inst.ID, "")
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

// pausableGroup adds a parent and a child under it to the project's backlog
// and returns their ids; the test manager already holds an item, so they are
// not B-1 and B-2.
func pausableGroup(t *testing.T, repoID string) (group, child int) {
	t.Helper()
	if _, err := backlog.Update(repoID, func(b *backlog.Backlog) error {
		g, err := b.Add(backlog.NewItem{Title: "group"}, time.Now())
		if err != nil {
			return err
		}
		c, err := b.Add(backlog.NewItem{Title: "child", Parent: g.ID}, time.Now())
		if err != nil {
			return err
		}
		group, child = g.ID, c.ID
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return group, child
}

// A group paused after the admission check, before the start is recorded,
// stops an automatic start inside the start transaction (Codex P2 on #6468):
// StartBacklogItem is called directly, past startBacklogItemCapped's check, as
// the race would reach it. The refusal is errNotRunnable, so the runner does not
// count it as a failed start.
func TestAutomaticStartRefusesAnItemPausedBeforeItsStartIsRecorded(t *testing.T) {
	m, repoID := runnerTestManager(t)
	group, child := pausableGroup(t, repoID)
	queueItem(t, m, repoID, child)
	setRunner(t, m, repoID, true, 1)
	if _, err := m.backlogService().UpdateItem(backlog.UpdateItemRequest{RepoID: repoID, ID: group, Paused: ptrTo(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.StartBacklogItem(backlog.StartRequest{RepoID: repoID, ID: child}, backlog.StartedByRunner); !errors.Is(err, errNotRunnable) {
		t.Fatalf("an automatic start under a paused group: %v", err)
	}
	if it := loadItem(t, repoID, child); it.Status != backlog.StatusQueued || len(it.Runs) != 0 {
		t.Fatalf("the paused group's item was started: %+v", it)
	}
}

// The same race for "af backlog run": RunNow chose the batch before the group
// was paused, and each pick reaches the start as a pass start, which the
// transaction refuses like the runner's (Codex P2 on #6468).
func TestRunPassStartRefusesAnItemPausedAfterThePassChoseIt(t *testing.T) {
	m, repoID := runnerTestManager(t)
	group, child := pausableGroup(t, repoID)
	queueItem(t, m, repoID, child)
	if _, err := m.backlogService().UpdateItem(backlog.UpdateItemRequest{RepoID: repoID, ID: group, Paused: ptrTo(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.startBacklogItemCapped(backlog.StartRequest{RepoID: repoID, ID: child}, backlog.StartedByRunPass); !errors.Is(err, errNotRunnable) {
		t.Fatalf("a pass start under a paused group: %v", err)
	}
	if it := loadItem(t, repoID, child); it.Status != backlog.StatusQueued || len(it.Runs) != 0 {
		t.Fatalf("the paused group's item was started: %+v", it)
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

// A daemon that inherited a registered account's config dir runs its sessions as
// that account, so the runner's gate identity for "no selection" is that account,
// not the ambient key (Codex P2 on #6300).
func TestPlanAccountFollowsTheInheritedConfigDir(t *testing.T) {
	home, err := config.GetConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "accounts", "claude", "work"))
	if got := planAccount(""); got != "work" {
		t.Fatalf("planAccount(\"\") = %q, want work", got)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(t.TempDir(), "elsewhere"))
	if got := planAccount(""); got != planusage.DefaultAccount {
		t.Fatalf("an unregistered dir = %q, want the ambient key", got)
	}
}

// An item whose live session was handed off to another agent is not continued
// automatically: the budget meters Claude only, so that work could not be bounded
// (Codex P2 on #6300). Item.Program names a NEW session's agent, not this one's.
func TestAutomaticContinuationSkipsASessionHandedOffToAnotherAgent(t *testing.T) {
	m, repoID, inst := startedItem(t)
	b, err := backlog.Load(repoID)
	if err != nil {
		t.Fatal(err)
	}
	inst.Program = "claude"
	if m.continuationUnmeterable(repoID, b, 1) {
		t.Fatal("a Claude session is meterable")
	}
	inst.Program = "codex"
	if !m.continuationUnmeterable(repoID, b, 1) {
		t.Fatal("a session now running Codex was treated as meterable")
	}
	if m.continuationUnmeterable(repoID, b, 99) {
		t.Fatal("an item with no live session starts a new one, which is the item's own program")
	}
}

// A limit recorded on a durable session row that cannot materialize after a
// restart still closes the gate, and evidence that cannot be read closes it too
// (Codex P2 on #6300).
func TestClaudeUsageLimitUsesDurableRowEvidenceAndFailsClosed(t *testing.T) {
	m, _ := runnerTestManager(t)
	prev := loadAccountLimitEvidenceForSwap
	t.Cleanup(func() { loadAccountLimitEvidenceForSwap = prev })
	now := time.Now()
	loadAccountLimitEvidenceForSwap = func() ([]session.AccountLimitObservationData, error) {
		return []session.AccountLimitObservationData{{Agent: tmux.ProgramClaude, Account: "work", ResetAt: now.Add(time.Hour)}}, nil
	}
	limited, until, err := m.claudeUsageLimit("work", now)
	if err != nil || !limited || until.IsZero() {
		t.Fatalf("limited=%v until=%v err=%v: the row's limit did not close the gate", limited, until, err)
	}
	if limited, _, _ := m.claudeUsageLimit("other", now); limited {
		t.Fatal("another account was limited by work's evidence")
	}
	loadAccountLimitEvidenceForSwap = func() ([]session.AccountLimitObservationData, error) {
		return nil, errors.New("unreadable row")
	}
	if _, _, err := m.claudeUsageLimit("work", now); err == nil {
		t.Fatal("unreadable evidence did not fail closed")
	}
}

// A start judges the agent it would actually run: a live session's current agent,
// else the item's own program for a new session (Codex P2 on #6300). An item that
// names codex but whose session was handed to Claude is continued; one that names
// claude whose session went to codex is not.
func TestStartUnmeterableJudgesTheLiveSessionBeforeTheItemProgram(t *testing.T) {
	m, repoID, inst := startedItem(t)
	load := func() *backlog.Backlog {
		b, err := backlog.Load(repoID)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	if _, err := m.backlogService().UpdateItem(backlog.UpdateItemRequest{RepoID: repoID, ID: 1, Program: ptrTo("codex")}); err != nil {
		t.Fatal(err)
	}
	inst.Program = "claude"
	if m.startUnmeterable(repoID, load(), 1) {
		t.Fatal("a session handed off to Claude is meterable whatever the item names")
	}
	if _, err := m.backlogService().UpdateItem(backlog.UpdateItemRequest{RepoID: repoID, ID: 1, Program: ptrTo("claude")}); err != nil {
		t.Fatal(err)
	}
	inst.Program = "codex"
	if !m.startUnmeterable(repoID, load(), 1) {
		t.Fatal("a session handed off to Codex is not meterable")
	}
}

// The runner refuses to admit an automatic start while the project's session view
// is incomplete: a row that failed to load can still be a live run holding a slot,
// so the slots in use may be undercounted (Codex P2 on #6300). An explicit start
// is the user's call and goes ahead.
func TestAutomaticStartIsRefusedWhileTheSessionViewIsIncomplete(t *testing.T) {
	m, repoID := runnerTestManager(t)
	queueItem(t, m, repoID, 1)
	setRunner(t, m, repoID, true, 1)
	m.mu.Lock()
	m.skippedRepos = []SkippedRepo{{RepoID: repoID, Reason: SkippedRepoReasonRowsFailedToLoad, FailedRows: 1}}
	m.mu.Unlock()
	if !m.backlogViewIncomplete(repoID) || m.backlogViewIncomplete("some-other-repo") {
		t.Fatal("the view is incomplete for this project only")
	}
	if _, err := m.startBacklogItemCapped(backlog.StartRequest{RepoID: repoID, ID: 1}, backlog.StartedByRunner); !errors.Is(err, errViewIncomplete) {
		t.Fatalf("an automatic start with a row that failed to load: %v", err)
	}
	if it := loadItem(t, repoID, 1); len(it.Runs) != 0 {
		t.Fatalf("a session was started: %+v", it)
	}
	m.mu.Lock()
	m.skippedRepos = nil
	m.mu.Unlock()
	if _, err := m.startBacklogItemCapped(backlog.StartRequest{RepoID: repoID, ID: 1}, backlog.StartedByRunner); err != nil {
		t.Fatalf("a complete view admits the start: %v", err)
	}
}

// A start is admitted on a fresh reading, and the choice is recomputed against it:
// the default account's plan can be empty while another account's continuation is
// picked, and the cached reading may be past that account's limit by now (Codex P2
// on #6300).
func TestPickStartsFreshRecomputesTheChoiceOnAFreshReading(t *testing.T) {
	cached := planusage.Usage{Runs: map[string]float64{"cached": 1}}
	fresh := planusage.Usage{Runs: map[string]float64{"fresh": 1}}
	refreshed := 0
	refresh := func() planusage.Usage { refreshed++; return fresh }
	choose := func(u planusage.Usage) backlog.RunnerPlan {
		if _, ok := u.Runs["fresh"]; ok {
			return backlog.RunnerPlan{} // the fresh reading closes the gate
		}
		return backlog.RunnerPlan{Start: []int{1}}
	}
	if p := pickStartsFresh(cached, refresh, choose); len(p.Start) != 0 || refreshed != 1 {
		t.Fatalf("a start the fresh reading closes was admitted: %+v (refreshed %d)", p, refreshed)
	}
	refreshed = 0
	if p := pickStartsFresh(fresh, refresh, choose); len(p.Start) != 0 || refreshed != 0 {
		t.Fatalf("nothing picked should not refresh: %+v (refreshed %d)", p, refreshed)
	}
}

// A queued item with a live session that spends a registered account: the review
// ended its run and the user queued it again, so the runner would continue it in
// place.
func queuedContinuationOn(t *testing.T, account string) (*Manager, string, *session.Instance) {
	t.Helper()
	m, repoID := runnerTestManager(t)
	home, err := config.GetConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if account != "" {
		if err := os.MkdirAll(filepath.Join(home, "accounts", "claude", account), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := m.StartBacklogItem(backlog.StartRequest{RepoID: repoID, ID: 1}, backlog.StartedByUser); err != nil {
		t.Fatal(err)
	}
	m.backgroundMutationWG.Wait()
	inst := m.itemSessions(repoID)[loadItem(t, repoID, 1).Runs[0].SessionID].instance
	inst.Account = account
	if !inst.DisarmBacklogRun() {
		t.Fatal("precondition: run active")
	}
	m.backlogRunEnded(repoID, inst.ID)
	queueItem(t, m, repoID, 1)
	return m, repoID, inst
}

// The runner is stopped only when no queued item can start. With the project's
// default account over its stop-at and a queued continuation on an account with
// room, selectStarts starts the continuation, so the runner view (the event and
// what Get returns) must not say "stopped by the budget" (#6309, found on
// 7d8eac6c14).
func TestRunnerViewIsNotStoppedWhileAContinuationOnAnOpenAccountStarts(t *testing.T) {
	m, repoID, _ := queuedContinuationOn(t, "alt")
	var sent []SendPromptRequest
	prev := sendBacklogPrompt
	sendBacklogPrompt = func(_ *Manager, req SendPromptRequest) error { sent = append(sent, req); return nil }
	t.Cleanup(func() { sendBacklogPrompt = prev })
	setRunner(t, m, repoID, true, 3)
	writeFreshReading(t, 95) // the default account is past its stop-at
	now := time.Now().UTC()
	if err := planusage.WriteReading("alt", planusage.Reading{
		FiveHour: &planusage.Limit{UsedPercentage: 10, ResetsAt: now.Add(2 * time.Hour).Unix()}, ReadAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	m.meterUsageWithin(0) // one real pass, so both accounts have a reading
	events := runnerEvents(t, m)

	// Queued, before the pass starts it: Get must not read "stopped" either.
	v, err := m.backlogService().Get(backlog.GetRequest{RepoID: repoID})
	if err != nil {
		t.Fatal(err)
	}
	if v.Runner.State == backlog.RunnerStopped || v.Runner.Gate != "" {
		t.Fatalf("Get says stopped while a queued continuation can start: %+v", v.Runner)
	}

	pass(m, repoID, time.Now())

	if len(sent) != 1 {
		t.Fatalf("the continuation on the open account was not started: %d prompts", len(sent))
	}
	var last backlog.RunnerEvent
	for _, ev := range events() {
		if ev.RepoID == repoID {
			last = ev
		}
	}
	if last.State == backlog.RunnerStopped || last.Gate != "" {
		t.Fatalf("the runner event says stopped while it starts work: %+v", last)
	}
}

// With nothing queued on an open account the default account's closed gate still
// stands: a new session would spend it.
func TestRunnerViewStaysStoppedWhenNoQueuedItemsAccountIsOpen(t *testing.T) {
	m, repoID, _ := queuedContinuationOn(t, "alt")
	setRunner(t, m, repoID, true, 3)
	writeFreshReading(t, 95)
	now := time.Now().UTC()
	if err := planusage.WriteReading("alt", planusage.Reading{
		FiveHour: &planusage.Limit{UsedPercentage: 96, ResetsAt: now.Add(2 * time.Hour).Unix()}, ReadAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	m.meterUsageWithin(0)
	v, err := m.backlogService().Get(backlog.GetRequest{RepoID: repoID})
	if err != nil {
		t.Fatal(err)
	}
	if v.Runner.State != backlog.RunnerStopped || v.Runner.Gate != backlog.GateStopAt {
		t.Fatalf("every account is past its stop-at: %+v", v.Runner)
	}
}

// A launch command that sets CLAUDE_CONFIG_DIR itself puts the session's usage
// under that dir's account, which is where the meter files it; the continuation
// gate must read the same account, not the ambient key (#6309).
func TestContinuationAccountFollowsTheLaunchCommandsConfigDir(t *testing.T) {
	m, repoID, inst := queuedContinuationOn(t, "work")
	home, err := config.GetConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	inst.Account = "" // no account field: only the launch command names the dir
	// The fixture session has no worktree; the launch command is resolved against one.
	repo := setupControlRepo(t)
	gw, err := sessiongit.NewGitWorktreeFromStorage(repo, repo, inst.Title, "main", "", false, true)
	if err != nil {
		t.Fatal(err)
	}
	inst.SetGitWorktreeForTest(gw)
	inst.Program = "CLAUDE_CONFIG_DIR=" + filepath.Join(home, "accounts", "claude", "work") + " claude"
	if _, _, err := inst.ClaudeRunTranscriptDirs(); err != nil {
		t.Fatalf("precondition: the session resolves its transcript dirs: %v", err)
	}
	b, err := backlog.Load(repoID)
	if err != nil {
		t.Fatal(err)
	}
	if got := m.continuationAccount(repoID, b, 1); got != "work" {
		t.Fatalf("an accountless session launched with CLAUDE_CONFIG_DIR=…/work continues on %q, want work", got)
	}
}

// The runner view agrees with the starts: when the pacer holds an account back
// (its reading is stale and a start already used the last reading), nothing is
// started on it, so the view is not "running" for that account either (Codex P2 on
// #6467 4448a7bf7c).
func TestRunnerViewIsNotRunningForAnAccountThePacerHoldsBack(t *testing.T) {
	m, repoID, _ := queuedContinuationOn(t, "alt")
	var sent []SendPromptRequest
	prev := sendBacklogPrompt
	sendBacklogPrompt = func(_ *Manager, req SendPromptRequest) error { sent = append(sent, req); return nil }
	t.Cleanup(func() { sendBacklogPrompt = prev })
	setRunner(t, m, repoID, true, 3)
	writeFreshReading(t, 95)
	readAt := time.Now().UTC()
	if err := planusage.WriteReading("alt", planusage.Reading{
		FiveHour: &planusage.Limit{UsedPercentage: 10, ResetsAt: readAt.Add(3 * time.Hour).Unix()}, ReadAt: readAt,
	}); err != nil {
		t.Fatal(err)
	}
	m.meterUsageWithin(0)
	events := runnerEvents(t, m)
	// The reading is stale at the pass, and a start was made after it was taken.
	at := readAt.Add(backlog.StaleReadingAfter + time.Minute)
	m.backlog.runner.noteStarted(readAt.Add(time.Minute))

	pass(m, repoID, at)

	if len(sent) != 0 {
		t.Fatalf("a start was made on an account the pacer holds back: %d prompts", len(sent))
	}
	var last backlog.RunnerEvent
	for _, ev := range events() {
		if ev.RepoID == repoID {
			last = ev
		}
	}
	if last.State == backlog.RunnerRunning {
		t.Fatalf("the runner event says running while the pacer holds the account back: %+v", last)
	}
}

// A usage-limit wall on a session with no account field belongs to the account its
// launch command's config dir names, the account a continuation gates on, not the
// ambient one (Codex P2 on #6467 4448a7bf7c).
func TestUsageLimitWallOnAnAccountlessOverrideSessionIsThatAccountsWall(t *testing.T) {
	m, _, inst := queuedContinuationOn(t, "work")
	home, err := config.GetConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	inst.Account = ""
	repo := setupControlRepo(t)
	gw, err := sessiongit.NewGitWorktreeFromStorage(repo, repo, inst.Title, "main", "", false, true)
	if err != nil {
		t.Fatal(err)
	}
	inst.SetGitWorktreeForTest(gw)
	inst.Program = "CLAUDE_CONFIG_DIR=" + filepath.Join(home, "accounts", "claude", "work") + " claude"
	now := time.Now()
	inst.SetLimitReached(now.Add(time.Hour))
	if _, acct, ok := inst.LimitIdentity(); !ok || acct != "" {
		t.Fatalf("precondition: a wall attributed to no account, got %q ok=%v", acct, ok)
	}
	if limited, _, err := m.claudeUsageLimit("work", now); err != nil || !limited {
		t.Fatalf("work limited=%v err=%v: the wall on its override session was not counted", limited, err)
	}
	if limited, _, _ := m.claudeUsageLimit(planusage.DefaultAccount, now); limited {
		t.Fatal("the ambient account was limited by work's wall")
	}
}

// The same holds when the launch resolves to the ambient key while the daemon's own
// environment names a registered account: the wall is the ambient account's, not
// the daemon's (Codex P2 on #6467 c9b3a0135f).
func TestUsageLimitWallOnAnOverrideToTheAmbientDirIsTheAmbientAccountsWall(t *testing.T) {
	m, _, inst := queuedContinuationOn(t, "work")
	home, err := config.GetConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLAUDE_CONFIG_DIR", filepath.Join(home, "accounts", "claude", "work"))
	inst.Account = ""
	repo := setupControlRepo(t)
	gw, err := sessiongit.NewGitWorktreeFromStorage(repo, repo, inst.Title, "main", "", false, true)
	if err != nil {
		t.Fatal(err)
	}
	inst.SetGitWorktreeForTest(gw)
	inst.Program = "CLAUDE_CONFIG_DIR=" + filepath.Join(t.TempDir(), ".claude") + " claude"
	now := time.Now()
	inst.SetLimitReached(now.Add(time.Hour))
	if limited, _, err := m.claudeUsageLimit(planusage.DefaultAccount, now); err != nil || !limited {
		t.Fatalf("ambient limited=%v err=%v: the wall on its override session was not counted", limited, err)
	}
	if limited, _, _ := m.claudeUsageLimit("work", now); limited {
		t.Fatal("the daemon's account was limited by the ambient override's wall")
	}
}
