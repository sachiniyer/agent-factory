package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/sachiniyer/agent-factory/backlog"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/planusage"
	"github.com/sachiniyer/agent-factory/session"
)

type backlogTestHost struct{}

func (backlogTestHost) Facts(string) backlog.Facts  { return nil }
func (backlogTestHost) ProjectName(string) string   { return "" }
func (backlogTestHost) Changed(string, []int)       {}
func (backlogTestHost) SlotsUsed(string) int        { return 0 }
func (backlogTestHost) SetAttention(_, _ string, _ int, _ string) {}
func (backlogTestHost) AnswerTarget(_, _ string) backlog.AnswerTarget {
	return backlog.TargetLive
}
func (backlogTestHost) DeliverAnswer(string, backlog.PendingAnswer) {}
func (backlogTestHost) Usage() planusage.Usage {
	at := backlogTestNow.Add(-90 * time.Second)
	return planusage.Usage{Account: planusage.DefaultAccount, ReadingAt: &at,
		Windows: planusage.Windows{SevenDay: planusage.PlanWindow{Plan: 40, Backlog: 10, ResetsAt: &at}}}
}
func backlogRoute[Req, Resp any](call func(Req) (Resp, error)) func([]byte) (any, error) {
	return func(raw []byte) (any, error) {
		r, err := decodeInto[Req](raw)
		if err != nil {
			return nil, err
		}
		return call(r)
	}
}

// stubBacklogDaemon routes the CLI to an in-process backlog service, with a
// JSON round trip as over HTTP. It returns the methods called.
func stubBacklogDaemon(t *testing.T) *[]string {
	t.Helper()
	setupRepoForCmd(t)
	svc := backlog.NewService(backlogTestHost{})
	empty := func(err error) (any, error) { return backlog.Empty{}, err }
	routes := map[string]func([]byte) (any, error){
		"GetBacklog":           backlogRoute(svc.Get),
		"AddBacklogItems":      backlogRoute(svc.Add),
		"UpdateBacklogItem":    backlogRoute(svc.UpdateItem),
		"SetBacklogItemStatus": backlogRoute(svc.SetItemStatus),
		"PostBacklogMessage":   backlogRoute(svc.PostMessage),
		"SetBacklogSettings":   backlogRoute(svc.SetSettings),
		"GetBudget":            backlogRoute(svc.GetBudget),
		"SetBudget":            backlogRoute(svc.SetBudget),
		"MoveBacklogItem": backlogRoute(func(r daemon.MoveBacklogItemRequest) (any, error) {
			return empty(svc.Move(r))
		}),
		"DeleteBacklogItem": backlogRoute(func(r daemon.DeleteBacklogItemRequest) (any, error) {
			return empty(svc.DeleteItem(r))
		}),
	}
	routes["StartBacklogItem"] = backlogRoute(func(r daemon.StartBacklogItemRequest) (any, error) {
		return daemon.StartBacklogItemResponse{Run: backlog.RunRef{State: backlog.RunStarting, StartedBy: backlog.StartedByUser}}, nil
	})
	prevSelf := backlogSelfSession
	backlogSelfSession = func() (*session.InstanceData, error) { return nil, errors.New("not in an af session") }
	var methods []string
	prev := callBacklogRoute
	callBacklogRoute = func(method string, req, resp any) error {
		methods = append(methods, method)
		route, ok := routes[method]
		if !ok {
			return fmt.Errorf("no stub for %s", method)
		}
		raw, err := json.Marshal(req)
		if err != nil {
			return err
		}
		out, err := route(raw)
		if err != nil {
			return err
		}
		b, err := json.Marshal(out)
		if err != nil {
			return err
		}
		return json.Unmarshal(b, resp)
	}
	prevEnv := envelopeOutput
	t.Cleanup(func() { callBacklogRoute = prev; envelopeOutput = prevEnv; backlogSelfSession = prevSelf })
	return &methods
}

