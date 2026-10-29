package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/hostlayout"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// The structural half of W3.5 (#5571): three properties, each enforced across the
// whole route table instead of per finding (the fifth review round on this
// scope). (1) Nothing returned to a sandbox credential — body or error — names
// the daemon host's layout. (2) Authorization is atomic with the action: each
// scoped mutation re-validates under the lock that performs it. (3) A create
// returns the id the daemon stored.

// useScheduler gives the fixture's server a task scheduler, so task mutations
// reach their real handlers instead of stopping at "no scheduler".
func (f *scopeFixture) useScheduler() {
	f.gated = withAuth(newHTTPMux(&controlServer{manager: f.mgr, scheduler: newTaskScheduler()}), &authGate{
		expectedToken: func() (string, error) { return "operator-token", nil },
		sandboxTokens: &f.mgr.sandboxTokens,
	}, nil)
}

// addStoreTask writes a task straight to the store and returns it as stored.
func addStoreTask(t *testing.T, tk task.Task) task.Task {
	t.Helper()
	created, err := task.AddTaskChecked(tk, task.ActorCLI, nil)
	require.NoError(t, err)
	return created
}

// --- (1) the response boundary -----------------------------------------------

// Every admitted route, driven with a sandbox credential on success AND refusal
// shapes, never answers with a daemon-host path. The needles are independent of
// the scrub's own layout list: the project's path, another project's path, and
// the AF home.
func TestSandboxScope_NoResponseCarriesTheDaemonHostLayout(t *testing.T) {
	f := newScopeFixture(t)
	f.useScheduler()
	home, err := config.GetConfigDir()
	require.NoError(t, err)
	otherPath := setupControlRepo(t)
	userHome, _ := os.UserHomeDir()
	needles := []string{f.me.Path, otherPath, home}
	if len(userHome) > 4 {
		needles = append(needles, userHome)
	}

	mine := addStoreTask(t, task.Task{ID: "t-mine", Prompt: "p", CronExpr: "0 * * * *", TargetSession: "peer", ProjectPath: f.me.Path})
	addStoreTask(t, task.Task{ID: "t-foreign", Prompt: "p", CronExpr: "0 * * * *", TargetSession: "foreign", ProjectPath: otherPath})
	addStoreTask(t, task.Task{ID: "t-watch", Prompt: "p", WatchCmd: "./w.sh", ProjectPath: f.me.Path})

	tab := func(extra string) string { return fmt.Sprintf(`{"id":%q%s}`, f.me.ID, extra) }
	requests := map[string][]string{
		"/v1/Snapshot": {`{}`, `{"repo_id":"` + f.repoB + `"}`, `{"live":true}`},
		"/v1/SendPrompt": {
			`{"prompt":"hi","title":"peer"}`, `{"prompt":"hi","title":"foreign"}`, `{"prompt":"hi","title":"does-not-exist"}`,
			`{"prompt":"hi","title":"local-one"}`, `{"prompt":"hi","id":"` + f.other.ID + `"}`, `{"prompt":"hi","repo_id":"` + f.repoB + `","title":"peer"}`,
		},
		"/v1/ArchiveSession": {`{"title":"peer"}`, `{"title":"foreign"}`, `{"title":"does-not-exist"}`, `{"id":"` + f.other.ID + `"}`},
		"/v1/CreateSession": {
			`{"title":"x","prompt":"p","host":"h1"}`, `{"title":"x","prompt":"p"}`, `{"title":"x","host":"h1","backend":"local"}`,
			`{"title":"x","host":"h1","repo_path":"` + otherPath + `"}`, `{"title":"x","host":"h1","in_place":true}`,
		},
		"/v1/CreateTab":  {tab(`,"command":"npm run dev","name":"dev"`), tab(`,"kind":"web","port":3000`), `{"id":"` + f.peer.ID + `","command":"x"}`},
		"/v1/CloseTab":   {tab(`,"tab_name":"nope"`), `{"id":"` + f.peer.ID + `","tab_name":"t"}`},
		"/v1/RenameTab":  {tab(`,"tab_name":"nope","new_name":"n"`), `{"id":"` + f.other.ID + `","tab_name":"t","new_name":"n"}`},
		"/v1/ReorderTab": {tab(`,"tab_name":"nope","new_index":1`), `{"id":"` + f.peer.ID + `","tab_name":"t","new_index":1}`},
		"/v1/ListTasks":  {`{}`},
		"/v1/AddTask": {
			`{"task":{"prompt":"p","cron_expr":"0 * * * *","target_session":"peer"}}`,
			`{"task":{"prompt":"p","cron_expr":"0 * * * *","target_session":"foreign","project_path":"` + otherPath + `"}}`,
			`{"task":{"prompt":"p","watch_cmd":"./w.sh","target_session":"peer"}}`,
			`{"task":{"prompt":"p","cron_expr":"0 * * * *","target_session":"does-not-exist"}}`,
		},
		"/v1/RemoveTask": {
			`{"id":"t-foreign"}`, `{"id":"t-watch"}`, `{"id":"no-such-task"}`,
			// The expectation's mismatch error is composed from the stored record,
			// host path included (#5893): it must never be consulted for a sandbox.
			`{"id":"t-foreign","expect":{"enforce":true,"project_path":""}}`,
			`{"id":"` + mine.ID + `","expect":{"enforce":true,"project_path":""}}`,
		},
	}

	// Completeness: a route admitted to the scope without a row here fails, so the
	// property cannot silently stop covering a new route.
	var admitted []string
	for _, rt := range servedHTTPRoutes() {
		if rt.sandboxAllowed {
			admitted = append(admitted, rt.Path)
		}
	}
	sort.Strings(admitted)
	for _, path := range admitted {
		require.Containsf(t, requests, path, "%s is admitted to the sandbox scope but has no request in the layout property test", path)
	}
	require.Len(t, requests, len(admitted), "the test names a route the scope does not admit")

	for path, bodies := range requests {
		for _, body := range bodies {
			code, out := f.call(t, f.secret, path, body)
			for _, needle := range needles {
				assert.NotContainsf(t, out, needle, "%s %s (status %d) must not carry the daemon host's layout", path, body, code)
			}
		}
	}

	// Anti-vacuous: the operator's own reads DO carry these paths, so the needles
	// are real and the sandbox's absence is the scrub's doing.
	_, op := f.call(t, "operator-token", "/v1/ListTasks", `{}`)
	assert.Contains(t, op, otherPath, "the operator sees host paths: the needle is real")
	code, opErr := f.call(t, "operator-token", "/v1/RemoveTask", `{"id":"t-foreign","expect":{"enforce":true,"project_path":""}}`)
	assert.Contains(t, opErr, otherPath, "the expectation mismatch error does carry the path for the operator (status %d)", code)
}

