package daemon

import (
	"fmt"
	"net"
	"net/rpc"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

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
	s.manager.publishEvent(agentproto.EventTaskCreated, req.Task)
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
	s.manager.publishEvent(agentproto.EventTaskUpdated, req.Task)
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
	s.manager.publishEvent(agentproto.EventTaskRemoved, task.Task{ID: req.ID})
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
	s.manager.publishEvent(agentproto.EventSessionCreated, data)
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
	s.manager.publishEvent(agentproto.EventSessionKilled, session.InstanceData{Title: req.Title})
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
	s.manager.publishEvent(agentproto.EventSessionArchived, session.InstanceData{Title: req.Title})
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
	s.manager.publishEvent(agentproto.EventSessionRestored, session.InstanceData{Title: req.Title})
	return nil
}

func (s *controlServer) RestoreSession(req RestoreSessionRequest, resp *RestoreSessionResponse) error {
	if err := s.requireManagerReady(); err != nil {
		return err
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	worktreePath, err := s.manager.RestoreSession(req)
	if err != nil {
		return err
	}
	resp.OK = true
	resp.WorktreePath = worktreePath
	s.manager.publishEvent(agentproto.EventSessionRestored, session.InstanceData{Title: req.Title})
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