// runBacklogCmd runs one af backlog command line and returns stdout.
func runBacklogCmd(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd, rest, err := BacklogCmd.Find(args)
	if err != nil {
		t.Fatal(err)
	}
	resetFlags(cmd)
	if err := cmd.ParseFlags(rest); err != nil {
		t.Fatal(err)
	}
	out, err := runCmdCaptureStdout(t, cmd, cmd.Flags().Args())
	return string(out), err
}

func resetFlags(cmd *cobra.Command) {
	cmd.LocalNonPersistentFlags().VisitAll(func(f *pflag.Flag) {
		_ = f.Value.Set(f.DefValue)
		f.Changed = false
	})
}

func decodeOut[T any](t *testing.T, out string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	return v
}

func TestBacklogAddListShow(t *testing.T) {
	stubBacklogDaemon(t)
	out, err := runBacklogCmd(t, "add", "Backlog store")
	if err != nil {
		t.Fatal(err)
	}
	if it := decodeOut[backlog.Item](t, out); it.ID != 1 || it.Title != "Backlog store" || it.Status != backlog.StatusDraft {
		t.Fatalf("%+v", it)
	}
	if _, err := runBacklogCmd(t, "add", "daemon RPCs", "--parent", "B-1", "--body", "the routes"); err != nil {
		t.Fatal(err)
	}
	if _, err := runBacklogCmd(t, "add", "package", "--parent", "1", "--before", "B-2"); err != nil {
		t.Fatal(err)
	}
	if _, err := runBacklogCmd(t, "add", "Docs", "--waits-on", "2,B-3"); err != nil {
		t.Fatal(err)
	}
	out, err = runBacklogCmd(t, "list")
	if err != nil {
		t.Fatal(err)
	}
	want := "B-1  Backlog store  · Draft · 0 of 2 done\n" +
		"  B-3  package  · Draft\n" +
		"  B-2  daemon RPCs  · Draft\n" +
		"B-4  Docs  · Draft\n"
	if out != want {
		t.Fatalf("list:\n%s\nwant:\n%s", out, want)
	}
	out, err = runBacklogCmd(t, "show", "B-2")
	if err != nil {
		t.Fatal(err)
	}
	e := decodeOut[backlogEntry](t, out)
	if e.Body != "the routes" || e.State != backlog.StateDraft || len(e.NeededBy) != 1 || e.NeededBy[0] != 4 || len(e.Children) != 0 {
		t.Fatalf("%+v", e)
	}
	if _, err := runBacklogCmd(t, "show", "9"); err == nil || err.Error() != `B-9 not found in project repo-a — run "af backlog list --all"` {
		t.Fatal(err)
	}
}

func TestBacklogListFilters(t *testing.T) {
	stubBacklogDaemon(t)
	out, err := runBacklogCmd(t, "list")
	if err != nil || out != "No items yet · Add one with \"af backlog add <title>\".\n" {
		t.Fatalf("%q %v", out, err)
	}
	for _, title := range []string{"a", "b", "c"} {
		if _, err := runBacklogCmd(t, "add", title); err != nil {
			t.Fatal(err)
		}
	}
	for _, args := range [][]string{{"queue", "1"}, {"done", "1"}, {"queue", "2"}} {
		if _, err := runBacklogCmd(t, args...); err != nil {
			t.Fatal(args, err)
		}
	}
	if out, _ := runBacklogCmd(t, "list"); strings.Contains(out, "B-1") {
		t.Fatalf("done item listed:\n%s", out)
	}
	if out, _ := runBacklogCmd(t, "list", "--all"); !strings.Contains(out, "B-1  a  · Done") {
		t.Fatalf("--all:\n%s", out)
	}
	if out, _ := runBacklogCmd(t, "list", "--state", "queued"); out != "B-2  b  · Queued\n" {
		t.Fatalf("--state:\n%s", out)
	}
	envelopeOutput = true
	out, err = runBacklogCmd(t, "list", "--state", "Draft")
	if err != nil || !strings.Contains(out, `"data"`) || !strings.Contains(out, `"title": "c"`) {
		t.Fatalf("--json: %s %v", out, err)
	}
	envelopeOutput = false
	if _, err := runBacklogCmd(t, "list", "--state", "blocked"); err == nil {
		t.Error("unknown state accepted")
	}
}

