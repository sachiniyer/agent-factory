package daemon

import (
	"net/http"
	"reflect"
	"strings"
)

// The HTTP surface catalog (#1029 PR 5). httpRoutes is the SINGLE SOURCE OF
// TRUTH describing every route the daemon-hosted HTTP/JSON server serves. Two
// consumers read it and only it:
//
//   - newHTTPMux (daemon/httpserver.go) registers exactly these routes, so the
//     server serves the catalog and nothing else.
//   - HTTPRoutes() exports the same list to the `af api` discovery command, so
//     the printed/JSON catalog can never drift from what the server registers.
//
// A drift guard test (httproutes_test.go) proves the mux serves precisely this
// set, so adding a route means adding one entry here — the server and the
// catalog move together by construction.

// HTTPRoute describes one route of the daemon HTTP/JSON API: its verb, path, a
// one-line description, and the JSON request-body field names (derived from the
// RPC request struct, so they cannot drift from the wire shape). The exported
// fields serialize into the `af api --json` catalog; the unexported handler
// binds the route to a live controlServer at mux-build time and never
// serializes.
type HTTPRoute struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description"`
	// RequestFields is the JSON body field names accepted by this route, in
	// declaration order. Nil (omitted) for routes with no body (GET /v1/health)
	// and no-argument POSTs (ListTasks).
	RequestFields []string `json:"request_fields,omitempty"`
	// handler builds the http.HandlerFunc for this route against a controlServer.
	// Unexported: net/json skips it (so it never leaks into the catalog) and
	// importers cannot set it.
	handler func(*controlServer) http.HandlerFunc
}

