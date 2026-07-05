package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/rpc"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/sachiniyer/agent-factory/task"
)

const (
	controlServiceName   = "Control"
	daemonSocketFileName = "daemon.sock"
	// daemonHTTPSocketFileName is the Unix socket the daemon-hosted HTTP/JSON
	// server (#1029 PR 4) listens on, alongside — never multiplexed onto — the
	// gob net/rpc control socket above. One listener, one protocol.
	daemonHTTPSocketFileName = "daemon-http.sock"
	daemonReadyTimeout       = 5 * time.Second
	daemonDialTimeout        = 250 * time.Millisecond
	// shutdownAckGrace delays the daemon main-loop teardown after a Shutdown
	// RPC handler returns so the response can flush back to the caller before
	// the listener closes.
	shutdownAckGrace = 50 * time.Millisecond
)

var ensureDaemonMu sync.Mutex

// statusPollLease bounds how long a single PauseStatusPoll silences an
// instance's daemon capture-pane liveness poll (#1160). The attached TUI
// renews it with a heartbeat every statusPollRenewInterval (< this lease) and
// clears it on clean detach, but this fixed SERVER-SIDE lease — never a
// client-supplied duration — is the leak-safety guarantee: a crashed TUI that
// never renews or resumes auto-resumes within one lease, so real tmux death is
// still detected on the next tick and the daemon can never be permanently
// blinded. var, not const, so tests can shrink it.
var statusPollLease = 3 * time.Second

// nowFunc is the clock used by the pause-poll lease logic (#1160), injectable
// so lease-expiry tests advance time deterministically instead of racing real
// sleeps.
var nowFunc = time.Now

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

type SendPromptRequest struct {
	Title  string `json:"title"`
	RepoID string `json:"repo_id"`
	Prompt string `json:"prompt"`
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

type ImportRemoteHookSessionsRequest struct {
	RepoPath string `json:"repo_path"`
}

type ImportRemoteHookSessionsResponse struct {
	Instances []session.InstanceData `json:"instances"`
}

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

// UpdateTaskRequest carries the edited task.Task to persist. task.UpdateTask
// preserves the scheduler-owned fields (LastRunAt/LastRunStatus/CreatedAt) from
// the freshly-loaded record under the file lock, so a stale client copy never
// clobbers them.
type UpdateTaskRequest struct {
	Task task.Task `json:"task"`
}
type UpdateTaskResponse struct {
	OK bool `json:"ok"`
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

// daemonStartingErrText is the wire-visible text of the warm-up error. net/rpc
// flattens server-side errors into plain strings, so clients cannot errors.Is
// against a sentinel value; IsDaemonStartingErr matches this text instead.
const daemonStartingErrText = "agent-factory daemon is starting (restoring sessions); retry shortly"

// errDaemonStarting is returned by state-dependent RPC handlers in the window
// between the control-socket bind and the completion of the instance restore
// (#829). The socket now binds before the restore, which can take minutes on
// remote-hook repos, so this window is user-visible.
func errDaemonStarting() error {
	return errors.New(daemonStartingErrText)
}

// IsDaemonStartingErr reports whether an RPC client error means the daemon is
// up but still restoring instances. Callers should treat it as retryable: the
// daemon is alive (EnsureDaemon's ping succeeds, so it must NOT spawn another)
// and the same request succeeds once the restore finishes.
func IsDaemonStartingErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), daemonStartingErrText)
}

// DaemonSocketPath returns the Unix socket path used by the local control
// plane.
func DaemonSocketPath() (string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, daemonSocketFileName), nil
}

