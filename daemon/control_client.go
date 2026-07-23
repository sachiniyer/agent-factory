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
	"github.com/sachiniyer/agent-factory/internal/sockpath"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

var ensureDaemonMu sync.Mutex

// daemonStartingErrText is the wire-visible text of the warm-up error. net/rpc
// flattens server-side errors into plain strings, so clients cannot errors.Is
// against a sentinel value; IsDaemonStartingErr matches this text instead.
const daemonStartingErrText = "agent-factory daemon is starting (restoring sessions); retry shortly"

// daemonUpgradeProbationErrText is the stable portion of the wire-visible
// probation refusal. The transaction ID follows it for diagnosis, so clients
// match the prefix rather than one complete dynamic string.
const daemonUpgradeProbationErrText = "agent-factory daemon is validating an upgrade"

// errDaemonStarting is returned by state-dependent RPC handlers in the window
// between the control-socket bind and the completion of the instance restore
// (#829). The socket now binds before the restore, which can take minutes on
// remote-hook repos, so this window is user-visible.
func errDaemonStarting() error {
	return errors.New(daemonStartingErrText)
}

func errDaemonUpgradeProbation(transactionID string) error {
	return fmt.Errorf("%s (transaction %s); retry shortly", daemonUpgradeProbationErrText, transactionID)
}

// IsDaemonStartingErr reports whether an RPC client error means the daemon is
// up but still restoring instances. Callers should treat it as retryable: the
// daemon is alive (EnsureDaemon's ping succeeds, so it must NOT spawn another)
// and the same request succeeds once the restore finishes.
func IsDaemonStartingErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), daemonStartingErrText)
}

// IsDaemonUpgradeProbationErr reports whether a daemon mutation was refused
// because a candidate is restored but its previous-binary supervisor has not
// released admission yet. Like the warm-up error, net/rpc flattens the server
// error to text, so this classifier matches the stable wire prefix.
func IsDaemonUpgradeProbationErr(err error) bool {
	return err != nil && strings.Contains(err.Error(), daemonUpgradeProbationErrText)
}

// IsDaemonAdmissionRetryable reports whether err is a lifecycle admission
// refusal expected to clear without restarting the daemon. Both the net/rpc
// and HTTP/TUI transports use this predicate so a new admission phase cannot
// become retryable on one transport while failing immediately on the other.
func IsDaemonAdmissionRetryable(err error) bool {
	return IsDaemonStartingErr(err) || IsDaemonUpgradeProbationErr(err)
}

// DaemonSocketPath returns the Unix socket path used by the local control
// plane.
//
// The length check happens HERE, where the path is resolved, rather than at
// net.Listen: every client and the daemon itself route through this function,
// so one check covers dialling and binding, and it fires before a listener has
// half-started. An over-long path otherwise fails inside the kernel as a bare
// "bind: invalid argument" that names neither the path, the limit, nor
// AGENT_FACTORY_HOME — the knob that fixes it (#1940).
func DaemonSocketPath() (string, error) {
	dir, err := config.GetConfigDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, daemonSocketFileName)
	if err := sockpath.Check("daemon control socket", path); err != nil {
		return "", err
	}
	return path, nil
}

// EnsureDaemon starts the daemon if the control socket is not already serving.
func EnsureDaemon() error {
	return ensureDaemonWithLauncher(launchDaemonProcessFn)
}

var launchDaemonProcessAtFn = launchDaemonProcessAt

// EnsureDaemonFromPath starts the daemon from execPath if the control socket is
// not already serving. It is used by post-upgrade restart paths after the
// current process's executable may have been replaced on disk: asking the
// still-running old process for os.Executable can resolve to a deleted inode,
// while execPath is the freshly written binary path the new daemon must run.
func EnsureDaemonFromPath(execPath string) error {
	return ensureDaemonWithPolicy(func() error {
		return launchDaemonProcessAtFn(execPath)
	}, false)
}

func ensureDaemonWithLauncher(launch func() error) error {
	return ensureDaemonWithPolicy(launch, true)
}