// httpRoutes is the authoritative route table. Order here is the order the
// `af api` catalog prints; it does not affect ServeMux dispatch.
var httpRoutes = []HTTPRoute{
	// Liveness.
	{
		Method:      http.MethodGet,
		Path:        "/v1/health",
		Description: "Liveness probe (alias for the Ping RPC); answers even while the daemon is restoring sessions.",
		handler:     func(cs *controlServer) http.HandlerFunc { return healthHandler(cs) },
	},

	// Sessions.
	{
		Method:        http.MethodPost,
		Path:          "/v1/CreateSession",
		Description:   "Create a new session (git worktree + agent) in a repo.",
		RequestFields: jsonFields(reflect.TypeOf(CreateSessionRequest{})),
		handler:       func(cs *controlServer) http.HandlerFunc { return rpcHandlerCtx(cs.createSession) },
	},
	{
		Method:        http.MethodPost,
		Path:          "/v1/ListBackends",
		Description:   "List the runtimes a session in this repo can be created on, whether the repo's config supports each, and the backend an unspecified create defaults to.",
		RequestFields: jsonFields(reflect.TypeOf(ListBackendsRequest{})),
		handler:       func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.ListBackends) },
	},
	{
		Method:        http.MethodPost,
		Path:          "/v1/ListPrograms",
		Description:   "List the agent programs a session can be created with, and the program an unspecified create defaults to.",
		RequestFields: jsonFields(reflect.TypeOf(ListProgramsRequest{})),
		handler:       func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.ListPrograms) },
	},
	{
		Method:        http.MethodPost,
		Path:          "/v1/Snapshot",
		Description:   "List sessions from the daemon's authoritative in-memory state (empty repo_id = all repos).",
		RequestFields: jsonFields(reflect.TypeOf(SnapshotRequest{})),
		handler:       func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.Snapshot) },
	},
	{
		Method:        http.MethodPost,
		Path:          "/v1/KillSession",
		Description:   "Tear down a session: kill its tmux/agent and remove its worktree and record.",
		RequestFields: jsonFields(reflect.TypeOf(KillSessionRequest{})),
		handler:       func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.KillSession) },
	},
	{
		Method:        http.MethodPost,
		Path:          "/v1/ArchiveSession",
		Description:   "Archive a session: tear down tmux and relocate its worktree to the archive dir, keeping the record.",
		RequestFields: jsonFields(reflect.TypeOf(ArchiveSessionRequest{})),
		handler:       func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.ArchiveSession) },
	},
	{
		Method:        http.MethodPost,
		Path:          "/v1/RestoreArchived",
		Description:   "Restore an archived session: move its worktree back next to the repo and re-spawn the agent.",
		RequestFields: jsonFields(reflect.TypeOf(RestoreArchivedRequest{})),
		handler:       func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.RestoreArchived) },
	},
	{
		Method:        http.MethodPost,
		Path:          "/v1/RestoreSession",
		Description:   "Restore an archived, Lost, or Dead session.",
		RequestFields: jsonFields(reflect.TypeOf(RestoreSessionRequest{})),
		handler:       func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.RestoreSession) },
	},
	{
		Method:        http.MethodPost,
		Path:          "/v1/SendPrompt",
		Description:   "Send a prompt to an existing session's agent.",
		RequestFields: jsonFields(reflect.TypeOf(SendPromptRequest{})),
		handler:       func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.SendPrompt) },
	},
	// Promoted out of internalHTTPRoutes in #1934 — the follow-up promised in
	// #1592 Phase 2 PR3, which then sat unwritten while the state it exits stayed
	// visible on every surface. A client can only call what HTTPRoutes()
	// advertises, so the web rendered a session as limit-blocked — its own glyph,
	// label and title prefix — and offered no way out. The STATE was deliberately
	// surfaced on all three surfaces; the EXIT existed on one.
	{
		Method:        http.MethodPost,
		Path:          "/v1/ResumeFromLimit",
		Description:   "Resume a usage-limit-blocked session: re-spawn if needed, re-deliver the pending prompt, clear the limit.",
		RequestFields: jsonFields(reflect.TypeOf(ResumeFromLimitRequest{})),
		handler:       func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.ResumeFromLimit) },
	},
	{
		Method:        http.MethodPost,
		Path:          "/v1/HandoffSession",
		Description:   "Continue a session under a different agent, in place: swap its agent program, keep its worktree and branch, and deliver a mission brief to the new agent.",
		RequestFields: jsonFields(reflect.TypeOf(HandoffSessionRequest{})),
		handler:       func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.HandoffSession) },
	},
	{
		Method:        http.MethodPost,
		Path:          "/v1/DeleteProject",
		Description:   "Delete a project (a repo's session grouping): archive its live sessions (restorable), tear down in-place ones, and drop its root_agents opt-in — the real git repo is untouched.",
		RequestFields: jsonFields(reflect.TypeOf(DeleteProjectRequest{})),
		handler:       func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.DeleteProject) },
	},
	{
		Method:        http.MethodPost,
		Path:          "/v1/DeliverPrompt",
		Description:   "Deliver a prompt to a session, auto-creating it if it does not exist yet.",
		RequestFields: jsonFields(reflect.TypeOf(DeliverPromptRequest{})),
		handler:       func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.DeliverPrompt) },
	},
	{
		Method:        http.MethodPost,
		Path:          "/v1/CreateTab",
		Description:   "Spawn a tab in a session: a process tab (command) or shell tab in the worktree, a web tab (kind=web) that iframes a url/port (localhost is daemon-proxied, external is direct), or a VS Code tab (kind=vscode) serving the session's worktree in a daemon-managed code-server (no url/port: the worktree is the target).",
		RequestFields: jsonFields(reflect.TypeOf(CreateTabRequest{})),
		handler:       func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.CreateTab) },
	},
	{
		Method:        http.MethodPost,
		Path:          "/v1/CloseTab",
		Description:   "Close a non-agent tab of a session (the agent tab cannot be closed). Address the tab by tab_id (its stable id) when you have one: it wins over tab_name/tab_index, which name a tab that may since have been closed and had its name or slot reused. A tab_id that no longer resolves is refused rather than falling back — closing is destructive, so a misroute kills the wrong tab's session.",
		RequestFields: jsonFields(reflect.TypeOf(CloseTabRequest{})),
		handler:       func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.CloseTab) },
	},
	{
		Method:        http.MethodPost,
		Path:          "/v1/RenameTab",
		Description:   "Rename a tab of a session. Only web, process and VS Code tabs can be renamed — agent and shell tabs render fixed labels. The name is sanitized and made unique, so the resolved name is returned. Address the tab by tab_id (its stable id) when you have one: it wins over tab_name/tab_index, which name a tab that may since have been closed and had its name or slot reused. A tab_id that no longer resolves is refused rather than falling back.",
		RequestFields: jsonFields(reflect.TypeOf(RenameTabRequest{})),
		handler:       func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.RenameTab) },
	},
	{
		Method:        http.MethodPost,
		Path:          "/v1/ReorderTab",
		Description:   "Move a tab within a session's roster. Index 0 is reserved for the agent tab, so only slots 1..n-1 can be moved or targeted. Address the tab by tab_id (its stable id) when you have one — see RenameTab; it matters most here, since a reorder is what invalidates every other client's tab_index.",
		RequestFields: jsonFields(reflect.TypeOf(ReorderTabRequest{})),
		handler:       func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.ReorderTab) },
	},
	{
		Method:        http.MethodPost,
		Path:          "/v1/SetPRInfo",
		Description:   "Record or clear the GitHub PR info for a session.",
		RequestFields: jsonFields(reflect.TypeOf(SetPRInfoRequest{})),
		handler:       func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.SetPRInfo) },
	},

	// Config. The read/write pair behind the web config editor; both are thin
	// wrappers over the same config package calls the TUI and `af config set`
	// make in-process, so the three surfaces cannot validate or write differently.
	{
		Method:        http.MethodPost,
		Path:          "/v1/GetConfig",
		Description:   "List every user-facing global config key with its purpose, type, default, and current value.",
		RequestFields: jsonFields(reflect.TypeOf(GetConfigRequest{})),
		handler:       func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.GetConfig) },
	},
	{
		Method:        http.MethodPost,
		Path:          "/v1/SetConfigValue",
		Description:   "Set one global config key, exactly as `af config set` does (validated, locked, atomic).",
		RequestFields: jsonFields(reflect.TypeOf(SetConfigValueRequest{})),
		handler:       func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.SetConfigValue) },
	},

	// Tasks.
	{
		Method:        http.MethodPost,
		Path:          "/v1/ListTasks",
		Description:   "List every task across all repos.",
		RequestFields: jsonFields(reflect.TypeOf(ListTasksRequest{})),
		handler:       func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.ListTasks) },
	},
	{
		Method:        http.MethodPost,
		Path:          "/v1/AddTask",
		Description:   "Append a new task and re-arm the scheduler.",
		RequestFields: jsonFields(reflect.TypeOf(AddTaskRequest{})),
		handler:       func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.AddTask) },
	},
	{
		Method:        http.MethodPost,
		Path:          "/v1/UpdateTask",
		Description:   "Apply a field-level patch to a task (only the fields in `update` are changed), preserving every unspecified field and the scheduler-owned fields.",
		RequestFields: jsonFields(reflect.TypeOf(UpdateTaskRequest{})),
		handler:       func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.UpdateTask) },
	},
	{
		Method:        http.MethodPost,
		Path:          "/v1/RemoveTask",
		Description:   "Remove a task by ID.",
		RequestFields: jsonFields(reflect.TypeOf(RemoveTaskRequest{})),
		handler:       func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.RemoveTask) },
	},
	{
		Method:        http.MethodPost,
		Path:          "/v1/TriggerTask",
		Description:   "Fire a cron task now through the daemon's scheduler path (refuses disabled and watch tasks).",
		RequestFields: jsonFields(reflect.TypeOf(TriggerTaskRequest{})),
		handler:       func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.TriggerTask) },
	},
}