// EnsureDaemon starts the daemon if the control socket is not already serving.
func EnsureDaemon() error {
	ensureDaemonMu.Lock()
	defer ensureDaemonMu.Unlock()

	if err := pingDaemon(); err == nil {
		return nil
	}

	// A previous daemon version may have a PID file but no control socket. Stop
	// it before launching the control-plane daemon so we do not run duplicate
	// AutoYes loops.
	if _, err := StopDaemon(); err != nil {
		log.WarningLog.Printf("failed to stop stale daemon before launch: %v", err)
	}

	if err := launchDaemonProcessFn(); err != nil {
		return err
	}

	deadline := time.Now().Add(daemonReadyTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		if err := pingDaemon(); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("daemon did not become ready: %w", lastErr)
}

func pingDaemon() error {
	var resp PingResponse
	return callDaemonNoEnsure("Ping", PingRequest{}, &resp)
}

// daemonWarmupWait bounds how long RPC clients wait for a warming daemon
// (socket bound, instance restore still running — #829) before surfacing the
// typed starting error. It mirrors daemonReadyTimeout, the wait callers
// already tolerated pre-#829 when EnsureDaemon polled for the socket: a local
// restore completes well inside this window so CLI/TUI calls just work, while
// a minutes-long remote-hook restore fails fast with an actionable message
// instead of hanging the caller. daemonWarmupPoll is the retry cadence.
const (
	daemonWarmupWait = daemonReadyTimeout
	daemonWarmupPoll = 100 * time.Millisecond
)

func callDaemon(method string, req any, resp any) error {
	if err := EnsureDaemon(); err != nil {
		return err
	}
	err := callDaemonNoEnsure(method, req, resp)
	// A warming daemon rejects state-dependent RPCs until its instance
	// restore completes (#829). Retry briefly so callers that race a fresh
	// daemon spawn (CLI create right after boot, task runs after an upgrade
	// respawn) succeed without every call site growing retry logic.
	deadline := time.Now().Add(daemonWarmupWait)
	for IsDaemonStartingErr(err) && time.Now().Before(deadline) {
		time.Sleep(daemonWarmupPoll)
		err = callDaemonNoEnsure(method, req, resp)
	}
	return err
}

func callDaemonNoEnsure(method string, req any, resp any) error {
	socketPath, err := DaemonSocketPath()
	if err != nil {
		return err
	}
	conn, err := net.DialTimeout("unix", socketPath, daemonDialTimeout)
	if err != nil {
		return err
	}
	client := rpc.NewClient(conn)
	defer client.Close()
	return client.Call(controlServiceName+"."+method, req, resp)
}

// CreateSession asks the daemon to create, start, and persist a session.
func CreateSession(req CreateSessionRequest) (*session.InstanceData, error) {
	var resp CreateSessionResponse
	if err := callDaemon("CreateSession", req, &resp); err != nil {
		return nil, err
	}
	return &resp.Instance, nil
}

// CreateTab asks the daemon to spawn, persist, and report a new process tab on
// an existing session. It returns the resolved (collision-suffixed) tab name so
// scripts/agents can address the tab.
func CreateTab(req CreateTabRequest) (string, error) {
	var resp CreateTabResponse
	if err := callDaemon("CreateTab", req, &resp); err != nil {
		return "", err
	}
	return resp.Name, nil
}

// CloseTab asks the daemon to close a non-agent tab on an existing session and
// returns the resolved name of the tab that was closed. It is the close-side
// counterpart of CreateTab.
func CloseTab(req CloseTabRequest) (string, error) {
	var resp CloseTabResponse
	if err := callDaemon("CloseTab", req, &resp); err != nil {
		return "", err
	}
	return resp.Name, nil
}

// SetPRInfo asks the daemon to record (or clear) a session's GitHub PR info.
func SetPRInfo(req SetPRInfoRequest) error {
	var resp SetPRInfoResponse
	if err := callDaemon("SetPRInfo", req, &resp); err != nil {
		return err
	}
	return nil
}

// The TUI read path is SnapshotWithAlarms (snapshot.go): it carries the session
// list plus the delivery-failure alarms in one authoritative response (#1238).
// SnapshotNoSpawn below is the CLI's non-spawning, instances-only read.

// ErrDaemonUnavailable signals that a non-spawning daemon read (SnapshotNoSpawn)
// found no reachable, ready daemon: the control socket is absent/refused or the
// daemon is still restoring instances (#829). It is the CLI read path's cue to
// fall back to reading instances.json off disk — never to spawn a daemon or to
// surface a transient RPC error from a read-only command (#1029 PR 2).
var ErrDaemonUnavailable = errors.New("daemon not available")

// SnapshotNoSpawn returns the daemon's authoritative session snapshot WITHOUT
// starting a daemon. Unlike Snapshot — which calls EnsureDaemon and spawns a
// daemon when none is running — this dials the existing control socket only if
// it is already serving. It is the read path for CLI commands (sessions
// list/get/whoami) that must keep working with no daemon present (scripts, CI)
// and must never launch one. When no live state is available it returns
// ErrDaemonUnavailable so the caller falls back to disk; it returns the live
// instances only on a clean Snapshot success.
func SnapshotNoSpawn(req SnapshotRequest) ([]session.InstanceData, error) {
	var resp SnapshotResponse
	if err := callDaemonNoEnsure("Snapshot", req, &resp); err != nil {
		// A dial failure means no daemon is running; a starting error (#829)
		// means one is warming up. Either way there is no authoritative live
		// state to read, so signal the caller to fall back to disk rather than
		// spawning a daemon or failing a read-only command.
		return nil, ErrDaemonUnavailable
	}
	return resp.Instances, nil
}

// KillSession asks the daemon to kill a session and remove it from storage.
func KillSession(req KillSessionRequest) error {
	var resp KillSessionResponse
	if err := callDaemon("KillSession", req, &resp); err != nil {
		return err
	}
	return nil
}

// ArchiveSession asks the daemon to archive a session (#1028) and returns the
// relocated worktree's new path.
func ArchiveSession(req ArchiveSessionRequest) (string, error) {
	var resp ArchiveSessionResponse
	if err := callDaemon("ArchiveSession", req, &resp); err != nil {
		return "", err
	}
	return resp.ArchivedPath, nil
}

// RestoreArchived asks the daemon to restore an archived session (#1028) and
// returns the worktree's restored path.
func RestoreArchived(req RestoreArchivedRequest) (string, error) {
	var resp RestoreArchivedResponse
	if err := callDaemon("RestoreArchived", req, &resp); err != nil {
		return "", err
	}
	return resp.WorktreePath, nil
}

// PauseStatusPoll asks the daemon to pause its capture-pane liveness poll for
// one attached session (#1160). Best-effort from the caller's side: the pause
// is lease-bounded server-side, so the worst case of a failed call is the
// daemon keeps polling — exactly the pre-#1160 behavior — never a broken
// attach or a permanently-blinded daemon.
func PauseStatusPoll(req PauseStatusPollRequest) error {
	var resp PauseStatusPollResponse
	if err := callDaemon("PauseStatusPoll", req, &resp); err != nil {
		return err
	}
	return nil
}

// ResumeStatusPoll asks the daemon to resume polling a session on a clean
// detach so its status refreshes on the next tick instead of after the lease
// expires (#1160).
func ResumeStatusPoll(req ResumeStatusPollRequest) error {
	var resp ResumeStatusPollResponse
	if err := callDaemon("ResumeStatusPoll", req, &resp); err != nil {
		return err
	}
	return nil
}

// SendPrompt asks the daemon to send a prompt to an existing session.
func SendPrompt(req SendPromptRequest) error {
	var resp SendPromptResponse
	if err := callDaemon("SendPrompt", req, &resp); err != nil {
		return err
	}
	return nil
}

// DeliverPrompt asks the daemon to deliver a prompt to a target session,
// auto-creating it when missing. It returns the recorded status ("started" or
// "sent"). Unlike a bare CreateSession-then-SendPrompt from the caller, the
// whole create-or-send decision runs under the daemon's per-target lock, so
// concurrent deliveries to the same shared target never drop a prompt (#865).
func DeliverPrompt(req DeliverPromptRequest) (string, error) {
	var resp DeliverPromptResponse
	if err := callDaemon("DeliverPrompt", req, &resp); err != nil {
		return "", err
	}
	return resp.Status, nil
}

// ListTasksNoSpawn returns the daemon's authoritative task list WITHOUT
// starting a daemon (#1029 PR 3). Like SnapshotNoSpawn it dials the existing
// control socket only if it is already serving and returns ErrDaemonUnavailable
// otherwise, so a read-only CLI command (tasks list/get) falls back to reading
// tasks.json off disk rather than ever launching a daemon. Task reads do not
// depend on the instance restore, so there is no warm-up starting-error window
// to wait out here.
func ListTasksNoSpawn() ([]task.Task, error) {
	var resp ListTasksResponse
	if err := callDaemonNoEnsure("ListTasks", ListTasksRequest{}, &resp); err != nil {
		return nil, ErrDaemonUnavailable
	}
	return resp.Tasks, nil
}

// AddTask asks the daemon to append a task and re-arm its schedule set. Like
// every callDaemon path it ensures the daemon is running first, so adding a task
// also brings the scheduler up — a task is not schedulable without a running
// daemon.
func AddTask(t task.Task) error {
	var resp AddTaskResponse
	return callDaemon("AddTask", AddTaskRequest{Task: t}, &resp)
}

// UpdateTask asks the daemon to persist an edited task and re-arm its schedule.
func UpdateTask(t task.Task) error {
	var resp UpdateTaskResponse
	return callDaemon("UpdateTask", UpdateTaskRequest{Task: t}, &resp)
}

// RemoveTask asks the daemon to delete a task and re-arm its schedule.
func RemoveTask(id string) error {
	var resp RemoveTaskResponse
	return callDaemon("RemoveTask", RemoveTaskRequest{ID: id}, &resp)
}

// TriggerTask asks the daemon to fire a task now through the shared RunTask
// firing path (the same entrypoint the in-daemon scheduler uses). Replaces the
// old in-process daemon.RunTask CLI call so CLI, TUI, and scheduler triggers all
// converge on one daemon-owned firing path (#1169-class fix).
func TriggerTask(id string) error {
	var resp TriggerTaskResponse
	return callDaemon("TriggerTask", TriggerTaskRequest{ID: id}, &resp)
}

// ImportRemoteHookSessions asks the daemon to reconcile remote sessions
// reported by list_cmd into persisted storage.
func ImportRemoteHookSessions(req ImportRemoteHookSessionsRequest) ([]session.InstanceData, error) {
	var resp ImportRemoteHookSessionsResponse
	if err := callDaemon("ImportRemoteHookSessions", req, &resp); err != nil {
		return nil, err
	}
	return resp.Instances, nil
}

// ShutdownResult reports how RequestShutdown stopped (or failed to stop) the
// running daemon. Used by upgrade.go and autoupdate.go to pick the right
// user-facing message after a binary swap.
type ShutdownResult int

const (
	// ShutdownNoDaemon means no daemon was running (no socket, ECONNREFUSED,
	// or PID-file scan found nothing). The upgrade prints bare success.
	ShutdownNoDaemon ShutdownResult = iota
	// ShutdownViaRPC means the daemon acknowledged the Shutdown RPC and is
	// exiting cleanly. The post-#501 happy path.
	ShutdownViaRPC
	// ShutdownViaSIGTERM means the daemon was a pre-#501 binary that did not
	// register the Shutdown RPC, so we located its PID and signaled it
	// directly. The upgrade prints a slightly different success message so
	// users know we used the fallback. See #504.
	ShutdownViaSIGTERM
	// ShutdownFailed means a daemon was proven to be listening on the
	// control socket (the Shutdown RPC came back as method-not-found, not
	// ECONNREFUSED) but the SIGTERM fallback could not locate a PID to
	// signal — e.g. no PID file AND pgrep is unavailable on this host. The
	// daemon is still running the old binary; the caller must surface the
	// recovery hint in the accompanying error. See #553.
	ShutdownFailed
	// ShutdownError means the control socket was present and a Shutdown RPC
	// was attempted, but it failed with an error that does NOT prove the
	// daemon absent and is NOT method-not-found: EACCES (socket exists but
	// the caller lacks permission to connect), ECONNRESET/EPIPE (the
	// connection was established then reset), or a dial timeout (the socket
	// is bound but the listener is unresponsive). All of these imply a daemon
	// WAS listening, so reporting ShutdownNoDaemon — documented as "no daemon
	// was running" — would mislabel the outcome. The daemon's final state is
	// unknown and it may still be running; the accompanying error carries the
	// detail. See #978.
	ShutdownError
)

// sigtermFallbackGrace is the max time we wait for a SIGTERM'd daemon to exit
// before escalating to SIGKILL.
const sigtermFallbackGrace = 5 * time.Second

// sigtermFallbackPoll is how often we check whether the SIGTERM'd daemon has
// exited.
const sigtermFallbackPoll = 100 * time.Millisecond

// RequestShutdown asks any running daemon to exit cleanly. The normal path
// uses the Shutdown RPC (#498/#501). When the running daemon is a pre-#501
// binary that does not register Shutdown, we fall back to locating the
// daemon's PID and sending SIGTERM directly (#504) so an `af upgrade` does
// not leave a stale daemon running the old binary.
//
// Returns (ShutdownNoDaemon, nil) when no daemon is running (no socket or
// ECONNREFUSED), (ShutdownViaRPC, nil) when the Shutdown RPC acknowledged,
// (ShutdownViaSIGTERM, nil) when the fallback signaled a real `af --daemon`
// process, (ShutdownFailed, err) when the daemon is provably running but
// the fallback could not locate or signal it (ambiguous pgrep matches, no
// PID file with pgrep unavailable, permission denied on signal) — the
// returned error carries the recovery hint the caller must surface — and
// (ShutdownError, err) when the socket was present but the Shutdown RPC
// failed with a transport error that is neither daemon-absent nor
// method-not-found (EACCES, ECONNRESET/EPIPE, dial timeout): a daemon was
// listening but its final state is unknown (#978).
func RequestShutdown() (ShutdownResult, error) {
	socketPath, err := DaemonSocketPath()
	if err != nil {
		return ShutdownNoDaemon, err
	}
	if _, statErr := os.Stat(socketPath); statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return ShutdownNoDaemon, nil
		}
		return ShutdownNoDaemon, statErr
	}
	var resp ShutdownResponse
	if rpcErr := callDaemonNoEnsure("Shutdown", ShutdownRequest{}, &resp); rpcErr != nil {
		if isDaemonAbsentErr(rpcErr) {
			return ShutdownNoDaemon, nil
		}
		if isRPCMethodNotFoundErr(rpcErr) {
			// Daemon is alive on the socket but does not speak Shutdown
			// (pre-#501 binary). Fall through to the PID-based fallback.
			return sigtermFallback()
		}
		// The socket was present (os.Stat above succeeded) and the error is
		// neither daemon-absent (ECONNREFUSED/ENOENT) nor method-not-found:
		// EACCES, ECONNRESET/EPIPE, or a dial timeout. Something was listening,
		// so ShutdownNoDaemon would mislabel this — report the ambiguous
		// contacted-but-errored outcome instead (#978).
		return ShutdownError, rpcErr
	}
	if !resp.OK {
		return ShutdownNoDaemon, fmt.Errorf("daemon Shutdown RPC returned OK=false")
	}
	return ShutdownViaRPC, nil
}

// shutdownCompleteGrace bounds how long WaitForShutdownCompletion polls for
// the control socket to stop answering; shutdownCompletePoll is the cadence.
// Package vars rather than constants so tests can shorten the timeout path,
// mirroring stopDaemonGrace/stopDaemonPoll. The grace matches
// sigtermFallbackGrace — the wait signalAndWait already imposes on the
// SIGTERM path. The poll is tighter than sigtermFallbackPoll because the
// normal RPC teardown completes just past shutdownAckGrace (50ms), so a 50ms
// cadence usually resolves the wait on its first or second check.
var (
	shutdownCompleteGrace = sigtermFallbackGrace
	shutdownCompletePoll  = shutdownAckGrace
)

// WaitForShutdownCompletion blocks until the daemon control socket stops
// answering pings, bounded by shutdownCompleteGrace. The Shutdown RPC
// acknowledges before the daemon tears down (shutdownAckGrace plus the
// teardown tail), so a caller that respawns immediately after RequestShutdown
// races the dying daemon: EnsureDaemon's liveness ping — or a unit-restarted
// daemon's startup ping guard — can see the old socket still answering, skip
// the spawn, and leave nothing running once the old daemon exits (#854).
// Callers on the shutdown-then-respawn path must wait for this to return
// before respawning. It mirrors signalAndWait's poll-until-dead discipline;
// on the SIGTERM fallback path the process is already gone, so the first ping
// fails and the wait returns immediately. Returns an error when the daemon is
// still answering at the deadline — the caller should warn and proceed.
func WaitForShutdownCompletion() error {
	deadline := time.Now().Add(shutdownCompleteGrace)
	for time.Now().Before(deadline) {
		if pingDaemon() != nil {
			return nil
		}
		time.Sleep(shutdownCompletePoll)
	}
	return fmt.Errorf("daemon control socket still answering %s after shutdown was acknowledged", shutdownCompleteGrace)
}

// isDaemonAbsentErr reports whether err from a dial/RPC call indicates that
// no daemon is currently listening on the control socket (vs. some other
// transport failure). Both ECONNREFUSED (stale socket, no listener) and
// ENOENT (socket removed between Stat and Dial) qualify. Application-level
// RPC errors (method-not-found, server panic) do NOT — those are handled
// separately by isRPCMethodNotFoundErr so we can route them to the SIGTERM
// fallback rather than treating them as "no daemon" and silently leaving the
// stale process running (#504).
func isDaemonAbsentErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, fs.ErrNotExist) {
		return true
	}
	return false
}

// isRPCMethodNotFoundErr reports whether err is the net/rpc server's reply
// for an unknown method or service. The connection succeeded (daemon is
// running, control socket is alive) but the registered service did not have
// the requested method — i.e. a pre-#501 daemon that never registered
// "Control.Shutdown". The stdlib returns this as rpc.ServerError with the
// literal prefix "rpc: can't find method " or "rpc: can't find service ".
func isRPCMethodNotFoundErr(err error) bool {
	if err == nil {
		return false
	}
	var serverErr rpc.ServerError
	if !errors.As(err, &serverErr) {
		return false
	}
	s := string(serverErr)
	return strings.Contains(s, "can't find method") || strings.Contains(s, "can't find service")
}