func TestBacklogEditMoveStatusDelete(t *testing.T) {
	methods := stubBacklogDaemon(t)
	for _, title := range []string{"a", "b"} {
		if _, err := runBacklogCmd(t, "add", title); err != nil {
			t.Fatal(err)
		}
	}
	out, err := runBacklogCmd(t, "edit", "2", "--title", "B", "--waits-on", "1", "--sequential=true")
	if err != nil {
		t.Fatal(err)
	}
	if it := decodeOut[backlog.Item](t, out); it.Title != "B" || len(it.WaitsOn) != 1 || !it.Sequential {
		t.Fatalf("%+v", it)
	}
	if out, err = runBacklogCmd(t, "edit", "2", "--waits-on", ""); err != nil || len(decodeOut[backlog.Item](t, out).WaitsOn) != 0 {
		t.Fatal(out, err)
	}
	if _, err := runBacklogCmd(t, "edit", "2"); err == nil {
		t.Error("edit with no fields accepted")
	}
	if _, err := runBacklogCmd(t, "move", "2", "--top"); err != nil {
		t.Fatal(err)
	}
	if out, _ := runBacklogCmd(t, "list"); out != "B-2  B  · Draft\nB-1  a  · Draft\n" {
		t.Fatalf("after move:\n%s", out)
	}
	if _, err := runBacklogCmd(t, "move", "2", "--top", "--after", "1"); err == nil {
		t.Error("two move targets accepted")
	}
	for _, step := range []struct {
		args []string
		want backlog.Status
	}{
		{[]string{"queue", "1"}, backlog.StatusQueued},
		{[]string{"draft", "1"}, backlog.StatusDraft},
		{[]string{"drop", "1"}, backlog.StatusDropped},
		{[]string{"reopen", "1"}, backlog.StatusDraft},
		{[]string{"done", "B-1"}, backlog.StatusDone},
	} {
		out, err := runBacklogCmd(t, step.args...)
		if err != nil || decodeOut[backlog.Item](t, out).Status != step.want {
			t.Fatalf("%v: %s %v", step.args, out, err)
		}
	}
	if _, err := runBacklogCmd(t, "draft", "1"); err == nil || err.Error() != `B-1 is done; reopen it with "af backlog reopen 1"` {
		t.Fatal(err)
	}
	if _, err := runBacklogCmd(t, "reopen", "2"); err == nil || err.Error() != "B-2 is draft; only a done or dropped item can be reopened" {
		t.Fatal(err)
	}
	if _, err := runBacklogCmd(t, "reopen", "8"); err == nil || !strings.Contains(err.Error(), "B-8 not found in project repo-a") {
		t.Fatal(err)
	}
	if _, err := runBacklogCmd(t, "review", "2"); err == nil || !strings.Contains(err.Error(), "only a running item goes to review") {
		t.Fatal(err)
	}
	if _, err := runBacklogCmd(t, "note", "2", "looks", "good"); err != nil {
		t.Fatal(err)
	}
	if _, err := runBacklogCmd(t, "delete", "2"); err == nil || err.Error() != "B-2 is a draft with runs or a thread; drop it instead" {
		t.Fatal(err)
	}
	if _, err := runBacklogCmd(t, "add", "c"); err != nil {
		t.Fatal(err)
	}
	*methods = nil
	if out, err := runBacklogCmd(t, "delete", "3"); err != nil || !strings.Contains(out, `"deleted": true`) {
		t.Fatal(out, err)
	}
	if strings.Join(*methods, ",") != "DeleteBacklogItem" {
		t.Fatal(*methods)
	}
	// A parent is refused by draft (the daemon would read it as a reopen of its
	// done children, leaving the queued ones eligible for the runner).
	out, err = runBacklogCmd(t, "add", "parent-to-draft")
	if err != nil {
		t.Fatal(out, err)
	}
	parentID := fmt.Sprint(decodeOut[backlog.Item](t, out).ID)
	if out, err := runBacklogCmd(t, "add", "child-of-it", "--parent", parentID); err != nil {
		t.Fatal(out, err)
	}
	if _, err := runBacklogCmd(t, "draft", parentID); err == nil || !strings.Contains(err.Error(), "is a parent; draft its children one by one") {
		t.Fatalf("draft on a parent: %v", err)
	}
}