// internalHTTPRoutes are routes the daemon SERVES over HTTP but deliberately
// keeps OUT of the public `af api` catalog (#1592 Phase 2 PR3). They exist so
// the TUI can drop net/rpc entirely and reach every verb it drives over HTTP,
// without advertising daemon-internal coordination as public API. newHTTPMux
// registers these alongside httpRoutes, but HTTPRoutes() (the `af api` catalog)
// returns only httpRoutes, so the discovery surface stays exactly the
// client-facing session/task ops it promised.
//
// ResumeFromLimit USED to live here, parked with a note calling it "a genuine
// client-facing session verb" whose promotion was "a one-line follow-up". That
// follow-up went unwritten for long enough that the web shipped the limit state
// with no way out of it (#1934), so it is now in httpRoutes above. The lesson is
// worth leaving here: a client-facing verb parked in this table is invisible to
// every client that is not the TUI, and nothing fails when it stays parked.
//
// What remains belongs here PERMANENTLY, not provisionally:
//   - Preview is the daemon-sole-capturer render path, an implementation detail of
//     how the TUI draws, not something a user scripts.
//   - Pause/ResumeStatusPoll are attach-coordination infra (best-effort poll
//     leases, #1160) that no CLI user should call.
//
// Before adding to this table, ask whether the verb is infra or merely
// unfinished. If a user could reasonably want it, it goes above.
var internalHTTPRoutes = []HTTPRoute{
	{
		Method:        http.MethodPost,
		Path:          "/v1/Preview",
		Description:   "Capture a session tab's content (daemon-sole-capturer render path for the TUI: remote/scroll/preview).",
		RequestFields: jsonFields(reflect.TypeOf(PreviewRequest{})),
		handler:       func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.Preview) },
	},
	{
		Method:        http.MethodPost,
		Path:          "/v1/PauseStatusPoll",
		Description:   "Pause the daemon's liveness poll for one attached session (best-effort attach coordination).",
		RequestFields: jsonFields(reflect.TypeOf(PauseStatusPollRequest{})),
		handler:       func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.PauseStatusPoll) },
	},
	{
		Method:        http.MethodPost,
		Path:          "/v1/ResumeStatusPoll",
		Description:   "Resume the daemon's liveness poll for a session on a clean detach.",
		RequestFields: jsonFields(reflect.TypeOf(ResumeStatusPollRequest{})),
		handler:       func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.ResumeStatusPoll) },
	},
}

