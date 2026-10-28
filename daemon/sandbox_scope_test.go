package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// W3.5 (#5571): the sandbox credential admits a local agent's af surface, bounded
// to its project. These tests drive REAL credentialed requests through the real
// mux and gate, because the binding is a chain (registry → gate → context →
// handler) and calling a handler with a hand-built context would prove the filter
// and none of the wiring.

// The scope table, pinned exactly. A route joins it only by a deliberate edit
// here AND an owner constraint in sandboxConstrainedRoutes.
func TestSandboxScopeTableIsPinned(t *testing.T) {
	want := []string{
		"/v1/AddTask", "/v1/ArchiveSession", "/v1/CloseTab", "/v1/CreateSession", "/v1/CreateTab",
		"/v1/ListTasks", "/v1/RemoveTask", "/v1/RenameTab", "/v1/ReorderTab", "/v1/SendPrompt", "/v1/Snapshot",
	}
	var got []string
	for path := range sandboxAllowedPaths {
		got = append(got, path)
	}
	sort.Strings(got)
	assert.Equal(t, want, got, "the sandbox scope changed: every entry needs its own handler-side owner constraint")

	for _, path := range []string{
		// Watch tasks run a script on the daemon host; a patch can turn a delivery
		// task into one that spawns locally; a trigger fires one now.
		"/v1/UpdateTask", "/v1/TriggerTask", "/v1/RestartTask", "/v1/ListOnComplete",
		// Accounts, tokens, config: other identities and operator state.
		"/v1/ListAccounts", "/v1/RegisterAccount", "/v1/AccountLogin", "/v1/QuotaReport",
		"/v1/GetConfig", "/v1/ExplainConfig", "/v1/SetConfigValue", "/v1/UnsetConfigValue",
		// Projects and paths: the operator's layout.
		"/v1/ListProjects", "/v1/ListDirectory", "/v1/RegisterProject", "/v1/DeleteProject",
		"/v1/ListBackends", "/v1/ListPrograms", "/v1/SuggestSessionName",
		// Lifecycle and delivery that reaches any agent on the machine.
		"/v1/DeliverPrompt", "/v1/KillSession", "/v1/RestoreSession", "/v1/RestoreArchived",
		"/v1/HandoffSession", "/v1/ResumeFromLimit", "/v1/PruneSessions",
		// Proposals, previews, daemon lifecycle.
		"/v1/StartProposal", "/v1/Preview", "/v1/PauseStatusPoll", "/v1/health",
	} {
		assert.Falsef(t, sandboxAllowedPath(path), "%s must never be reachable with a sandbox credential", path)
	}
}

type scopeFixture struct {
	mgr    *Manager
	repoA  string // the credential owner's project
	repoB  string // another project
	me     *session.Instance
	peer   *session.Instance // same project, off-box
	local  *session.Instance // same project, runs on the daemon host
	other  *session.Instance // another project, off-box
	secret string
	gated  http.Handler
	// backends is the persisted backend type per session title (the seam).
	backends map[string]string
	rows     func(insts ...*session.Instance) []byte
}

