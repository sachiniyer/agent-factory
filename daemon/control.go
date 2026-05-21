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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/git"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

const (
	controlServiceName       = "Control"
	daemonSocketFileName     = "daemon.sock"
	daemonReadyTimeout       = 5 * time.Second
	daemonDialTimeout        = 250 * time.Millisecond
	maxTrustPromptAttempts   = 20
	trustPromptRetryDelay    = time.Second
	waitForReadyTimeout      = 60 * time.Second
	waitForReadyPollInterval = 500 * time.Millisecond
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

type ShutdownRequest struct{}
type ShutdownResponse struct {
	OK bool
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

	if err := launchDaemonProcess(); err != nil {
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

func callDaemon(method string, req any, resp any) error {
	if err := EnsureDaemon(); err != nil {
		return err
	}
	return callDaemonNoEnsure(method, req, resp)
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
	shutdownCh   chan struct{}
	shutdownOnce sync.Once
}

func (s *controlServer) Ping(_ PingRequest, resp *PingResponse) error {
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

func (s *controlServer) CreateSession(req CreateSessionRequest, resp *CreateSessionResponse) error {
	data, err := s.manager.CreateSession(req)
	if err != nil {
		return err
	}
	resp.Instance = data
	return nil
}

func (s *controlServer) KillSession(req KillSessionRequest, resp *KillSessionResponse) error {
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
	data, err := s.manager.ImportRemoteHookSessions(req)
	if err != nil {
		return err
	}
	resp.Instances = data
	return nil
}

// startControlServer registers the control RPC service on the Unix socket and
// returns a cleanup function that closes the listener and removes the socket
// file. When shutdownCh is non-nil, the Shutdown RPC will close it on the
// first invocation, allowing the daemon main loop to exit on RPC request.
func startControlServer(manager *Manager, shutdownCh chan struct{}) (func() error, error) {
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
		err := listener.Close()
		_ = os.Remove(socketPath)
		return err
	}, nil
}

// Manager owns the daemon's authoritative session mutations.
type Manager struct {
	cfg *config.Config

	mu                  sync.Mutex
	storage             *session.Storage
	instances           map[string]*session.Instance
	reservedTitles      map[string]struct{}
	reservedRemoteNames map[string]struct{}
	repoStartLocks      map[string]*sync.Mutex
}

func NewManager(cfg *config.Config) (*Manager, error) {
	state := config.LoadState()
	storage, err := session.NewStorage(state, "")
	if err != nil {
		return nil, fmt.Errorf("failed to initialize storage: %w", err)
	}
	instances, err := refreshDaemonInstances(nil)
	if err != nil {
		return nil, err
	}
	return &Manager{
		cfg:                 cfg,
		storage:             storage,
		instances:           instances,
		reservedTitles:      make(map[string]struct{}),
		reservedRemoteNames: make(map[string]struct{}),
		repoStartLocks:      make(map[string]*sync.Mutex),
	}, nil
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
		req.Program = m.cfg.DefaultProgram
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
		err = startAndSendPrompt(instance, req.Prompt)
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
	// Title comparisons are case-insensitive: sanitizeBranchName lowercases
	// titles when deriving git branch names, so two case-variant titles
	// (e.g. "MyApp" and "myapp") would map to the same branch and the
	// second worktree create would fail with a cryptic git error. Reject
	// the conflict here, before any worktree or tmux setup runs. (#605)
	if existing, kind := m.findTitleConflictLocked(repoID, title, diskData); existing != "" {
		switch {
		case existing == title:
			if kind == titleConflictReserved {
				return fmt.Errorf("session with title %q is already reserved", title)
			}
			return fmt.Errorf("session with title %q already exists", title)
		default:
			return fmt.Errorf("session titled %q conflicts with existing session %q (case-insensitive comparison; sanitize collides at git layer)", title, existing)
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
// given candidate under case-insensitive comparison, along with the source of
// the conflict. An empty result means the title is available. Comparisons are
// case-insensitive so that titles which would collide at the git branch layer
// (sanitizeBranchName lowercases) are rejected before worktree setup. (#605)
func (m *Manager) findTitleConflictLocked(repoID, title string, diskData []session.InstanceData) (string, titleConflictKind) {
	for key := range m.reservedTitles {
		rid, existing := splitDaemonInstanceKey(key)
		if rid == repoID && strings.EqualFold(existing, title) {
			return existing, titleConflictReserved
		}
	}
	for key, inst := range m.instances {
		rid, _ := splitDaemonInstanceKey(key)
		if rid != repoID || inst == nil {
			continue
		}
		if strings.EqualFold(inst.Title, title) {
			return inst.Title, titleConflictLive
		}
	}
	for _, data := range diskData {
		if !strings.EqualFold(data.Title, title) {
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
	repoCfg, err := config.LoadRepoConfig(repo.ID)
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
	for rid, raw := range allInstances {
		var data []session.InstanceData
		if err := json.Unmarshal(raw, &data); err != nil {
			continue
		}
		for i := range data {
			if data[i].Title == title {
				return &data[i], rid, nil
			}
		}
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
	return tmux.NewTmuxSessionFromSanitizedName(sanitizedName, "").Close()
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
func ghostCleanup(data *session.InstanceData, title string) {
	ghostCleanupWorktree(data, title)
	if data.TmuxName != "" {
		if killErr := ghostKillTmuxByName(data.TmuxName); killErr != nil {
			log.WarningLog.Printf("ghost session %q: tmux cleanup failed: %v", title, killErr)
		}
	}
}

func startAndSendPrompt(instance *session.Instance, prompt string) error {
	if err := instance.Start(true); err != nil {
		return err
	}
	if instance.IsRemote() {
		return nil
	}
	if prompt == "" {
		return nil
	}
	if err := waitForReady(instance); err != nil {
		return err
	}
	for attempts := 0; instance.CheckAndHandleTrustPrompt(); attempts++ {
		if attempts+1 >= maxTrustPromptAttempts {
			return fmt.Errorf("trust prompt did not dismiss after %d attempts", maxTrustPromptAttempts)
		}
		time.Sleep(trustPromptRetryDelay)
		if err := waitForReady(instance); err != nil {
			return err
		}
	}
	if prompt != "" {
		if err := instance.SendPromptCommand(prompt); err != nil {
			return err
		}
	}
	return nil
}

func waitForReady(instance *session.Instance) error {
	timeout := time.After(waitForReadyTimeout)
	ticker := time.NewTicker(waitForReadyPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			content, err := instance.Preview()
			if err != nil {
				log.ErrorLog.Printf("waitForReady timed out (preview also failed: %v)", err)
				return formatWaitForReadyTimeoutError(waitForReadyTimeout, "")
			}
			log.ErrorLog.Printf("waitForReady timed out. Last pane content: %s", content)
			return formatWaitForReadyTimeoutError(waitForReadyTimeout, content)
		case <-ticker.C:
			content, err := instance.Preview()
			if err != nil {
				continue
			}
			if isReadyContent(content) {
				return nil
			}
		}
	}
}

// formatWaitForReadyTimeoutError builds the user-facing timeout error. When
// the captured pane content is non-empty, the error body carries a trimmed
// snippet of the last few lines so users see what the agent was doing instead
// of an opaque "timed out" message. See sachiniyer/agent-factory#502.
func formatWaitForReadyTimeoutError(timeout time.Duration, content string) error {
	base := fmt.Sprintf("timed out waiting for program to start (%s)", timeout)
	snippet := trimPaneSnippet(content)
	if snippet == "" {
		return errors.New(base)
	}
	var b strings.Builder
	b.WriteString(base)
	b.WriteString("\nlast pane content:")
	for _, line := range strings.Split(snippet, "\n") {
		b.WriteString("\n  ")
		b.WriteString(line)
	}
	return errors.New(b.String())
}

// trimPaneSnippet returns at most the last 5 non-empty trailing lines of the
// captured pane content, capped at 400 bytes. ANSI escape sequences are left
// intact — keeping the snippet short matters more than stripping them.
func trimPaneSnippet(content string) string {
	lines := strings.Split(content, "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) == 0 {
		return ""
	}
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	out := strings.Join(lines, "\n")
	if len(out) > 400 {
		out = out[len(out)-400:]
	}
	return out
}

func isReadyContent(content string) bool {
	if strings.Contains(content, "❯") ||
		strings.Contains(content, "Do you trust") ||
		strings.Contains(content, "new MCP server") {
		return true
	}
	return strings.Contains(content, "Open documentation url") &&
		strings.Contains(content, "(D)on't ask again")
}

func splitDaemonInstanceKey(key string) (string, string) {
	for i := 0; i < len(key); i++ {
		if key[i] == 0 {
			return key[:i], key[i+1:]
		}
	}
	return "", key
}
