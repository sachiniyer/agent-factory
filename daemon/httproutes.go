package daemon

import (
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"time"
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
	// sandboxAllowed opts this route into the SCOPED sandbox callback credential
	// (#2999). Zero value false means DENIED, deliberately: a route added later is
	// unreachable by a sandbox until someone consciously opts it in. A denylist
	// would grant every future route by default, which is the wrong direction for
	// a credential handed to a machine af provisioned but does not trust.
	//
	// THE RULE FOR ADDING ONE, and it took two review rounds to state correctly
	// (#3012). A route may be opted in only if an attacker who has taken the
	// sandbox cannot use it to gain authority over the HOST, or to learn the
	// operator's private layout. What remains is capability DISCOVERY: what this
	// daemon could offer, never what the operator has or what it will go do.
	//
	// The rule was first written as "it cannot name another session", which is a
	// SYMPTOM of the real predicate and let CreateSession through — it names no
	// session and still starts an agent on the host with an attacker's repo_path,
	// program, and prompt, which is the host-side instruction authority denying
	// DeliverPrompt exists to withhold. Naming is one route to that authority, not
	// the definition of it. Test the authority, not the shape of the request.
	//
	// Denied by this rule, with the reason each is tempting:
	//
	//   - CreateSession — no session named; starts one on the host anyway.
	//   - SendPrompt, CreateTab, AddTask (via Task.TargetSession) — reach an
	//     existing agent, reproducing DeliverPrompt one at a time.
	//   - Snapshot — the enumeration that makes the above aimable.
	//   - ListProjects, ListDirectory — read-only, and still hand a compromised
	//     sandbox the operator's absolute host paths and an arbitrary directory
	//     walk of the daemon's filesystem. Reconnaissance is authority's
	//     precursor, and a sandbox has no use for the HOST's tree: the picker
	//     that consumes ListDirectory (#2788) is the operator's own client,
	//     holding the operator's own token.
	//   - ListBackends, ListPrograms — the same oracle in a quieter form, and the
	//     reason this list is shorter than it looks like it should be. Both take a
	//     caller-supplied repo_path and answer through config.RepoFromPath, which
	//     wraps git's own stderr: a caller learns "No such file or directory" vs
	//     "Permission denied" vs "not a git repository" for any path it guesses,
	//     and on success learns where the enclosing git root is. Confirming a
	//     guessed path is weaker than enumerating one, but it is the same class,
	//     and a rule that admits it is a rule with an exception carved for
	//     convenience.
	//
	//   - SuggestSessionName — the last one standing under the old scheme, and it
	//     fell too. It takes no arguments and returns a random unused name, which
	//     looks like the one harmless thing on the surface. But it avoids every live
	//     title ACROSS ALL REPOS (its handler says so), the wordlist is finite, and
	//     the sandbox holds that wordlist in the `af` binary af itself put there —
	//     so sampling until the free combinations run out reveals which ones are
	//     persistently missing, i.e. the operator's live session titles and their
	//     activity. An oracle can be a route that returns nothing about its own
	//     answer.
	//
	// THAT EMPTIED THE TABLE, which is what #3056 changed. Both halves of the
	// surface had failed for opposite reasons: parameterised routes need "the
	// caller's OWN repo/session", which a flag cannot express, and parameterless
	// routes answer from global state because it is the only state they have.
	//
	// THE CONTRACT NOW, and it is two conditions, not one:
	//
	//  1. This flag admits a route to the SCOPE. It does not authorize a request.
	//  2. The route's HANDLER must enforce its own owner constraint, reading
	//     sandboxOwner(ctx) — the session the presented credential belongs to,
	//     carried from the gate (see sandbox_owner.go).
	//
	// A route admitted under (1) without (2) is a boundary that reads as enforced
	// and is not. That is not left to a reader: sandboxConstrainedRoutes names every
	// route that has a constraint, and a test asserts it equals this table in both
	// directions, so opting a route in without giving it one FAILS. The rule used to
	// be prose here, and prose is exactly what the four rounds above drifted from.
	sandboxAllowed bool
	// requestType is the RPC request struct this route decodes, kept so a consumer
	// that needs the FULL body shape can reflect it rather than re-deriving a
	// second, driftable list. RequestFields above is computed FROM it (see
	// HTTPRoutes), so the published top-level names and the type can never
	// disagree. Unexported for the same reason as handler: it never serializes.
	requestType reflect.Type
}