func newScopeFixture(t *testing.T) *scopeFixture {
	t.Helper()
	mgr, repoA, repoPath := newStatusTestManager(t)
	const repoB = "zz-other-project-0001"

	backends := map[string]string{}
	prev := instanceBackendType
	instanceBackendType = func(i *session.Instance) string { return backends[i.Title] }
	t.Cleanup(func() { instanceBackendType = prev })

	f := &scopeFixture{mgr: mgr, repoA: repoA, repoB: repoB, backends: backends}
	mk := func(repoID, title, backend string) *session.Instance {
		inst, err := session.NewInstance(session.InstanceOptions{Title: title, Path: repoPath, Program: "claude"})
		require.NoError(t, err)
		inst.SetStartedForTest(true)
		inst.SetStatusForTest(session.Ready)
		backends[title] = backend
		mgr.mu.Lock()
		mgr.instances[daemonInstanceKey(repoID, title)] = inst
		mgr.mu.Unlock()
		return inst
	}
	f.me = mk(repoA, "me", "ssh")
	f.peer = mk(repoA, "peer", "sandbox")
	f.local = mk(repoA, "local-one", "local")
	f.other = mk(repoB, "foreign", "ssh")

	rows := func(insts ...*session.Instance) []byte {
		var data []session.InstanceData
		for _, i := range insts {
			data = append(data, session.InstanceData{ID: i.ID, Title: i.Title, Path: repoPath, Status: session.Running})
		}
		raw, err := json.Marshal(data)
		require.NoError(t, err)
		return raw
	}
	f.rows = rows
	require.NoError(t, config.LoadState().SaveInstances(repoA, rows(f.me, f.peer, f.local)))
	require.NoError(t, config.LoadState().SaveInstances(repoB, rows(f.other)))

	secret, err := mgr.sandboxTokens.mint(f.me.ID)
	require.NoError(t, err)
	f.secret = secret
	mux := newHTTPMux(&controlServer{manager: mgr})
	f.gated = withAuth(mux, &authGate{
		expectedToken: func() (string, error) { return "operator-token", nil },
		sandboxTokens: &mgr.sandboxTokens,
	}, nil)
	return f
}

func (f *scopeFixture) call(t *testing.T, token, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.gated.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

const scopeRefusal = "not permitted for a sandbox credential"

func (f *scopeFixture) assertRefused(t *testing.T, path, body, why string) {
	t.Helper()
	code, out := f.call(t, f.secret, path, body)
	assert.Containsf(t, out, scopeRefusal, "%s %s: %s (status %d)", path, body, why, code)
}

func titlesOf(insts []session.InstanceData) []string {
	var out []string
	for _, d := range insts {
		out = append(out, d.Title)
	}
	sort.Strings(out)
	return out
}

func TestSandboxScope_SnapshotIsTheOwnersProjectOnly(t *testing.T) {
	f := newScopeFixture(t)
	snap := func(token string) SnapshotResponse {
		code, out := f.call(t, token, "/v1/Snapshot", `{}`)
		require.Equal(t, http.StatusOK, code, out)
		var env struct {
			Data SnapshotResponse `json:"data"`
		}
		require.NoError(t, json.Unmarshal([]byte(out), &env))
		return env.Data
	}
	// Anti-vacuous: the operator sees every project.
	assert.Equal(t, []string{"foreign", "local-one", "me", "peer"}, titlesOf(snap("operator-token").Instances))

	got := snap(f.secret)
	assert.Equal(t, []string{"local-one", "me", "peer"}, titlesOf(got.Instances), "the project's sessions, and no other project's")
	assert.Equal(t, f.me.ID, got.Caller)
	for _, d := range got.Instances {
		assert.Empty(t, d.Path, "the projection must still withhold the daemon host's layout")
	}

	// Naming another project's repo id changes nothing: the filter is the owner's.
	code, out := f.call(t, f.secret, "/v1/Snapshot", `{"repo_id":"`+f.repoB+`"}`)
	require.Equal(t, http.StatusOK, code, out)
	var env struct {
		Data SnapshotResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &env))
	assert.NotContains(t, titlesOf(env.Data.Instances), "foreign")
}

// authorize runs the same resolution every session verb runs before it delegates.
func (f *scopeFixture) authorize(owner *session.Instance, id, title string, selfOnly bool) error {
	_, _, err := f.mgr.authorizeSandboxSession(owner.ID, id, title, selfOnly)
	return err
}

func (f *scopeFixture) assertAuthorizes(t *testing.T, id, title string, selfOnly bool, why string) {
	t.Helper()
	assert.NoErrorf(t, f.authorize(f.me, id, title, selfOnly), "%s (id=%q title=%q)", why, id, title)
}

func (f *scopeFixture) assertDenies(t *testing.T, id, title string, selfOnly bool, why string) {
	t.Helper()
	err := f.authorize(f.me, id, title, selfOnly)
	require.Errorf(t, err, "%s (id=%q title=%q)", why, id, title)
}

