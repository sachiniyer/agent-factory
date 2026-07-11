package daemon

import (
	"errors"
	"fmt"
	"net"
	"net/rpc"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

var ensureDaemonMu sync.Mutex

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
	return ensureDaemonWithLauncher(launchDaemonProcessFn)
}

// EnsureDaemonFromPath starts the daemon from execPath if the control socket is
// not already serving. It is used by post-upgrade restart paths after the
// current process's executable may have been replaced on disk: asking the
// still-running old process for os.Executable can resolve to a deleted inode,
// while execPath is the freshly written binary path the new daemon must run.
func EnsureDaemonFromPath(execPath string) error {
	return ensureDaemonWithLauncher(func() error {
		return launchDaemonProcessAt(execPath)
	})
}

func ensureDaemonWithLauncher(launch func() error) error {
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

	if err := launch(); err != nil {
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

// The TUI's control + read path moved onto the HTTP apiclient in #1592 Phase 2
// PR3, so the net/rpc client wrappers only the TUI called — SetPRInfo,
// ImportRemoteHookSessions, PauseStatusPoll, ResumeStatusPoll (here) and
// ResumeFromLimit / SnapshotWithAlarms (in limit.go / snapshot.go) — are gone.
// The controlServer handlers stay: the gob control socket still SERVES every
// verb for CLI/internal callers; only the TUI-only Go client wrappers were
// removed. SnapshotNoSpawn below remains the CLI's non-spawning, instances-only
// read.

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

// RestoreSession asks the daemon to restore an archived, Lost, or Dead session.
func RestoreSession(req RestoreSessionRequest) (string, error) {
	var resp RestoreSessionResponse
	if err := callDaemon("RestoreSession", req, &resp); err != nil {
		return "", err
	}
	return resp.WorktreePath, nil
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