type controlServer struct {
	manager      *Manager
	scheduler    *taskScheduler
	watchers     *watcherSupervisor
	shutdownCh   chan struct{}
	shutdownOnce sync.Once
}

func (s *controlServer) Ping(_ PingRequest, resp *PingResponse) error {
	resp.OK = true
	return nil
}

// PauseStatusPoll pauses the daemon's capture-pane liveness poll for one
// attached session (#1160). Deliberately NOT gated on requireManagerReady:
// it is a lightweight, lease-bounded map write on the dedicated pausedMu (not
// m.mu), independent of the instance restore — a pause that lands during
// warm-up is honored once the instance is restored, and it can never corrupt
// state the way a create/kill racing the restore could. A nil manager (some
// test control servers) is a no-op ack.
func (s *controlServer) PauseStatusPoll(req PauseStatusPollRequest, resp *PauseStatusPollResponse) error {
	if s.manager != nil {
		s.manager.PauseStatusPoll(req.RepoID, req.Title)
	}
	resp.OK = true
	return nil
}

// ResumeStatusPoll clears a pause set by PauseStatusPoll (#1160). Same
// lightweight, ungated conventions as PauseStatusPoll.
func (s *controlServer) ResumeStatusPoll(req ResumeStatusPollRequest, resp *ResumeStatusPollResponse) error {
	if s.manager != nil {
		s.manager.ResumeStatusPoll(req.RepoID, req.Title)
	}
	resp.OK = true
	return nil
}

// reloadTaskSchedules re-arms the daemon's cron scheduler and watcher
// supervisor from tasks.json. It is the shared refresh the ReloadTasks poke and
// the task CRUD RPCs (Add/Update/RemoveTask) both invoke after a write, so the
// write and its schedule refresh happen atomically in-daemon and no separate
// ReloadTasks poke is needed for CRUD.
//
// During warm-up (#829) the scheduler and watcher supervisor have not started
// yet; RunDaemon reloads both from tasks.json right after the restore completes,
// so a change just written is picked up then. It returns nil (nothing to
// reload) instead of erroring — the write is already durable.
func (s *controlServer) reloadTaskSchedules() error {
	if s.scheduler == nil {
		return fmt.Errorf("this daemon does not host a task scheduler")
	}
	if s.manager != nil && !s.manager.Ready() {
		return nil
	}
	if err := s.scheduler.Reload(); err != nil {
		return err
	}
	// Watch tasks live in the watcher supervisor; one reload poke re-arms
	// both trigger types (#782 phase 2). Nil only in tests that exercise the
	// scheduler alone.
	if s.watchers != nil {
		if err := s.watchers.Reload(); err != nil {
			return err
		}
	}
	return nil
}

func (s *controlServer) ReloadTasks(_ ReloadTasksRequest, resp *ReloadTasksResponse) error {
	if err := s.reloadTaskSchedules(); err != nil {
		return err
	}
	resp.OK = true
	return nil
}

// ListTasks returns the full task list read from tasks.json (#1029 PR 3).
// Deliberately NOT gated on requireManagerReady: task state lives on disk,
// independent of the instance restore, so a read is always safe and always
// current even while the daemon is warming up.
func (s *controlServer) ListTasks(_ ListTasksRequest, resp *ListTasksResponse) error {
	tasks, err := task.LoadTasks()
	if err != nil {
		return err
	}
	resp.Tasks = tasks
	return nil
}

// AddTask persists a new task and re-arms the schedule set (#1029 PR 3). The
// write goes through task.AddTask (config.WithFileLock + saveTasks) — the same
// path clients used to call directly — so the on-disk format is unchanged; the
// difference is the daemon now owns it and refreshes its own scheduler/watchers
// in the same call.
func (s *controlServer) AddTask(req AddTaskRequest, resp *AddTaskResponse) error {
	if err := task.AddTask(req.Task); err != nil {
		return err
	}
	if err := s.reloadTaskSchedules(); err != nil {
		return err
	}
	resp.OK = true
	return nil
}

func (s *controlServer) UpdateTask(req UpdateTaskRequest, resp *UpdateTaskResponse) error {
	if err := task.UpdateTask(req.Task); err != nil {
		return err
	}
	if err := s.reloadTaskSchedules(); err != nil {
		return err
	}
	resp.OK = true
	return nil
}

func (s *controlServer) RemoveTask(req RemoveTaskRequest, resp *RemoveTaskResponse) error {
	if err := task.RemoveTask(req.ID); err != nil {
		return err
	}
	if err := s.reloadTaskSchedules(); err != nil {
		return err
	}
	resp.OK = true
	return nil
}

// TriggerTask fires a task NOW through the shared RunTask firing path — the same
// entrypoint the in-daemon scheduler uses (#1029 PR 3). This unifies the CLI
// `af tasks trigger`, the TUI run-now, and the cron scheduler on one
// daemon-owned firing path, replacing the old in-process daemon.RunTask CLI call
// (#1169-class fix). RunTask preserves the guards: watch tasks and disabled
// tasks are refused.
func (s *controlServer) TriggerTask(req TriggerTaskRequest, resp *TriggerTaskResponse) error {
	if err := RunTask(req.ID); err != nil {
		return err
	}
	resp.OK = true
	return nil
}

// Shutdown acknowledges a request to terminate the daemon, then asynchronously
// signals the main loop to tear down after a short grace period. The grace
// lets the RPC response flush back to the caller before the listener closes.
func (s *controlServer) Shutdown(_ ShutdownRequest, resp *ShutdownResponse) error {
	resp.OK = true
	if s.shutdownCh == nil {
		return nil
	}
	s.shutdownOnce.Do(func() {
		go func() {
			time.Sleep(shutdownAckGrace)
			close(s.shutdownCh)
		}()
	})
	return nil
}

// requireManagerReady gates RPC handlers that read or mutate restored session
// state. During warm-up (socket bound, restore still running — #829) they fail
// fast with errDaemonStarting rather than operating on an empty instance map:
// a CreateSession could race the restore into duplicate Instances, and a
// KillSession/SendPrompt would construct throwaway instances from disk that
// the restore then orphans. Ping and Shutdown stay available throughout.
func (s *controlServer) requireManagerReady() error {
	if s.manager == nil || s.manager.Ready() {
		return nil
	}
	return errDaemonStarting()
}

func (s *controlServer) CreateSession(req CreateSessionRequest, resp *CreateSessionResponse) error {
	if err := s.requireManagerReady(); err != nil {
		return err
	}
	data, err := s.manager.CreateSession(req)
	if err != nil {
		return err
	}
	resp.Instance = data
	return nil
}

