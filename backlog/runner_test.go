package backlog

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/planusage"
)

var runnerNow = time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)

func planWindow(plan, backlog float64, resetsIn time.Duration) planusage.PlanWindow {
	at := runnerNow.Add(resetsIn)
	return planusage.PlanWindow{Plan: plan, Backlog: backlog, ResetsAt: &at, All: 10}
}

func readUsage(five, week planusage.PlanWindow, age time.Duration) planusage.Usage {
	at := runnerNow.Add(-age)
	return planusage.Usage{ReadingAt: &at, Windows: planusage.Windows{FiveHour: five, SevenDay: week}}
}

func gateIn(u planusage.Usage) GateInput {
	return GateInput{Budget: *NewBudget(), Usage: u, Now: runnerNow, Loc: time.UTC}
}

func TestCheckGates(t *testing.T) {
	ok := readUsage(planWindow(20, 5, 2*time.Hour), planWindow(30, 10, 4*24*time.Hour), time.Minute)
	cases := []struct {
		name   string
		in     func() GateInput
		gate   string
		reason string
	}{
		{"open", func() GateInput { return gateIn(ok) }, "", ""},
		{"share of the 5-hour window", func() GateInput {
			return gateIn(readUsage(planWindow(60, 50, 2*time.Hour), planWindow(30, 10, 96*time.Hour), 0))
		}, GateShare, "Backlog used its 50% of the 5-hour window · resumes 14:00"},
		{"share of the week", func() GateInput {
			return gateIn(readUsage(planWindow(20, 5, 2*time.Hour), planWindow(60, 51, 96*time.Hour), 0))
		}, GateShare, "Backlog used its 50% of the week · resumes Mon 12:00"},
		{"share 0 stops everything", func() GateInput {
			in := gateIn(ok)
			in.Budget.Share = 0
			return in
		}, GateShare, "Backlog share is 0% · raise it with \"af backlog budget --share 50\""},
		{"share 0 with a dollar limit and no reading", func() GateInput {
			in := gateIn(planusage.Usage{})
			in.Budget.Share, in.Budget.USDLimit = 0, 10
			return in
		}, GateShare, ""},
		{"stop at", func() GateInput {
			return gateIn(readUsage(planWindow(91, 5, 2*time.Hour), planWindow(30, 10, 96*time.Hour), 0))
		}, GateStopAt, "Plan at 91% of the 5-hour window (stop at 90%) · resumes 14:00"},
		{"usage limit with reset", func() GateInput {
			in := gateIn(ok)
			in.Limited, in.LimitUntil = true, runnerNow.Add(3*time.Hour)
			return in
		}, GateUsageLimit, "Usage limit until 15:00"},
		{"usage limit, reset unknown", func() GateInput {
			in := gateIn(ok)
			in.Limited = true
			return in
		}, GateUsageLimit, "Usage limit"},
		{"no reading", func() GateInput { return gateIn(planusage.Usage{}) }, GateNoReading, ""},
		{"first meter pass not done", func() GateInput { return gateIn(planusage.Usage{Metering: true}) }, GateMetering, "Reading plan usage…"},
		{"reset windows count as no reading", func() GateInput {
			return gateIn(readUsage(planusage.PlanWindow{}, planusage.PlanWindow{}, 6*time.Hour))
		}, GateNoReading, ""},
		{"no reading with a dollar limit", func() GateInput {
			in := gateIn(planusage.Usage{})
			in.Budget.USDLimit = 10
			return in
		}, "", ""},
		{"dollar limit spent in the last 5 hours", func() GateInput {
			u := planusage.Usage{}
			u.Windows.FiveHour.BacklogCost = 10.5
			in := gateIn(u)
			in.Budget.USDLimit = 10
			return in
		}, GateUSDLimit, "Backlog spent $10.50 of its $10.00 limit in the last 5 hours"},
		{"dollar limit spent in the week of a reading", func() GateInput {
			u := ok
			u.Windows.FiveHour.BacklogCost, u.Windows.SevenDay.BacklogCost = 2, 12
			in := gateIn(u)
			in.Budget.USDLimit = 10
			return in
		}, GateUSDLimit, "Backlog spent $12.00 of its $10.00 limit in the week · resumes Mon 12:00"},
		{"dollar limit not reached", func() GateInput {
			u := ok
			u.Windows.FiveHour.BacklogCost, u.Windows.SevenDay.BacklogCost = 9, 9.99
			in := gateIn(u)
			in.Budget.USDLimit = 10
			return in
		}, "", ""},
		{"limit record unreadable", func() GateInput {
			in := gateIn(ok)
			in.LimitUnknown = "af cannot read /x/ledger.json; fix or remove it"
			return in
		}, GateUsageLimit, "Can't tell whether the plan is at its usage limit · af cannot read /x/ledger.json; fix or remove it"},
		{"account unknown", func() GateInput {
			in := gateIn(ok)
			in.NoAccount = "af cannot tell which account"
			return in
		}, GateNoAccount, "Af cannot tell which account"},
		{"unmeterable", func() GateInput {
			u := ok
			u.Unmeterable = map[string]string{"s2": UnmeterableProgram(7, Where{Name: "detail"}, "aider"), "s1": "no price for model x"}
			return gateIn(u)
		}, GateUnmeterable, "No price for model x"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := CheckGates(c.in())
			if g.Name != c.gate || (c.reason != "" && g.Reason != c.reason) {
				t.Fatalf("got %+v, want %s %q", g, c.gate, c.reason)
			}
			if g.Open() != (c.gate == "") {
				t.Fatal(g)
			}
		})
	}
	if g := CheckGates(gateIn(planusage.Usage{})); !strings.HasPrefix(g.Reason, "No plan usage seen yet") {
		t.Fatal(g.Reason)
	}
}

