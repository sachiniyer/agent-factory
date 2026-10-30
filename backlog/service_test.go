package backlog

import (
	"math"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/planusage"
)

type testHost struct {
	name    string
	changed [][]int
	lookups int
	slots   int
	facts   Facts
	// attention is each session's mirrored attention reason.
	attention map[string]string
	target    AnswerTarget
	delivered []PendingAnswer
	usage     planusage.Usage
}

func (h *testHost) SetAttention(_, sessionID, reason string) {
	if h.attention == nil {
		h.attention = map[string]string{}
	}
	h.attention[sessionID] = reason
}
func (h *testHost) AnswerTarget(string, string) AnswerTarget { return h.target }
func (h *testHost) DeliverAnswer(_ string, a PendingAnswer) {
	h.delivered = append(h.delivered, a)
}

func (h *testHost) Facts(string) Facts          { return h.facts }
func (h *testHost) ProjectName(string) string   { h.lookups++; return h.name }
func (h *testHost) SlotsUsed(string) int        { return h.slots }
func (h *testHost) Usage() planusage.Usage      { return h.usage }
func (h *testHost) Changed(_ string, ids []int) { h.changed = append(h.changed, ids) }
func (h *testHost) last() []int                 { return h.changed[len(h.changed)-1] }
func ptr[T any](v T) *T                         { return &v }
func (h *testHost) reset()                      { h.changed = nil }
func (h *testHost) Gate(string) Gate {
	b, err := LoadBudget()
	if err != nil {
		return Gate{Name: "budget", Reason: err.Error()}
	}
	return CheckGates(GateInput{Budget: *b, Usage: h.usage, Now: time.Now(), Loc: time.UTC})
}

func newTestService(t *testing.T) (*Service, *testHost) {
	t.Helper()
	home(t)
	h := &testHost{name: "agent-factory"}
	s := NewService(h)
	s.now = func() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC) }
	return s, h
}

func mustAdd(t *testing.T, s *Service, items ...ItemInput) map[string]int {
	t.Helper()
	out, err := s.Add(AddRequest{RepoID: repoID, Items: items})
	if err != nil {
		t.Fatal(err)
	}
	return out.IDs
}

func TestServiceResolve(t *testing.T) {
	s, _ := newTestService(t)
	for _, req := range []GetRequest{{}, {RepoID: repoID, RepoPath: "/x"}} {
		if _, err := s.Get(req); err == nil || !strings.Contains(err.Error(), "exactly one of repo_id or repo_path") {
			t.Errorf("%+v: %v", req, err)
		}
	}
	if _, err := s.Get(GetRequest{RepoID: "../x"}); err == nil {
		t.Error("bad repo id accepted")
	}
}

func TestServiceRepoPath(t *testing.T) {
	s, h := newTestService(t)
	h.name = ""
	dir := filepath.Join(t.TempDir(), "myproj")
	if out, err := exec.Command("git", "init", dir).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	if _, err := s.Add(AddRequest{RepoPath: dir, Items: []ItemInput{{Key: "a", Title: "A"}}}); err != nil {
		t.Fatal(err)
	}
	_, err := s.UpdateItem(UpdateItemRequest{RepoPath: dir, ID: 9, Title: ptr("x")})
	if err == nil || err.Error() != `B-9 not found in project myproj — run "af backlog list --all"` {
		t.Fatal(err)
	}
}

func TestServiceGetEmpty(t *testing.T) {
	s, _ := newTestService(t)
	v, err := s.Get(GetRequest{RepoID: repoID})
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Backlog.Items) != 0 || len(v.States) != 0 {
		t.Fatalf("%+v", v)
	}
	want := RunnerView{State: RunnerOff, Reason: "Items run only when you start them.", Max: 1}
	if v.Runner != want {
		t.Fatalf("runner %+v", v.Runner)
	}
}

