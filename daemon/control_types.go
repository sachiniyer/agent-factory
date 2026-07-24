package daemon

import (
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// CreateSessionRequest is the daemon-owned session creation contract used by
// the TUI, CLI, and scheduled task runner.
//
// The json tags on this and the other RPC request/response structs are for the
// daemon-hosted HTTP server (#1029 PR 4): they define the HTTP JSON body shape.
// The existing net/rpc control socket is unaffected — gob encoding ignores json
// tags entirely and keys off the Go field names.
type CreateSessionRequest struct {
	Title     string `json:"title"`
	TitleBase string `json:"title_base"`
	RepoPath  string `json:"repo_path"`
	Program   string `json:"program"`
	Prompt    string `json:"prompt"`
	// TaskID records which task's delivery spawned this session, and
	// MaxConcurrentRuns carries that task's cap so the manager can decide
	// admission under its own lock — the only place a burst cannot race the check
	// against the create (#1892). TaskID is persisted on the instance so the cap
	// counts a task's in-flight sessions by provenance rather than by a title
	// prefix, and so the count survives a daemon restart.
	//
	// Both are `json:"-"`: they ride the net/rpc GOB control socket (which encodes
	// exported fields and ignores json tags), so the daemon's own task-delivery
	// loopback still carries them, while the HTTP/JSON plane — the user-facing
	// surface, reachable over TCP with a token since #1592 — cannot set them at
	// all. encoding/json drops a "-" field on decode, and jsonFields skips it, so
	// it is neither accepted nor advertised in the route catalog.
	//
	// That boundary is the point: provenance is an assertion the daemon makes
	// about its own delivery, never a claim a client gets to make. Were it
	// settable over HTTP, anyone could create an ordinary session tagged with a
	// capped task's id and have countTaskRunsLocked charge it against that task —
	// consuming its slots and parking its events, from a session that task never
	// spawned.
	TaskID            string `json:"-"`
	MaxConcurrentRuns int    `json:"-"`
	// InPlace attaches the session to the repo's existing working tree at its
	// current branch (`af sessions create --here`) instead of creating a new
	// git worktree+branch; kill/cleanup leaves the user's tree and branch
	// intact. Incompatible with ForceRemote.
	InPlace     bool `json:"in_place"`
	ForceRemote bool `json:"force_remote"`
	// Backend explicitly selects the session's runtime (the `--backend` create
	// flag): one of local|docker|ssh|hook. Empty resolves from the repo's
	// `backend` config key, defaulting to local.
	Backend string `json:"backend,omitempty"`

	// allowReserved lets the daemon's own root-agent ensure loop (#1106)
	// create the reserved "root" title that reserveCreate rejects for
	// everyone else. Deliberately unexported: net/rpc's gob encoding skips
	// unexported fields, so no RPC client (TUI, CLI, API) can ever set it —
	// only in-process daemon code can.
	allowReserved bool
}

type CreateSessionResponse struct {
	Instance session.InstanceData `json:"instance"`
}

type KillSessionRequest struct {
	Title  string `json:"title"`
	RepoID string `json:"repo_id"`
	// ID is the session's stable id (session.InstanceData.ID). When non-empty it
	// is the PRIMARY lookup key: the daemon resolves the target by id first and
	// only falls back to {Title, RepoID} when it is empty. Web clients send it so
	// a duplicate title across repos can't target the wrong session on this
	// destructive action — the write-path analogue of the id-keyed read/stream
	// paths (#1592 Phase 5 PR5). Retained TUI actions send it; one-shot CLI
	// callers resolve by repo-scoped title.
	ID string `json:"id"`
	// No force field: kill always destroys the session since the unmerged-work
	// guard was dropped (#1579). The CLI `--force` flag is accepted as a no-op
	// but is never sent to the daemon, so the request shape exposes no
	// misleading "safer kill" knob. Use ArchiveSession to keep a session
	// restorable instead.
}

type KillSessionResponse struct {
	OK bool `json:"ok"`
}

// ArchiveSessionRequest asks the daemon to archive a session (#1028): tear down
// its tmux, relocate its worktree to the global archive dir, and mark it
// Archived while preserving the record.
type ArchiveSessionRequest struct {
	Title  string `json:"title"`
	RepoID string `json:"repo_id"`
	// ID is the session's stable id; see KillSessionRequest.ID. When non-empty
	// the daemon resolves the archive target by id first, so a web archive can't
	// hit the wrong session under a cross-repo title collision (#1592 Phase 5
	// follow-up). Retained TUI actions send it; one-shot CLI callers resolve by
	// {Title, RepoID}.
	ID string `json:"id"`
}

type ArchiveSessionResponse struct {
	OK bool `json:"ok"`
	// ArchivedPath is the new on-disk location of the relocated worktree.
	ArchivedPath string `json:"archived_path"`
}

// RestoreArchivedRequest asks the daemon to restore an archived session (#1028):
// move its worktree back next to the repo, re-spawn the agent, and mark it
// Running.
type RestoreArchivedRequest struct {
	Title  string `json:"title"`
	RepoID string `json:"repo_id"`
	// ID is the session's stable id; see KillSessionRequest.ID. When non-empty
	// the daemon resolves the archived row by id first and never falls back to a
	// same-title replacement. Title+repo remains the one-shot CLI compatibility
	// path.
	ID string `json:"id"`
}

type RestoreArchivedResponse struct {
	OK bool `json:"ok"`
	// WorktreePath is the on-disk location the worktree was restored to.
	WorktreePath string `json:"worktree_path"`
}

// RestoreSessionRequest asks the daemon to restore a restorable session:
// archived sessions are moved back from the archive, while Lost/Dead sessions
// are recovered in place.
type RestoreSessionRequest struct {
	Title  string `json:"title"`
	RepoID string `json:"repo_id"`
	// ID is the session's stable id; see KillSessionRequest.ID. Retained UI
	// actions send it so a restore queued for one row cannot resurrect a
	// different session that reuses the title before dispatch.
	ID string `json:"id"`
}

type RestoreSessionResponse struct {
	OK bool `json:"ok"`
	// WorktreePath is the on-disk location of the restored/recovered worktree.
	WorktreePath string `json:"worktree_path"`
}

// DeleteProjectRequest asks the daemon to delete a project — a repo grouping of
// sessions (#1735). The delete is ARCHIVE-THEN-REMOVE and reversible: every live
// session of the repo is archived (worktree relocated, branch/state preserved,
// restorable via RestoreArchived), the repo's root_agents opt-in is dropped, and
// the always-on root agent (if any) is stopped — the user's real git repo is
// never touched. Restoring any archived session brings the project back.
//
// RepoPath is the repo root (the stable project id clients group by:
// worktree.repo_path). RepoID is the precomputed id; when empty the daemon
// derives it from RepoPath. At least one must be set. Deleting an unknown or
// already-empty project is a clean no-op, not an error.
type DeleteProjectRequest struct {
	RepoPath string `json:"repo_path"`
	RepoID   string `json:"repo_id"`
}

type DeleteProjectResponse struct {
	OK bool `json:"ok"`
	// ArchivedCount is how many live sessions were archived (restorable).
	ArchivedCount int `json:"archived_count"`
	// KilledCount is how many live sessions could not be archived and were torn
	// down instead — only in-place/external worktrees (the root agent, `--here`
	// sessions), whose kill never touches the user's tree or branch.
	KilledCount int `json:"killed_count"`
}

// RegisterProjectRequest asks the daemon to register a git checkout as a durable,
// sessionless project in the #2355 registry (#2456). Path is a user-supplied
// path — an absolute path, or one with a leading ~ — that the DAEMON resolves on
// its own filesystem: it expands ~, resolves symlinks, and walks to the git
// checkout's canonical main-repo root. Resolving daemon-side is what makes the
// web/remote path correct — the path names a directory on the daemon host, not
// the client's. Registration is idempotent: a known checkout is a no-op success
// that returns the existing identity.
type RegisterProjectRequest struct {
	Path string `json:"path"`
}

// RegisterProjectResponse carries the durable identity the registration
// resolved to. The project appears in ListProjects and, once #2456's UI slices
// land, as an empty row in the project switcher until a session is created into
// it. OK is always true on a nil error (a redundant flag kept for wire symmetry
// with the other project RPCs).
type RegisterProjectResponse struct {
	OK      bool           `json:"ok"`
	Project config.Project `json:"project"`
}

type SendPromptRequest struct {
	Title  string `json:"title"`
	RepoID string `json:"repo_id"`
	Prompt string `json:"prompt"`
	// ID is the session's stable id; see KillSessionRequest.ID. When non-empty
	// the daemon resolves the prompt target by id first, so a web send-prompt
	// can't land on the wrong session under a cross-repo title collision (#1592
	// Phase 5 follow-up). TUI/CLI/delivery callers omit it and resolve by title.
	ID string `json:"id"`
}

type SendPromptResponse struct {
	OK bool `json:"ok"`
}

// DeliverPromptRequest asks the daemon to deliver a prompt to a target session,
// auto-creating that session when it does not exist yet. It is the race-safe
// successor to the deliverTaskPrompt "check existence, then create-or-send"
// sequence that dropped a prompt whenever two deliveries to the same missing
// target ran concurrently (#865). RepoPath (not RepoID) is required because a
// create needs the worktree root; the repo ID is resolved from it.
type DeliverPromptRequest struct {
	Title    string `json:"title"`
	RepoPath string `json:"repo_path"`
	Program  string `json:"program"`
	Prompt   string `json:"prompt"`
	// DeferWhileAttached is set by the automated task-delivery path (cron +
	// watch) so DeliverPrompt holds the send when a TUI is attached full-screen
	// to an existing target session, rather than pasting a prompt + Enter into a
	// pane the user is actively typing in — which would append to and submit
	// their half-typed message (#1586). Manual sends (af sessions send-prompt)
	// leave it false so an explicit, user-initiated send always lands
	// immediately.
	DeferWhileAttached bool `json:"defer_while_attached,omitempty"`
}

// DeliverPromptResponse reports how the prompt was delivered. Status is
// "started" when this call created the target session (the prompt was its
// initial prompt) and "sent" when it was sent into a session that already
// existed — the same status vocabulary deliverTaskPrompt records on a task.
type DeliverPromptResponse struct {
	Status string `json:"status"`
}

// CreateTabRequest asks the daemon to spawn a tab in the target session's
// worktree. Title selects the session; RepoID scopes the lookup like the other
// sessions verbs (empty = all-repo).
//
// Four kinds of tab can be created:
//   - Process tab (Shell=false, the #930 PR 5 default): runs Command in the
//     worktree. Name is the optional display name (a default is derived from
//     Command's basename when empty). Command must be non-empty.
//   - Shell tab (Shell=true): runs $SHELL in the worktree, exactly like the TUI's
//     `t` key (Instance.AddShellTab). Command/Name are ignored; the name is the
//     auto-derived unique "shell"/"shell-2"/… The TUI routes its `t` mutation
//     here so the daemon — not the TUI — owns the tab write (#960 PR 2).
//   - Web tab (Kind="web"): an iframe/URL tab with no PTY. URL is the target
//     (or Port as a localhost:<port> convenience). See the Kind/URL/Port fields.
//   - VS Code tab (Kind="vscode"): a code-server editor on the session's
//     worktree, with no PTY and no target — URL/Port/Command are REJECTED, since
//     the worktree is always what it opens. The editor is daemon-managed (one per
//     session, spawned lazily on first render); creating the tab neither starts a
//     process nor requires code-server to be installed.
//
// Either way the resolved, collision-suffixed name is returned.
type CreateTabRequest struct {
	Title   string `json:"title"`
	RepoID  string `json:"repo_id"`
	Command string `json:"command"`
	Name    string `json:"name"`
	Shell   bool   `json:"shell"`
	// Kind selects the tab type. Empty (the default) means a process tab (or a
	// shell tab when Shell is set); "web" creates a URL/iframe tab with no PTY,
	// targeting URL (or Port as a localhost:<port> convenience); "vscode" creates
	// a VS Code editor tab on the session's worktree, which takes no target. The
	// vocabulary is session.ParseTabKindName — shared with the CLI, so the two
	// can't drift. Value types so they travel over both the gob control socket
	// (CLI) and JSON (HTTP) without the zero-value pointer elision gob applies to
	// *T fields.
	Kind string `json:"kind,omitempty"`
	// URL is the target of a web tab (Kind=="web"): a loopback dev-server address
	// the daemon reverse-proxies or an external absolute URL the web UI iframes.
	URL string `json:"url,omitempty"`
	// Port is a convenience for a web tab: when set (and URL is empty) the target
	// becomes http://localhost:<port>.
	Port int `json:"port,omitempty"`
	// ID is the session's stable id; see KillSessionRequest.ID. When non-empty
	// the daemon resolves the target session by id first, so web and retained TUI
	// tab-create actions cannot hit a same-title replacement (#1592 Phase 5 PR7,
	// #2358). One-shot CLI callers resolve by {Title, RepoID}.
	ID string `json:"id"`
}

type CreateTabResponse struct {
	// ID is the stable identity minted by the daemon in the same transaction
	// that creates and persists the tab. It is additive for mixed-version
	// clients: an older daemon omits it, yielding the explicit empty-id fallback.
	ID   string `json:"id,omitempty"`
	Name string `json:"name"`
	// TmuxName is the tmux session the tab was actually spawned under, or empty
	// for a kind that owns no PTY (web, vscode — see session.TabKind.HasTmux).
	//
	// It is normally "<agent session>__<name>", but the two are independent
	// namespaces and diverge after a rename (#1957): a tab named "fresh" spawns
	// under "…__fresh-2" when an earlier tab that was renamed AWAY from "fresh"
	// still holds "…__fresh". Reporting it is what lets the TUI attach its
	// instant-display projection to the exact session (Instance.AttachShellTab)
	// instead of re-deriving one from the name and landing on the older tab's.
	// It also makes the reservation inspectable rather than invisible, which is
	// the complaint #1957 opened with.
	TmuxName string `json:"tmux_name,omitempty"`
}

// CloseTabRequest asks the daemon to close a non-agent tab of a session and
// persist the shrunk tab list (#960 PR 1). Title selects the session; RepoID
// scopes the lookup like the other sessions verbs (empty = all-repo). The tab
// is identified by stable TabID when supplied, then TabName; when both are empty
// TabIndex selects the tab by its 0-based position. The agent tab (index 0)
// cannot be closed —
// use KillSession to tear down the whole session — and remote sessions' tabs
// are fixed by their hook config, so closing them is refused. This mirrors the
// TUI's `w` rule (handleCloseTab) and #930 PR 4/PR 6 semantics.
type CloseTabRequest struct {
	Title    string `json:"title"`
	RepoID   string `json:"repo_id"`
	TabName  string `json:"tab_name"`
	TabIndex int    `json:"tab_index"`
	// ID is the session's stable id; see KillSessionRequest.ID. When non-empty
	// the daemon resolves the target session by id first, so web and retained TUI
	// tab-close actions cannot hit a same-title replacement (#1592 Phase 5 PR7,
	// #2358). One-shot CLI callers resolve by {Title, RepoID}.
	ID string `json:"id"`
	// TabID is the TAB's stable id; see RenameTabRequest.TabID for the full
	// argument. Close is the verb that needs it MOST, because it is the only
	// destructive one: rename and reorder land a wrong-tab mutation the user can
	// undo, while a wrong-tab close kills that tab's tmux session and whatever was
	// running in it (#1971).
	//
	// The reuse it defends against is not theoretical. uniqueTabName
	// (session/tab_names.go) hands a freed name straight back to the next tab that
	// asks for it, so between a client resolving "the tab named preview" and the
	// daemon handling the close, another client can close that tab and have a NEW
	// tab reissued the freed name — and a name-keyed close then kills the new tab.
	//
	// Same contract as the siblings: non-empty WINS over TabName/TabIndex, an
	// unresolvable id is REFUSED rather than fallen back to the name (#1779), and
	// empty means "no id supplied" — the CLI and a freshly-created/legacy TUI tab
	// whose projection has not adopted its daemon ID yet fall back to
	// TabName/TabIndex exactly as before.
	TabID string `json:"tab_id"`
}

type CloseTabResponse struct {
	Name string `json:"name"`
}

// RenameTabRequest asks the daemon to relabel one tab of a session and persist
// the change (#1813). Title selects the session; RepoID scopes the lookup like
// the other sessions verbs (empty = all-repo). The tab is identified by TabName
// (preferred); when TabName is empty TabIndex selects it by 0-based position —
// the same resolution CloseTabRequest uses.
//
// NewName is sanitized to the tmux-safe token set and made unique within the
// session exactly as tab-create's --name is, so the resolved name may differ
// from the requested one ("dup" -> "dup-2"); RenameTabResponse carries what was
// actually applied. A NewName that sanitizes to nothing is an error, not a
// silent fall back to a default.
//
// Only tabs that display their name can be renamed — web, process and VS Code
// tabs (session.TabKindRenameable). The agent and shell tabs render fixed labels
// ("Agent"/"Terminal") on every
// surface, so renaming them would be a no-op and is refused. Remote sessions'
// tabs are fixed by their hook config, and an archived session's tabs are inert
// (#1809), so both are refused.
//
// Every field is a value type, never a *T: this request travels the gob control
// socket as well as JSON, and gob elides zero-value pointers (a *string "" would
// arrive nil), so plain fields are what make the two transports agree (#1700).
type RenameTabRequest struct {
	Title    string `json:"title"`
	RepoID   string `json:"repo_id"`
	TabName  string `json:"tab_name"`
	TabIndex int    `json:"tab_index"`
	NewName  string `json:"new_name"`
	// ID is the session's stable id; see KillSessionRequest.ID. When non-empty
	// the daemon resolves the target session by id first, so a tab rename can't
	// hit the wrong session under a cross-repo title collision (#1592 Phase 5
	// PR7). TUI/CLI callers omit it and resolve by {Title, RepoID}.
	ID string `json:"id"`
	// TabID is the TAB's stable id (#1738) — the same protection ID gives the
	// session, applied one level down (#1929). TabName and TabIndex are both
	// REUSABLE: a name is freed by a close and handed to the next tab that asks
	// for it, and an index shifts on every close/reorder. So a client that
	// resolves a tab, then sends its name, addresses whatever tab answers to that
	// name by the time the daemon handles the request — which after a concurrent
	// close+create is a DIFFERENT tab. TabID is minted once and never reused, so
	// it names the tab the client actually meant.
	//
	// When non-empty it WINS: the name/index in the same request are ignored
	// rather than cross-checked, because the whole point is that the name may
	// have changed underneath the client. A non-empty id that no longer resolves
	// is REFUSED, never fallen back to the name — see resolveTabTarget (#1779).
	// Empty means "no id supplied", the CLI/TUI path, and resolution proceeds by
	// TabName/TabIndex exactly as before.
	TabID string `json:"tab_id"`
}

// RenameTabResponse carries the RESOLVED name — sanitized and collision-suffixed
// — which is what clients must render: it is the name the tab actually has, and
// the name every other tab verb now addresses it by.
type RenameTabResponse struct {
	Name string `json:"name"`
}

// ReorderTabRequest asks the daemon to move one tab within a session's roster
// and persist the new order (#1813). Title/RepoID/TabName/TabIndex resolve the
// session and the tab exactly as RenameTabRequest does.
//
// NewIndex is the tab's destination read in the FINAL roster, so moving tab 1 to
// index 3 of a 4-tab session leaves it last. Index 0 is rejected in both
// directions: it is reserved for the agent tab, which the rest of the session
// package identifies positionally (archive teardown, the agent conversation, the
// agent tmux session all read Tabs[0]), so only slots 1..n-1 may be permuted.
//
// Value types only, for the same gob reason as RenameTabRequest.
type ReorderTabRequest struct {
	Title    string `json:"title"`
	RepoID   string `json:"repo_id"`
	TabName  string `json:"tab_name"`
	TabIndex int    `json:"tab_index"`
	NewIndex int    `json:"new_index"`
	// ID is the session's stable id; see RenameTabRequest.ID.
	ID string `json:"id"`
	// TabID is the tab's stable id; see RenameTabRequest.TabID. Reorder needs it
	// at least as much as rename does: it is the verb that INVALIDATES every
	// other client's TabIndex, so two clients reordering the same roster are the
	// likeliest way to send a request whose index no longer means what the sender
	// meant (#1929).
	TabID string `json:"tab_id"`
}

// ReorderTabResponse carries the moved tab's name and its resolved final index.
type ReorderTabResponse struct {
	Name  string `json:"name"`
	Index int    `json:"index"`
}

// SetPRInfoRequest records (or clears) the GitHub PR info for a session and
// persists it (#960). ID is authoritative when present; legacy callers resolve
// by {Title, RepoID}. A zero-value PRInfo (Number 0) clears the recorded info,
// matching how pr_info round-trips through storage (FromInstanceData treats
// Number 0 as "no PR").
type SetPRInfoRequest struct {
	Title  string `json:"title"`
	RepoID string `json:"repo_id"`
	// ID is the session's stable id; see KillSessionRequest.ID. The TUI's PR
	// lookup is asynchronous, so a completed fetch must not persist onto a
	// different same-title row that appeared while gh was running.
	ID string `json:"id"`
	// PRInfo is the fetched projection to persist.
	PRInfo session.PRInfoData `json:"pr_info"`
}

type SetPRInfoResponse struct {
	OK bool `json:"ok"`
}

// PauseStatusPollRequest asks the daemon to pause its per-instance capture-pane
// liveness poll for ONE session while a TUI is attached full-screen to it
// (#1160, Fix A follow-up to #1157). ID identifies the lease owner when
// present; legacy clients fall back to {Title, RepoID}. There is deliberately NO
// client-supplied duration: the daemon always applies its own fixed
// statusPollLease, so a misbehaving or crashed client can never silence an
// instance for an unbounded time. The TUI renews the lease with a heartbeat
// while attached and clears it with ResumeStatusPoll on detach.
type PauseStatusPollRequest struct {
	Title  string `json:"title"`
	RepoID string `json:"repo_id"`
	// ID is the attached session's stable id. Keying the lease by identity keeps
	// an old heartbeat from pausing a different session that reused the title.
	ID string `json:"id"`
}

type PauseStatusPollResponse struct {
	OK bool `json:"ok"`
}

// ResumeStatusPollRequest clears a pause set by PauseStatusPoll so the daemon's
// poll resumes immediately on a clean detach rather than waiting out the lease
// (#1160).
type ResumeStatusPollRequest struct {
	Title  string `json:"title"`
	RepoID string `json:"repo_id"`
	// ID is the stable lease key; see PauseStatusPollRequest.ID.
	ID string `json:"id"`
}

type ResumeStatusPollResponse struct {
	OK bool `json:"ok"`
}

// SnapshotRequest, SnapshotResponse, and the DeliveryAlarm projection live in
// snapshot.go (extracted to keep control.go under its file-length ceiling,
// #1145).

type PingRequest struct{}

// DaemonBootConfig is the small immutable config posture a running daemon
// reports through Ping. It is deliberately narrower than config.Config: status
// only needs the listener/auth values that can differ after a supported
// hand-edit, and Ping must never become a general config or secret export.
type DaemonBootConfig struct {
	ListenAddr           string `json:"listen_addr"`
	RequireToken         bool   `json:"require_token"`
	RequireLoopbackToken bool   `json:"require_loopback_token"`
}

type PingResponse struct {
	OK bool `json:"ok"`
	// Version is the af build version the responding daemon is running, so a
	// client can compare it against its own and detect skew (#1044). It rides
	// Ping because Ping is the one RPC that answers throughout the daemon's
	// warm-up, and because Ping already backs GET /v1/health — one field
	// serves both the control socket and HTTP clients.
	//
	// A daemon built before this field existed simply omits it, so a newer
	// client decodes "". Empty from a *responding* daemon is therefore a
	// positive skew signal (the daemon predates version reporting, so it is
	// older than this client), not merely an unknown.
	Version string `json:"version"`
	// BootID is unique to this daemon process start. TransactionID is retained
	// for the full candidate boot, including after it reaches ready, so an
	// upgrade supervisor can reject a different daemon that happened to answer
	// the same socket (#1947).
	BootID        string `json:"boot_id,omitempty"`
	TransactionID string `json:"transaction_id,omitempty"`
	// Phase distinguishes liveness from operational readiness. Ping answers
	// while warming and in upgrade probation; only ready means daemon-owned work
	// and ordinary mutations have been admitted.
	Phase     DaemonPhase          `json:"phase,omitempty"`
	Listeners DaemonListenerStatus `json:"listeners"`
	// PID identifies the process which answered this Ping. daemon.pid is only a
	// disk record and can be stale, so it cannot prove that the service manager's
	// MainPID owns the responder (#2168 Phase 4).
	PID int `json:"pid,omitempty"`
	// BootConfig is nil only for a responder predating this additive field (or a
	// synthetic test server with no manager). A pointer preserves the difference
	// between an older daemon and the valid false value of RequireToken.
	BootConfig *DaemonBootConfig `json:"boot_config,omitempty"`
}

type ReloadTasksRequest struct{}
type ReloadTasksResponse struct {
	OK bool `json:"ok"`
}

// Task CRUD RPCs (#1029 PR 3). They promote task writes to the daemon so it is
// the sole task writer among clients — exactly the model sessions already
// follow — and the write and the scheduler/watcher refresh happen atomically
// in-process (no separate ReloadTasks poke). The on-disk tasks.json format is
// unchanged; the daemon reuses the same task.AddTask/UpdateTask/RemoveTask
// (config.WithFileLock + saveTasks) that clients used to call directly.

// ListTasksRequest asks the daemon for the full task list. There is
// deliberately no repo filter: the daemon returns every repo's tasks and the
// CLI applies its --repo filter, matching the disk-read fallback
// (task.LoadTasks). It is the read side of the task single-writer model, mirror
// of Snapshot for sessions.
type ListTasksRequest struct{}
type ListTasksResponse struct {
	Tasks []task.Task `json:"tasks"`
}

// AddTaskRequest carries a fully-populated task.Task to append. The CLI/TUI
// still build and validate the struct (flag parsing, ID generation, program
// resolution); the daemon re-validates via task.AddTask and owns the write.
type AddTaskRequest struct {
	Task task.Task `json:"task"`
}
type AddTaskResponse struct {
	OK bool `json:"ok"`
}

// UpdateTaskRequest carries a FIELD-LEVEL patch (#1700): the ID of the task to
// edit and a task.TaskUpdate holding only the field(s) the caller intends to
// change. The daemon merges the patch onto the freshly-loaded record under the
// file lock, leaving every unspecified field as-stored — so a single-field edit
// (the enable/disable toggle sends just Enabled) cannot clobber a concurrent
// edit another client made to a different field. This replaces the prior
// full-struct read-modify-write, which re-applied every user field from the
// caller's possibly-stale copy. Scheduler-owned fields (LastRunAt/LastRunStatus/
// CreatedAt) are never patchable — UpdateTaskStatus stays their writer.
//
// Expect optionally carries the project the caller authorized the id against,
// re-verified under the same lock — see task.ProjectExpectation.
type UpdateTaskRequest struct {
	ID     string                  `json:"id"`
	Update task.TaskUpdate         `json:"update"`
	Expect task.ProjectExpectation `json:"expect,omitempty"`
}

// UpdateTaskResponse returns the merged record the write produced, so the CLI
// can print the authoritative post-edit task and the daemon publishes the full
// task (not the partial patch) on its EventTaskUpdated.
type UpdateTaskResponse struct {
	OK   bool      `json:"ok"`
	Task task.Task `json:"task"`
}

// Expect optionally carries the project the caller authorized the id against,
// re-verified under the same lock — see task.ProjectExpectation.
type RemoveTaskRequest struct {
	ID     string                  `json:"id"`
	Expect task.ProjectExpectation `json:"expect,omitempty"`
}
type RemoveTaskResponse struct {
	OK bool `json:"ok"`
}

// RestartTaskRequest asks the daemon to stop and replace one enabled watch
// task from its current on-disk definition. Expect carries the same project
// compare-and-swap as the other id-addressed task mutations.
type RestartTaskRequest struct {
	ID     string                  `json:"id"`
	Expect task.ProjectExpectation `json:"expect,omitempty"`
}
type RestartTaskResponse struct {
	OK bool `json:"ok"`
}

// TriggerTaskRequest asks the daemon to fire a task NOW through the same
// RunTask firing path the in-daemon scheduler uses (#1029 PR 3 / #1169-class
// fix). The handler preserves RunTask's guards: watch tasks and disabled tasks
// are refused.
// Expect optionally carries the project the caller authorized the id against,
// re-verified against the same load that produces the fired record — see
// task.ProjectExpectation.
type TriggerTaskRequest struct {
	ID     string                  `json:"id"`
	Expect task.ProjectExpectation `json:"expect,omitempty"`
}
type TriggerTaskResponse struct {
	OK bool `json:"ok"`
}

type ShutdownRequest struct{}
type ShutdownResponse struct {
	OK bool `json:"ok"`
}

// SpawnConfigAgentRequest asks the daemon to start a config agent in a bare tmux
// session at AF home — no Instance, no worktree, no branch, and no row in the
// session list.
//
// Every field is a PLAIN VALUE, deliberately. The control socket is net/rpc gob,
// which elides zero-value fields, so a *bool false or a *string "" would arrive
// as nil (#1700): optional-pointer fields on this plane need a JSON-backed codec
// to survive. Nothing here is optional, so the hazard cannot apply.
type SpawnConfigAgentRequest struct {
	// Program is the fully resolved command to run (program_overrides already
	// applied by the caller, which is also what preflighted it).
	Program string `json:"program"`
	// Prompt is the briefing, delivered over a tmux paste buffer after the agent
	// is ready. Unbounded: it is streamed via stdin, not passed as an argument.
	Prompt string `json:"prompt"`
}

// SpawnConfigAgentResponse returns the tmux session name AND the absolute socket
// path the client attaches to.
//
// SocketPath is a PLAIN STRING, deliberately, and for the same reason the
// request's fields are: the control socket is net/rpc gob, which elides
// zero-value fields, so a *string would arrive as nil when empty (#1700). A
// missing socket path is a legitimate value here — the daemon returns it empty
// when it could not resolve the socket, and the attach then falls back to its
// default socket — so it must transmit as "" rather than be laundered into nil.
type SpawnConfigAgentResponse struct {
	SessionName string `json:"session_name"`
	// SocketPath is the absolute tmux server socket the session lives on, so the
	// client can pin it with `tmux -S <path> attach-session` (#2019). Empty when
	// the daemon could not resolve it; the attach falls back to the default socket.
	SocketPath string `json:"socket_path"`
}

// ReapConfigAgentRequest tears down a config-agent session once the client is
// done with it.
type ReapConfigAgentRequest struct {
	SessionName string `json:"session_name"`
}

// ReapConfigAgentResponse is empty: reaping either succeeded or errored.
type ReapConfigAgentResponse struct{}

// -- Config (the web config editor's read/write pair) --
//
// config.toml is NOT daemon-owned state. Unlike instances.json (the #960
// single-writer model), it is a file the README tells users to hand-edit, read
// by af and the daemon at startup and guarded by a file lock rather than by
// daemon ownership. These RPCs therefore exist for REACH, not for arbitration:
// the web UI is a browser and cannot touch the user's disk, so it asks the
// daemon to run the same config.SetGlobalConfigValue call the TUI and
// `af config set` run in their own process. There is no daemon-side copy of
// config, no cache to invalidate, and no writer to serialize against beyond the
// lock every writer already takes.

// GetConfigRequest asks for the config manifest zipped with the user's live
// values — every user-facing global key, whether it is settable, and what it is
// set to now. There is no key filter: the manifest is ~20 entries and the editor
// renders all of them.
type GetConfigRequest struct{}
type GetConfigResponse struct {
	Entries []config.ConfigEntry `json:"entries"`
	// Path is the config.toml the values were read from, so the UI can tell the
	// user which file it is editing (a user with AF_HOME set is otherwise left
	// guessing).
	Path string `json:"path"`
}

// SetConfigValueRequest sets one key, exactly as `af config set key value` does.
// Value is the raw string form; the daemon hands it to the same validator, so an
// invalid value is rejected here with the identical message rather than being
// written and discovered at the next startup.
type SetConfigValueRequest struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}
type SetConfigValueResponse struct {
	// Result is config.SetResult verbatim — key, canonical value, path, and
	// RequiresRestart — so the web UI echoes what was actually written rather
	// than what it believes it sent.
	Result *config.SetResult `json:"result"`
	// RestartNotice is the sentence to show when Result.RequiresRestart is set.
	// It rides on the response rather than being duplicated in the web bundle so
	// the TUI, the web UI, and the CLI cannot drift into three different
	// accounts of when an edit takes effect.
	RestartNotice string `json:"restart_notice"`
}