// SendPrompt and ArchiveSession share one resolution: an off-box session of the
// credential's project. The mutating handlers are not executed here — the
// authorization is what changed, and a refused request never reaches them.
func TestSandboxScope_SessionVerbsStayInTheProject(t *testing.T) {
	f := newScopeFixture(t)
	f.assertAuthorizes(t, "", "peer", false, "an off-box session of the project, by title")
	f.assertAuthorizes(t, f.peer.ID, "", false, "by id")
	f.assertAuthorizes(t, f.me.ID, "", false, "itself")
	f.assertAuthorizes(t, "", "", false, "naming nothing means itself, never any")

	f.assertDenies(t, f.other.ID, "", false, "another project, by id")
	f.assertDenies(t, "", "foreign", false, "another project, by title: the repo is the owner's whatever is named")
	f.assertDenies(t, f.local.ID, "", false, "a session that runs on the daemon host, by id")
	f.assertDenies(t, "", "local-one", false, "a session that runs on the daemon host, by title")
	f.assertDenies(t, "no-such-id", "", false, "an unknown id never falls back to a title match")

	// Through the real mux, a refused request answers with the scope's refusal.
	f.assertRefused(t, "/v1/SendPrompt", `{"prompt":"hi","id":"`+f.other.ID+`"}`, "another project, by id")
	f.assertRefused(t, "/v1/SendPrompt", `{"prompt":"hi","title":"foreign","repo_id":"`+f.repoB+`"}`, "another project, by title and repo")
	f.assertRefused(t, "/v1/SendPrompt", `{"prompt":"hi","title":"local-one"}`, "runs on the daemon host")
	f.assertRefused(t, "/v1/ArchiveSession", `{"id":"`+f.other.ID+`"}`, "another project")
	f.assertRefused(t, "/v1/ArchiveSession", `{"title":"local-one"}`, "runs on the daemon host")
}

func TestSandboxScope_TabVerbsActOnTheOwnSessionOnly(t *testing.T) {
	f := newScopeFixture(t)
	f.assertAuthorizes(t, f.me.ID, "", true, "the own session")
	f.assertDenies(t, f.peer.ID, "", true, "a peer in the SAME project is another session: tabs are self-only")
	f.assertDenies(t, f.other.ID, "", true, "another project")
	f.assertDenies(t, "", "peer", true, "a peer, by title")

	assert.NoError(t, checkSandboxTabKind(CreateTabRequest{Command: "npm run dev", Name: "dev"}), "a process tab")
	assert.NoError(t, checkSandboxTabKind(CreateTabRequest{Shell: true}), "a shell tab")
	assert.NoError(t, checkSandboxTabKind(CreateTabRequest{Kind: "shell"}))
	for name, req := range map[string]CreateTabRequest{
		"web by port":  {Kind: "web", Port: 3000},
		"web by url":   {Kind: "web", URL: "http://x"},
		"vscode":       {Kind: "vscode"},
		"docs":         {Kind: "docs", Proposal: "x"},
		"smuggled url": {Command: "x", URL: "http://x"},
	} {
		assert.Errorf(t, checkSandboxTabKind(req), "%s is served by the daemon host", name)
	}

	for _, route := range []string{"/v1/CreateTab", "/v1/CloseTab", "/v1/RenameTab", "/v1/ReorderTab"} {
		body := `{"id":"` + f.peer.ID + `","command":"x","tab_name":"t","new_name":"n"}`
		f.assertRefused(t, route, body, "a peer's tabs")
		f.assertRefused(t, route, `{"id":"`+f.other.ID+`","command":"x","tab_name":"t","new_name":"n"}`, "another project's tabs")
	}
	f.assertRefused(t, "/v1/CreateTab", `{"id":"`+f.me.ID+`","kind":"web","port":3000}`, "web tabs are proxied by the daemon host")
	f.assertRefused(t, "/v1/CreateTab", `{"id":"`+f.me.ID+`","kind":"vscode"}`, "VS Code tabs run on the daemon host")
}