// RequestFieldPaths returns the request body's JSON field paths, RECURSING into
// struct-typed fields as dotted paths ("update.enabled"), in declaration order.
//
// This exists because RequestFields stops at depth 1, which published a route
// whose body is a nested object as a single opaque key: `AddTask` advertised
// `task` and nothing more, so the reference could not be used to construct a
// valid request at all (#2918). A caller who read it and sent a flat payload got
// a confusing rejection.
//
// Deliberately a METHOD rather than a field: encoding/json ignores methods, so
// `af api --json` — a published contract — stays byte-identical while the
// generated reference gains the shape it was missing.
func (r HTTPRoute) RequestFieldPaths() []string {
	return jsonFieldPaths(r.requestType, 0)
}

// httpRoutes is the authoritative route table. Order here is the order the
// `af api` catalog prints; it does not affect ServeMux dispatch.
var httpRoutes = []HTTPRoute{
	// Liveness.
	{
		Method:      http.MethodGet,
		Path:        "/v1/health",
		Description: "Lifecycle health probe (alias for Ping): version, boot/transaction identity, phase, and bound listeners; answers before readiness.",
		handler:     func(cs *controlServer) http.HandlerFunc { return healthHandler(cs) },
	},

	// Sessions.
	{
		Method:      http.MethodPost,
		Path:        "/v1/CreateSession",
		Description: "Create a new session (git worktree + agent) in a repo.",
		requestType: reflect.TypeOf(CreateSessionRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandlerCtx(cs.createSession) },
	},
	{
		Method:      http.MethodPost,
		Path:        "/v1/ListBackends",
		Description: "List the runtimes a session in this repo can be created on, whether the repo's config supports each, and the backend an unspecified create defaults to.",
		requestType: reflect.TypeOf(ListBackendsRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.ListBackends) },
	},
	{
		Method:      http.MethodPost,
		Path:        "/v1/ListPrograms",
		Description: "List the agent programs a session can be created with, and the program an unspecified create defaults to.",
		requestType: reflect.TypeOf(ListProgramsRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.ListPrograms) },
	},
	{
		Method:      http.MethodPost,
		Path:        "/v1/SuggestSessionName",
		Description: "Suggest a random, readable session name (adjective-noun) not used by any live session, for the create form's autocreate placeholder.",
		requestType: reflect.TypeOf(SuggestSessionNameRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.SuggestSessionName) },
	},
	{
		Method:         http.MethodPost,
		Path:           "/v1/Snapshot",
		sandboxAllowed: true,
		Description:    "List sessions from the daemon's authoritative in-memory state (empty repo_id = all repos).",
		requestType:    reflect.TypeOf(SnapshotRequest{}),
		// rpcHandlerCtx, not rpcHandler: the handler needs the request context to
		// see whether the caller is a sandbox and narrow to its own session (#3056).
		handler: func(cs *controlServer) http.HandlerFunc { return rpcHandlerCtx(cs.snapshot) },
	},
	{
		Method:      http.MethodPost,
		Path:        "/v1/KillSession",
		Description: "Tear down a session: kill its tmux/agent and remove its worktree and record.",
		requestType: reflect.TypeOf(KillSessionRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandlerCtx(cs.killSession) },
	},
	{
		Method:      http.MethodPost,
		Path:        "/v1/ArchiveSession",
		Description: "Archive a session: tear down tmux and relocate its worktree to the archive dir, keeping the record; refused before mutation when enabled tasks target it.",
		requestType: reflect.TypeOf(ArchiveSessionRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.ArchiveSession) },
	},
	{
		Method:      http.MethodPost,
		Path:        "/v1/RestoreArchived",
		Description: "Restore an archived session: move its worktree back next to the repo and re-spawn the agent.",
		requestType: reflect.TypeOf(RestoreArchivedRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.RestoreArchived) },
	},
	{
		Method:      http.MethodPost,
		Path:        "/v1/RestoreSession",
		Description: "Restore an archived, Lost, or Dead session.",
		requestType: reflect.TypeOf(RestoreSessionRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.RestoreSession) },
	},
	{
		Method:      http.MethodPost,
		Path:        "/v1/SendPrompt",
		Description: "Send a prompt to an existing session's agent.",
		requestType: reflect.TypeOf(SendPromptRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.SendPrompt) },
	},
	// Promoted out of internalHTTPRoutes in #1934 — the follow-up promised in
	// #1592 Phase 2 PR3, which then sat unwritten while the state it exits stayed
	// visible on every surface. A client can only call what HTTPRoutes()
	// advertises, so the web rendered a session as limit-blocked — its own glyph,
	// label and title prefix — and offered no way out. The STATE was deliberately
	// surfaced on all three surfaces; the EXIT existed on one.
	{
		Method:      http.MethodPost,
		Path:        "/v1/ResumeFromLimit",
		Description: "Resume a usage-limit-blocked session: re-spawn if needed, re-deliver the pending prompt, clear the limit.",
		requestType: reflect.TypeOf(ResumeFromLimitRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.ResumeFromLimit) },
	},
	{
		Method:      http.MethodPost,
		Path:        "/v1/HandoffSession",
		Description: "Continue a session under a different agent, in place: swap its agent program, keep its worktree and branch, and deliver a mission brief to the new agent.",
		requestType: reflect.TypeOf(HandoffSessionRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.HandoffSession) },
	},
	{
		Method:      http.MethodPost,
		Path:        "/v1/DeleteProject",
		Description: "Delete a project (a repo's session grouping): archive its live sessions (restorable), tear down in-place ones, and drop its root_agents opt-in — the real git repo is untouched.",
		requestType: reflect.TypeOf(DeleteProjectRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.DeleteProject) },
	},
	{
		Method:      http.MethodPost,
		Path:        "/v1/RegisterProject",
		Description: "Register a git checkout as a durable, sessionless project by path (expand ~, resolve the git root, validate, persist to the registry) — resolved on the daemon's filesystem, idempotent for a known checkout.",
		requestType: reflect.TypeOf(RegisterProjectRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.RegisterProject) },
	},
	{
		Method:      http.MethodPost,
		Path:        "/v1/ListProjects",
		Description: "List every durable project in the daemon's registry (id, last-known root, path_exists) — the read a web/TUI client unions with its derived project list.",
		requestType: reflect.TypeOf(ListProjectsRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.ListProjects) },
	},
	// The Add-project picker's read (#2788). PUBLIC, not internal: a client that
	// cannot see the daemon host's filesystem needs it to offer anything better
	// than "type an absolute path", so it is client-facing by definition — and
	// the internalHTTPRoutes note below is explicit that a verb a user could
	// reasonably want belongs here.
	{
		Method:      http.MethodPost,
		Path:        "/v1/ListDirectory",
		Description: "List the child DIRECTORIES of one directory on the daemon's filesystem, marking which are git checkouts — the read behind an Add-project picker. Resolves ~ and symlinks and answers with canonical paths; an unreadable directory is an error, never an empty list.",
		requestType: reflect.TypeOf(ListDirectoryRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.ListDirectory) },
	},
	{
		Method:      http.MethodPost,
		Path:        "/v1/DeliverPrompt",
		Description: "Deliver a prompt to a session, auto-creating it if it does not exist yet.",
		requestType: reflect.TypeOf(DeliverPromptRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.DeliverPrompt) },
	},
	{
		Method:      http.MethodPost,
		Path:        "/v1/CreateTab",
		Description: "Spawn a tab in a session: a process tab (command) or shell tab in the worktree, a web tab (kind=web) that iframes a url/port (localhost is daemon-proxied, external is direct), or a VS Code tab (kind=vscode) serving the session's worktree in a daemon-managed code-server (no url/port: the worktree is the target).",
		requestType: reflect.TypeOf(CreateTabRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.CreateTab) },
	},
	{
		Method:      http.MethodPost,
		Path:        "/v1/CloseTab",
		Description: "Close a non-agent tab of a session (the agent tab cannot be closed). Address the tab by tab_id (its stable id) when you have one: it wins over tab_name/tab_index, which name a tab that may since have been closed and had its name or slot reused. A tab_id that no longer resolves is refused rather than falling back — closing is destructive, so a misroute kills the wrong tab's session.",
		requestType: reflect.TypeOf(CloseTabRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandlerCtx(cs.closeTab) },
	},
	{
		Method:      http.MethodPost,
		Path:        "/v1/RenameTab",
		Description: "Rename a tab of a session. Only web, process and VS Code tabs can be renamed — agent and shell tabs render fixed labels. The name is sanitized and made unique, so the resolved name is returned. Address the tab by tab_id (its stable id) when you have one: it wins over tab_name/tab_index, which name a tab that may since have been closed and had its name or slot reused. A tab_id that no longer resolves is refused rather than falling back.",
		requestType: reflect.TypeOf(RenameTabRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.RenameTab) },
	},
	{
		Method:      http.MethodPost,
		Path:        "/v1/ReorderTab",
		Description: "Move a tab within a session's roster. Index 0 is reserved for the agent tab, so only slots 1..n-1 can be moved or targeted. Address the tab by tab_id (its stable id) when you have one — see RenameTab; it matters most here, since a reorder is what invalidates every other client's tab_index.",
		requestType: reflect.TypeOf(ReorderTabRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.ReorderTab) },
	},
	{
		Method:      http.MethodPost,
		Path:        "/v1/SetPRInfo",
		Description: "Compatibility route for older clients to record or clear GitHub PR info. New clients should call RefreshPRInfo with session identity only so discovery and projected fields stay daemon-owned.",
		requestType: reflect.TypeOf(SetPRInfoRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.SetPRInfo) },
	},
	{
		Method:      http.MethodPost,
		Path:        "/v1/RefreshPRInfo",
		Description: "Ask the daemon to refresh a session's GitHub PR projection. The request carries session identity only; discovery is cancellable and server-side debounced. Returns an error when gh is unavailable in the daemon environment.",
		requestType: reflect.TypeOf(RefreshPRInfoRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandlerCtx(cs.refreshPRInfo) },
	},

	// Config. The read/write pair behind the web config editor; both are thin
	// wrappers over the same config package calls the TUI and `af config set`
	// make in-process, so the three surfaces cannot validate or write differently.
	{
		Method:      http.MethodPost,
		Path:        "/v1/GetConfig",
		Description: "List every user-facing global config key with its purpose, type, default, and current value.",
		requestType: reflect.TypeOf(GetConfigRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.GetConfig) },
	},
	{
		Method:      http.MethodPost,
		Path:        "/v1/GetTheme",
		Description: "Return the daemon's resolved semantic color palette for renderer clients.",
		requestType: reflect.TypeOf(GetThemeRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.GetTheme) },
	},
	{
		Method:      http.MethodPost,
		Path:        "/v1/SetConfigValue",
		Description: "Set one global config key, exactly as `af config set` does (validated, locked, atomic).",
		requestType: reflect.TypeOf(SetConfigValueRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.SetConfigValue) },
	},

	// Tasks.
	{
		Method:      http.MethodPost,
		Path:        "/v1/ListTasks",
		Description: "List every task across all repos.",
		requestType: reflect.TypeOf(ListTasksRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.ListTasks) },
	},
	{
		Method:      http.MethodPost,
		Path:        "/v1/AddTask",
		Description: "Append a new task and re-arm the scheduler; an enabled archived/archiving target_session is refused before commit.",
		requestType: reflect.TypeOf(AddTaskRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.AddTask) },
	},
	{
		Method:      http.MethodPost,
		Path:        "/v1/UpdateTask",
		Description: "Apply a field-level patch to a task (only the fields in `update` are changed), preserving every unspecified field and the scheduler-owned fields; an enabled archived/archiving target_session is refused before commit.",
		requestType: reflect.TypeOf(UpdateTaskRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.UpdateTask) },
	},
	{
		Method:      http.MethodPost,
		Path:        "/v1/RemoveTask",
		Description: "Remove a task by ID.",
		requestType: reflect.TypeOf(RemoveTaskRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.RemoveTask) },
	},
	{
		Method:      http.MethodPost,
		Path:        "/v1/RestartTask",
		Description: "Stop and replace one enabled watch task without overlapping its process tree.",
		requestType: reflect.TypeOf(RestartTaskRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.RestartTask) },
	},
	{
		Method:      http.MethodPost,
		Path:        "/v1/TriggerTask",
		Description: "Fire a cron task now through the daemon's scheduler path (refuses disabled and watch tasks).",
		requestType: reflect.TypeOf(TriggerTaskRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.TriggerTask) },
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
		Method:      http.MethodPost,
		Path:        "/v1/Preview",
		Description: "Capture a session tab's content (daemon-sole-capturer render path for the TUI: remote/scroll/preview).",
		requestType: reflect.TypeOf(PreviewRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.Preview) },
	},
	{
		Method:      http.MethodPost,
		Path:        "/v1/PauseStatusPoll",
		Description: "Pause the daemon's liveness poll for one attached session (best-effort attach coordination).",
		requestType: reflect.TypeOf(PauseStatusPollRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.PauseStatusPoll) },
	},
	{
		Method:      http.MethodPost,
		Path:        "/v1/ResumeStatusPoll",
		Description: "Resume the daemon's liveness poll for a session on a clean detach.",
		requestType: reflect.TypeOf(ResumeStatusPollRequest{}),
		handler:     func(cs *controlServer) http.HandlerFunc { return rpcHandler(cs.ResumeStatusPoll) },
	},
}