// servedHTTPRoutes is every route newHTTPMux registers: the public catalog plus
// the internal routes. The mux serves this union; HTTPRoutes() exposes only the
// public half. Keeping them as one concatenation here means "what is served" has
// a single definition the drift-guard test locks against.
func servedHTTPRoutes() []HTTPRoute {
	out := make([]HTTPRoute, 0, len(httpRoutes)+len(internalHTTPRoutes))
	out = append(out, httpRoutes...)
	out = append(out, internalHTTPRoutes...)
	return out
}

// HTTPRoutes returns a copy of the PUBLIC HTTP/JSON API catalog for discovery
// (`af api`). It is a pure, read-only description of the client-facing routes:
// it does NOT dial the socket or spawn the daemon, and it deliberately excludes
// internalHTTPRoutes so the advertised surface stays client-facing-only. The
// copy protects the internal table from mutation by callers.
func HTTPRoutes() []HTTPRoute {
	out := make([]HTTPRoute, len(httpRoutes))
	copy(out, httpRoutes)
	return out
}

// jsonFields returns the JSON body field names of an RPC request struct in
// declaration order, deriving the catalog's request_fields straight from the
// wire structs so they can never drift from what the server decodes. Unexported
// fields (net/rpc's gob and encoding/json both skip them) and json:"-" fields
// are omitted.
func jsonFields(t reflect.Type) []string {
	var fields []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported
			continue
		}
		name := strings.Split(f.Tag.Get("json"), ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		fields = append(fields, name)
	}
	return fields
}