func TestSandboxScope_CreateSessionNeedsAListedHostAndTheDefaultAccount(t *testing.T) {
	f := newScopeFixture(t)
	f.assertRefused(t, "/v1/CreateSession", `{"title":"x","prompt":"p"}`, "no host: would start on the daemon box")
	f.assertRefused(t, "/v1/CreateSession", `{"title":"x","backend":"local","host":"h1"}`, "backend local")
	f.assertRefused(t, "/v1/CreateSession", `{"title":"x","backend":"docker","host":"h1"}`, "backend docker")
	f.assertRefused(t, "/v1/CreateSession", `{"title":"x","host":"h1","in_place":true}`, "--here attaches the daemon's tree")
	f.assertRefused(t, "/v1/CreateSession", `{"title":"x","host":"h1","account":"someone-else"}`, "another account")
}

func TestCheckSandboxCreate(t *testing.T) {
	assert.NoError(t, checkSandboxCreate(CreateSessionRequest{Host: "h1"}))
	assert.NoError(t, checkSandboxCreate(CreateSessionRequest{Host: "h1", Backend: "ssh"}))
	for name, req := range map[string]CreateSessionRequest{
		"no host":       {},
		"blank host":    {Host: "  "},
		"local":         {Host: "h", Backend: "local"},
		"hook":          {Host: "h", Backend: "hook"},
		"in place":      {Host: "h", InPlace: true},
		"account":       {Host: "h", Account: "a"},
		"blank account": {Host: "h", Account: " a "},
	} {
		assert.Errorf(t, checkSandboxCreate(req), "%s must be refused", name)
	}
}

func TestSandboxScope_TasksDeliverPromptsInsideTheProject(t *testing.T) {
	f := newScopeFixture(t)
	addTask := func(taskJSON string) string { return `{"task":` + taskJSON + `}` }

	assert.NoError(t, checkSandboxTaskShape(task.Task{CronExpr: "0 * * * *", TargetSession: "peer"}))
	for name, tk := range map[string]task.Task{
		"watch task":              {WatchCmd: "./watch.sh", TargetSession: "peer"},
		"watch alongside cron":    {WatchCmd: "./w.sh", CronExpr: "* * * * *", TargetSession: "peer"},
		"neither cron nor watch":  {TargetSession: "peer"},
		"no target (would spawn)": {CronExpr: "* * * * *"},
	} {
		assert.Errorf(t, checkSandboxTaskShape(tk), "%s must be refused", name)
	}

	f.assertRefused(t, "/v1/AddTask", addTask(`{"prompt":"p","watch_cmd":"./watch.sh","target_session":"peer"}`), "watch tasks run a script on the daemon host")
	f.assertRefused(t, "/v1/AddTask", addTask(`{"prompt":"p","cron_expr":"* * * * *"}`), "no target: would spawn on the daemon host")
	f.assertRefused(t, "/v1/AddTask", addTask(`{"prompt":"p","cron_expr":"* * * * *","target_session":"foreign"}`), "another project's session")
	f.assertRefused(t, "/v1/AddTask", addTask(`{"prompt":"p","cron_expr":"* * * * *","target_session":"local-one"}`), "a session on the daemon host")

	// ListTasks / RemoveTask see only the owner's project, never a watch task.
	path := f.me.Path
	mine := task.Task{ID: "t-mine", Prompt: "p", CronExpr: "0 * * * *", TargetSession: "peer", ProjectPath: path, RepoID: f.repoA}
	foreign := task.Task{ID: "t-foreign", Prompt: "p", CronExpr: "0 * * * *", TargetSession: "foreign", ProjectPath: path, RepoID: f.repoB}
	watch := task.Task{ID: "t-watch", Prompt: "p", WatchCmd: "./w.sh", ProjectPath: path, RepoID: f.repoA}
	for _, tk := range []task.Task{mine, foreign, watch} {
		require.NoError(t, task.AddTask(tk))
	}
	_, out := f.call(t, f.secret, "/v1/ListTasks", `{}`)
	assert.Contains(t, out, "t-mine")
	assert.NotContains(t, out, "t-foreign", "another project's tasks must not be listed")
	_, opOut := f.call(t, "operator-token", "/v1/ListTasks", `{}`)
	assert.Contains(t, opOut, "t-foreign", "anti-vacuous: the operator sees every project's tasks")

	f.assertRefused(t, "/v1/RemoveTask", `{"id":"t-foreign"}`, "another project's task")
	f.assertRefused(t, "/v1/RemoveTask", `{"id":"t-watch"}`, "watch tasks are not managed from a sandbox")
}