// sandboxAllowedPath reports whether a sandbox callback credential may call the
// given request path (#2999).
//
// Derived from the served table rather than a second list: a route's capability
// is declared beside the route, so there is no separate inventory to fall out of
// step with it. Anything not in the table — the WS stream planes, the
// config-assistant, the webtab proxy, the catch-all — is denied by falling
// through, which is the same default the table itself applies.
func sandboxAllowedPath(path string) bool {
	return sandboxAllowedPaths[path]
}

// sandboxAllowedPaths is the derived lookup, filled in init() rather than by a
// var initializer. That is not style: httpRoutes' initializer transitively
// references the auth gate, which references this, and Go rejects the resulting
// package-variable initialization cycle. Filling it in init() — which runs after
// variable initialization — keeps the capability declared beside its route while
// leaving the lookup a plain map read.
var sandboxAllowedPaths = map[string]bool{}

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

// init derives every route's published top-level field names from its
// requestType, IN PLACE in the authoritative tables.
//
// In place, not in the copies the accessors hand out (#2968 review): httpRoutes
// is documented as the single source of truth and is read directly — by
// TestHTTPRoutes_RequestFieldsMatchWireStruct among others — so populating only
// the copies would leave the authoritative table saying something different from
// what every consumer sees. Deriving here rather than writing the list into each
// entry keeps ONE statement of a route's request shape, which is its type, so
// the published names cannot drift from the wire struct.
func init() {
	fillRequestFields(httpRoutes)
	fillRequestFields(internalHTTPRoutes)
	for _, rt := range servedHTTPRoutes() {
		if rt.sandboxAllowed {
			sandboxAllowedPaths[rt.Path] = true
		}
	}
}