func TestBacklogAddFile(t *testing.T) {
	stubBacklogDaemon(t)
	path := filepath.Join(t.TempDir(), "items.json")
	batch := `{"proposal": "backlog", "items": [
		{"key": "p", "title": "Backlog store", "sequential": true},
		{"key": "a", "parent": "p", "title": "package"},
		{"key": "b", "parent": "p", "title": "daemon RPCs", "waits_on": ["a"]}]}`
	if err := os.WriteFile(path, []byte(batch), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runBacklogCmd(t, "add", "--file", path)
	if err != nil {
		t.Fatal(err)
	}
	if got := decodeOut[map[string]map[string]int](t, out)["ids"]; got["p"] != 1 || got["a"] != 2 || got["b"] != 3 {
		t.Fatal(out)
	}
	if out, err = runBacklogCmd(t, "add", "--file", path); err != nil || decodeOut[map[string]map[string]int](t, out)["ids"]["b"] != 3 {
		t.Fatalf("re-run: %s %v", out, err)
	}
	if _, err := runBacklogCmd(t, "add", "--file", path, "--parent", "1"); err == nil {
		t.Error("--file with --parent accepted")
	}
	if err := os.WriteFile(path, []byte(`{"items": [{"title": "x", "bogus": 1}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runBacklogCmd(t, "add", "--file", path); err == nil {
		t.Error("unknown field accepted")
	}
	if _, err := runBacklogCmd(t, "add"); err == nil {
		t.Error("add with no title accepted")
	}
}

var backlogTestNow = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func TestBacklogRunnerAndBudget(t *testing.T) {
	stubBacklogDaemon(t)
	prevNow := backlogNow
	backlogNow = func() time.Time { return backlogTestNow }
	t.Cleanup(func() { backlogNow = prevNow })
	out, err := runBacklogCmd(t, "runner")
	if err != nil {
		t.Fatal(err)
	}
	v := decodeOut[backlogRunnerView](t, out)
	if v.State != backlog.RunnerOff || v.Runner || v.MaxRunning != 1 || v.OnDone != backlog.OnDoneKeep || v.Budget.Share != 50 || v.Budget.MaxPerRun != 10 {
		t.Fatalf("%+v", v)
	}
	if out, err = runBacklogCmd(t, "runner", "on", "--max-running", "2", "--on-done", "archive"); err != nil {
		t.Fatal(err)
	}
	if v = decodeOut[backlogRunnerView](t, out); !v.Runner || v.Max != 2 || v.OnDone != "archive" || v.State != backlog.RunnerWaiting {
		t.Fatalf("%+v", v)
	}
	if _, err := runBacklogCmd(t, "runner", "maybe"); err == nil {
		t.Error("bad switch accepted")
	}
	if _, err := runBacklogCmd(t, "runner", "--max-running", "9"); err == nil {
		t.Error("max running 9 accepted")
	}
	for _, step := range []struct {
		args []string
		want func(backlog.Budget) bool
	}{
		{[]string{"budget"}, func(b backlog.Budget) bool { return b.Share == 50 && b.StopAt == 90 && b.USDLimit == 0 }},
		{[]string{"budget", "--share", "30%", "--stop-at", "95"}, func(b backlog.Budget) bool { return b.Share == 30 && b.StopAt == 95 }},
		{[]string{"budget", "--max-per-run", "0"}, func(b backlog.Budget) bool { return b.MaxPerRun == 0 && b.Share == 30 }},
		{[]string{"budget", "--usd-limit", "$12.50"}, func(b backlog.Budget) bool { return b.USDLimit == 12.5 }},
	} {
		out, err := runBacklogCmd(t, step.args...)
		v := decodeOut[backlogBudgetView](t, out)
		if err != nil || !step.want(v.Budget) || v.Windows.SevenDay.Backlog != 10 || v.ReadingAge != "1m30s" {
			t.Fatalf("%v: %s %v", step.args, out, err)
		}
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"budget", "--share", "101"}, "--share must be between 0 and 100"},
		{[]string{"budget", "--stop-at=-1"}, "--stop-at must be between 0 and 100"},
		{[]string{"budget", "--max-per-run", "NaN"}, "--max-per-run must be between 0 and 100"},
		{[]string{"budget", "--share", "half"}, "--share must be between 0 and 100"},
		{[]string{"budget", "--usd-limit", "inf"}, `--usd-limit must be a finite amount of US dollars, 0 or more — for example 5 or 2.50, not "inf"`},
		{[]string{"budget", "--usd-limit", "1e400"}, `--usd-limit must be a finite amount of US dollars, 0 or more — for example 5 or 2.50, not "1e400"`},
		{[]string{"budget", "--usd-limit=-1"}, `--usd-limit must be a finite amount of US dollars, 0 or more — for example 5 or 2.50, not "-1"`},
	} {
		if _, err := runBacklogCmd(t, c.args...); err == nil || err.Error() != c.want {
			t.Errorf("%v: %v", c.args, err)
		}
	}
	if _, err := runBacklogCmd(t, "budget", "--share", "101"); err == nil || err.Error() != "--share must be between 0 and 100" {
		t.Fatal(err)
	}
	if out, _ := runBacklogCmd(t, "runner"); decodeOut[backlogRunnerView](t, out).State != backlog.RunnerIdle {
		t.Fatal(out)
	}
}

func TestRemoteProjectName(t *testing.T) {
	for in, want := range map[string]string{"/srv/proj": "proj", "/srv/proj/": "proj", ".": "", "/": "", "..": "", "a/..": ""} {
		if got := remoteProjectName(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestBacklogRemoteNeedsRepo(t *testing.T) {
	t.Setenv("AF_DAEMON_URL", "http://127.0.0.1:1")
	prev := repoFlag
	repoFlag = ""
	t.Cleanup(func() { repoFlag = prev })
	if _, err := backlogProject(); err == nil || !strings.Contains(err.Error(), "--repo is required with --daemon-url") {
		t.Fatal(err)
	}
	repoFlag = "/srv/proj"
	tgt, err := backlogProject()
	if err != nil || tgt.repoPath != "/srv/proj" || tgt.repoID != "" {
		t.Fatal(tgt, err)
	}
}

// reopen on a parent with one done leaf and one still-open leaf is valid: the
// service reopens the closed leaves only (Codex P2 on #6288).
func TestBacklogReopenAPartiallyOpenParent(t *testing.T) {
	stubBacklogDaemon(t)
	for _, args := range [][]string{{"add", "parent"}, {"add", "done-child", "--parent", "1"}, {"add", "open-child", "--parent", "1"}} {
		if out, err := runBacklogCmd(t, args...); err != nil {
			t.Fatal(args, out, err)
		}
	}
	if out, err := runBacklogCmd(t, "done", "2"); err != nil {
		t.Fatal(out, err)
	}
	if out, err := runBacklogCmd(t, "queue", "3"); err != nil {
		t.Fatal(out, err)
	}
	if out, err := runBacklogCmd(t, "reopen", "1"); err != nil {
		t.Fatalf("a partially open parent was refused: %v %s", err, out)
	}
	if out, err := runBacklogCmd(t, "show", "2"); err != nil || !strings.Contains(out, `"status": "draft"`) {
		t.Fatalf("the done child was not reopened: %s %v", out, err)
	}
	if _, err := runBacklogCmd(t, "reopen", "1"); err == nil || !strings.Contains(err.Error(), "no done or dropped children") {
		t.Fatalf("a parent with nothing closed must be refused: %v", err)
	}
}