func ensureDaemonWithPolicy(launch func() error, preferUnit bool) error {
	ensureDaemonMu.Lock()
	defer ensureDaemonMu.Unlock()

	if err := pingDaemon(); err == nil {
		return nil
	}
	if preferUnit {
		configDir, configErr := config.GetConfigDir()
		if configErr != nil {
			log.WarningLog.Printf("could not resolve AF home while choosing daemon supervisor; using ad-hoc launch: %v", configErr)
		} else {
			owner, ownerErr := ResolveSupervisionOwner(configDir)
			switch {
			case ownerErr != nil:
				log.WarningLog.Printf("could not determine daemon supervision owner; using ad-hoc launch: %v", ownerErr)
			case owner == OwnerUnit:
				return ensureDaemonThroughUnit(launch)
			}
		}
	}
	return ensureDaemonAdHoc(launch)
}

func ensureDaemonThroughUnit(launch func() error) error {
	unitDeadline := time.Now().Add(ensureUnitStartTimeout)

	unitErr := runEnsureUnitStartCommand(unitDeadline)
	if unitErr == nil {
		unitErr = waitForDaemonReady(unitDeadline)
		if unitErr == nil {
			return nil
		}
	}
	log.WarningLog.Printf("failed to start daemon through its installed service; falling back to an ad-hoc daemon: %v", unitErr)
	if err := ensureDaemonAdHoc(launch); err != nil {
		return fmt.Errorf("installed daemon service failed: %v; ad-hoc fallback failed: %w", unitErr, err)
	}
	// The ad-hoc fallback brought up a reachable daemon. EnsureDaemon's contract is
	// "daemon reachable when I return nil", and every caller (callDaemon,
	// withDaemonHTTP, attach) hard-returns on a non-nil result and skips the RPC —
	// so returning a supervision-degradation error here failed the first client
	// action on any host without a systemd user bus even though the daemon was
	// serving, self-healing only on the second call (#2373). The degradation is
	// still surfaced where the user looks: the warning above, and af doctor /
	// af daemon status carry a supervision-owner row. Report success.
	return nil
}

func ensureDaemonAdHoc(launch func() error) error {
	// A previous daemon version may have a PID file but no control socket. Stop
	// it before launching the control-plane daemon so we do not run duplicate
	// scheduler and session-monitor loops. StopDaemon is also how an
	// alive-but-unreachable daemon
	// (its control socket removed/corrupted) is reclaimed: it SIGTERMs the
	// holder, which releases the per-home lock on exit, so the launch below
	// acquires a free lock. That reclaim-then-respawn is what keeps auto-start
	// within the singleton invariant — the freshly spawned daemon binds only
	// once the previous one is gone. A spurious spawn that races a still-live
	// holder can never become a second daemon: the child fails fast on the
	// exclusive startup lock (see RunDaemon / acquireHomeLock).
	if _, err := StopDaemon(); err != nil {
		log.WarningLog.Printf("failed to stop stale daemon before launch: %v", err)
	}

	// No auth-posture pre-flight here any more (#2168 Phase 0). This used to load
	// the config and return the #2090 refusal before spawning, because a spawned
	// daemon's stderr is discarded (startDaemonChild) and the refusal would
	// otherwise reach the user only as the 5s "did not become ready" timeout
	// below. There is nothing left to pre-flight: a tokenless network bind starts
	// and serves, so this path can no longer predict a startup failure — and
	// keeping the check would turn the very config the owner chose to allow into
	// an `af` that refuses to run at all.
	//
	// The exposure is still reported, on surfaces the user is actually looking
	// at: `af config set` warns at write time, the daemon warns once when the
	// listener binds (startHTTPServer), and `af doctor` / `af daemon status`
	// carry a row for it.

	if err := launch(); err != nil {
		return err
	}

	return waitForDaemonReady(time.Now().Add(daemonReadyTimeout))
}