func fillRequestFields(routes []HTTPRoute) {
	for i := range routes {
		routes[i].RequestFields = jsonFields(routes[i].requestType)
	}
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

// maxRequestNestDepth bounds the recursive walk. Request payloads are shallow;
// this only stops a self-referential type from looping.
const maxRequestNestDepth = 3

// jsonFieldPaths returns a request struct's JSON field paths in declaration
// order, descending into struct-typed fields and joining with ".".
//
// Slices and maps are NOT descended: their element shape is a separate question
// from "what keys does this body take", and rendering `tasks[].id` in a
// reference that has no array syntax elsewhere would invent notation. Pointers
// are followed, since a *T body field is the same object shape as T.
func jsonFieldPaths(t reflect.Type, depth int) []string {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct || depth > maxRequestNestDepth {
		return nil
	}
	var paths []string
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
		paths = append(paths, name)

		ft := f.Type
		for ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() != reflect.Struct || isOpaqueJSONStruct(ft) {
			continue
		}
		for _, nested := range jsonFieldPaths(ft, depth+1) {
			paths = append(paths, name+"."+nested)
		}
	}
	return paths
}

// isOpaqueJSONStruct reports structs that marshal as a single value despite being
// structs, so their internals are not keys a caller sets. time.Time is the one
// that matters here; anything implementing json.Marshaler is treated the same,
// because whatever it emits is not its field layout.
func isOpaqueJSONStruct(t reflect.Type) bool {
	if t == reflect.TypeOf(time.Time{}) {
		return true
	}
	marshaler := reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	return t.Implements(marshaler) || reflect.PointerTo(t).Implements(marshaler)
}

// jsonFields returns the JSON body field names of an RPC request struct in
// declaration order, deriving the catalog's request_fields straight from the
// wire structs so they can never drift from what the server decodes. Unexported
// fields (net/rpc's gob and encoding/json both skip them) and json:"-" fields
// are omitted.
func jsonFields(t reflect.Type) []string {
	if t == nil {
		return nil
	}
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