func (s *controlServer) CreateTab(req CreateTabRequest, resp *CreateTabResponse) error {
	if err := s.requireManagerReady(); err != nil {
		return err
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	name, err := s.manager.CreateTab(req)
	if err != nil {
		return err
	}
	resp.Name = name
	return nil
}

func (s *controlServer) CloseTab(req CloseTabRequest, resp *CloseTabResponse) error {
	if err := s.requireManagerReady(); err != nil {
		return err
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	name, err := s.manager.CloseTab(req)
	if err != nil {
		return err
	}
	resp.Name = name
	return nil
}

func (s *controlServer) SetPRInfo(req SetPRInfoRequest, resp *SetPRInfoResponse) error {
	if err := s.requireManagerReady(); err != nil {
		return err
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	if err := s.manager.SetPRInfo(req); err != nil {
		return err
	}
	resp.OK = true
	return nil
}

func (s *controlServer) Snapshot(req SnapshotRequest, resp *SnapshotResponse) error {
	if err := s.requireManagerReady(); err != nil {
		return err
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	resp.Instances = s.manager.Snapshot(req.RepoID)
	resp.DeliveryAlarms = s.deliveryAlarms(req.RepoID)
	return nil
}

func (s *controlServer) KillSession(req KillSessionRequest, resp *KillSessionResponse) error {
	if err := s.requireManagerReady(); err != nil {
		return err
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	if err := s.manager.KillSession(req); err != nil {
		return err
	}
	resp.OK = true
	return nil
}

func (s *controlServer) ArchiveSession(req ArchiveSessionRequest, resp *ArchiveSessionResponse) error {
	if err := s.requireManagerReady(); err != nil {
		return err
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	archivedPath, err := s.manager.ArchiveSession(req)
	if err != nil {
		return err
	}
	resp.OK = true
	resp.ArchivedPath = archivedPath
	return nil
}

func (s *controlServer) RestoreArchived(req RestoreArchivedRequest, resp *RestoreArchivedResponse) error {
	if err := s.requireManagerReady(); err != nil {
		return err
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	worktreePath, err := s.manager.RestoreArchived(req)
	if err != nil {
		return err
	}
	resp.OK = true
	resp.WorktreePath = worktreePath
	return nil
}

func (s *controlServer) SendPrompt(req SendPromptRequest, resp *SendPromptResponse) error {
	if err := s.requireManagerReady(); err != nil {
		return err
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	if err := s.manager.SendPrompt(req); err != nil {
		return err
	}
	resp.OK = true
	return nil
}

// validateRPCRepoID enforces the RepoID shape for RPC requests that allow an
// empty value to mean "search all repos". A non-empty RepoID must satisfy
// config.ValidateRepoID so it cannot escape the per-repo file scope through
// path traversal characters (#515).
func validateRPCRepoID(repoID string) error {
	if repoID == "" {
		return nil
	}
	if err := config.ValidateRepoID(repoID); err != nil {
		return fmt.Errorf("rejected RPC request: %w", err)
	}
	return nil
}

func (s *controlServer) DeliverPrompt(req DeliverPromptRequest, resp *DeliverPromptResponse) error {
	if err := s.requireManagerReady(); err != nil {
		return err
	}
	status, err := s.manager.DeliverPrompt(req)
	if err != nil {
		return err
	}
	resp.Status = status
	return nil
}

func (s *controlServer) ImportRemoteHookSessions(req ImportRemoteHookSessionsRequest, resp *ImportRemoteHookSessionsResponse) error {
	if err := s.requireManagerReady(); err != nil {
		return err
	}
	data, err := s.manager.ImportRemoteHookSessions(req)
	if err != nil {
		return err
	}
	resp.Instances = data
	return nil
}

// daemonSpawnLockTarget returns the lock target whose adjacent flock file
// (daemon.spawn.lock, via config.WithFileLock) serializes the daemon spawn
// window across processes.
func daemonSpawnLockTarget() (string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "daemon.spawn"), nil
}

// testHookSpawnPingPassed runs between the under-lock ping re-check and the
// socket bind in bindControlServerExclusive. Tests substitute it to hold a
// spawner inside that window and prove a concurrent spawner cannot enter it
// at the same time. No-op in production.
var testHookSpawnPingPassed = func() {}

// bindControlServerExclusive re-checks for a live daemon and binds the
// control socket while holding an exclusive cross-process file lock, making
// the ping→bind sequence atomic across processes. RunDaemon's top-of-function
// ping guard rejects the common duplicate-daemon cases, but two daemons
// starting near-simultaneously can both pass that ping before either binds;
// the second startControlServer would then unlink and rebind the socket path,
// orphaning the first daemon — alive and looping, but unreachable (#718).
//
// The lock is held only for the ping+bind window, not the daemon lifetime,
// and flock is released by the kernel if the holder dies, so a crashed
// spawner cannot wedge future spawns.
//
// Returns alreadyRunning=true when a live daemon answered the under-lock
// ping; the caller must exit cleanly (a non-zero exit would trip the
// autostart unit's Restart=on-failure into a retry loop against the live
// daemon).
func bindControlServerExclusive(manager *Manager, scheduler *taskScheduler, watchers *watcherSupervisor, shutdownCh chan struct{}) (closeFn func() error, alreadyRunning bool, err error) {
	lockTarget, lockTargetErr := daemonSpawnLockTarget()
	if lockTargetErr != nil {
		return nil, false, lockTargetErr
	}
	lockErr := config.WithFileLock(lockTarget, func() error {
		if pingErr := pingDaemon(); pingErr == nil {
			alreadyRunning = true
			return nil
		}
		testHookSpawnPingPassed()
		var serverErr error
		closeFn, serverErr = startControlServer(manager, scheduler, watchers, shutdownCh)
		return serverErr
	})
	if lockErr != nil {
		return nil, false, lockErr
	}
	return closeFn, alreadyRunning, nil
}

// startControlServer registers the control RPC service on the Unix socket and
// returns a cleanup function that closes the listener (which also unlinks the
// socket file). When shutdownCh is non-nil, the Shutdown RPC will close it on the
// first invocation, allowing the daemon main loop to exit on RPC request.
// scheduler may be nil for servers that do not host task schedules (tests);
// the ReloadTasks RPC then returns an error. watchers may likewise be nil,
// in which case ReloadTasks only refreshes cron entries.
func startControlServer(manager *Manager, scheduler *taskScheduler, watchers *watcherSupervisor, shutdownCh chan struct{}) (func() error, error) {
	socketPath, err := DaemonSocketPath()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(socketPath), 0755); err != nil {
		return nil, err
	}
	_ = os.Remove(socketPath)

	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(socketPath, 0600); err != nil {
		_ = listener.Close()
		return nil, err
	}

	server := rpc.NewServer()
	if err := server.RegisterName(controlServiceName, &controlServer{
		manager:    manager,
		scheduler:  scheduler,
		watchers:   watchers,
		shutdownCh: shutdownCh,
	}); err != nil {
		_ = listener.Close()
		return nil, err
	}

	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go server.ServeConn(conn)
		}
	}()

	return func() error {
		// Closing the listener also unlinks the socket file (net's default
		// unlink-on-close for unix listeners it created). Deliberately no
		// explicit os.Remove here: between the close-unlink and an explicit
		// Remove, a new daemon can pass its ping check and bind a fresh
		// socket at the same path, and the Remove would delete the new
		// daemon's socket, orphaning it — the same race class as #718/#767.
		return listener.Close()
	}, nil
}

// Manager owns the daemon's authoritative session mutations.
type Manager struct {
	cfg *config.Config

	// limitDetector is the resolved usage-limit matcher set (#1146), built once
	// from cfg.LimitPatterns at construction (it compiles the override regexes)
	// and reused across poll ticks. Immutable; read lock-free by the poll loop.
	limitDetector task.LimitDetector

	// ready is closed once the initial instance restore has completed. Until
	// then the daemon is "warming up": the control socket is already bound
	// (#829) but state-dependent RPCs return errDaemonStarting.
	ready     chan struct{}
	readyOnce sync.Once

	mu                  sync.Mutex
	storage             *session.Storage
	instances           map[string]*session.Instance
	reservedTitles      map[string]struct{}
	reservedRemoteNames map[string]struct{}
	repoStartLocks      map[string]*sync.Mutex
	// targetLocks serializes DeliverPrompt per (repo, title) so concurrent
	// deliveries to the same shared target session create it once and deliver
	// the rest in arrival order instead of racing creation and dropping the
	// losers' prompts (#865). Lazily populated like repoStartLocks.
	targetLocks map[string]*sync.Mutex
	// rootEnsureStates tracks per-configured-repo retry state for the
	// root-agent ensure loop (#1106), keyed by the root_agents config key
	// (the repo path as written in config.json).
	rootEnsureStates map[string]*rootEnsureState
	// rootKilledAt records repos (by repo ID) whose root agent was explicitly
	// killed, and WHEN. The ensure loop honors the kill only for
	// rootKillHealDelay, then self-heals a still-configured root (#1223): config
	// (root_agents), not a runtime kill, decides whether an always-on root runs.
	rootKilledAt map[string]time.Time
	// killsInFlight marks sessions (by daemon instance key) whose KillSession
	// teardown is currently running, so the status poll's finish-kill pass for
	// tombstoned records (#1108) never runs a second concurrent teardown of
	// the same session, and a duplicate KillSession RPC is rejected instead of
	// double-killing.
	killsInFlight map[string]struct{}
	// lostRestoreStates tracks per-session retry state for the Lost-session
	// restore loop (#1108 PR 2), keyed by daemon instance key — the general
	// sibling of rootEnsureStates.
	lostRestoreStates map[string]*lostRestoreState
	// limitResumeStates tracks per-session retry state for the usage-limit
	// auto-resume scheduler (#1146 PR3), keyed by daemon instance key — the
	// opt-in sibling of lostRestoreStates. Guarded by m.mu.
	limitResumeStates map[string]*limitResumeState
	// instanceOpLocks serializes the mutually-exclusive per-session
	// operations — kill teardown and Lost-recovery — by daemon instance key.
	// killsInFlight alone is a point-in-time signal; this lock is what makes
	// a KillSession arriving mid-Recover WAIT for the recover attempt and
	// then tear the restored session down, instead of interleaving a teardown
	// with a re-spawn. The recover side only TryLocks (the poll goroutine
	// must never stall behind a slow teardown). Lazily populated like
	// repoStartLocks; entries are never removed (a few bytes per session ever
	// touched).
	instanceOpLocks map[string]*sync.Mutex

	// pausedPolls records sessions whose daemon capture-pane liveness poll is
	// paused while a TUI is attached full-screen to them (#1160), keyed by
	// daemon instance key → lease expiry. Guarded by pausedMu, a DEDICATED
	// mutex (NOT m.mu): refreshInstanceStatus deliberately snapshots under m.mu
	// and then runs each slow tmux probe with m.mu RELEASED so a hung probe
	// can't block unrelated RPCs — the pause check runs inside that lock-free
	// window, so reusing m.mu would reintroduce exactly the contention the
	// release avoids. Each entry is lease-bounded (statusPollLease): a crashed
	// TUI that never sends Resume auto-resumes within one lease, so the pause
	// can never permanently blind the daemon.
	pausedMu    sync.Mutex
	pausedPolls map[string]time.Time
}

// NewManager constructs a manager and synchronously restores all persisted
// instances into it, returning only once the manager is ready. RunDaemon
// deliberately does NOT use this: it builds the shell with newManagerShell,
// binds the control socket, and only then runs RestoreInstances — the restore
// can take minutes on remote-hook repos and must not delay the bind (#829).
func NewManager(cfg *config.Config) (*Manager, error) {
	manager, err := newManagerShell(cfg)
	if err != nil {
		return nil, err
	}
	if err := manager.RestoreInstances(); err != nil {
		return nil, err
	}
	return manager, nil
}

// newManagerShell constructs a Manager with no instances loaded. The manager
// reports !Ready() until RestoreInstances completes.
func newManagerShell(cfg *config.Config) (*Manager, error) {
	state := config.LoadState()
	storage, err := session.NewStorage(state, "")
	if err != nil {
		return nil, fmt.Errorf("failed to initialize storage: %w", err)
	}
	return &Manager{
		cfg:                 cfg,
		limitDetector:       task.NewLimitDetector(cfg.LimitPatterns),
		ready:               make(chan struct{}),
		storage:             storage,
		instances:           make(map[string]*session.Instance),
		reservedTitles:      make(map[string]struct{}),
		reservedRemoteNames: make(map[string]struct{}),
		repoStartLocks:      make(map[string]*sync.Mutex),
		targetLocks:         make(map[string]*sync.Mutex),
		rootEnsureStates:    make(map[string]*rootEnsureState),
		rootKilledAt:        make(map[string]time.Time),
		killsInFlight:       make(map[string]struct{}),
		lostRestoreStates:   make(map[string]*lostRestoreState),
		limitResumeStates:   make(map[string]*limitResumeState),
		instanceOpLocks:     make(map[string]*sync.Mutex),
		pausedPolls:         make(map[string]time.Time),
	}, nil
}

// RestoreInstances loads every repo's persisted instances into the manager
// and marks it ready. This is the slow part of daemon startup — restoring a
// remote-hook session shells out to the repo's list_cmd (often ssh) per
// session — which is why RunDaemon runs it only after the control socket is
// bound (#829). Replacing the instance map wholesale is safe: every RPC that
// mutates it is gated on Ready, and the refresh poll loop starts after the
// restore completes.
func (m *Manager) RestoreInstances() error {
	instances, err := refreshDaemonInstances(nil)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.instances = instances
	m.mu.Unlock()
	m.readyOnce.Do(func() { close(m.ready) })
	return nil
}

// Ready reports whether the initial instance restore has completed.
func (m *Manager) Ready() bool {
	select {
	case <-m.ready:
		return true
	default:
		return false
	}
}

func (m *Manager) RefreshInstances() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.refreshLocked()
}

func (m *Manager) InstancesSnapshot() []*session.Instance {
	m.mu.Lock()
	defer m.mu.Unlock()
	return daemonInstances(m.instances)
}

// RefreshStatuses recomputes every started instance's status the way the TUI
// metadata tick used to (#935) and persists each transition through the
// targeted single-writer path. With the daemon the sole owner of session state
// (#960 PR 4/5), status is authoritative HERE and projected to the TUI via
// Snapshot — the TUI no longer computes it. Called once per poll from RunDaemon,
// alongside the AutoYes pass it now subsumes.
//
// The instance list is snapshotted under m.mu, then each instance's (possibly
// slow) tmux probes run with the lock released so a hung capture-pane can't
// block unrelated manager RPCs.
// PauseStatusPoll pauses the daemon's capture-pane liveness poll for one
// attached session for statusPollLease from now (#1160). Renewing (the TUI's
// heartbeat) just pushes the expiry out; the pause is per-instance, so every
// other session keeps refreshing during the attach.
func (m *Manager) PauseStatusPoll(repoID, title string) {
	key := daemonInstanceKey(repoID, title)
	m.pausedMu.Lock()
	m.pausedPolls[key] = nowFunc().Add(statusPollLease)
	m.pausedMu.Unlock()
}

// ResumeStatusPoll clears a pause immediately on a clean detach so the poll
// resumes on the next tick rather than waiting out the lease (#1160).
func (m *Manager) ResumeStatusPoll(repoID, title string) {
	key := daemonInstanceKey(repoID, title)
	m.pausedMu.Lock()
	delete(m.pausedPolls, key)
	m.pausedMu.Unlock()
}

// isPollPaused reports whether an instance's poll is currently paused (#1160).
// A present-but-expired lease is lazily deleted and reported unpaused, so a
// crashed TUI that never sent Resume auto-resumes within one lease — the
// crash-safety property that keeps a pause from ever permanently blinding the
// daemon.
func (m *Manager) isPollPaused(repoID, title string) bool {
	key := daemonInstanceKey(repoID, title)
	m.pausedMu.Lock()
	defer m.pausedMu.Unlock()
	expiry, ok := m.pausedPolls[key]
	if !ok {
		return false
	}
	if nowFunc().Before(expiry) {
		return true
	}
	delete(m.pausedPolls, key) // lease lapsed — lazy GC, then poll as normal
	return false
}

func (m *Manager) RefreshStatuses() {
	type entry struct {
		repoID   string
		instance *session.Instance
	}
	m.mu.Lock()
	entries := make([]entry, 0, len(m.instances))
	for key, inst := range m.instances {
		repoID, _ := splitDaemonInstanceKey(key)
		entries = append(entries, entry{repoID: repoID, instance: inst})
	}
	m.mu.Unlock()

	for _, e := range entries {
		m.refreshInstanceStatus(e.repoID, e.instance)
	}
}

// refreshInstanceStatus mirrors the old runMetadataTick body for one instance:
//   - skip unstarted instances and any with an in-flight op (an archive/restore
//     mid-teardown, a create/kill overlay — probing or writing either would poke
//     a session whose tmux is being spun up or torn down, #844/#1195);
//   - dismiss a pending trust prompt (CheckAndHandleTrustPrompt), moved here from
//     the TUI so it works whether or not a TUI is attached;
//   - HasUpdated → Running; a waiting prompt → TapEnter (the AutoYes path, which
//     this poll already owned — unchanged by #960);
//   - otherwise probe liveness: a vanished tmux/remote session → Lost (never
//     repainted Ready, the #935 invariant the hollow status-dot rendering
//     relies on; Lost rather than Dead since #1108 — no kill intent on record
//     means the session is recovery-eligible), a live idle one → Ready;
//   - a session carrying the kill-intent tombstone (#1108) short-circuits all
//     of the above: its interrupted teardown is finished instead.
//
// The poll writes only the liveness axis (SetLiveness), gated on there being no
// in-flight op — so it can never clobber a concurrent kill/archive marker, which
// lives on the separate op axis (#1195). Only a real transition is persisted, and it persists
// under the per-repo start lock (mirroring CreateTab/CloseTab/SetPRInfo) through
// the targeted writer persistInstanceData — never a whole-list re-marshal, the
// dual-writer clobber surface #960 PR 4 retired — so an idle session never churns
// instances.json.
func (m *Manager) refreshInstanceStatus(repoID string, instance *session.Instance) {
	if instance == nil || !instance.Started() {
		return
	}
	if instance.UserKilled() {
		// A surviving kill-intent tombstone (#1108) means a previous
		// KillSession was interrupted after committing to the kill. The only
		// valid future for this session is finishing that teardown — never
		// probing it, never marking it Lost, never restoring it.
		m.finishUserKill(repoID, instance)
		return
	}
	if instance.GetInFlightOp() != session.OpNone {
		// An op is mid-flight (archive/restore teardown, create/kill overlay): the
		// poll must not probe a session whose tmux is being spun up/torn down and
		// mark it Lost — the op's executor writes the settled liveness. Replaces
		// the old Loading/Deleting skip (#1195).
		return
	}
	if instance.GetLiveness() == session.LiveArchived {
		// Archived (#1028): no tmux to probe, inert (started=false) so already
		// skipped by !Started above — belt-and-suspenders against a future change.
		return
	}
	if m.isPollPaused(repoID, instance.Title) {
		// A TUI is attached full-screen to this instance (#1160). It owns the
		// shared tmux server for the attach duration; the daemon's capture-pane
		// liveness probe here would needlessly contend with the live attach and
		// hurt input responsiveness (Fix A follow-up to #1157). Skip the probe.
		// The status is left UNCHANGED — a paused instance is known-attached-and-
		// alive, never marked Lost (#1108): it has not vanished. Leak-safe: the
		// pause is lease-bounded (statusPollLease), so a crashed TUI that never
		// sends Resume auto-resumes within one lease and real death is detected
		// on the next tick — the pause can never permanently blind the daemon.
		return
	}

	instance.CheckAndHandleTrustPrompt()
	before := instance.GetLiveness()
	beforeReset, _ := instance.LimitResetAt()
	// HasUpdated hands back the captured pane content so the idle branch can run
	// the usage-limit detector (#1146) without a second capture-pane.
	updated, hasPrompt, content := instance.HasUpdated()
	if hasPrompt {
		// Tap enter whenever a prompt is waiting (TapEnter is a no-op unless
		// AutoYes is on), independent of `updated` — exactly as the pre-#965
		// AutoYes loop did with `if _, hasPrompt := instance.HasUpdated(); …`.
		// A prompt's text is itself fresh output, so a just-appeared prompt
		// commonly reports (updated, hasPrompt) == (true, true); folding the tap
		// into the switch below `case updated` swallowed it on that first tick
		// and only tapped on the next poll — a one-interval AutoYes delay (#992).
		instance.TapEnter()
	}
	switch {
	case updated:
		instance.SetLiveness(session.LiveRunning)
	case hasPrompt:
		// A waiting prompt with otherwise-unchanged output: leave the status for
		// the next tick to resolve, exactly as runMetadataTick did. The
		// prompt-tap already fired above regardless of `updated`.
	case !instance.TmuxAlive():
		// HasUpdated returned (false,false), which a healthy idle session and a
		// dead one both produce — indistinguishable on their own. Probe liveness
		// only on this idle branch so a vanished session is marked Lost and
		// rendered distinctly rather than repainted as a green Ready dot it can
		// no longer back (#935). Lost, not Dead (#1108): there is no kill
		// intent on record, so the session vanished out from under a live
		// record — an outage/reboot casualty that is recovery-eligible, not a
		// corpse the user wanted gone.
		instance.SetLiveness(session.LiveLost)
	default:
		// Idle output: settle to Ready, or LimitReached when the pane shows a
		// usage-limit banner for a claude/codex session (#1146). content is
		// HasUpdated's capture (no re-capture); see resolveIdleLiveness.
		m.resolveIdleLiveness(instance, content)
	}

	// Persist a liveness OR usage-limit reset-time change (#1146); see limit.go.
	m.persistPollChange(repoID, instance, before, beforeReset)
}

// SaveInstances writes the manager's authoritative in-memory instances to disk
// as a straight per-repo marshal (#960 PR 4). With the daemon the sole writer of
// instances.json there is no competing snapshot to reconcile, so this is no
// longer a merge. Every mutation already persists through a targeted writer
// (appendInstanceData / persistInstanceData / DeleteInstance) as it happens; this
// full save is just the shutdown checkpoint.
func (m *Manager) SaveInstances() error {
	return m.storage.SaveInstances(m.InstancesSnapshot())
}

// Snapshot returns the authoritative InstanceData for every session the manager
// owns, scoped to repoID (all repos when repoID is empty). It is the read side
// of the single-writer model (#960 PR 3): the manager's in-memory instance map
// IS the source of truth, so the TUI mirrors this projection instead of
// re-reading instances.json. Pure read — it copies the instance pointers under
// m.mu, then serializes each via ToInstanceData (which takes the instance's own
// lock) OUTSIDE m.mu so a slow serialize never blocks a concurrent mutation.
// Results are ordered by (repo, title) key for a stable diff, so the TUI
// reconcile does not repaint on map-iteration jitter.
func (m *Manager) Snapshot(repoID string) []session.InstanceData {
	m.mu.Lock()
	keys := make([]string, 0, len(m.instances))
	for key := range m.instances {
		if repoID != "" {
			rid, _ := splitDaemonInstanceKey(key)
			if rid != repoID {
				continue
			}
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	insts := make([]*session.Instance, 0, len(keys))
	for _, key := range keys {
		if inst := m.instances[key]; inst != nil {
			insts = append(insts, inst)
		}
	}
	m.mu.Unlock()

	data := make([]session.InstanceData, 0, len(insts))
	for _, inst := range insts {
		data = append(data, inst.ToInstanceData())
	}
	return data
}

func (m *Manager) refreshLocked() error {
	refreshed, err := refreshDaemonInstances(m.instances)
	if err != nil {
		return err
	}
	m.instances = refreshed
	return nil
}

func (m *Manager) CreateSession(req CreateSessionRequest) (session.InstanceData, error) {
	if req.Program == "" {
		// Default from the repo-resolved config so an in-repo
		// default_program applies to daemon-created sessions (task runs,
		// API creates) too. Falls back to the daemon's global config when
		// the repo path can't be resolved — reserveCreate will surface
		// that error with more context right after.
		req.Program = m.cfg.DefaultProgram
		if req.RepoPath != "" {
			if repo, err := config.RepoFromPath(req.RepoPath); err == nil {
				if resolved, rerr := config.ResolveConfig(repo.Root); rerr == nil {
					req.Program = resolved.DefaultProgram
				}
			}
		}
	}
	repo, title, release, err := m.reserveCreate(req)
	if err != nil {
		return session.InstanceData{}, err
	}
	defer release()

	repoStartLock := m.startLockForRepo(repo.ID)
	repoStartLock.Lock()
	defer repoStartLock.Unlock()

	instance, err := session.NewInstance(session.InstanceOptions{
		Title:       title,
		Path:        repo.Root,
		Program:     req.Program,
		AutoYes:     req.AutoYes,
		InPlace:     req.InPlace,
		ForceRemote: req.ForceRemote,
	})
	if err != nil {
		return session.InstanceData{}, err
	}

	// Single creation flow (#930 PR 3): every instance owns its worktree 1:1.
	// InPlace only changes WHICH worktree that is — the repo's own working tree,
	// marked external — not the flow itself. finishCreateStart marks the instance
	// live, PARKS it at a usage-limit wall (#1146 PR4), or returns a fatal error.
	if serr := finishCreateStart(instance, req.Prompt, task.StartAndSendPrompt(instance, req.Prompt)); serr != nil {
		_ = instance.Kill()
		return session.InstanceData{}, fmt.Errorf("failed to start instance: %w", serr)
	}
	data := instance.ToInstanceData()

	// Register the in-memory instance and persist it to disk inside the
	// same critical section. The daemon refresh loop rebuilds
	// session.Instance objects from disk for any key it does not already
	// see in m.instances, so a window where the entry exists on disk but
	// not in memory would let refresh construct a duplicate Instance
	// (opening a fresh PTY in the tmux backend) that gets orphaned when
	// the original is later stored under the same key.
	key := daemonInstanceKey(repo.ID, title)
	persistErr := func() error {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.instances[key] = instance
		if err := appendInstanceData(repo.ID, data); err != nil {
			delete(m.instances, key)
			return err
		}
		return nil
	}()
	if persistErr != nil {
		_ = instance.Kill()
		return session.InstanceData{}, persistErr
	}

	return data, nil
}

func (m *Manager) startLockForRepo(repoID string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()

	lock := m.repoStartLocks[repoID]
	if lock == nil {
		lock = &sync.Mutex{}
		m.repoStartLocks[repoID] = lock
	}
	return lock
}

// opLockFor returns the per-session operation lock serializing kill teardown
// against Lost-recovery for one daemon instance key (#1108 PR 2).
func (m *Manager) opLockFor(key string) *sync.Mutex {
	m.mu.Lock()
	defer m.mu.Unlock()

	lock := m.instanceOpLocks[key]
	if lock == nil {
		lock = &sync.Mutex{}
		m.instanceOpLocks[key] = lock
	}
	return lock
}

func (m *Manager) reserveCreate(req CreateSessionRequest) (*config.RepoContext, string, func(), error) {
	if req.RepoPath == "" {
		return nil, "", nil, fmt.Errorf("repo path is required")
	}
	repo, err := config.RepoFromPath(req.RepoPath)
	if err != nil {
		return nil, "", nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.refreshLocked(); err != nil {
		return nil, "", nil, err
	}

	diskData, err := loadRepoInstanceData(repo.ID)
	if err != nil {
		return nil, "", nil, err
	}

	title := req.Title
	if title == "" {
		base := req.TitleBase
		if base == "" {
			return nil, "", nil, fmt.Errorf("session title is required")
		}
		title, err = m.nextAvailableTitleLocked(repo.ID, repo.Root, base, req.Program, req.ForceRemote, diskData)
		if err != nil {
			return nil, "", nil, err
		}
	} else if err := m.validateTitleAvailableLocked(repo.ID, repo.Root, title, req.Program, req.ForceRemote, req.allowReserved, diskData); err != nil {
		return nil, "", nil, err
	}

	key := daemonInstanceKey(repo.ID, title)
	remoteName := ""
	if req.ForceRemote {
		remoteName = session.Slugify(title)
		if _, ok := m.reservedRemoteNames[remoteName]; ok {
			return nil, "", nil, fmt.Errorf("remote hook name %q is already reserved", remoteName)
		}
	}

	m.reservedTitles[key] = struct{}{}
	if remoteName != "" {
		m.reservedRemoteNames[remoteName] = struct{}{}
	}
	release := func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		delete(m.reservedTitles, key)
		if remoteName != "" {
			delete(m.reservedRemoteNames, remoteName)
		}
	}

	return repo, title, release, nil
}

func (m *Manager) nextAvailableTitleLocked(repoID, repoPath, baseTitle, program string, remote bool, diskData []session.InstanceData) (string, error) {
	for i := 1; i <= 10000; i++ {
		candidate := baseTitle
		if i > 1 {
			candidate = fmt.Sprintf("%s-%d", baseTitle, i)
		}
		if err := m.validateTitleAvailableLocked(repoID, repoPath, candidate, program, remote, false, diskData); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not find an available title for %q", baseTitle)
}

func (m *Manager) validateTitleAvailableLocked(repoID, repoPath, title, program string, remote, allowReserved bool, diskData []session.InstanceData) error {
	// Whitespace-only titles (e.g. "   ") are non-empty and so slip past a bare
	// == "" check, creating sessions with effectively blank names (#973). Trim
	// before the emptiness gate; the TUI naming flow applies the same check.
	if strings.TrimSpace(title) == "" {
		return fmt.Errorf("session title is required")
	}
	// The "root" title belongs to the daemon-managed root agent (#1106).
	// Every creation path lands here — TUI, `af sessions create`, task
	// spawns, DeliverPrompt auto-creates — so this single gate reserves the
	// name everywhere. Only the daemon's own ensure loop passes
	// allowReserved; title-base derivation (nextAvailableTitleLocked) never
	// does, so a base of "root" skips to "root-2" instead of erroring.
	if !allowReserved && session.IsReservedTitle(title) {
		return fmt.Errorf("session title %q is reserved for the daemon-managed root agent; pick another name (to run a root agent on this repo, add it to root_agents in ~/.agent-factory/config.json)", title)
	}
	// Titles are sanitized into git branch names (git.SanitizeBranchName
	// lowercases, turns spaces into dashes, strips unsafe chars, and collapses
	// dashes), so distinct titles can map to the same branch: "MyApp"/"myapp"
	// (#605) or "A B"/"a-b" (#741) both collide. The second worktree create
	// would otherwise fail with a cryptic git error, so reject the conflict
	// here, before any worktree or tmux setup runs.
	if existing, kind := m.findTitleConflictLocked(repoID, title, diskData); existing != "" {
		switch {
		case existing == title:
			if kind == titleConflictReserved {
				return fmt.Errorf("session with title %q is already reserved: %w", title, errConcurrentCreate)
			}
			return fmt.Errorf("session with title %q already exists: %w", title, errConcurrentCreate)
		default:
			return fmt.Errorf("session titled %q conflicts with existing session %q: both sanitize to the same git branch %q", title, existing, m.branchForTitle(title))
		}
	}
	if remote {
		candidate := session.Slugify(title)
		if _, ok := m.reservedRemoteNames[candidate]; ok {
			return fmt.Errorf("remote hook name %q is already reserved", candidate)
		}
		for _, data := range diskData {
			if data.BackendType != "remote" {
				continue
			}
			if session.RemoteHookName(data.Title, data.RemoteMeta) == candidate {
				return fmt.Errorf("remote session titled %q already maps to hook name %q", data.Title, candidate)
			}
		}
		return nil
	}
	if tmuxSession := tmux.NewTmuxSessionForRepo(title, repoPath, program); tmuxSession.DoesSessionExist() {
		// A tmux session exists with no daemon reservation, in-memory instance,
		// or disk record — an orphan left by a crash or an external process.
		// No creator will ever finish it, so this stays a plain error (not
		// errConcurrentCreate): DeliverPrompt must fail fast with cleanup
		// guidance rather than wait out waitForTargetSession's timeout (#916).
		return fmt.Errorf("conflicting tmux session %q is already running; no agent-factory session owns it. Clean it up with: tmux kill-session -t %s", title, tmuxSession.SanitizedName())
	}
	return nil
}

type titleConflictKind int

const (
	titleConflictNone titleConflictKind = iota
	titleConflictReserved
	titleConflictLive
	titleConflictDisk
)

// findTitleConflictLocked returns the existing title that conflicts with the
// given candidate, along with the source of the conflict. An empty result means
// the title is available. Two titles conflict when they derive the same git
// branch name: branches are produced by git.SanitizeBranchName, which lowercases
// and normalizes (spaces -> dashes, unsafe chars stripped, dashes collapsed),
// so distinct titles like "MyApp"/"myapp" (#605) or "A B"/"a-b" (#741) can map
// to one branch. Rejecting the collision here keeps the second worktree create
// from failing with a cryptic git error.
func (m *Manager) findTitleConflictLocked(repoID, title string, diskData []session.InstanceData) (string, titleConflictKind) {
	for key := range m.reservedTitles {
		rid, existing := splitDaemonInstanceKey(key)
		if rid == repoID && m.titlesCollide(existing, title) {
			return existing, titleConflictReserved
		}
	}
	for key, inst := range m.instances {
		rid, _ := splitDaemonInstanceKey(key)
		if rid != repoID || inst == nil {
			continue
		}
		if m.titlesCollide(inst.Title, title) {
			return inst.Title, titleConflictLive
		}
	}
	for _, data := range diskData {
		if !m.titlesCollide(data.Title, title) {
			continue
		}
		// Loading entries are transient TUI state with an empty worktree
		// path and cannot be restored. Older TUI binaries (#551) could
		// persist them to disk on quit, where they would block title
		// reuse forever. Treat them as ghosts that the next save will
		// reap rather than as live reservations.
		if data.Status == session.Loading {
			continue
		}
		return data.Title, titleConflictDisk
	}
	return "", titleConflictNone
}

// titlesCollide reports whether two session titles cannot coexist in the same
// repo because they would derive the same git branch. It delegates to the shared
// git.TitlesCollide helper so the daemon's authoritative validation and the
// TUI's naming pre-check stay in lockstep (#936).
func (m *Manager) titlesCollide(a, b string) bool {
	return git.TitlesCollide(a, b, m.cfg.BranchPrefix)
}

// branchForTitle derives the git branch name for a session title using the same
// prefix and sanitization the git worktree layer applies, so the daemon can
// detect branch collisions before worktree setup runs.
func (m *Manager) branchForTitle(title string) string {
	return git.BranchForTitle(m.cfg.BranchPrefix, title)
}

func (m *Manager) KillSession(req KillSessionRequest) error {
	instance, repoID, data, err := m.findSession(req.Title, req.RepoID)
	if err != nil {
		return err
	}

	key := daemonInstanceKey(repoID, req.Title)
	m.mu.Lock()
	if _, busy := m.killsInFlight[key]; busy {
		m.mu.Unlock()
		return fmt.Errorf("kill already in progress for session %q", req.Title)
	}
	m.killsInFlight[key] = struct{}{}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.killsInFlight, key)
		m.mu.Unlock()
	}()

	// Serialize against a Lost-recovery in flight for this session (#1108
	// PR 2): a kill arriving mid-Recover waits for the recover attempt to
	// finish and then tears the (possibly just-restored) session down —
	// never an interleaved teardown-vs-respawn. killsInFlight is registered
	// BEFORE this acquire, so the restore loop's in-lock re-check sees the
	// kill intent and aborts instead of racing to go first.
	opLock := m.opLockFor(key)
	opLock.Lock()
	defer opLock.Unlock()

	// Persist the kill-intent tombstone BEFORE teardown begins (#1108): if the
	// daemon dies or the teardown errors between here and DeleteInstance, the
	// surviving record is provably a user kill — the status poll finishes the
	// teardown instead of classifying the vanished session Lost and restoring
	// it. Best-effort: a failed tombstone write degrades to today's crash
	// window, which must not block the kill itself.
	m.persistKillTombstone(repoID, instance, data)

	if instance != nil {
		if err := instance.Kill(); err != nil {
			return fmt.Errorf("failed to kill instance: %w", err)
		}
	} else if data != nil {
		ghostCleanup(data, req.Title)
	}

	state := config.LoadState()
	storage, err := session.NewStorage(state, repoID)
	if err != nil {
		return err
	}
	if err := storage.DeleteInstance(req.Title); err != nil {
		return fmt.Errorf("failed to delete instance from storage: %w", err)
	}

	m.mu.Lock()
	delete(m.instances, key)
	if session.IsReservedTitle(req.Title) {
		// An explicit kill is honored only briefly: the ensure loop suppresses
		// re-creation for rootKillHealDelay, then self-heals a still-configured
		// root (#1223). Config (root_agents) is the source of truth — removing
		// the repo from it is the only permanent stop. Recorded even for
		// unconfigured repos (harmless — the loop never visits them — and it
		// keeps kill-vs-config-change ordering race-free).
		m.rootKilledAt[repoID] = nowFunc()
		log.InfoLog.Printf("root agent for repo %s killed by user; the ensure loop will re-create it in ~%s unless the repo is removed from root_agents", repoID, rootKillHealDelay)
	}
	m.mu.Unlock()
	return nil
}

// persistKillTombstone writes the kill-intent tombstone (#1108) for the session
// KillSession is about to tear down, so a record surviving a crash or teardown
// failure mid-kill is never classified Lost and restored. Best-effort by
// design: a failed write only degrades to the pre-tombstone crash window.
func (m *Manager) persistKillTombstone(repoID string, instance *session.Instance, data *session.InstanceData) {
	var d session.InstanceData
	switch {
	case instance != nil:
		instance.MarkUserKilled()
		d = instance.ToInstanceData()
	case data != nil:
		d = *data
		d.UserKilled = true
	default:
		return
	}
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	err := persistInstanceData(repoID, d)
	repoStartLock.Unlock()
	if err != nil {
		log.WarningLog.Printf("failed to persist kill tombstone for %q: %v", d.Title, err)
	}
}

// finishUserKill completes the teardown of a session whose record carries the
// kill-intent tombstone (#1108): the previous KillSession was interrupted by a
// daemon crash or a teardown error after the tombstone write. Mirrors the tail
// of KillSession — best-effort Kill, targeted record delete, map removal — and
// retries on the next poll if the record delete fails. Skips while an explicit
// KillSession for the same session is still in flight.
func (m *Manager) finishUserKill(repoID string, instance *session.Instance) {
	key := daemonInstanceKey(repoID, instance.Title)
	m.mu.Lock()
	if _, busy := m.killsInFlight[key]; busy {
		m.mu.Unlock()
		return
	}
	m.killsInFlight[key] = struct{}{}
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		delete(m.killsInFlight, key)
		m.mu.Unlock()
	}()

	// TryLock, not Lock: this runs on the poll goroutine, which must not
	// stall behind a concurrent slow operation on this session; the next
	// poll retries. (A KillSession in flight was already skipped above, so
	// contention here is only a still-releasing lock.)
	opLock := m.opLockFor(key)
	if !opLock.TryLock() {
		return
	}
	defer opLock.Unlock()

	log.WarningLog.Printf("finishing interrupted kill of session %q (tombstoned record survived its teardown)", instance.Title)
	// Best-effort: the backing tmux session is typically already gone; Kill
	// failures here only mean there is less left to tear down.
	if err := instance.Kill(); err != nil {
		log.WarningLog.Printf("finishing kill of %q: teardown reported: %v", instance.Title, err)
	}
	storage, err := session.NewStorage(config.LoadState(), repoID)
	if err != nil {
		log.WarningLog.Printf("finishing kill of %q: %v", instance.Title, err)
		return
	}
	if err := storage.DeleteInstance(instance.Title); err != nil {
		log.WarningLog.Printf("finishing kill of %q: failed to delete record (will retry next poll): %v", instance.Title, err)
		return
	}
	m.mu.Lock()
	delete(m.instances, key)
	m.mu.Unlock()
}

func (m *Manager) SendPrompt(req SendPromptRequest) error {
	if req.Prompt == "" {
		return fmt.Errorf("prompt is required")
	}
	instance, _, _, err := m.findSession(req.Title, req.RepoID)
	if err != nil {
		return err
	}
	if instance == nil {
		return fmt.Errorf("failed to restore instance %q", req.Title)
	}
	if err := instance.SendPromptCommand(req.Prompt); err != nil {
		return fmt.Errorf("failed to send prompt: %w", err)
	}
	return nil
}

// CreateTab spawns a Process-kind tab running req.Command in the target
// session's worktree, persists the grown tab list, and returns the resolved tab
// name (#930 PR 5). It mirrors CreateSession's discipline: the find+spawn+persist
// runs under the per-repo start lock so a concurrent CreateSession/CreateTab on
// the same repo can't race the tab list or derive a duplicate name. The new tab
// is persisted immediately (ToInstanceData serializes its command + tmux name,
// and restoreLocalTabs reconnects it by exact name on reload) so it survives a
// daemon/af restart — Sachin's hard #930 requirement. Rejected for remote/hook
// instances (no local worktree, and the hook protocol can't run arbitrary
// commands — a remote session's only terminal tab is the terminal_cmd one), an
// empty command, or an instance already at the soft cap (maxTabs, enforced by
// AddProcessTab).
func (m *Manager) CreateTab(req CreateTabRequest) (string, error) {
	if !req.Shell && strings.TrimSpace(req.Command) == "" {
		return "", fmt.Errorf("a process tab requires a non-empty command (--command)")
	}

	instance, repoID, _, err := m.findSession(req.Title, req.RepoID)
	if err != nil {
		return "", err
	}
	if instance == nil {
		return "", fmt.Errorf("failed to restore instance %q", req.Title)
	}
	if instance.IsRemote() {
		return "", fmt.Errorf("cannot create a tab on remote session %q: remote sessions have no local worktree and the hook protocol can't run arbitrary commands; their terminal tab comes from remote_hooks.terminal_cmd", req.Title)
	}

	// Serialize the tab spawn against an archive/kill/restore teardown+move for
	// this session and reject if it is archived/mid-archive (#1195); see
	// archiveExclusiveTabLock for the op-lock ordering and orphan rationale.
	opLock, err := m.archiveExclusiveTabLock(daemonInstanceKey(repoID, req.Title), instance)
	if err != nil {
		return "", err
	}
	defer opLock.Unlock()

	// Serialize against other create/tab mutations on this repo, mirroring
	// CreateSession, so two concurrent CreateTab calls never derive the same name
	// or interleave a spawn-then-persist with another save.
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	defer repoStartLock.Unlock()

	// A shell tab runs $SHELL (the TUI `t` mutation, #960 PR 2); a process tab
	// runs the requested command (the CLI/API path, #930 PR 5).
	var tab *session.Tab
	if req.Shell {
		tab, err = instance.AddShellTab()
	} else {
		tab, err = instance.AddProcessTab(req.Command, req.Name)
	}
	if err != nil {
		return "", err
	}

	// Persist through the targeted per-repo writer (persistInstanceData) — the
	// clobber-safe single-writer direction of #960 — rather than a whole-list
	// SaveInstances, which would re-serialize the manager's entire view and was
	// the dual-writer clobber surface PR 4 retires. Mirrors CloseTab/SetPRInfo.
	if err := persistInstanceData(repoID, instance.ToInstanceData()); err != nil {
		// Roll back the just-spawned tab so a persist failure does not leave a
		// live tmux session that vanishes from the tab list on restart.
		if closeErr := instance.CloseTab(instance.TabCount() - 1); closeErr != nil {
			log.WarningLog.Printf("CreateTab %q: rolling back unpersisted tab failed: %v", req.Title, closeErr)
		}
		return "", fmt.Errorf("failed to persist new tab: %w", err)
	}
	return tab.Name, nil
}

// CloseTab closes a non-agent tab of the target session, kills its tmux
// session, and persists the shrunk tab list (#960 PR 1). It is the close-side
// counterpart of CreateTab and mirrors its discipline: find the session, run
// the mutate+persist under the per-repo start lock so a concurrent
// CreateSession/CreateTab/CloseTab on the same repo can't interleave with the
// tab-list write, and persist through the targeted per-repo writer
// (persistInstanceData) rather than a whole-list SaveInstances — the
// clobber-safe single-writer direction of #960.
//
// The tab is resolved by TabName when set, otherwise by TabIndex. The agent
// tab (index 0) is unclosable (KillSession tears down the whole session
// instead) and remote sessions' tabs are fixed by their hook config, matching
// the TUI's `w` rule (handleCloseTab). Returns the resolved name of the closed
// tab. Unlike CreateTab there is no rollback on persist failure: CloseTab has
// already killed the tab's tmux session, so there is nothing live left to
// orphan — the in-memory list (tab removed) is the more accurate state, and the
// stale disk record is harmless (its session is dead and won't reconnect).
func (m *Manager) CloseTab(req CloseTabRequest) (string, error) {
	instance, repoID, _, err := m.findSession(req.Title, req.RepoID)
	if err != nil {
		return "", err
	}
	if instance == nil {
		return "", fmt.Errorf("failed to restore instance %q", req.Title)
	}
	if instance.IsRemote() {
		return "", fmt.Errorf("cannot close a tab on remote session %q: its tabs are fixed by remote_hooks config, not user-managed", req.Title)
	}

	// Serialize against other create/tab mutations on this repo, mirroring
	// CreateTab, so the tab-list mutate+persist never interleaves with another
	// save on the same repo.
	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	defer repoStartLock.Unlock()

	// Resolve the target tab. TabName takes precedence; otherwise TabIndex.
	tabs := instance.GetTabs()
	idx := req.TabIndex
	name := req.TabName
	if name != "" {
		idx = -1
		for i, tab := range tabs {
			if tab.Name == name {
				idx = i
				break
			}
		}
		if idx < 0 {
			return "", fmt.Errorf("session %q has no tab named %q", req.Title, name)
		}
	} else {
		if idx < 0 || idx >= len(tabs) {
			return "", fmt.Errorf("session %q has no tab at index %d", req.Title, idx)
		}
		name = tabs[idx].Name
	}
	if idx == 0 {
		return "", fmt.Errorf("the agent tab of session %q can't be closed; kill the session instead", req.Title)
	}

	if err := instance.CloseTab(idx); err != nil {
		return "", err
	}

	if err := persistInstanceData(repoID, instance.ToInstanceData()); err != nil {
		return "", fmt.Errorf("failed to persist tab close: %w", err)
	}
	return name, nil
}

// SetPRInfo records (or clears) the GitHub PR info for the target session and
// persists it (#960 PR 1). A zero-value PRInfo (Number 0) clears the recorded
// info. It mirrors CreateTab's discipline — find, mutate+persist under the
// per-repo start lock, persist through the targeted writer (persistInstanceData)
// — and rolls the in-memory value back on persist failure so memory and disk
// stay consistent. This is the daemon-side write the TUI performs today via
// prInfoUpdatedMsg + a full-list save (#921); the TUI is switched to it in PR 2.
func (m *Manager) SetPRInfo(req SetPRInfoRequest) error {
	instance, repoID, _, err := m.findSession(req.Title, req.RepoID)
	if err != nil {
		return err
	}
	if instance == nil {
		return fmt.Errorf("failed to restore instance %q", req.Title)
	}

	repoStartLock := m.startLockForRepo(repoID)
	repoStartLock.Lock()
	defer repoStartLock.Unlock()

	var info *git.PRInfo
	if req.PRInfo.Number != 0 {
		info = &git.PRInfo{
			Number: req.PRInfo.Number,
			Title:  req.PRInfo.Title,
			URL:    req.PRInfo.URL,
			State:  req.PRInfo.State,
		}
	}
	prev := instance.GetPRInfo()
	instance.SetPRInfo(info)

	if err := persistInstanceData(repoID, instance.ToInstanceData()); err != nil {
		// Keep memory consistent with disk on a persist failure.
		instance.SetPRInfo(prev)
		return fmt.Errorf("failed to persist PR info: %w", err)
	}
	return nil
}

func (m *Manager) findSession(title, repoID string) (*session.Instance, string, *session.InstanceData, error) {
	if title == "" {
		return nil, "", nil, fmt.Errorf("session title is required")
	}

	m.mu.Lock()
	if err := m.refreshLocked(); err != nil {
		m.mu.Unlock()
		return nil, "", nil, err
	}
	if repoID != "" {
		key := daemonInstanceKey(repoID, title)
		if instance := m.instances[key]; instance != nil {
			m.mu.Unlock()
			return instance, repoID, nil, nil
		}
	} else {
		for key, instance := range m.instances {
			if instance.Title == title {
				rid, _ := splitDaemonInstanceKey(key)
				m.mu.Unlock()
				return instance, rid, nil, nil
			}
		}
	}
	m.mu.Unlock()

	data, rid, err := findInstanceDataByTitle(title, repoID)
	if err != nil {
		return nil, "", nil, err
	}
	instance, restoreErr := fromInstanceDataForRefresh(*data)
	if restoreErr != nil {
		return nil, rid, data, nil
	}

	// We built `instance` from disk with m.mu released, so a concurrent
	// refresh (or another RPC) may have restored and registered the canonical
	// Instance for this session during the window (#867). Returning our freshly
	// built duplicate would hand the caller an *untracked* Instance: SendPrompt
	// would leak its restore-time attach PTY, and KillSession would call
	// instance.Kill() — tearing down the tmux session and worktree that the
	// canonical, still-tracked Instance shares. Re-acquire the lock and:
	//   - if a tracked Instance now exists, drop our duplicate (closing only
	//     its attach resources, never the shared session) and operate on the
	//     tracked one; otherwise
	//   - register our Instance so callers operate on a tracked Instance, just
	//     as the refresh loop would have, instead of an orphan.
	key := daemonInstanceKey(rid, title)
	m.mu.Lock()
	if tracked := m.instances[key]; tracked != nil {
		m.mu.Unlock()
		if err := instance.CloseAttachOnly(); err != nil {
			log.WarningLog.Printf("findSession %q: closing duplicate instance attach failed: %v", title, err)
		}
		return tracked, rid, data, nil
	}
	// Match the refresh loop: instances the daemon tracks always run AutoYes.
	instance.SetAutoYes(true)
	m.instances[key] = instance
	m.mu.Unlock()
	return instance, rid, data, nil
}

func (m *Manager) ImportRemoteHookSessions(req ImportRemoteHookSessionsRequest) ([]session.InstanceData, error) {
	if req.RepoPath == "" {
		return nil, fmt.Errorf("repo path is required")
	}
	repo, err := config.RepoFromPath(req.RepoPath)
	if err != nil {
		return nil, err
	}
	repoCfg, err := config.ResolveConfig(repo.Root)
	if err != nil {
		return nil, err
	}
	if repoCfg.RemoteHooks == nil || repoCfg.RemoteHooks.ListCmd == "" {
		return nil, nil
	}

	listed, err := session.ListRemoteHookInstanceData(repo.Root, *repoCfg.RemoteHooks, time.Now())
	if err != nil {
		return nil, err
	}

	imported := make([]session.InstanceData, 0, len(listed))
	if err := config.UpdateRepoInstances(repo.ID, func(raw json.RawMessage) (json.RawMessage, error) {
		var existing []session.InstanceData
		if err := json.Unmarshal(raw, &existing); err != nil {
			return nil, fmt.Errorf("failed to parse existing instances: %w", err)
		}
		existingTitles := make(map[string]bool, len(existing))
		existingHookNames := make(map[string]bool)
		for _, data := range existing {
			existingTitles[data.Title] = true
			if data.BackendType == "remote" {
				existingHookNames[session.RemoteHookName(data.Title, data.RemoteMeta)] = true
			}
		}
		for _, data := range listed {
			name := session.RemoteHookName(data.Title, data.RemoteMeta)
			if existingTitles[data.Title] || existingHookNames[name] {
				continue
			}
			existing = append(existing, data)
			imported = append(imported, data)
			existingTitles[data.Title] = true
			existingHookNames[name] = true
		}
		return json.Marshal(existing)
	}); err != nil {
		return nil, err
	}

	m.mu.Lock()
	_ = m.refreshLocked()
	m.mu.Unlock()
	return imported, nil
}

func appendInstanceData(repoID string, data session.InstanceData) error {
	return config.UpdateRepoInstances(repoID, func(raw json.RawMessage) (json.RawMessage, error) {
		var existing []session.InstanceData
		if err := json.Unmarshal(raw, &existing); err != nil {
			return nil, fmt.Errorf("failed to parse existing instances: %w", err)
		}
		for i := range existing {
			if existing[i].Title != data.Title {
				continue
			}
			// A Loading ghost left by an older TUI binary (#551) should
			// be overwritten rather than blocking the new session.
			// validateTitleAvailableLocked already cleared this title,
			// so reaching here under a same-titled non-Loading entry
			// is a real conflict.
			if existing[i].Status == session.Loading {
				existing[i] = data
				return json.MarshalIndent(existing, "", "  ")
			}
			return nil, fmt.Errorf("session with title %q already exists: %w", data.Title, errConcurrentCreate)
		}
		existing = append(existing, data)
		return json.MarshalIndent(existing, "", "  ")
	})
}

// persistInstanceData replaces the on-disk record for data.Title in repoID's
// instances file with data, under the per-repo file lock, leaving every other
// record untouched. It is the targeted, clobber-safe persist primitive for
// in-place mutations of an existing session (CloseTab, SetPRInfo) — the
// single-writer direction of #960 — analogous to appendInstanceData for
// creates and storage.DeleteInstance for kills. It deliberately does NOT use a
// whole-list SaveInstances, which would re-serialize the manager's entire view
// and reintroduce the dual-writer clobber surface #960 is retiring. Errors when
// no record with that title exists (the caller already resolved a live
// instance, so a missing disk record means storage drifted out from under us).
func persistInstanceData(repoID string, data session.InstanceData) error {
	found := false
	if err := config.UpdateRepoInstances(repoID, func(raw json.RawMessage) (json.RawMessage, error) {
		var existing []session.InstanceData
		if err := json.Unmarshal(raw, &existing); err != nil {
			return nil, fmt.Errorf("failed to parse existing instances: %w", err)
		}
		for i := range existing {
			if existing[i].Title == data.Title {
				existing[i] = data
				found = true
				return json.MarshalIndent(existing, "", "  ")
			}
		}
		// Leave the file unchanged when the record is absent; the caller turns
		// !found into an error below.
		return raw, nil
	}); err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("instance %q not found in storage", data.Title)
	}
	return nil
}

func loadRepoInstanceData(repoID string) ([]session.InstanceData, error) {
	raw, err := config.LoadRepoInstances(repoID)
	if err != nil {
		return nil, err
	}
	var data []session.InstanceData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("failed to parse existing instances: %w", err)
	}
	return data, nil
}

func findInstanceDataByTitle(title, repoID string) (*session.InstanceData, string, error) {
	if repoID != "" {
		data, err := loadRepoInstanceData(repoID)
		if err != nil {
			return nil, "", err
		}
		for i := range data {
			if data[i].Title == title {
				return &data[i], repoID, nil
			}
		}
		return nil, "", fmt.Errorf("instance %q not found", title)
	}

	allInstances, err := config.LoadAllRepoInstances()
	if err != nil {
		return nil, "", fmt.Errorf("failed to load instances: %w", err)
	}
	var corrupted []string
	for rid, raw := range allInstances {
		var data []session.InstanceData
		if err := json.Unmarshal(raw, &data); err != nil {
			// Warn and record the corrupted repo rather than silently
			// skipping it (#730). If the target title lives in this repo we
			// would otherwise report a misleading "not found".
			log.WarningLog.Printf("daemon skipping repo %s: corrupted instances.json: %v", rid, err)
			corrupted = append(corrupted, rid)
			continue
		}
		for i := range data {
			if data[i].Title == title {
				return &data[i], rid, nil
			}
		}
	}
	if len(corrupted) > 0 {
		sort.Strings(corrupted)
		return nil, "", fmt.Errorf("instance %q not found; %d repo(s) have a corrupted instances.json that may be hiding it: %s", title, len(corrupted), strings.Join(corrupted, ", "))
	}
	return nil, "", fmt.Errorf("instance %q not found", title)
}

// ghostKillTmuxByName issues a tmux kill-session for a persisted sanitized
// name. Package-level so tests can stub it without invoking real tmux. The
// af_ prefix check refuses to act on names the daemon would never write, so a
// corrupted store can't make us kill an unrelated tmux session. Mirror of the
// api/sessions.go helper added in #536 — duplicated here because daemon/
// cannot import api/ without a cycle.
var ghostKillTmuxByName = func(sanitizedName string) error {
	if !strings.HasPrefix(sanitizedName, tmux.TmuxPrefix) {
		return fmt.Errorf("refusing to kill tmux session without %q prefix: %q", tmux.TmuxPrefix, sanitizedName)
	}
	return tmux.NewTmuxSessionFromSanitizedName(sanitizedName, "").CloseAndWaitForPaneExit()
}

// ghostCleanupWorktree performs best-effort worktree teardown for a ghost
// session whose live restore failed. Package-level so tests can stub it.
// Deliberately no uncommitted-changes check here, unlike the TUI kill path
// (#815): this runs daemon-side with no user to warn, only for sessions whose
// records are already unrestorable, and the caller has already committed to
// deleting the record — a status probe could only block cleanup, not save data.
var ghostCleanupWorktree = func(data *session.InstanceData, title string) {
	if data.Worktree.RepoPath == "" || data.Worktree.WorktreePath == "" || data.Worktree.ExternalWorktree {
		return
	}
	branchCreatedByUs := true
	if data.Worktree.BranchCreatedByUs != nil {
		branchCreatedByUs = *data.Worktree.BranchCreatedByUs
	}
	gw, gwErr := git.NewGitWorktreeFromStorage(
		data.Worktree.RepoPath,
		data.Worktree.WorktreePath,
		data.Worktree.SessionName,
		data.Worktree.BranchName,
		data.Worktree.BaseCommitSHA,
		data.Worktree.ExternalWorktree,
		branchCreatedByUs,
	)
	if gwErr != nil {
		log.WarningLog.Printf("ghost session %q: failed to load worktree for cleanup: %v", title, gwErr)
		return
	}
	if cleanupErr := gw.Cleanup(); cleanupErr != nil {
		log.WarningLog.Printf("ghost session %q: worktree cleanup failed: %v", title, cleanupErr)
	}
}

// ghostCleanup runs best-effort teardown of a ghost session's external
// resources. Tmux teardown is independent of worktree state (#516/#549): a
// ghost record can have an empty worktree path while a tmux session with the
// persisted name is still running, so the two branches share no condition.
// Tmux goes FIRST: a still-running agent writing into the worktree while git
// recursively deletes it leaks a half-deleted directory (#802).
func ghostCleanup(data *session.InstanceData, title string) {
	if data.TmuxName != "" {
		if killErr := ghostKillTmuxByName(data.TmuxName); killErr != nil {
			log.WarningLog.Printf("ghost session %q: tmux cleanup failed: %v", title, killErr)
		}
	}
	ghostCleanupWorktree(data, title)
}