func TestSandboxScope_DeniedRoutesNeverReachAHandler(t *testing.T) {
	f := newScopeFixture(t)
	for _, path := range []string{
		"/v1/UpdateTask", "/v1/TriggerTask", "/v1/RestartTask", "/v1/DeliverPrompt", "/v1/KillSession",
		"/v1/GetConfig", "/v1/SetConfigValue", "/v1/ListAccounts", "/v1/RegisterAccount", "/v1/AccountLogin",
		"/v1/ListProjects", "/v1/ListDirectory", "/v1/RestoreSession", "/v1/HandoffSession",
	} {
		code, _ := f.call(t, f.secret, path, `{}`)
		assert.Equalf(t, http.StatusUnauthorized, code, "%s must be refused at the gate for a sandbox credential", path)
	}
}

// The operator's own authority is untouched by the scoping.
func TestSandboxScope_OperatorIsNotNarrowed(t *testing.T) {
	f := newScopeFixture(t)
	code, out := f.call(t, "operator-token", "/v1/SendPrompt", `{"prompt":"hi","id":"`+f.other.ID+`"}`)
	assert.NotContains(t, out, scopeRefusal, "the operator may address any project's session (status %d)", code)
}

// A task created by a sandbox credential is enforced on EVERY delivery, not only
// at creation (Codex P1 on #5823). The task carries the owner session as durable
// provenance; each firing re-checks that the target is still an existing,
// unarchived, off-box session in the owner's project, and never creates one.
func TestSandboxTaskIsRecheckedOnEveryDelivery(t *testing.T) {
	deliver := func(f *scopeFixture) error {
		return f.mgr.checkSandboxTaskDelivery(f.me.ID, f.repoA, "peer")
	}

	t.Run("a valid target still delivers", func(t *testing.T) {
		f := newScopeFixture(t)
		assert.NoError(t, deliver(f))
	})
	t.Run("the target is archived", func(t *testing.T) {
		f := newScopeFixture(t)
		require.NoError(t, deliver(f), "anti-vacuous: valid before the change")
		f.peer.SetStatusForTest(session.Archived)
		err := deliver(f)
		require.Error(t, err)
		assert.ErrorIs(t, err, errSandboxOutOfScope)
	})
	t.Run("the target title now names a session on the daemon host", func(t *testing.T) {
		f := newScopeFixture(t)
		require.NoError(t, deliver(f))
		f.backends["peer"] = "local"
		err := deliver(f)
		require.Error(t, err)
		assert.ErrorIs(t, err, errSandboxOutOfScope)
	})
	t.Run("the target moved to another project", func(t *testing.T) {
		f := newScopeFixture(t)
		require.NoError(t, deliver(f))
		f.mgr.mu.Lock()
		delete(f.mgr.instances, daemonInstanceKey(f.repoA, "peer"))
		f.mgr.instances[daemonInstanceKey(f.repoB, "peer")] = f.peer
		f.mgr.mu.Unlock()
		require.NoError(t, config.LoadState().SaveInstances(f.repoA, f.rows(f.me, f.local)))
		require.NoError(t, config.LoadState().SaveInstances(f.repoB, f.rows(f.other, f.peer)))
		err := deliver(f)
		require.Error(t, err)
		assert.ErrorIs(t, err, errSandboxOutOfScope)
	})
	t.Run("the delivery resolved a different project than the owner's", func(t *testing.T) {
		f := newScopeFixture(t)
		err := f.mgr.checkSandboxTaskDelivery(f.me.ID, f.repoB, "peer")
		require.Error(t, err)
		assert.ErrorIs(t, err, errSandboxOutOfScope)
	})
	t.Run("the owner session is gone", func(t *testing.T) {
		f := newScopeFixture(t)
		f.mgr.mu.Lock()
		delete(f.mgr.instances, daemonInstanceKey(f.repoA, "me"))
		f.mgr.mu.Unlock()
		require.NoError(t, config.LoadState().SaveInstances(f.repoA, f.rows(f.peer, f.local)))
		err := deliver(f)
		require.Error(t, err)
		assert.ErrorIs(t, err, errSandboxOutOfScope)
	})
	t.Run("no target", func(t *testing.T) {
		f := newScopeFixture(t)
		assert.ErrorIs(t, f.mgr.checkSandboxTaskDelivery(f.me.ID, f.repoA, "  "), errSandboxOutOfScope)
	})
}

