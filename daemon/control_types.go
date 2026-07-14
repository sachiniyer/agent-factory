package daemon

import (
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
	AutoYes   bool   `json:"auto_yes"`
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
	// paths (#1592 Phase 5 PR5). TUI/CLI callers omit it and resolve by title.
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
	// follow-up). TUI/CLI callers omit it and resolve by {Title, RepoID}.
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
	AutoYes  bool   `json:"auto_yes"`
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
// Two kinds of tab can be created:
//   - Process tab (Shell=false, the #930 PR 5 default): runs Command in the
//     worktree. Name is the optional display name (a default is derived from
//     Command's basename when empty). Command must be non-empty.
//   - Shell tab (Shell=true): runs $SHELL in the worktree, exactly like the TUI's
//     `t` key (Instance.AddShellTab). Command/Name are ignored; the name is the
//     auto-derived unique "shell"/"shell-2"/… The TUI routes its `t` mutation
//     here so the daemon — not the TUI — owns the tab write (#960 PR 2).
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
	// targeting URL (or Port as a localhost:<port> convenience). Value types so
	// they travel over both the gob control socket (CLI) and JSON (HTTP) without
	// the zero-value pointer elision gob applies to *T fields.
	Kind string `json:"kind,omitempty"`
	// URL is the target of a web tab (Kind=="web"): a loopback dev-server address
	// the daemon reverse-proxies or an external absolute URL the web UI iframes.
	URL string `json:"url,omitempty"`
	// Port is a convenience for a web tab: when set (and URL is empty) the target
	// becomes http://localhost:<port>.
	Port int `json:"port,omitempty"`
	// ID is the session's stable id; see KillSessionRequest.ID. When non-empty
	// the daemon resolves the target session by id first, so a web tab-create
	// can't hit the wrong session under a cross-repo title collision (#1592
	// Phase 5 PR7). TUI/CLI callers omit it and resolve by {Title, RepoID}.
	ID string `json:"id"`
}

type CreateTabResponse struct {
	Name string `json:"name"`
}

// CloseTabRequest asks the daemon to close a non-agent tab of a session and
// persist the shrunk tab list (#960 PR 1). Title selects the session; RepoID
// scopes the lookup like the other sessions verbs (empty = all-repo). The tab
// is identified by TabName (preferred); when TabName is empty TabIndex selects
// the tab by its 0-based position. The agent tab (index 0) cannot be closed —
// use KillSession to tear down the whole session — and remote sessions' tabs
// are fixed by their hook config, so closing them is refused. This mirrors the
// TUI's `w` rule (handleCloseTab) and #930 PR 4/PR 6 semantics.
type CloseTabRequest struct {
	Title    string `json:"title"`
	RepoID   string `json:"repo_id"`
	TabName  string `json:"tab_name"`
	TabIndex int    `json:"tab_index"`
	// ID is the session's stable id; see KillSessionRequest.ID. When non-empty
	// the daemon resolves the target session by id first, so a web tab-close
	// can't hit the wrong session under a cross-repo title collision (#1592
	// Phase 5 PR7). TUI/CLI callers omit it and resolve by {Title, RepoID}.
	ID string `json:"id"`
}

type CloseTabResponse struct {
	Name string `json:"name"`
}

// SetPRInfoRequest records (or clears) the GitHub PR info for a session and
// persists it (#960 PR 1). Title selects the session; RepoID scopes the lookup
// (empty = all-repo). A zero-value PRInfo (Number 0) clears the recorded info,
// matching how pr_info round-trips through storage (FromInstanceData treats
// Number 0 as "no PR"). This is the daemon-side write that the TUI performs
// today via prInfoUpdatedMsg + a full-list save (#921); PR 1 only adds the
// mutation — the TUI is not switched to it until PR 2.
type SetPRInfoRequest struct {
	Title  string             `json:"title"`
	RepoID string             `json:"repo_id"`
	PRInfo session.PRInfoData `json:"pr_info"`
}

type SetPRInfoResponse struct {
	OK bool `json:"ok"`
}

// PauseStatusPollRequest asks the daemon to pause its per-instance capture-pane
// liveness poll for ONE session while a TUI is attached full-screen to it
// (#1160, Fix A follow-up to #1157). Title selects the session; RepoID scopes
// the lookup like the other sessions verbs. There is deliberately NO
// client-supplied duration: the daemon always applies its own fixed
// statusPollLease, so a misbehaving or crashed client can never silence an
// instance for an unbounded time. The TUI renews the lease with a heartbeat
// while attached and clears it with ResumeStatusPoll on detach.
type PauseStatusPollRequest struct {
	Title  string `json:"title"`
	RepoID string `json:"repo_id"`
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
}

type ResumeStatusPollResponse struct {
	OK bool `json:"ok"`
}

// SnapshotRequest, SnapshotResponse, and the DeliveryAlarm projection live in
// snapshot.go (extracted to keep control.go under its file-length ceiling,
// #1145).

type PingRequest struct{}
type PingResponse struct {
	OK bool `json:"ok"`
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
type UpdateTaskRequest struct {
	ID     string          `json:"id"`
	Update task.TaskUpdate `json:"update"`
}

// UpdateTaskResponse returns the merged record the write produced, so the CLI
// can print the authoritative post-edit task and the daemon publishes the full
// task (not the partial patch) on its EventTaskUpdated.
type UpdateTaskResponse struct {
	OK   bool      `json:"ok"`
	Task task.Task `json:"task"`
}

type RemoveTaskRequest struct {
	ID string `json:"id"`
}
type RemoveTaskResponse struct {
	OK bool `json:"ok"`
}

// TriggerTaskRequest asks the daemon to fire a task NOW through the same
// RunTask firing path the in-daemon scheduler uses (#1029 PR 3 / #1169-class
// fix). The handler preserves RunTask's guards: watch tasks and disabled tasks
// are refused.
type TriggerTaskRequest struct {
	ID string `json:"id"`
}
type TriggerTaskResponse struct {
	OK bool `json:"ok"`
}

type ShutdownRequest struct{}
type ShutdownResponse struct {
	OK bool `json:"ok"`
}