// A string an instance's projection noted is withheld by the very next sandbox
// response, including one created a moment ago: the layout is maintained as
// producers note it, not recomputed per request, so nothing on the response path
// walks sessions or takes their locks (internal/hostlayout). Daemon-originated
// fields are scrubbed; the values of caller-text keys come back verbatim.
func TestSandboxResponsesWithholdWhatAProjectionJustNoted(t *testing.T) {
	fresh := "/srv/just-created-" + t.Name() + "/worktrees/child"
	inst, err := session.NewInstance(session.InstanceOptions{Title: "child", Path: fresh, Program: "claude"})
	require.NoError(t, err)
	_ = inst.ToInstanceData() // the projection a create response is built from

	handler := scrubbedSandboxResponses(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"last_run_status":"errored: ` + fresh + `","prompt":"` + fresh + `"},"error":null}`))
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/x", nil))
	var env struct {
		Data map[string]string `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	assert.NotContains(t, env.Data["last_run_status"], fresh, "a daemon-originated field is scrubbed")
	assert.Contains(t, env.Data["last_run_status"], hostlayout.Withheld)
	assert.Equal(t, fresh, env.Data["prompt"], "a caller-text value comes back exactly as stored")
}

// Errors never reflect input: a typed refusal passes unchanged, any other message
// is replaced by a fixed one, and the daemon path inside it is gone with it.
func TestSandboxErrorsAreTypedOrFixed(t *testing.T) {
	hostlayout.Default.Note("/srv/err-test/secret-project")
	_ = sandboxRefusal("that session is not available to this credential") // registers the text
	typedRefusal := errSandboxOutOfScope.Error() + ": that session is not available to this credential"
	for name, tc := range map[string]struct {
		status  int
		message string
		code    string
		want    string
	}{
		"a typed refusal is kept":      {500, typedRefusal, "", typedRefusal},
		"a pre-flight refusal is kept": {500, typedRefusal + "; " + notDeliveredMarker, "", typedRefusal + "; " + notDeliveredMarker},
		"a lookalike prefix is not":    {500, errSandboxOutOfScope.Error() + ": /srv/err-test/secret-project", "", sandboxGenericFailureMsg},
		"a validation error":           {400, `backend "/srv/err-test/secret-project" is not supported`, "", sandboxGenericRequestMsg},
		"a decode error":               {400, `malformed JSON request body: json: unknown field "/srv/err-test/secret-project"`, "", sandboxGenericRequestMsg},
		"a server error":               {500, "open /srv/err-test/secret-project/.git: permission denied", "", sandboxGenericFailureMsg},
		"a committed follow-up":        {500, "task saved but reload failed in /srv/err-test/secret-project", "mutation_committed", sandboxCommittedMsg},
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{"data": nil, "error": map[string]any{"message": tc.message, "code": tc.code, "stored_id": "ab12cd34"}})
			require.NoError(t, err)
			out := sanitizeSandboxBody("/v1/x", tc.status, raw)
			var env struct {
				Error struct {
					Message  string `json:"message"`
					Code     string `json:"code"`
					StoredID string `json:"stored_id"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(out, &env))
			assert.Equal(t, tc.want, env.Error.Message)
			assert.NotContains(t, string(out), "secret-project")
			assert.Equal(t, tc.code, env.Error.Code, "the machine-readable code survives")
			assert.Equal(t, "ab12cd34", env.Error.StoredID, "and so does the stored id")
		})
	}
}

// DIFFERENTIAL: a response never depends on whether a caller-supplied string is
// in the host layout (#6014 review: the scrub made guessed paths an existence
// oracle). For every admitted route and every string field of its request, a guess
// that IS in the layout (A) and one that is not (B) must produce responses that
// are identical except for the literal guess, or contain neither.
func TestSandboxScope_ResponsesDoNotDependOnCallerInputMembership(t *testing.T) {
	f := newScopeFixture(t)
	f.useScheduler()
	guessA := f.me.Path // a real project path: noted by NewInstance
	guessB := "/srv/not-a-real-project-b3e1"
	require.NotEqual(t, hostlayout.Default.ScrubString(guessA), guessA, "anti-vacuous: guess A is in the layout")
	require.Equal(t, hostlayout.Default.ScrubString(guessB), guessB, "and guess B is not")
	addStoreTask(t, task.Task{ID: "t-mine", Prompt: "p", CronExpr: "0 * * * *", TargetSession: "peer", ProjectPath: f.me.Path})

	const G = "%G%"
	type shape struct{ route, body string }
	task_ := func(field string) string {
		base := map[string]string{"prompt": "p", "cron_expr": "0 * * * *", "target_session": "peer"}
		base[field] = G
		raw, _ := json.Marshal(map[string]any{"task": base})
		return string(raw)
	}
	obj := func(base map[string]string, field string) string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		m[field] = G
		raw, _ := json.Marshal(m)
		return string(raw)
	}
	var shapes []shape
	add := func(route string, base map[string]string, fields ...string) {
		for _, fld := range fields {
			shapes = append(shapes, shape{route, obj(base, fld)})
		}
	}
	add("/v1/Snapshot", nil, "repo_id")
	add("/v1/SendPrompt", map[string]string{"prompt": "hi", "title": "peer"}, "prompt", "title", "id", "repo_id")
	add("/v1/ArchiveSession", map[string]string{"title": "nope"}, "title", "id", "repo_id")
	add("/v1/CreateSession", map[string]string{"title": "x", "host": "h1"}, "title", "prompt", "host", "backend", "repo_path", "account")
	for _, fld := range []string{"id", "title", "command", "name", "kind", "url", "proposal"} {
		shapes = append(shapes, shape{"/v1/CreateTab", obj(map[string]string{"id": f.me.ID, "command": "x"}, fld)})
	}
	add("/v1/CloseTab", map[string]string{"id": f.me.ID, "tab_name": "nope"}, "id", "title", "tab_name")
	add("/v1/RenameTab", map[string]string{"id": f.me.ID, "tab_name": "nope", "new_name": "n"}, "id", "title", "tab_name", "new_name")
	add("/v1/ReorderTab", map[string]string{"id": f.me.ID, "tab_name": "nope"}, "id", "title", "tab_name")
	shapes = append(shapes, shape{"/v1/ListTasks", `{}`})
	for _, fld := range []string{"prompt", "name", "target_session", "cron_expr", "watch_cmd", "project_path", "host", "on_complete", "program"} {
		shapes = append(shapes, shape{"/v1/AddTask", task_(fld)})
	}
	add("/v1/RemoveTask", map[string]string{"id": "t-mine"}, "id")
	shapes = append(shapes, shape{"/v1/RemoveTask", `{"id":"t-mine","expect":{"enforce":true,"project_path":"` + G + `"}}`})

	covered := map[string]bool{}
	for _, sh := range shapes {
		covered[sh.route] = true
	}
	for _, rt := range servedHTTPRoutes() {
		if rt.sandboxAllowed {
			require.Truef(t, covered[rt.Path], "%s is admitted to the sandbox scope but has no shape in the differential test", rt.Path)
		}
	}

	stamp := regexp.MustCompile(`\d{4}-\d{2}-\d{2}T[0-9:.+\-Z]+`)
	hexish := regexp.MustCompile(`[0-9a-f]{8,}`)
	normalize := func(out, guess string) string {
		esc, _ := json.Marshal(guess)
		out = strings.ReplaceAll(out, guess, "<G>")
		out = strings.ReplaceAll(out, string(esc[1:len(esc)-1]), "<G>")
		out = strings.ReplaceAll(out, hostlayout.Withheld, "<W>")
		out = stamp.ReplaceAllString(out, "<T>")
		return hexish.ReplaceAllString(out, "<hex>")
	}
	for _, sh := range shapes {
		codeA, outA := f.call(t, f.secret, sh.route, strings.ReplaceAll(sh.body, G, guessA))
		codeB, outB := f.call(t, f.secret, sh.route, strings.ReplaceAll(sh.body, G, guessB))
		assert.Equalf(t, codeA, codeB, "%s %s: the status depends on whether the guess is in the layout", sh.route, sh.body)
		assert.Equalf(t, normalize(outA, guessA), normalize(outB, guessB),
			"%s %s: the response depends on whether the caller's guess is in the host layout (an existence oracle)", sh.route, sh.body)
	}

	// A later read must not depend on it either: the stored text a sandbox
	// supplied comes back verbatim whether or not it is in the layout.
	for _, guess := range []string{guessA, guessB} {
		_, out := f.call(t, f.secret, "/v1/AddTask", `{"task":{"prompt":"`+guess+`","cron_expr":"0 * * * *","target_session":"peer"}}`)
		assert.Contains(t, out, guess, "the stored prompt comes back as supplied")
		_, list := f.call(t, f.secret, "/v1/ListTasks", `{}`)
		assert.Contains(t, list, guess, "and so does a later listing")
	}
}

// No admitted route streams: the response boundary buffers a whole response, and
// Flush is a no-op, so a streaming handler admitted to the scope would stall
// until it finished. Every admitted route is a request/response RPC.
func TestSandboxScope_NoAdmittedRouteStreams(t *testing.T) {
	for _, rt := range servedHTTPRoutes() {
		if rt.sandboxAllowed {
			assert.Equalf(t, http.MethodPost, rt.Method, "%s is admitted to the sandbox scope but is not a request/response POST RPC", rt.Path)
			for _, streaming := range []string{"stream", "attach", "events", "preview", "ws"} {
				assert.NotContainsf(t, strings.ToLower(rt.Path), streaming, "%s looks like a streaming route", rt.Path)
			}
		}
	}
}

// The create response is the manager's complete InstanceData; a sandbox gets the
// snapshot projection (#5888).
func TestSandboxCreateResponseIsProjected(t *testing.T) {
	resp := CreateSessionResponse{Instance: session.InstanceData{
		ID: "sess-1", Title: "child", Path: "/home/op/proj", TmuxName: "af_child",
		Worktree: session.GitWorktreeData{WorktreePath: "/home/op/wt/child", RepoPath: "/home/op/proj"},
	}}
	projectSandboxCreateResponse(&resp)
	raw, err := json.Marshal(resp)
	require.NoError(t, err)
	assert.Equal(t, "sess-1", resp.Instance.ID)
	assert.Equal(t, "child", resp.Instance.Title)
	for _, leaked := range []string{"/home/op", "af_child"} {
		assert.NotContains(t, string(raw), leaked)
	}
}

// --- (2) authorization atomic with the action ---------------------------------

// A rebind (or conversion) landing between the scoped RemoveTask's early read and
// the delete must never carry the removal across the project boundary: the
// authorization is re-run on the record loaded under the lock that deletes.
func TestSandboxRemoveTaskIsAtomicWithItsAuthorization(t *testing.T) {
	cases := map[string]func(t *testing.T, f *scopeFixture, otherPath string){
		"rebound to another project": func(t *testing.T, f *scopeFixture, otherPath string) {
			require.NoError(t, task.RemoveTask("t-race", task.ProjectExpectation{}))
			addStoreTask(t, task.Task{ID: "t-race", Prompt: "p", CronExpr: "0 * * * *", TargetSession: "foreign", ProjectPath: otherPath})
		},
		"converted to a watch task": func(t *testing.T, f *scopeFixture, otherPath string) {
			require.NoError(t, task.RemoveTask("t-race", task.ProjectExpectation{}))
			addStoreTask(t, task.Task{ID: "t-race", Prompt: "p", WatchCmd: "./w.sh", ProjectPath: f.me.Path})
		},
	}
	for name, interleave := range cases {
		t.Run(name, func(t *testing.T) {
			f := newScopeFixture(t)
			f.useScheduler()
			otherPath := setupControlRepo(t)
			addStoreTask(t, task.Task{ID: "t-race", Prompt: "p", CronExpr: "0 * * * *", TargetSession: "peer", ProjectPath: f.me.Path})

			testHookSandboxRemoveAfterPrecheck = func() { interleave(t, f, otherPath) }
			t.Cleanup(func() { testHookSandboxRemoveAfterPrecheck = nil })

			_, out := f.call(t, f.secret, "/v1/RemoveTask", `{"id":"t-race"}`)
			assert.Contains(t, out, scopeRefusal, "the removal must be refused on the task as it is NOW")
			got, err := task.GetTask("t-race")
			require.NoError(t, err, "a refused removal must leave the task")
			assert.NotNil(t, got)
		})
	}

	t.Run("control: no interleaving, the same removal succeeds", func(t *testing.T) {
		f := newScopeFixture(t)
		f.useScheduler()
		addStoreTask(t, task.Task{ID: "t-race", Prompt: "p", CronExpr: "0 * * * *", TargetSession: "peer", ProjectPath: f.me.Path})
		code, out := f.call(t, f.secret, "/v1/RemoveTask", `{"id":"t-race"}`)
		assert.Equal(t, http.StatusOK, code, out)
		_, err := task.GetTask("t-race")
		assert.Error(t, err, "the authorized removal took effect")
	})
}

// The expectation a sandbox sends is never consulted (#5893): a mismatching one
// neither blocks its own project's removal nor carries a path back.
func TestSandboxRemoveTaskIgnoresTheCallersExpectation(t *testing.T) {
	f := newScopeFixture(t)
	f.useScheduler()
	addStoreTask(t, task.Task{ID: "t-mine", Prompt: "p", CronExpr: "0 * * * *", TargetSession: "peer", ProjectPath: f.me.Path})
	code, out := f.call(t, f.secret, "/v1/RemoveTask", `{"id":"t-mine","expect":{"enforce":true,"project_path":"/elsewhere"}}`)
	assert.Equal(t, http.StatusOK, code, out)
	assert.NotContains(t, out, f.me.Path)
	assert.NotContains(t, out, "/elsewhere")
}

// A session replaced under the same title between authorization and the send or
// archive is never reached: the action is addressed by the authorized ID and
// re-validated on the instance it acts on.
func TestSandboxSessionVerbsAreAtomicWithTheirAuthorization(t *testing.T) {
	for _, route := range []struct{ path, body string }{
		{"/v1/SendPrompt", `{"prompt":"hi","title":"peer"}`},
		{"/v1/ArchiveSession", `{"title":"peer"}`},
	} {
		t.Run(route.path, func(t *testing.T) {
			f := newScopeFixture(t)
			b, err := session.NewInstance(session.InstanceOptions{Title: "peer", Path: f.me.Path, Program: "claude"})
			require.NoError(t, err)
			b.SetStartedForTest(true)
			b.SetStatusForTest(session.Ready)
			testHookSandboxAfterAuthorize = func() {
				// A is archived and B takes its title in the same project.
				f.mgr.mu.Lock()
				f.mgr.instances[daemonInstanceKey(f.repoA, "peer")] = b
				f.mgr.mu.Unlock()
			}
			t.Cleanup(func() { testHookSandboxAfterAuthorize = nil })
			require.NotEqual(t, f.peer.ID, b.ID)

			code, out := f.call(t, f.secret, route.path, route.body)
			assert.NotEqual(t, http.StatusOK, code, "the action must not land on a session that was never authorized: %s", out)
			assert.False(t, b.IsArchived(), "the replacement session must be untouched")
			assert.Equal(t, session.Ready, b.GetStatus())
		})
	}
}

// The re-check itself, table-driven over the states it must refuse.
func TestSandboxSessionStillAuthorized(t *testing.T) {
	f := newScopeFixture(t)
	prev := instanceBackendType
	defer func() { instanceBackendType = prev }()
	instanceBackendType = func(i *session.Instance) string { return f.backends[i.Title] }

	assert.NoError(t, sandboxSessionStillAuthorized(f.peer, f.peer.ID), "the authorized, off-box session")
	assert.Error(t, sandboxSessionStillAuthorized(nil, f.peer.ID), "gone")
	assert.Error(t, sandboxSessionStillAuthorized(f.other, f.peer.ID), "another session now holds the slot")
	assert.Error(t, sandboxSessionStillAuthorized(f.local, f.local.ID), "now (or always) a daemon-host session")
	assert.Error(t, sandboxSessionStillAuthorized(f.peer, ""), "no authorized id means nothing is authorized")
	assert.Error(t, f.mgr.sandboxSessionGuard(f.peer.ID, f.repoA)(f.peer, f.repoB), "resolved in another project")
}

// --- (3) the created id is returned -------------------------------------------

var mintedTaskID = regexp.MustCompile(`^[0-9a-f]{8}$`)

// A sandbox AddTask discards the caller's id and mints its own, so the response
// must carry it: the caller's get and remove then address the stored task (#5890).
func TestSandboxAddTaskReturnsTheMintedID(t *testing.T) {
	f := newScopeFixture(t)
	f.useScheduler()
	code, out := f.call(t, f.secret, "/v1/AddTask",
		`{"task":{"id":"caller-chosen","prompt":"p","cron_expr":"0 * * * *","target_session":"peer"}}`)
	require.Equal(t, http.StatusOK, code, out)
	var env struct {
		Data AddTaskResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &env))
	require.NotNil(t, env.Data.Task, "the response carries the stored record: %s", out)
	id := env.Data.Task.ID
	require.Regexp(t, mintedTaskID, id, "the response names the daemon-minted id: %s", out)
	assert.Empty(t, env.Data.Task.ProjectPath, "the stored record is the sandbox-safe projection")
	assert.Empty(t, env.Data.Task.RepoID)
	assert.NotEqual(t, "caller-chosen", id)

	_, list := f.call(t, f.secret, "/v1/ListTasks", `{}`)
	assert.Contains(t, list, id, "the returned id addresses the stored task")
	assert.NotContains(t, list, "caller-chosen")
	code, out = f.call(t, f.secret, "/v1/RemoveTask", fmt.Sprintf(`{"id":%q}`, id))
	assert.Equal(t, http.StatusOK, code, out)
}

// A task that is STORED but whose scheduler reload fails answers an error with no
// response body; the stored id rides the error envelope so the caller can still
// get and remove it (#5890).
func TestCommittedAddErrorEnvelopeCarriesTheStoredID(t *testing.T) {
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/AddTask", nil)
	writeHTTPError(rec, r, http.StatusInternalServerError,
		&mutationCommittedError{err: errors.New("reload failed"), storedID: "ab12cd34"})
	body := rec.Body.String()
	assert.Contains(t, body, `"stored_id":"ab12cd34"`)
	assert.Contains(t, body, `"code":"mutation_committed"`)

	rec = httptest.NewRecorder()
	writeHTTPError(rec, r, http.StatusInternalServerError, &mutationCommittedError{err: errors.New("reload failed")})
	assert.NotContains(t, rec.Body.String(), "stored_id", "no stored id, none invented")
}