func TestStaleReading(t *testing.T) {
	if StaleReading(planusage.Usage{}, runnerNow) {
		t.Fatal("no reading is for the gates, not the pacer")
	}
	if StaleReading(readUsage(planusage.PlanWindow{}, planusage.PlanWindow{}, 15*time.Minute), runnerNow) {
		t.Fatal("15 minutes is still fresh")
	}
	if !StaleReading(readUsage(planusage.PlanWindow{}, planusage.PlanWindow{}, 16*time.Minute), runnerNow) {
		t.Fatal("16 minutes is stale")
	}
}

func TestStartPacerOneStartAcrossProjects(t *testing.T) {
	var p StartPacer
	stale := readUsage(planusage.PlanWindow{}, planusage.PlanWindow{}, 20*time.Minute)
	fresh := readUsage(planusage.PlanWindow{}, planusage.PlanWindow{}, time.Minute)
	if got := p.Limit(fresh, runnerNow); got != -1 {
		t.Fatalf("fresh: %d", got)
	}
	// Pass 1: project A may start one; project B, in the same pass, none.
	if got := p.Limit(stale, runnerNow); got != 1 {
		t.Fatalf("pass 1, project A: %d", got)
	}
	p.Started(runnerNow)
	if got := p.Limit(stale, runnerNow); got != 0 {
		t.Fatalf("pass 1, project B: %d", got)
	}
	// Pass 2, 5 s later, the reading is still older than the start.
	next := runnerNow.Add(5 * time.Second)
	if got := p.Limit(stale, next); got != 0 {
		t.Fatalf("pass 2: %d", got)
	}
	// A reading newer than the start, gone stale again, allows one more.
	later := runnerNow.Add(time.Hour)
	at := runnerNow.Add(time.Minute)
	newer := planusage.Usage{ReadingAt: &at}
	if got := p.Limit(newer, later); got != 1 {
		t.Fatalf("after a newer reading: %d", got)
	}
}

// runnerBacklog is B-1 parent (B-2 done, B-3 queued), B-4 queued waiting on
// B-5 draft, B-6 working, B-7 queued, B-8 queued.
func runnerBacklog(t *testing.T) (*Backlog, map[int]Derived) {
	t.Helper()
	b := mk(t,
		Item{ID: 1},
		Item{ID: 2, Parent: 1, Status: StatusDone},
		Item{ID: 3, Parent: 1, Status: StatusQueued},
		Item{ID: 4, Status: StatusQueued, WaitsOn: []int{5}},
		Item{ID: 5},
		Item{ID: 6, Status: StatusRunning, Runs: []RunRef{{SessionID: "s6", State: RunStarted}}},
		Item{ID: 7, Status: StatusQueued},
		Item{ID: 8, Status: StatusQueued},
	)
	b.Settings.Runner, b.Settings.MaxRunning = true, 4
	return b, b.DeriveAll(stubFacts(map[string]SessionFacts{"s6": {Live: true, RunActive: true}}))
}