// The daemon's delivery boundary enforces it: a broken target skips the run with
// the reason, sends nothing, and creates nothing.
func TestSandboxTaskDeliveryNeverCreatesItsTarget(t *testing.T) {
	f := newScopeFixture(t)
	f.backends["peer"] = "local" // retitled into a session on the daemon host

	f.mgr.mu.Lock()
	before := len(f.mgr.instances)
	f.mgr.mu.Unlock()
	_, _, _, err := f.mgr.deliverPromptWithOutcome(DeliverPromptRequest{
		Title: "peer", RepoPath: f.me.Path, Prompt: "x", TaskOrigin: true,
		TaskRepoID: f.repoA, SandboxOwner: f.me.ID,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), scopeRefusal)
	assert.Contains(t, err.Error(), notDeliveredMarker, "a skipped run is pre-flight: it refunds the rate slot")
	f.mgr.mu.Lock()
	after := len(f.mgr.instances)
	f.mgr.mu.Unlock()
	assert.Equal(t, before, after, "no session may be created by a refused sandbox-task delivery")

	// A target that does not exist at all is refused, not auto-created.
	_, _, _, err = f.mgr.deliverPromptWithOutcome(DeliverPromptRequest{
		Title: "does-not-exist", RepoPath: f.me.Path, Prompt: "x", TaskOrigin: true,
		TaskRepoID: f.repoA, SandboxOwner: f.me.ID,
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), scopeRefusal)
	f.mgr.mu.Lock()
	assert.Equal(t, before, len(f.mgr.instances))
	f.mgr.mu.Unlock()
}

// A sandbox-created task with no target would spawn a session on the daemon
// host: the run is skipped before any create is attempted.
func TestSandboxTaskWithoutTargetNeverSpawns(t *testing.T) {
	prev := createSessionForTask
	createSessionForTask = func(CreateSessionRequest) (*session.InstanceData, error) {
		t.Fatal("a sandbox-created task must never create a session")
		return nil, nil
	}
	t.Cleanup(func() { createSessionForTask = prev })

	_, _, err := deliverTaskPromptRun(&task.Task{ID: "t1", SandboxOwner: "sess-a", ProjectPath: t.TempDir()}, "p", false, new(task.RunKey))
	require.Error(t, err)
	assert.ErrorIs(t, err, errSandboxOutOfScope)
}

// The provenance is stamped by the daemon from the credential, over anything the
// caller sent, so the agent cannot claim another owner or clear its own.
func TestStampSandboxTaskOverwritesCallerProvenance(t *testing.T) {
	proj := sandboxProject{repoID: "r1", path: "/proj"}
	got := stampSandboxTask(task.Task{
		Prompt: "p", CronExpr: "0 * * * *", TargetSession: " peer ",
		SandboxOwner: "someone-else", Host: "h1", RepoID: "r-other", ProjectPath: "/elsewhere", OnComplete: "kill",
	}, "sess-a", proj, "peer")
	assert.Equal(t, "sess-a", got.SandboxOwner)
	assert.Equal(t, "peer", got.TargetSession)
	assert.Equal(t, "/proj", got.ProjectPath)
	assert.Empty(t, got.RepoID)
	assert.Empty(t, got.Host)
	assert.Empty(t, got.OnComplete)
}
