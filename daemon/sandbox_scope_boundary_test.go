package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

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
	cs := &controlServer{manager: f.mgr, scheduler: newTaskScheduler()}
	// A successful create, without provisioning a host: the full InstanceData the
	// manager would return, built from the request, so the response boundary is
	// driven on a create that SUCCEEDS (the path a refusal never reaches).
	cs.createSessionFn = func(_ context.Context, req CreateSessionRequest, resp *CreateSessionResponse) error {
		resp.Instance = session.InstanceData{
			ID: "created-1", Title: req.Title, Program: req.Program, Branch: "af/" + req.Title,
			Path: f.me.Path, TmuxName: "af_" + req.Title, BackendType: req.Backend, Host: req.Host,
			Worktree: session.GitWorktreeData{RepoPath: f.me.Path, WorktreePath: f.me.Path + "/.wt/" + req.Title},
		}
		return nil
	}
	f.gated = withAuth(newHTTPMux(cs), &authGate{
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
		// The owner id replaces every selector, so a peer's, another project's and
		// an unknown name all answer about the caller's own session.
		"/v1/SetAttention": {
			`{"title":"peer","reason":"r"}`, `{"title":"foreign","reason":"r"}`, `{"title":"does-not-exist"}`,
			`{"id":"` + f.other.ID + `","reason":"r"}`, `{"repo_id":"` + f.repoB + `","title":"peer","clear":true}`,
			`{"reason":"` + otherPath + `"}`,
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
		_, _ = w.Write([]byte(`{"data":{"tasks":[{"id":"t1","last_run_status":"errored: ` + fresh + `","prompt":"` + fresh + `"}]},"error":null}`))
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/ListTasks", nil))
	var env struct {
		Data struct {
			Tasks []map[string]string `json:"tasks"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env))
	require.Len(t, env.Data.Tasks, 1)
	assert.NotContains(t, env.Data.Tasks[0]["last_run_status"], fresh, "a daemon-originated field is scrubbed")
	assert.Contains(t, env.Data.Tasks[0]["last_run_status"], hostlayout.Withheld)
	assert.Equal(t, fresh, env.Data.Tasks[0]["prompt"], "a caller-originated value comes back exactly as stored")
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

// scrubWatch records every time the backstop scrub changes a daemon-class string
// in a success body. The invariant (#5571 W3.5, fourth oracle round): the typed
// projection and the classification already removed everything layout-shaped, so
// the scrub changes NOTHING in any sandbox success body.
func scrubWatch(t *testing.T) func() []string {
	t.Helper()
	var mu sync.Mutex
	var hits []string
	sandboxScrubChanged = func(original string) {
		mu.Lock()
		hits = append(hits, original)
		mu.Unlock()
	}
	t.Cleanup(func() { sandboxScrubChanged = nil })
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), hits...)
	}
}

// requestTypes is the request type of every route admitted to the sandbox scope.
var requestTypes = map[string]reflect.Type{
	"/v1/Snapshot":       reflect.TypeFor[SnapshotRequest](),
	"/v1/SendPrompt":     reflect.TypeFor[SendPromptRequest](),
	"/v1/SetAttention":   reflect.TypeFor[SetAttentionRequest](),
	"/v1/ArchiveSession": reflect.TypeFor[ArchiveSessionRequest](),
	"/v1/CreateSession":  reflect.TypeFor[CreateSessionRequest](),
	"/v1/CreateTab":      reflect.TypeFor[CreateTabRequest](),
	"/v1/CloseTab":       reflect.TypeFor[CloseTabRequest](),
	"/v1/RenameTab":      reflect.TypeFor[RenameTabRequest](),
	"/v1/ReorderTab":     reflect.TypeFor[ReorderTabRequest](),
	"/v1/ListTasks":      reflect.TypeFor[ListTasksRequest](),
	"/v1/AddTask":        reflect.TypeFor[AddTaskRequest](),
	"/v1/RemoveTask":     reflect.TypeFor[RemoveTaskRequest](),
}

// stringFieldPaths lists the JSON path of every settable string field of t,
// recursing into nested structs (a task inside AddTask, an expectation inside
// RemoveTask). time.Time and collections are not strings a caller types.
func stringFieldPaths(t reflect.Type, prefix []string) [][]string {
	t = derefType(t)
	var out [][]string
	for name, f := range jsonFieldsOf(t) {
		path := append(append([]string{}, prefix...), name)
		ft := derefType(f.typ)
		switch {
		case ft.Kind() == reflect.String:
			out = append(out, path)
		case ft.Kind() == reflect.Struct && ft != reflect.TypeFor[time.Time]():
			out = append(out, stringFieldPaths(ft, path)...)
		}
	}
	sort.Slice(out, func(i, j int) bool { return strings.Join(out[i], ".") < strings.Join(out[j], ".") })
	return out
}

func setJSONPath(m map[string]any, path []string, val string) {
	for _, k := range path[:len(path)-1] {
		next, ok := m[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			m[k] = next
		}
		m = next
	}
	m[path[len(path)-1]] = val
}

func cloneJSONMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		if sub, ok := v.(map[string]any); ok {
			v = cloneJSONMap(sub)
		}
		out[k] = v
	}
	return out
}

// DIFFERENTIAL, driven from the REQUEST TYPES: a response never depends on whether
// a caller-supplied string is in the host layout (#6014 review: the scrub made a
// guessed path an existence oracle). For every admitted route, every settable
// string field of its request type, and every route shape that changes validation
// (a listed host or none, each backend), a guess that IS in the layout (A) and one
// that is not (B) must produce responses identical except for the literal guess, or
// containing neither. Because the fields come from reflection, a field added to a
// request type later is covered with no edit here.
func TestSandboxScope_ResponsesDoNotDependOnCallerInputMembership(t *testing.T) {
	f := newScopeFixture(t)
	f.useScheduler()
	scrubHits := scrubWatch(t)
	guessA := f.me.Path // a real project path: noted by NewInstance
	guessB := "/srv/not-a-real-project-b3e1"
	require.NotEqual(t, hostlayout.Default.ScrubString(guessA), guessA, "anti-vacuous: guess A is in the layout")
	require.Equal(t, hostlayout.Default.ScrubString(guessB), guessB, "and guess B is not")
	addStoreTask(t, task.Task{ID: "t-mine", Prompt: "p", CronExpr: "0 * * * *", TargetSession: "peer", ProjectPath: f.me.Path})

	// Baseline bodies per route and validation shape; the field under test is set
	// on top of each. CreateSession varies host and backend, which change which
	// checks run before anything else.
	type baseShape struct {
		name string
		body map[string]any
	}
	bases := map[string][]baseShape{
		"/v1/Snapshot":       {{"empty", map[string]any{}}},
		"/v1/SendPrompt":     {{"to a peer", map[string]any{"prompt": "hi", "title": "peer"}}},
		"/v1/SetAttention":   {{"own session", map[string]any{"id": f.me.ID, "reason": "r"}}},
		"/v1/ArchiveSession": {{"unknown", map[string]any{"title": "nope"}}},
		"/v1/CreateTab":      {{"own session", map[string]any{"id": f.me.ID, "command": "x"}}},
		"/v1/CloseTab":       {{"own session", map[string]any{"id": f.me.ID, "tab_name": "nope"}}},
		"/v1/RenameTab":      {{"own session", map[string]any{"id": f.me.ID, "tab_name": "nope", "new_name": "n"}}},
		"/v1/ReorderTab":     {{"own session", map[string]any{"id": f.me.ID, "tab_name": "nope"}}},
		"/v1/ListTasks":      {{"empty", map[string]any{}}},
		"/v1/AddTask": {{"cron to a peer", map[string]any{"task": map[string]any{
			"prompt": "p", "cron_expr": "0 * * * *", "target_session": "peer"}}}},
		"/v1/RemoveTask": {{"own task", map[string]any{"id": "t-mine"}}},
	}
	for _, host := range []string{"", "h1"} {
		for _, backend := range []string{"", "ssh", "local", "docker", "sandbox"} {
			bases["/v1/CreateSession"] = append(bases["/v1/CreateSession"], baseShape{
				fmt.Sprintf("host=%q backend=%q", host, backend),
				map[string]any{"title": "x", "host": host, "backend": backend},
			})
		}
	}

	// Completeness: every admitted route has a request type and a base shape, so a
	// newly admitted route cannot silently stop being covered.
	for _, rt := range servedHTTPRoutes() {
		if rt.sandboxAllowed {
			require.Containsf(t, requestTypes, rt.Path, "%s is admitted to the sandbox scope but has no request type in the differential test", rt.Path)
			require.NotEmptyf(t, bases[rt.Path], "%s has no base shape", rt.Path)
		}
	}
	require.Len(t, requestTypes, len(sandboxAllowedPaths), "the test names a route the scope does not admit")

	stamp := regexp.MustCompile(`\d{4}-\d{2}-\d{2}T[0-9:.+\-Z]+`)
	hexish := regexp.MustCompile(`[0-9a-f]{8,}`)
	clock := regexp.MustCompile(`"operation_clock_ms":\d+`)
	normalize := func(out, guess string) string {
		esc, _ := json.Marshal(guess)
		out = strings.ReplaceAll(out, guess, "<G>")
		out = strings.ReplaceAll(out, string(esc[1:len(esc)-1]), "<G>")
		out = strings.ReplaceAll(out, hostlayout.Withheld, "<W>")
		out = stamp.ReplaceAllString(out, "<T>")
		// The snapshot's operation clock advances between any two reads.
		out = clock.ReplaceAllString(out, `"operation_clock_ms":0`)
		return hexish.ReplaceAllString(out, "<hex>")
	}
	// A removal that succeeds consumes its task, so the second guess would be
	// answered about a task that no longer exists — a difference of STATE, not of
	// the guess. Restore the fixture's own task before every call.
	ensureMine := func() {
		if _, err := task.GetTask("t-mine"); err != nil {
			addStoreTask(t, task.Task{ID: "t-mine", Prompt: "p", CronExpr: "0 * * * *", TargetSession: "peer", ProjectPath: f.me.Path})
		}
	}

	routes := make([]string, 0, len(requestTypes))
	for route := range requestTypes {
		routes = append(routes, route)
	}
	sort.Strings(routes)
	checked := 0
	for _, route := range routes {
		paths := stringFieldPaths(requestTypes[route], nil)
		for _, base := range bases[route] {
			for _, path := range paths {
				mk := func(guess string) string {
					body := cloneJSONMap(base.body)
					setJSONPath(body, path, guess)
					raw, err := json.Marshal(body)
					require.NoError(t, err)
					return string(raw)
				}
				ensureMine()
				codeA, outA := f.call(t, f.secret, route, mk(guessA))
				ensureMine()
				codeB, outB := f.call(t, f.secret, route, mk(guessB))
				where := fmt.Sprintf("%s [%s] field %s", route, base.name, strings.Join(path, "."))
				assert.Equalf(t, codeA, codeB, "%s: the status depends on whether the guess is in the layout", where)
				assert.Equalf(t, normalize(outA, guessA), normalize(outB, guessB),
					"%s: the response depends on whether the caller's guess is in the host layout (an existence oracle)", where)
				checked++
			}
		}
	}
	require.Greater(t, checked, 100, "the reflection-driven matrix shrank unexpectedly (%d cases)", checked)
	assert.Emptyf(t, scrubHits(), "the backstop scrub changed a success body: the projection or the classification let a layout-shaped string through")

	// A later read must not depend on it either: the stored text a sandbox
	// supplied comes back verbatim whether or not it is in the layout.
	for _, guess := range []string{guessA, guessB} {
		_, out := f.call(t, f.secret, "/v1/AddTask", `{"task":{"prompt":"`+guess+`","cron_expr":"0 * * * *","target_session":"peer"}}`)
		assert.Contains(t, out, guess, "the stored prompt comes back as supplied")
		_, list := f.call(t, f.secret, "/v1/ListTasks", `{}`)
		assert.Contains(t, list, guess, "and so does a later listing")
	}

	// And the create that SUCCEEDS no longer returns the program text at all.
	_, out := f.call(t, f.secret, "/v1/CreateSession", `{"title":"x","host":"h1","program":"claude --append-system-prompt `+guessA+`"}`)
	assert.NotContains(t, out, "append-system-prompt", "a sandbox's program text is omitted from the create response")
	assert.Contains(t, out, "created-1", "anti-vacuous: the create succeeded and returned the projected row")
}

// STORE-THEN-READ: a value a sandbox can WRITE is caller text wherever it is read
// back, so reading it back must not depend on whether it is in the host layout
// either — a per-request differential cannot see an oracle whose write and read
// are separate requests. For every settable string field of every route that
// stores something (AddTask, CreateTab, RenameTab), write A (in the layout) and B
// (not), read back through every read route (ListTasks, Snapshot), and require the
// reads to match apart from the literal guess.
//
// Delivery alarms are the one daemon-composed string that embeds a task's target
// and last error, so they are withheld from a sandbox outright (snapshot): that
// is asserted here too, with the reason they are unreachable.
func TestSandboxScope_StoreThenReadDoesNotDependOnMembership(t *testing.T) {
	f := newScopeFixture(t)
	f.useScheduler()
	scrubHits := scrubWatch(t)
	guessA := f.me.Path
	guessB := "/srv/not-a-real-project-b3e1"
	require.NotEqual(t, hostlayout.Default.ScrubString(guessA), guessA, "anti-vacuous: guess A is in the layout")

	writes := map[string]map[string]any{
		"/v1/AddTask":   {"task": map[string]any{"prompt": "p", "cron_expr": "0 * * * *", "target_session": "peer"}},
		"/v1/CreateTab": {"id": f.me.ID, "command": "x"},
		"/v1/RenameTab": {"id": f.me.ID, "tab_name": "nope", "new_name": "n"},
	}
	reads := map[string]string{"/v1/ListTasks": `{}`, "/v1/Snapshot": `{}`}

	stamp := regexp.MustCompile(`\d{4}-\d{2}-\d{2}T[0-9:.+\-Z]+`)
	hexish := regexp.MustCompile(`[0-9a-f]{8,}`)
	clock := regexp.MustCompile(`"operation_clock_ms":\d+`)
	normalize := func(out, guess string) string {
		esc, _ := json.Marshal(guess)
		out = strings.ReplaceAll(out, guess, "<G>")
		out = strings.ReplaceAll(out, string(esc[1:len(esc)-1]), "<G>")
		out = strings.ReplaceAll(out, hostlayout.Withheld, "<W>")
		out = stamp.ReplaceAllString(out, "<T>")
		// The snapshot's operation clock advances between any two reads.
		out = clock.ReplaceAllString(out, `"operation_clock_ms":0`)
		return hexish.ReplaceAllString(out, "<hex>")
	}
	cycle := func(route string, path []string, guess string) map[string]string {
		require.NoError(t, task.DeleteAllTasks())
		body := cloneJSONMap(writes[route])
		setJSONPath(body, path, guess)
		raw, err := json.Marshal(body)
		require.NoError(t, err)
		f.call(t, f.secret, route, string(raw))
		got := map[string]string{}
		for readRoute, readBody := range reads {
			_, out := f.call(t, f.secret, readRoute, readBody)
			got[readRoute] = out
		}
		return got
	}
	checked := 0
	for route := range writes {
		for _, path := range stringFieldPaths(requestTypes[route], nil) {
			a := cycle(route, path, guessA)
			b := cycle(route, path, guessB)
			for readRoute := range reads {
				assert.Equalf(t, normalize(a[readRoute], guessA), normalize(b[readRoute], guessB),
					"write %s field %s, read %s: the read-back depends on whether the stored text is in the host layout", route, strings.Join(path, "."), readRoute)
				checked++
			}
		}
	}
	require.Greater(t, checked, 40, "the store-then-read matrix shrank unexpectedly (%d cases)", checked)
	assert.Emptyf(t, scrubHits(), "the backstop scrub changed a success body: the projection or the classification let a layout-shaped string through")

	// The one daemon-composed string that can embed caller text, a delivery alarm,
	// never reaches a sandbox: watch-task diagnostics are emptied for a sandbox
	// credential (and a sandbox cannot create the watch tasks that raise them).
	_, snap := f.call(t, f.secret, "/v1/Snapshot", `{}`)
	assert.NotContains(t, snap, "delivery_alarms")
	assert.NotContains(t, snap, "skipped_repos")
}

// Every string a sandbox response can carry is classified (sandbox_classify.go):
// reflection over each admitted route's response type, and over what the
// InstanceData and Task projections actually let through, fails on any string
// field with no class, and on any class entry for a field that no longer exists.
func TestSandboxStringFieldsAreAllClassified(t *testing.T) {
	instT, taskT := reflect.TypeFor[session.InstanceData](), reflect.TypeFor[task.Task]()
	isStringKind := func(t reflect.Type) bool { return derefElem(t).Kind() == reflect.String }

	// 1. The response types.
	seen := map[reflect.Type]bool{}
	var walk func(reflect.Type)
	walk = func(rtype reflect.Type) {
		rtype = derefElem(rtype)
		if rtype.Kind() != reflect.Struct || seen[rtype] || rtype == instT || rtype == taskT || rtype == reflect.TypeFor[time.Time]() {
			return
		}
		seen[rtype] = true
		for name, f := range jsonFieldsOf(rtype) {
			if isStringKind(f.typ) {
				_, ok := sandboxStringClasses[f.owner][name]
				assert.Truef(t, ok, "%s.%s is a string a sandbox response can carry but has no class in sandboxStringClasses: declare it daemon-originated (scrubbed) or caller-originated (verbatim), or omit it", f.owner.Name(), name)
			}
			walk(f.typ)
		}
	}
	for _, rt := range servedHTTPRoutes() {
		if rt.sandboxAllowed {
			typ, ok := sandboxResponseTypes[rt.Path]
			require.Truef(t, ok, "%s is admitted to the sandbox scope but has no response type in sandboxResponseTypes", rt.Path)
			walk(typ)
		}
	}

	// 2. What the projections let through, measured: populate EVERY string field,
	// project, and require each surviving one to be classified.
	fill := func(v reflect.Value) {
		for i := 0; i < v.NumField(); i++ {
			if f := v.Field(i); f.CanSet() && f.Kind() == reflect.String {
				f.SetString("x")
			}
		}
	}
	check := func(proj any, owner reflect.Type) {
		pv := reflect.ValueOf(proj)
		for name, f := range jsonFieldsOf(owner) {
			if f.typ.Kind() != reflect.String {
				continue
			}
			var field reflect.Value
			for i := 0; i < pv.NumField(); i++ {
				if owner.Field(i).Tag.Get("json") != "" && strings.Split(owner.Field(i).Tag.Get("json"), ",")[0] == name {
					field = pv.Field(i)
				}
			}
			if field.IsValid() && !field.IsZero() {
				_, ok := sandboxStringClasses[owner][name]
				assert.Truef(t, ok, "%s.%s survives the sandbox projection but has no class in sandboxStringClasses", owner.Name(), name)
			}
		}
	}
	var inst session.InstanceData
	fill(reflect.ValueOf(&inst).Elem())
	check(sandboxSafeInstanceData(inst), instT)
	assert.Empty(t, sandboxSafeInstanceData(inst).Program, "Program is caller-supplied text the sandbox does not need back: omitted")
	var tk task.Task
	fill(reflect.ValueOf(&tk).Elem())
	check(sandboxSafeTask(tk), taskT)

	// 3. No stale entries, and classDaemon is reserved for CLOSED vocabularies:
	// every such entry names the vocabulary it draws from. A free-form daemon string
	// (a path, a message, a tmux name, a name derived from caller text) is
	// classWithheld, never classDaemon.
	for owner, classes := range sandboxStringClasses {
		fields := jsonFieldsOf(owner)
		for name, spec := range classes {
			if spec.class == classDaemon {
				assert.NotEmptyf(t, spec.vocab, "%s.%s is classDaemon but names no closed vocabulary: a free-form daemon string must be withheld", owner.Name(), name)
			} else {
				assert.Emptyf(t, spec.vocab, "%s.%s names a vocabulary but is not classDaemon", owner.Name(), name)
			}
			f, ok := fields[name]
			assert.Truef(t, ok && isStringKind(f.typ), "sandboxStringClasses names %s.%s, which is not a string field", owner.Name(), name)
		}
	}
}

// derefElem unwraps pointers, slices, arrays and maps to the element type.
func derefElem(t reflect.Type) reflect.Type {
	for t.Kind() == reflect.Ptr || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	return t
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
// snapshot projection (#5888). The picked host survives it, so the CLI's post-create
// guard (api/sessions.go) can audit what came back against the --host it sent; the
// daemon-host layout (Path, TmuxName, Worktree) is still withheld.
func TestSandboxCreateResponseIsProjected(t *testing.T) {
	resp := CreateSessionResponse{Instance: session.InstanceData{
		ID: "sess-1", Title: "child", Path: "/home/op/proj", TmuxName: "af_child",
		Host:     "box1",
		Worktree: session.GitWorktreeData{WorktreePath: "/home/op/wt/child", RepoPath: "/home/op/proj"},
	}}
	projectSandboxCreateResponse(&resp)
	raw, err := json.Marshal(resp)
	require.NoError(t, err)
	assert.Equal(t, "sess-1", resp.Instance.ID)
	assert.Equal(t, "child", resp.Instance.Title)
	assert.Equal(t, "box1", resp.Instance.Host, "the picked host survives the projection so the CLI guard does not misread it as version skew")
	for _, leaked := range []string{"/home/op", "af_child"} {
		assert.NotContains(t, string(raw), leaked)
	}
}

// A sandbox create that honors the --host pick must carry the host BACK in the
// create response, or the CLI's post-create guard (api/sessions.go, "did not
// record host") misreads the deliberately scoped/withheld field as version skew
// and reports a false failure for every sandbox-credentialed create (# introduced
// by 3e42066f). This drives the real scoped handler through the gated mux so the
// projection boundary — not a hand-built context — is what the assertion measures.
func TestSandboxCreateResponseCarriesTheHostPickThroughTheProjection(t *testing.T) {
	f := newScopeFixture(t)
	f.useScheduler()
	code, out := f.call(t, f.secret, "/v1/CreateSession", `{"title":"child","host":"box1"}`)
	require.Equalf(t, http.StatusOK, code, "a sandbox create with --host must succeed: %s", out)
	var env struct {
		Data CreateSessionResponse `json:"data"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &env))
	data := env.Data.Instance
	assert.Equal(t, "box1", data.Host, "the picked host must survive the sandbox create-response projection so the CLI guard does not see version skew")

	// The CLI guard's exact condition: it must NOT fire for a create that honored the pick.
	createHostFlag := "box1"
	assert.Falsef(t, createHostFlag != "" && data.Host != createHostFlag,
		"the CLI post-create guard would fire (createHostFlag=%q, data.Host=%q) and report a false failure", createHostFlag, data.Host)

	// The host survives the projection, but the daemon-host LAYOUT it names — the
	// absolute repo root, the worktree, the tmux session — must still be withheld.
	assert.Empty(t, data.Path, "the daemon host's absolute repo root is still withheld")
	assert.Empty(t, data.TmuxName, "the tmux session name is still withheld")
	assert.Empty(t, data.Worktree.WorktreePath, "the worktree path is still withheld")
	assert.Empty(t, data.Worktree.RepoPath, "the worktree's repo root is still withheld")
	assert.Empty(t, data.Program, "the caller's program text is still withheld")
	assert.NotContains(t, out, f.me.Path, "no daemon-host layout path leaked alongside the host name")
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
	decode := func(rec *httptest.ResponseRecorder) (code, storedID string) {
		var env struct {
			Error struct {
				Code     string `json:"code"`
				StoredID string `json:"stored_id"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &env), rec.Body.String())
		return env.Error.Code, env.Error.StoredID
	}
	code, storedID := decode(rec)
	assert.Equal(t, "mutation_committed", code)
	assert.Equal(t, "ab12cd34", storedID)

	rec = httptest.NewRecorder()
	writeHTTPError(rec, r, http.StatusInternalServerError, &mutationCommittedError{err: errors.New("reload failed")})
	code, storedID = decode(rec)
	assert.Equal(t, "mutation_committed", code)
	assert.Empty(t, storedID, "no stored id, none invented")
}

// A daemon path ending in a backslash is withheld whole and the envelope stays
// valid JSON (the longer escaped spelling must win over its raw prefix).
func TestSandboxBodyWithATrailingBackslashPathStaysValidJSON(t *testing.T) {
	hostlayout.Default.Note(`/srv/backslash-test-project\`)
	raw, err := json.Marshal(map[string]any{"data": map[string]any{"last_run_status": `errored: /srv/backslash-test-project\`}, "error": nil})
	require.NoError(t, err)
	out := sanitizeSandboxBody("/v1/x", http.StatusOK, raw)
	require.True(t, json.Valid(out), "%s", out)
	assert.NotContains(t, string(out), "backslash-test-project")
	assert.Contains(t, string(out), hostlayout.Withheld)
}

// No path a sandbox supplies is ever resolved, stat'ed or validated against the
// host: the expectation on a RemoveTask is ignored without being inspected, so the
// same removal gets the same answer whatever the expectation names — existing or
// not, absolute or relative, empty, or malformed.
func TestSandboxRemoveTaskNeverInspectsTheExpectedPath(t *testing.T) {
	f := newScopeFixture(t)
	f.useScheduler()
	for _, expected := range []string{f.me.Path, "/srv/not-a-real-project-b3e1", "/", "", "relative/path", "/nonexistent/../x", "~", "/proc/self/root"} {
		addStoreTask(t, task.Task{ID: "t-expect", Prompt: "p", CronExpr: "0 * * * *", TargetSession: "peer", ProjectPath: f.me.Path})
		body, err := json.Marshal(map[string]any{"id": "t-expect", "expect": map[string]any{"enforce": true, "project_path": expected}})
		require.NoError(t, err)
		code, out := f.call(t, f.secret, "/v1/RemoveTask", string(body))
		assert.Equalf(t, http.StatusOK, code, "expect.project_path=%q must neither block nor change the answer: %s", expected, out)
		_, err = task.GetTask("t-expect")
		assert.Errorf(t, err, "the removal took effect for expect.project_path=%q", expected)
	}
}