func in(states map[int]Derived, used int) RunnerInput {
	return RunnerInput{States: states, Used: used, Limit: -1}
}

func TestPlanRunnerStartsQueuedInTreeOrder(t *testing.T) {
	b, states := runnerBacklog(t)
	p := b.PlanRunner(in(states, 1))
	if !reflect.DeepEqual(p.Start, []int{3, 7, 8}) || p.View.State != RunnerRunning || p.View.Used != 1 || p.View.Max != 4 {
		t.Fatalf("%+v", p)
	}
	if p := b.PlanRunner(in(states, 2)); !reflect.DeepEqual(p.Start, []int{3, 7}) {
		t.Fatalf("free slots: %+v", p.Start)
	}
	if p := b.PlanRunner(in(states, 4)); len(p.Start) != 0 || p.View.State != RunnerRunning {
		t.Fatalf("full: %+v", p)
	}
	if p := b.PlanRunner(in(states, 6)); len(p.Start) != 0 {
		t.Fatalf("over full: %+v", p)
	}
}

func TestPlanRunnerLimitAndSkip(t *testing.T) {
	b, states := runnerBacklog(t)
	i := in(states, 0)
	i.Limit = 1
	if p := b.PlanRunner(i); !reflect.DeepEqual(p.Start, []int{3}) {
		t.Fatalf("limit 1: %+v", p.Start)
	}
	i.Limit = 0
	if p := b.PlanRunner(i); len(p.Start) != 0 || p.View.State != RunnerIdle || p.View.Reason != "Waiting for a fresh plan reading." {
		t.Fatalf("limit 0: %+v", p)
	}
	i.Limit = -1
	i.Skip = func(id int) bool { return id == 3 }
	if p := b.PlanRunner(i); !reflect.DeepEqual(p.Start, []int{7, 8}) {
		t.Fatalf("skip: %+v", p.Start)
	}
}