func TestServiceAdd(t *testing.T) {
	s, h := newTestService(t)
	ids := mustAdd(t, s, ItemInput{Key: "p", Title: "Parent", Sequential: true},
		ItemInput{Key: "a", Title: "A", Parent: Ref{Key: "p"}},
		ItemInput{Key: "b", Title: "B", Parent: Ref{Key: "p"}, WaitsOn: []Ref{{Key: "a"}}})
	if !reflect.DeepEqual(ids, map[string]int{"p": 1, "a": 2, "b": 3}) {
		t.Fatal(ids)
	}
	if !reflect.DeepEqual(h.last(), []int{1, 2, 3}) {
		t.Fatal(h.changed)
	}
	out, err := s.Add(AddRequest{RepoID: repoID, Items: []ItemInput{{Key: "item", Title: "First", Parent: Ref{ID: 1}}}, Before: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Items) != 1 || out.Items[0].ID != 4 || out.Items[0].Status != StatusDraft {
		t.Fatalf("%+v", out)
	}
	v, _ := s.Get(GetRequest{RepoID: repoID})
	if got := v.Backlog.Children(1); !reflect.DeepEqual(got, []int{4, 2, 3}) {
		t.Fatal(got)
	}
	if v.States[3].State != StateDraft {
		t.Fatal(v.States[3])
	}

	if _, err := s.Add(AddRequest{RepoID: repoID, Items: []ItemInput{{Title: "a"}, {Title: "b"}}, After: 1}); err == nil {
		t.Error("before/after with a batch accepted")
	}
	if _, err := s.Add(AddRequest{RepoID: repoID, Items: []ItemInput{{Title: "a", WaitsOn: []Ref{{Key: "x"}}}}, After: 1}); err == nil {
		t.Error("key ref with after accepted")
	}
	if _, err := s.Add(AddRequest{RepoID: repoID, Items: []ItemInput{{Title: "x", WaitsOn: []Ref{{ID: 99}}}}}); err == nil ||
		!strings.Contains(err.Error(), "in project agent-factory") {
		t.Errorf("not found without project: %v", err)
	}
}

func TestServiceUpdateMoveDelete(t *testing.T) {
	s, h := newTestService(t)
	mustAdd(t, s, ItemInput{Key: "a", Title: "A"}, ItemInput{Key: "b", Title: "B"}, ItemInput{Key: "c", Title: "C"})
	out, err := s.UpdateItem(UpdateItemRequest{RepoID: repoID, ID: 2, Title: ptr("B2"), WaitsOn: ptr([]int{1})})
	if err != nil || out.Item.Title != "B2" || !reflect.DeepEqual(out.Item.WaitsOn, []int{1}) {
		t.Fatal(out, err)
	}
	if _, err := s.UpdateItem(UpdateItemRequest{RepoID: repoID, ID: 1, WaitsOn: ptr([]int{2})}); err == nil ||
		!strings.Contains(err.Error(), "cannot wait on") {
		t.Fatal(err)
	}
	if err := s.Move(MoveRequest{RepoID: repoID, ID: 3, Top: true}); err != nil {
		t.Fatal(err)
	}
	if err := s.Move(MoveRequest{RepoID: repoID, ID: 2, Into: 3}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(h.last(), []int{2, 3}) {
		t.Fatalf("move into: %v", h.changed)
	}
	v, _ := s.Get(GetRequest{RepoID: repoID})
	var order []int
	for _, it := range v.Backlog.Items {
		order = append(order, it.ID)
	}
	if !reflect.DeepEqual(order, []int{3, 2, 1}) || v.Backlog.Item(2).Parent != 3 {
		t.Fatal(order)
	}
	if err := s.DeleteItem(DeleteItemRequest{RepoID: repoID, ID: 3}); err == nil || !strings.Contains(err.Error(), "has children") {
		t.Fatal(err)
	}
	h.reset()
	if err := s.DeleteItem(DeleteItemRequest{RepoID: repoID, ID: 1}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(h.changed, [][]int{{1, 2}}) {
		t.Fatal(h.changed)
	}
	v, _ = s.Get(GetRequest{RepoID: repoID})
	if v.Backlog.Item(1) != nil || len(v.Backlog.Item(2).WaitsOn) != 0 {
		t.Fatalf("%+v", v.Backlog.Items)
	}
}

func TestServiceDeleteRefusals(t *testing.T) {
	s, _ := newTestService(t)
	mustAdd(t, s, ItemInput{Key: "a", Title: "A"}, ItemInput{Key: "b", Title: "B"})
	if _, err := s.SetItemStatus(SetItemStatusRequest{RepoID: repoID, ID: 1, Status: StatusQueued}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteItem(DeleteItemRequest{RepoID: repoID, ID: 1}); err == nil || err.Error() != "B-1 is queued; only a draft can be deleted — drop it instead" {
		t.Fatal(err)
	}
	if _, err := s.PostMessage(PostMessageRequest{RepoID: repoID, ID: 2, Kind: KindNote, Text: "hi"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteItem(DeleteItemRequest{RepoID: repoID, ID: 2}); err == nil || err.Error() != "B-2 is a draft with runs or a thread; drop it instead" {
		t.Fatal(err)
	}
	if err := s.DeleteItem(DeleteItemRequest{RepoID: repoID, ID: 7}); err == nil || err.Error() != `B-7 not found in project agent-factory — run "af backlog list --all"` {
		t.Fatal(err)
	}
}

func TestServiceStatus(t *testing.T) {
	s, h := newTestService(t)
	mustAdd(t, s, ItemInput{Key: "p", Title: "P"}, ItemInput{Key: "a", Title: "A", Parent: Ref{Key: "p"}},
		ItemInput{Key: "b", Title: "B", Parent: Ref{Key: "p"}})
	out, err := s.SetItemStatus(SetItemStatusRequest{RepoID: repoID, ID: 1, Status: StatusQueued})
	if err != nil {
		t.Fatal(err)
	}
	if out.Item.ID != 1 || !reflect.DeepEqual(h.last(), []int{2, 3}) {
		t.Fatal(out, h.changed)
	}
	if out, err = s.SetItemStatus(SetItemStatusRequest{RepoID: repoID, ID: 2, Status: StatusDone}); err != nil || out.Item.Status != StatusDone {
		t.Fatal(out, err)
	}
	if out, err = s.SetItemStatus(SetItemStatusRequest{RepoID: repoID, ID: 2, Status: StatusDraft}); err != nil || out.Item.Status != StatusDraft {
		t.Fatal(out, err)
	}
	if _, err := s.SetItemStatus(SetItemStatusRequest{RepoID: repoID, ID: 3, Status: StatusReview}); err == nil {
		t.Error("queued to review accepted")
	}
	if _, err := s.SetItemStatus(SetItemStatusRequest{RepoID: repoID, ID: 3, Status: StatusDone, PR: "u"}); err == nil {
		t.Error("pr without session accepted")
	}
	if _, err := s.SetItemStatus(SetItemStatusRequest{RepoID: repoID, ID: 3, Status: StatusReview, SessionID: "s1"}); err == nil ||
		!strings.Contains(err.Error(), "not the latest run of B-3") {
		t.Error(err)
	}
}

func TestServiceAgentReview(t *testing.T) {
	s, _ := newTestService(t)
	mustAdd(t, s, ItemInput{Key: "a", Title: "A"})
	if _, err := Update(repoID, func(b *Backlog) error {
		it := b.Item(1)
		it.Status = StatusRunning
		it.Runs = []RunRef{{SessionID: "s0", State: RunStarted}, {SessionID: "s1", State: RunStarted}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetItemStatus(SetItemStatusRequest{RepoID: repoID, ID: 1, Status: StatusReview, SessionID: "s0"}); err == nil ||
		!strings.Contains(err.Error(), "not the latest run") {
		t.Errorf("an older run's session sent the item to review: %v", err)
	}
	if _, err := s.SetItemStatus(SetItemStatusRequest{RepoID: repoID, ID: 1, Status: StatusDone, SessionID: "s1"}); err == nil {
		t.Error("agent done accepted")
	}
	out, err := s.SetItemStatus(SetItemStatusRequest{RepoID: repoID, ID: 1, Status: StatusReview, SessionID: "s1", PR: "https://x/pull/1"})
	if err != nil || out.Item.Status != StatusReview || out.Item.PR != "https://x/pull/1" {
		t.Fatal(out, err)
	}
	msg, err := s.PostMessage(PostMessageRequest{RepoID: repoID, ID: 1, Kind: KindNote, Text: "x", SessionID: "s1"})
	if err != nil || msg.Message.Author != "agent:s1" {
		t.Fatal(msg, err)
	}
}

func TestServiceNotes(t *testing.T) {
	s, _ := newTestService(t)
	mustAdd(t, s, ItemInput{Key: "a", Title: "A"})
	for i, text := range []string{"one", " two "} {
		out, err := s.PostMessage(PostMessageRequest{RepoID: repoID, ID: 1, Kind: KindNote, Text: text})
		if err != nil || out.Message.ID != i+1 || out.Message.Author != AuthorUser || out.Message.Text != strings.TrimSpace(text) {
			t.Fatal(out, err)
		}
	}
	for _, req := range []PostMessageRequest{
		{RepoID: repoID, ID: 1, Kind: KindQuestion, Text: "q"},
		{RepoID: repoID, ID: 1, Kind: KindNote, Text: "  "},
		{RepoID: repoID, ID: 1, Kind: KindNote, Text: "x", SessionID: "nope"},
	} {
		if _, err := s.PostMessage(req); err == nil {
			t.Errorf("%+v accepted", req)
		}
	}
}

func TestServiceSettings(t *testing.T) {
	s, _ := newTestService(t)
	out, err := s.SetSettings(SetSettingsRequest{RepoID: repoID, Runner: ptr(true), MaxRunning: ptr(3), OnDone: ptr(OnDoneArchive)})
	if err != nil || out.Settings != (Settings{Runner: true, MaxRunning: 3, OnDone: OnDoneArchive}) {
		t.Fatal(out, err)
	}
	for _, req := range []SetSettingsRequest{
		{RepoID: repoID, MaxRunning: ptr(0)}, {RepoID: repoID, MaxRunning: ptr(9)}, {RepoID: repoID, OnDone: ptr("kill")},
	} {
		if _, err := s.SetSettings(req); err == nil {
			t.Errorf("%+v accepted", req)
		}
	}
	v, _ := s.Get(GetRequest{RepoID: repoID})
	if v.Runner.State != RunnerWaiting || v.Runner.Gate != GateNoReading || v.Runner.Max != 3 {
		t.Fatalf("%+v", v.Runner)
	}
}

func TestServiceMoveOutReportsOldParent(t *testing.T) {
	s, h := newTestService(t)
	mustAdd(t, s, ItemInput{Key: "p", Title: "P"}, ItemInput{Key: "a", Title: "A", Parent: Ref{Key: "p"}},
		ItemInput{Key: "b", Title: "B", Parent: Ref{Key: "p"}})
	if err := s.Move(MoveRequest{RepoID: repoID, ID: 3, After: 1}); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(h.last(), []int{3, 1}) {
		t.Fatal(h.changed)
	}
}

func TestServiceBatchReportsKeylessItems(t *testing.T) {
	s, h := newTestService(t)
	mustAdd(t, s, ItemInput{Key: "x", Title: "X"})
	out, err := s.Add(AddRequest{RepoID: repoID, Items: []ItemInput{{Title: "A"}, {Key: "b", Title: "B"}, {Title: "C", Parent: Ref{ID: 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, it := range out.Items {
		got = append(got, it.ID)
	}
	if !reflect.DeepEqual(got, []int{4, 2, 3}) || !reflect.DeepEqual(h.last(), []int{4, 2, 3}) {
		t.Fatalf("items %v, event %v", got, h.changed)
	}
	if !reflect.DeepEqual(out.IDs, map[string]int{"b": 3}) {
		t.Fatal(out.IDs)
	}
}

func TestServiceProposalBatchReportsUpdates(t *testing.T) {
	s, h := newTestService(t)
	batch := AddRequest{RepoID: repoID, Proposal: "p", Items: []ItemInput{{Key: "a", Title: "A"}, {Key: "b", Title: "B"}}}
	if _, err := s.Add(batch); err != nil {
		t.Fatal(err)
	}
	mustAdd(t, s, ItemInput{Key: "z", Title: "other"})
	batch.Items = append(batch.Items, ItemInput{Key: "c", Title: "C"})
	out, err := s.Add(batch)
	if err != nil || len(out.Items) != 3 || !reflect.DeepEqual(h.last(), []int{1, 2, 4}) {
		t.Fatal(out, err, h.changed)
	}
}

func TestServiceLooksUpNameOnlyForNotFound(t *testing.T) {
	s, h := newTestService(t)
	mustAdd(t, s, ItemInput{Key: "a", Title: "A"})
	if _, err := s.Get(GetRequest{RepoID: repoID}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetItemStatus(SetItemStatusRequest{RepoID: repoID, ID: 1, Status: StatusQueued}); err != nil {
		t.Fatal(err)
	}
	if h.lookups != 0 {
		t.Fatalf("%d name lookups on success", h.lookups)
	}
	if err := s.DeleteItem(DeleteItemRequest{RepoID: repoID, ID: 5}); err == nil || h.lookups != 1 {
		t.Fatal(err, h.lookups)
	}
}

func TestServiceBudget(t *testing.T) {
	s, h := newTestService(t)
	reset := time.Date(2026, 10, 20, 14, 0, 0, 0, time.UTC)
	h.usage = planusage.Usage{Account: "default", Windows: planusage.Windows{FiveHour: planusage.PlanWindow{Plan: 30, Backlog: 12, ResetsAt: &reset}}}
	b, err := s.GetBudget(GetBudgetRequest{})
	want := Budget{SchemaVersion: BudgetSchemaVersion, Share: 50, StopAt: 90, MaxPerRun: 10}
	if err != nil || b.Budget != want || b.Windows.FiveHour.Backlog != 12 || b.Account != "default" {
		t.Fatal(b, err)
	}
	out, err := s.SetBudget(SetBudgetRequest{Share: ptr(30.0), StopAt: ptr(100.0)})
	want.Share, want.StopAt = 30, 100
	if err != nil || out.Budget != want || out.Windows.FiveHour.Plan != 30 {
		t.Fatal(out, err)
	}
	out, err = s.SetBudget(SetBudgetRequest{MaxPerRun: ptr(0.0), USDLimit: ptr(10.0)})
	want.MaxPerRun, want.USDLimit = 0, 10
	if err != nil || out.Budget != want {
		t.Fatal(out, err)
	}
	for _, c := range []struct {
		req  SetBudgetRequest
		want string
	}{
		{SetBudgetRequest{}, "nothing to change — use --share, --stop-at, --max-per-run or --usd-limit"},
		{SetBudgetRequest{Share: ptr(101.0)}, "--share must be between 0 and 100"},
		{SetBudgetRequest{StopAt: ptr(-1.0)}, "--stop-at must be between 0 and 100"},
		{SetBudgetRequest{MaxPerRun: ptr(math.NaN())}, "--max-per-run must be between 0 and 100"},
		{SetBudgetRequest{Share: ptr(math.Inf(1))}, "--share must be between 0 and 100"},
		{SetBudgetRequest{USDLimit: ptr(-1.0)}, "--usd-limit must be a finite amount of US dollars, 0 or more"},
		{SetBudgetRequest{USDLimit: ptr(math.Inf(1))}, "--usd-limit must be a finite amount of US dollars, 0 or more"},
		{SetBudgetRequest{Share: ptr(10.0), USDLimit: ptr(math.NaN())}, "--usd-limit must be a finite amount of US dollars, 0 or more"},
	} {
		if _, err := s.SetBudget(c.req); err == nil || err.Error() != c.want {
			t.Errorf("%+v: %v", c.req, err)
		}
	}
	if got, _ := s.GetBudget(GetBudgetRequest{}); got.Budget != want {
		t.Fatalf("a refused change was saved: %+v", got.Budget)
	}
}

func TestServiceRunnerView(t *testing.T) {
	s, h := newTestService(t)
	if _, err := s.SetSettings(SetSettingsRequest{RepoID: repoID, Runner: ptr(true)}); err != nil {
		t.Fatal(err)
	}
	reset := time.Now().Add(2 * time.Hour)
	reading := planusage.Usage{Windows: planusage.Windows{FiveHour: planusage.PlanWindow{Plan: 20, Backlog: 5, ResetsAt: &reset}}}
	steps := []struct {
		edit  func(*Budget)
		usage planusage.Usage
		state string
		gate  string
	}{
		{func(*Budget) {}, planusage.Usage{}, RunnerWaiting, GateNoReading},
		{func(*Budget) {}, planusage.Usage{Metering: true}, RunnerWaiting, GateMetering},
		{func(b *Budget) { b.Share = 0 }, reading, RunnerStopped, GateShare},
		{func(b *Budget) { b.Share = 50 }, reading, RunnerIdle, ""},
		{func(b *Budget) { b.USDLimit = 10 }, planusage.Usage{}, RunnerIdle, ""},
	}
	for i, st := range steps {
		if _, err := UpdateBudget(func(b *Budget) error { st.edit(b); return nil }); err != nil {
			t.Fatal(err)
		}
		h.usage = st.usage
		v, err := s.Get(GetRequest{RepoID: repoID})
		if err != nil || v.Runner.State != st.state || v.Runner.Gate != st.gate {
			t.Fatalf("step %d: %+v %v", i, v.Runner, err)
		}
	}
	if _, err := s.SetSettings(SetSettingsRequest{RepoID: repoID, Runner: ptr(false)}); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.Get(GetRequest{RepoID: repoID}); v.Runner.State != RunnerOff {
		t.Fatalf("%+v", v.Runner)
	}
}

// The backlog RPCs resolve repo_path like the other daemon routes: a relative
// path is refused instead of resolved against the daemon's working directory
// (Codex P2 on #6295).
func TestResolveRefusesARelativeRepoPath(t *testing.T) {
	s := NewService(&testHost{})
	for _, rel := range []string{".", "repo", "../x"} {
		if _, err := s.Resolve("", rel); err == nil || !strings.Contains(err.Error(), "must be absolute") {
			t.Errorf("Resolve(%q) = %v, want a refusal", rel, err)
		}
	}
}
