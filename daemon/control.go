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
	daemonReadyTimeout   = 5 * time.Second
	daemonDialTimeout    = 250 * time.Millisecond
	// shutdownAckGrace delays the daemon main-loop teardown after a Shutdown
	// RPC handler returns so the response can flush back to the caller before
	// the listener closes.
	shutdownAckGrace = 50 * time.Millisecond
)

var ensureDaemonMu sync.Mutex

// CreateSessionRequest is the daemon-owned session creation contract used by
// the TUI, CLI, and scheduled task runner.
type CreateSessionRequest struct {
	Title                  string
	TitleBase              string
	RepoPath               string
	Program                string
	Prompt                 string
	AutoYes                bool
	ForceRemote            bool
	ExistingWorktreePath   string
	ExistingWorktreeBranch string
}

type CreateSessionResponse struct {
	Instance session.InstanceData
}

type KillSessionRequest struct {
	Title  string
	RepoID string
}

type KillSessionResponse struct {
	OK bool
}

type SendPromptRequest struct {
	Title  string
	RepoID string
	Prompt string
}

type SendPromptResponse struct {
	OK bool
}

type ImportRemoteHookSessionsRequest struct {
	RepoPath string
}

type ImportRemoteHookSessionsResponse struct {
	Instances []session.InstanceData
}

type PingRequest struct{}
type PingResponse struct {
	OK bool
}

type ReloadTasksRequest struct{}
type ReloadTasksResponse struct {
	OK bool
}

type ShutdownRequest struct{}
type ShutdownResponse struct {
	OK bool
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
	if err := StopDaemon(); err != nil {
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

// KillSession asks the daemon to kill a session and remove it from storage.
func KillSession(req KillSessionRequest) error {
	var resp KillSessionResponse
	if err := callDaemon("KillSession", req, &resp); err != nil {
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

// ReloadTasks asks the daemon to re-read tasks.json and rebuild its cron
// schedule set. Task CRUD paths (CLI, API, TUI) call this after writing the
// file so schedule changes take effect without a daemon restart. Like every
// callDaemon path it ensures the daemon is running first, so adding a task
// also brings the scheduler up.
func ReloadTasks() error {
	var resp ReloadTasksResponse
	return callDaemon("ReloadTasks", ReloadTasksRequest{}, &resp)
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
// process, and (ShutdownFailed, err) when the daemon is provably running but
// the fallback could not locate or signal it (ambiguous pgrep matches, no
// PID file with pgrep unavailable, permission denied on signal) — the
// returned error carries the recovery hint the caller must surface.
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
		return ShutdownNoDaemon, rpcErr
	}
	if !resp.OK {
		return ShutdownNoDaemon, fmt.Errorf("daemon Shutdown RPC returned OK=false")
	}
	return ShutdownViaRPC, nil
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

func (s *controlServer) ReloadTasks(_ ReloadTasksRequest, resp *ReloadTasksResponse) error {
	if s.scheduler == nil {
		return fmt.Errorf("this daemon does not host a task scheduler")
	}
	// During warm-up (#829) the scheduler and watcher supervisor have not
	// started yet; RunDaemon reloads both from tasks.json right after the
	// restore completes, so a change the caller just wrote is picked up then.
	// Ack instead of erroring — the write is already durable and there is
	// nothing running to reload.
	if s.manager != nil && !s.manager.Ready() {
		resp.OK = true
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
		ready:               make(chan struct{}),
		storage:             storage,
		instances:           make(map[string]*session.Instance),
		reservedTitles:      make(map[string]struct{}),
		reservedRemoteNames: make(map[string]struct{}),
		repoStartLocks:      make(map[string]*sync.Mutex),
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

func (m *Manager) SaveInstances() error {
	return m.storage.SaveInstances(m.InstancesSnapshot())
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
		ForceRemote: req.ForceRemote,
	})
	if err != nil {
		return session.InstanceData{}, err
	}

	if req.ExistingWorktreePath != "" && req.ForceRemote {
		return session.InstanceData{}, fmt.Errorf("remote sessions cannot use an existing local worktree")
	}

	if req.ExistingWorktreePath != "" {
		err = instance.StartWithExistingWorktree(req.ExistingWorktreePath, req.ExistingWorktreeBranch)
	} else {
		err = task.StartAndSendPrompt(instance, req.Prompt)
	}
	if err != nil {
		_ = instance.Kill()
		return session.InstanceData{}, fmt.Errorf("failed to start instance: %w", err)
	}

	instance.SetStatus(session.Running)
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
	} else if err := m.validateTitleAvailableLocked(repo.ID, repo.Root, title, req.Program, req.ForceRemote, diskData); err != nil {
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
		if err := m.validateTitleAvailableLocked(repoID, repoPath, candidate, program, remote, diskData); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("could not find an available title for %q", baseTitle)
}

func (m *Manager) validateTitleAvailableLocked(repoID, repoPath, title, program string, remote bool, diskData []session.InstanceData) error {
	if title == "" {
		return fmt.Errorf("session title is required")
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
				return fmt.Errorf("session with title %q is already reserved", title)
			}
			return fmt.Errorf("session with title %q already exists", title)
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
	if tmux.NewTmuxSessionForRepo(title, repoPath, program).DoesSessionExist() {
		return fmt.Errorf("tmux session for title %q already exists", title)
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
// repo because they would derive the same git branch. Exact (case-insensitive)
// duplicates always collide; beyond that, the titles collide when they sanitize
// to the same branch name (e.g. "A B" and "a-b" -> "af-a-b"). The EqualFold
// guard also covers titles made only of unsafe characters, whose sanitized
// branch is a random fallback that would otherwise never compare equal.
func (m *Manager) titlesCollide(a, b string) bool {
	if strings.EqualFold(a, b) {
		return true
	}
	return m.branchForTitle(a) == m.branchForTitle(b)
}

// branchForTitle derives the git branch name for a session title using the same
// prefix and sanitization the git worktree layer applies, so the daemon can
// detect branch collisions before worktree setup runs.
func (m *Manager) branchForTitle(title string) string {
	return git.SanitizeBranchName(m.cfg.BranchPrefix + title)
}

func (m *Manager) KillSession(req KillSessionRequest) error {
	instance, repoID, data, err := m.findSession(req.Title, req.RepoID)
	if err != nil {
		return err
	}

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
	delete(m.instances, daemonInstanceKey(repoID, req.Title))
	m.mu.Unlock()
	return nil
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
	instance, restoreErr := session.FromInstanceData(*data)
	if restoreErr != nil {
		return nil, rid, data, nil
	}
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
			return nil, fmt.Errorf("session with title %q already exists", data.Title)
		}
		existing = append(existing, data)
		return json.MarshalIndent(existing, "", "  ")
	})
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

func splitDaemonInstanceKey(key string) (string, string) {
	for i := 0; i < len(key); i++ {
		if key[i] == 0 {
			return key[:i], key[i+1:]
		}
	}
	return "", key
}