func TestRunnerHoldsItemAfterFailedStart(t *testing.T) {
	b := mk(t, Item{ID: 1, Status: StatusQueued}, Item{ID: 2, Status: StatusQueued})
	b.Settings.Runner, b.Settings.MaxRunning = true, 1
	if _, err := b.BeginStart(1, StartedByRunner, nil, runnerNow); err != nil {
		t.Fatal(err)
	}
	if err := b.FinishStart(1, runnerNow, "", "", "", errors.New("no worktree"), runnerNow); err != nil {
		t.Fatal(err)
	}
	if b.Item(1).Status != StatusQueued {
		t.Fatal(b.Item(1).Status)
	}
	if p := b.PlanRunner(in(b.DeriveAll(nil), 0)); !reflect.DeepEqual(p.Start, []int{2}) {
		t.Fatalf("the failed item is held and the next one starts: %+v", p.Start)
	}
	if got, _ := b.RunNow(b.DeriveAll(nil), 0, 2); !reflect.DeepEqual(got, []int{2}) {
		t.Fatalf("run holds it too: %v", got)
	}
	title := "retry"
	if _, err := b.Edit(1, Edit{Title: &title}, runnerNow.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if p := b.PlanRunner(in(b.DeriveAll(nil), 0)); !reflect.DeepEqual(p.Start, []int{1}) {
		t.Fatalf("a user edit releases it: %+v", p.Start)
	}
}

func TestPlanRunnerOffAndStopped(t *testing.T) {
	b, states := runnerBacklog(t)
	g := Gate{Name: GateShare, Reason: "Backlog used its 50% of the week · resumes Mon 09:00"}
	i := in(states, 0)
	i.Gate = g
	p := b.PlanRunner(i)
	if len(p.Start) != 0 || p.View.State != RunnerStopped || p.View.Gate != GateShare || p.View.Reason != g.Reason {
		t.Fatalf("%+v", p)
	}
	b.Settings.Runner = false
	p = b.PlanRunner(in(states, 0))
	if len(p.Start) != 0 || p.View.State != RunnerOff || p.View.Reason != "Items run only when you start them." {
		t.Fatalf("%+v", p)
	}
}

func TestPlanRunnerWaitsForAReading(t *testing.T) {
	b, states := runnerBacklog(t)
	i := in(states, 0)
	i.Gate = CheckGates(gateIn(planusage.Usage{}))
	if p := b.PlanRunner(i); p.View.State != RunnerWaiting || p.View.Gate != GateNoReading || len(p.Start) != 0 {
		t.Fatalf("%+v", p)
	}
}

func TestPlanRunnerIdle(t *testing.T) {
	b := mk(t, Item{ID: 1}, Item{ID: 2, Status: StatusReview})
	b.Settings.Runner = true
	p := b.PlanRunner(in(b.DeriveAll(nil), 0))
	if p.View.State != RunnerIdle || p.View.Reason != "Nothing queued." || len(p.Start) != 0 {
		t.Fatalf("%+v", p)
	}
}

func TestRunNow(t *testing.T) {
	b, states := runnerBacklog(t)
	b.Settings.Runner = false
	if got, _ := b.RunNow(states, 1, 0); !reflect.DeepEqual(got, []int{3}) {
		t.Fatalf("default count: %v", got)
	}
	if got, _ := b.RunNow(states, 1, 10); !reflect.DeepEqual(got, []int{3, 7, 8}) {
		t.Fatalf("count past free slots: %v", got)
	}
	if got, reason := b.RunNow(states, 4, 1); got != nil || reason != `all 4 slots are in use; wait for a run to end or raise them with "af backlog runner --max-running 5"` {
		t.Fatalf("%v %q", got, reason)
	}
	b.Settings.MaxRunning = MaxMaxRunning
	if _, reason := b.RunNow(states, 8, 1); reason != "all 8 slots are in use; wait for a run to end" {
		t.Fatal(reason)
	}
	empty := mk(t, Item{ID: 1})
	if got, reason := empty.RunNow(empty.DeriveAll(nil), 0, 1); got != nil || !strings.HasPrefix(reason, "nothing queued") {
		t.Fatalf("%v %q", got, reason)
	}
}

func TestRunUse(t *testing.T) {
	w := planWindow(40, 10, time.Hour)
	if use, ok := RunUse(2.5, w); !ok || use != 10 {
		t.Fatal(use, ok)
	}
	if _, ok := RunUse(1, planusage.PlanWindow{}); ok {
		t.Fatal("no reading")
	}
	w.All = 0
	if _, ok := RunUse(1, w); ok {
		t.Fatal("no metered cost")
	}
}

func capBacklog(t *testing.T, r1 RunRef) (*Backlog, map[int]Derived) {
	t.Helper()
	r1.SessionID, r1.State = "s1", RunStarted
	b := mk(t,
		Item{ID: 1, Status: StatusRunning, Runs: []RunRef{r1}},
		Item{ID: 2, Status: StatusRunning, Runs: []RunRef{{SessionID: "s2", State: RunStarted}}},
		Item{ID: 3, Status: StatusReview, Runs: []RunRef{{SessionID: "s3", State: RunStarted}}},
	)
	return b, b.DeriveAll(stubFacts(map[string]SessionFacts{
		"s1": {Live: true, RunActive: true}, "s2": {Live: true}, "s3": {Live: true},
	}))
}

func oneWindow(w planusage.PlanWindow) func(string) planusage.PlanWindow {
	return func(string) planusage.PlanWindow { return w }
}

func TestCheckCaps(t *testing.T) {
	w := planWindow(40, 20, time.Hour)
	reset := w.ResetsAt.Unix()
	cost := map[string]float64{"s1": 3, "s2": 9, "s3": 9}
	inWindow := RunRef{StartedAt: runnerNow.Add(-time.Hour)}
	b, states := capBacklog(t, inWindow)
	got := b.CheckCaps(states, cost, oneWindow(w), 10)
	if len(got.Over) != 1 || got.Over[0] != (OverCap{ID: 1, SessionID: "s1", Cost: 3, Reset: reset, Use: 12}) {
		t.Fatalf("only the working run counts: %+v", got)
	}
	if len(got.Rebase) != 1 || got.Rebase[0] != (CapBase{ID: 1, SessionID: "s1", Reset: reset, Base: 0}) {
		t.Fatalf("a run started in the window counts from 0: %+v", got.Rebase)
	}
	if got := b.CheckCaps(states, cost, oneWindow(w), 12); len(got.Over) != 0 {
		t.Fatalf("at the cap is not over it: %+v", got)
	}
	if got := b.CheckCaps(states, cost, oneWindow(w), 0); len(got.Over)+len(got.Rebase) != 0 {
		t.Fatalf("0 turns the cap off: %+v", got)
	}
	if got := b.CheckCaps(states, map[string]float64{}, oneWindow(w), 10); len(got.Over)+len(got.Rebase) != 0 {
		t.Fatalf("an unmetered run: %+v", got)
	}
	if got := b.CheckCaps(states, cost, oneWindow(planusage.PlanWindow{}), 10); len(got.Over)+len(got.Rebase) != 0 {
		t.Fatalf("no window: %+v", got)
	}

	b, states = capBacklog(t, RunRef{StartedAt: inWindow.StartedAt, CapReset: reset, CapBase: 1})
	if got := b.CheckCaps(states, cost, oneWindow(w), 10); len(got.Over)+len(got.Rebase) != 0 {
		t.Fatalf("use counts from the base: %+v", got)
	}
	if got := b.CheckCaps(states, map[string]float64{"s1": 4}, oneWindow(w), 10); len(got.Over) != 1 {
		t.Fatalf("passes the cap again: %+v", got)
	}
}

func TestCheckCapsAcrossWindowReset(t *testing.T) {
	oldReset := runnerNow.Add(-time.Minute).Unix()
	b, states := capBacklog(t, RunRef{StartedAt: runnerNow.Add(-3 * time.Hour), CapReset: oldReset, CapBase: 0})
	// The new window has little metered cost so far; the run's lifetime cost
	// of 30 would be 400% of it.
	w := planWindow(4, 1, 5*time.Hour-time.Minute)
	w.All = 1
	got := b.CheckCaps(states, map[string]float64{"s1": 30}, oneWindow(w), 10)
	if len(got.Over) != 0 {
		t.Fatalf("cost from the old window counted: %+v", got)
	}
	if len(got.Rebase) != 1 || got.Rebase[0].Base != 30 || got.Rebase[0].Reset != w.ResetsAt.Unix() {
		t.Fatalf("rebase %+v", got.Rebase)
	}
	if ids := b.SetCapBases(got.Rebase); len(ids) != 1 || b.Item(1).Runs[0].CapBase != 30 {
		t.Fatalf("%v %+v", ids, b.Item(1).Runs[0])
	}
	if got := b.CheckCaps(states, map[string]float64{"s1": 30.2}, oneWindow(w), 10); len(got.Over) != 0 || len(got.Rebase) != 0 {
		t.Fatalf("0.2 of 1 is 0.8%%: %+v", got)
	}
	if got := b.CheckCaps(states, map[string]float64{"s1": 33}, oneWindow(w), 10); len(got.Over) != 1 {
		t.Fatalf("3 of 1 at 4%% is 12%%: %+v", got)
	}
}

func TestAskOverCap(t *testing.T) {
	b, _ := capBacklog(t, RunRef{})
	c := OverCap{ID: 1, SessionID: "s1", Cost: 3, Reset: 99, Use: 10.44}
	q, err := b.AskOverCap(c, 10, runnerNow)
	if err != nil {
		t.Fatal(err)
	}
	want := "This run used 10.4% of your 5-hour window, over the 10% cap per run. Answer to continue, or send it to review."
	if q.Text != want || q.Author != AuthorAF || q.Kind != KindQuestion {
		t.Fatalf("%+v", q)
	}
	it := b.Item(1)
	if it.Runs[0].CapBase != 3 || it.Runs[0].CapReset != 99 || it.OpenQuestion() == nil {
		t.Fatalf("%+v", it)
	}
	if _, err := b.AskOverCap(c, 10, runnerNow); err == nil {
		t.Fatal("asked twice")
	}
	if _, err := b.AskOverCap(OverCap{ID: 2, SessionID: "other"}, 10, runnerNow); err == nil {
		t.Fatal("asked on a run that is not the latest")
	}
}

func TestSendToReviewClosesAFQuestion(t *testing.T) {
	b, _ := capBacklog(t, RunRef{})
	if _, err := b.AskOverCap(OverCap{ID: 1, SessionID: "s1", Cost: 3, Use: 12}, 10, runnerNow); err != nil {
		t.Fatal(err)
	}
	if _, err := b.SetStatus(1, StatusReview, ActorUser, nil, runnerNow); err != nil {
		t.Fatal(err)
	}
	it := b.Item(1)
	if it.OpenQuestion() != nil || it.Thread[0].Closed != ClosedSentToReview {
		t.Fatalf("%+v", it.Thread)
	}
	if got := b.Derive(1, nil).State; got != StateInReview {
		t.Fatal(got)
	}

	if _, err := b.Ask(2, "s2", Question{Text: "which db?"}, runnerNow); err != nil {
		t.Fatal(err)
	}
	if _, err := b.SetStatus(2, StatusReview, ActorUser, nil, runnerNow); err != nil {
		t.Fatal(err)
	}
	if b.Item(2).OpenQuestion() == nil {
		t.Fatal("the agent's own question stays open")
	}
}

func TestResetLabel(t *testing.T) {
	if got := ResetLabel(runnerNow.Add(23*time.Hour), runnerNow, time.UTC); got != "11:00" {
		t.Fatal(got)
	}
	if got := ResetLabel(runnerNow.Add(25*time.Hour), runnerNow, time.UTC); got != "Fri 13:00" {
		t.Fatal(got)
	}
	if Percent(12.5) != "12.5%" || Percent(50) != "50%" {
		t.Fatal(Percent(12.5), Percent(50))
	}
}

// With plan usage off ([backlog] plan_usage, the default) there is no budget to
// hold the runner to, so it starts nothing and says why. Starting by hand still
// works (that path takes no gate). The usd limit cannot gate either: it needs the
// transcript metering that is off.
func TestRunnerStaysClosedWhileTheBudgetCannotBeMetered(t *testing.T) {
	off := planusage.Usage{Disabled: true}
	for _, usd := range []float64{0, 10} {
		in := gateIn(off)
		in.Budget.USDLimit = usd
		g := CheckGates(in)
		if g.Name != GatePlanUsageOff || !strings.Contains(g.Reason, "af config set backlog.plan_usage true") ||
			!strings.Contains(g.Reason, "af backlog start") {
			t.Fatalf("usd_limit %v: gate %+v", usd, g)
		}
	}
	// It outranks a stale "no reading" wait: the runner must not start one item to take a reading.
	b, states := runnerBacklog(t)
	i := in(states, 0)
	i.Gate = CheckGates(gateIn(off))
	p := b.PlanRunner(i)
	if len(p.Start) != 0 || p.View.State != RunnerStopped || p.View.Gate != GatePlanUsageOff {
		t.Fatalf("%+v", p)
	}
}

// The runner closes while the meter is unavailable (Codex P2 on #6300): the last
// good result is not stale-but-usable, and a dollar-limit-only budget must not
// keep starting against spend it can no longer see.
func TestRunnerGateClosesWhileTheMeterIsUnavailable(t *testing.T) {
	in := gateIn(planusage.Usage{Unavailable: "plan/meter.json: invalid character"})
	in.Budget.USDLimit = 10
	g := CheckGates(in)
	if g.Name != GateMeterUnavailable || !strings.Contains(g.Reason, "invalid character") {
		t.Fatalf("gate %+v", g)
	}
}

func TestRunnerEligibleOnlyForAQueuedUnblockedLeaf(t *testing.T) {
	b, states := runnerBacklog(t)
	var id int
	for _, it := range b.Items {
		if states[it.ID].State == StateQueued && !b.IsParent(it.ID) {
			id = it.ID
			break
		}
	}
	if id == 0 || !b.RunnerEligible(id, states) {
		t.Fatalf("fixture has no eligible item: %d", id)
	}
	drafted := map[int]Derived{}
	for k, v := range states {
		drafted[k] = v
	}
	drafted[id] = Derived{State: StateDraft}
	if b.RunnerEligible(id, drafted) {
		t.Fatal("an item the user moved back to draft is still eligible")
	}
	drafted[id] = Derived{State: StateWaiting}
	if b.RunnerEligible(id, drafted) {
		t.Fatal("an item with an unmet dependency is eligible")
	}
}