func waitForDaemonReady(deadline time.Time) error {
	var lastErr error
	for time.Now().Before(deadline) {
		if err := pingDaemon(); err == nil {
			return nil
		} else {
			lastErr = err
		}
		time.Sleep(50 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = errors.New("readiness deadline elapsed before the daemon could be probed")
	}
	return fmt.Errorf("daemon did not become ready: %w", lastErr)
}

func pingDaemon() error {
	_, err := pingDaemonResponse()
	return err
}

// pingDaemonResponse pings the daemon and returns its full reply, so callers
// that need the reported version (`af doctor`'s skew check) read it from the
// same probe that establishes liveness. Never ensures a daemon: doctor is
// read-only and must not spawn the thing it is diagnosing.
func pingDaemonResponse() (PingResponse, error) {
	var resp PingResponse
	err := callDaemonNoEnsure("Ping", PingRequest{}, &resp)
	return resp, err
}

// daemonAdmissionRetryWait bounds how long RPC clients wait for a transient
// lifecycle admission refusal before surfacing it. It mirrors
// daemonReadyTimeout, the wait callers already tolerated pre-#829 when
// EnsureDaemon polled for the socket: a local restore or probation release
// completes inside this window so calls just work, while a stuck transition
// fails fast with its actionable message instead of hanging the caller.
// daemonAdmissionRetryPoll is the retry cadence.
const (
	daemonAdmissionRetryWait = daemonReadyTimeout
	daemonAdmissionRetryPoll = 100 * time.Millisecond
)

func callDaemon(method string, req any, resp any) error {
	if err := EnsureDaemon(); err != nil {
		return err
	}
	err := callDaemonNoEnsure(method, req, resp)
	// A warming daemon rejects state-dependent RPCs until restore completes;
	// an upgrade candidate rejects mutations until its validator releases
	// probation. Both are alive and retryable, so callers share one bounded
	// retry rather than growing per-call-site lifecycle logic.
	deadline := time.Now().Add(daemonAdmissionRetryWait)
	for IsDaemonAdmissionRetryable(err) && time.Now().Before(deadline) {
		time.Sleep(daemonAdmissionRetryPoll)
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

// RenameTab asks the daemon to relabel one tab of an existing session. It
// returns the RESOLVED name — sanitized and collision-suffixed — which is what
// the tab is actually called afterwards, so callers print that rather than the
// name that was requested.
func RenameTab(req RenameTabRequest) (string, error) {
	var resp RenameTabResponse
	if err := callDaemon("RenameTab", req, &resp); err != nil {
		return "", err
	}
	return resp.Name, nil
}

// ReorderTab asks the daemon to move one tab within a session's roster,
// returning the moved tab's name and its final index.
func ReorderTab(req ReorderTabRequest) (string, int, error) {
	var resp ReorderTabResponse
	if err := callDaemon("ReorderTab", req, &resp); err != nil {
		return "", 0, err
	}
	return resp.Name, resp.Index, nil
}

// The TUI's control + read path moved onto the HTTP apiclient in #1592 Phase 2
// PR3, so the net/rpc client wrappers only the TUI called — SetPRInfo,
// PauseStatusPoll, ResumeStatusPoll (here) and ResumeFromLimit /
// SnapshotWithAlarms (in limit.go / snapshot.go) — are gone.
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

// PreviewSession captures one session tab through the daemon's sole Preview
// handler. Unlike SnapshotNoSpawn, previewing a live terminal is an active read:
// it ensures the daemon is running and waits through daemon warm-up, just like
// the other session control calls.
func PreviewSession(req PreviewRequest) (content string, gone, tabGone bool, err error) {
	var resp PreviewResponse
	if err := callDaemon("Preview", req, &resp); err != nil {
		return "", false, false, err
	}
	return resp.Content, resp.Gone, resp.TabGone, nil
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

// ResumeFromLimit asks the daemon to retry a session parked at a usage-limit
// wall. The TUI and web reach the identical controlServer handler over HTTP;
// this wrapper gives the CLI its existing gob-control-socket transport without
// duplicating the recovery action.
func ResumeFromLimit(req ResumeFromLimitRequest) error {
	var resp ResumeFromLimitResponse
	if err := callDaemon("ResumeFromLimit", req, &resp); err != nil {
		return err
	}
	if !resp.OK {
		return fmt.Errorf("resume was not performed: %s", resp.Reason)
	}
	return nil
}

// HandoffSession asks the daemon to continue a session under a different agent,
// in place (#2013): swap the agent program, keep the worktree and branch, and
// deliver a mission brief to the incoming agent.
func HandoffSession(req HandoffSessionRequest) (HandoffSessionResponse, error) {
	var resp HandoffSessionResponse
	if err := callDaemon("HandoffSession", req, &resp); err != nil {
		return HandoffSessionResponse{}, err
	}
	return resp, nil
}

// RestoreSession asks the daemon to restore an archived, Lost, or Dead session.
func RestoreSession(req RestoreSessionRequest) (string, error) {
	var resp RestoreSessionResponse
	if err := callDaemon("RestoreSession", req, &resp); err != nil {
		return "", err
	}
	return resp.WorktreePath, nil
}

// DeleteProject asks the daemon to delete a project (#1735): archive its live
// sessions (restorable), tear down any in-place ones, and drop its root_agents
// opt-in. Returns how many sessions were archived and how many were torn down.
func DeleteProject(req DeleteProjectRequest) (DeleteProjectResponse, error) {
	var resp DeleteProjectResponse
	if err := callDaemon("DeleteProject", req, &resp); err != nil {
		return DeleteProjectResponse{}, err
	}
	return resp, nil
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

// UpdateTask asks the daemon to apply a field-level patch to the task with the
// given id and re-arm its schedule, returning the merged record (#1700). Only
// the patch's non-nil fields are written, so a single-field edit never clobbers
// a concurrent edit another client made to a different field.
func UpdateTask(id string, update task.TaskUpdate, expect task.ProjectExpectation) (task.Task, error) {
	var resp UpdateTaskResponse
	if err := callDaemon("UpdateTask", UpdateTaskRequest{ID: id, Update: update, Expect: expect}, &resp); err != nil {
		return task.Task{}, err
	}
	return resp.Task, nil
}

// RemoveTask asks the daemon to delete a task and re-arm its schedule.
func RemoveTask(id string, expect task.ProjectExpectation) error {
	var resp RemoveTaskResponse
	return callDaemon("RemoveTask", RemoveTaskRequest{ID: id, Expect: expect}, &resp)
}

// RestartTask asks the daemon to stop and replace one enabled watch command,
// waiting until the old process tree is gone and the replacement has started.
func RestartTask(id string, expect task.ProjectExpectation) error {
	var resp RestartTaskResponse
	return callDaemon("RestartTask", RestartTaskRequest{ID: id, Expect: expect}, &resp)
}

// TriggerTask asks the daemon to fire a task now through the shared RunTask
// firing path (the same entrypoint the in-daemon scheduler uses). Replaces the
// old in-process daemon.RunTask CLI call so CLI, TUI, and scheduler triggers all
// converge on one daemon-owned firing path (#1169-class fix).
func TriggerTask(id string, expect task.ProjectExpectation) error {
	var resp TriggerTaskResponse
	return callDaemon("TriggerTask", TriggerTaskRequest{ID: id, Expect: expect}, &resp)
}

// SpawnConfigAgent asks the daemon to start a config agent in a bare tmux session
// and returns the session name AND the absolute socket path to attach to.
// callDaemon carries the warm-up retry, so pressing the hotkey while the daemon
// is still starting waits rather than failing. The socket path may be empty (the
// daemon could not resolve it); the attach then falls back to the default socket.
func SpawnConfigAgent(req SpawnConfigAgentRequest) (string, string, error) {
	var resp SpawnConfigAgentResponse
	if err := callDaemon("SpawnConfigAgent", req, &resp); err != nil {
		return "", "", err
	}
	return resp.SessionName, resp.SocketPath, nil
}

// ReapConfigAgent tears down a config-agent session once the caller is done with
// it. The daemon's own shutdown reap is the backstop if this never arrives.
func ReapConfigAgent(sessionName string) error {
	var resp ReapConfigAgentResponse
	return callDaemon("ReapConfigAgent", ReapConfigAgentRequest{SessionName: sessionName}, &resp)
}
