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

// RequestShutdown asks any running daemon to exit cleanly via the control
// socket. Returns (true, nil) when a daemon acknowledged the request and
// will exit, (false, nil) when no daemon is running (no socket or connection
// refused — common in CI, fresh installs, or interactive `af upgrade`), and
// (false, err) for unexpected errors.
//
// This is invoked after `af upgrade` / autoUpdate() write a new binary so the
// running daemon process, which still references the old binary's inode, is
// taken down. A subsequent RPC call (CreateSession, KillSession, etc.) will
// EnsureDaemon-respawn from the freshly written binary.
func RequestShutdown() (bool, error) {
	socketPath, err := DaemonSocketPath()
	if err != nil {
		return false, err
	}
	if _, statErr := os.Stat(socketPath); statErr != nil {
		if errors.Is(statErr, fs.ErrNotExist) {
			return false, nil
		}
		return false, statErr
	}
	var resp ShutdownResponse
	if err := callDaemonNoEnsure("Shutdown", ShutdownRequest{}, &resp); err != nil {
		if isDaemonAbsentErr(err) {
			return false, nil
		}
		return false, err
	}
	return resp.OK, nil
}

// isDaemonAbsentErr reports whether err from a dial/RPC call indicates that
// no daemon is currently listening on the control socket (vs. some other
// transport failure). Both ECONNREFUSED (stale socket, no listener) and
// ENOENT (socket removed between Stat and Dial) qualify.
func isDaemonAbsentErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, fs.ErrNotExist) {
		return true
	}
	return false
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
	if err := s.manager.KillSession(req); err != nil {
		return err
	}
	resp.OK = true
	return nil
}

func (s *controlServer) SendPrompt(req SendPromptRequest, resp *SendPromptResponse) error {
	if err := s.manager.SendPrompt(req); err != nil {
		return err
	}
	resp.OK = true
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
	if err := appendInstanceData(repo.ID, data); err != nil {
		_ = instance.Kill()
		return session.InstanceData{}, err
	}

	m.mu.Lock()
	m.instances[daemonInstanceKey(repo.ID, title)] = instance
	m.mu.Unlock()

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
	key := daemonInstanceKey(repoID, title)
	if _, ok := m.reservedTitles[key]; ok {
		return fmt.Errorf("session with title %q is already reserved", title)
	}
	if _, ok := m.instances[key]; ok {
		return fmt.Errorf("session with title %q already exists", title)
	}
	for _, data := range diskData {
		if data.Title == title {
			return fmt.Errorf("session with title %q already exists", title)
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
		cleanupGhostWorktree(*data, req.Title)
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
			if existing[i].Title == data.Title {
				return nil, fmt.Errorf("session with title %q already exists", data.Title)
			}
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

func cleanupGhostWorktree(data session.InstanceData, title string) {
	if data.Worktree.RepoPath == "" || data.Worktree.WorktreePath == "" || data.Worktree.ExternalWorktree {
		return
	}
	branchCreatedByUs := true
	if data.Worktree.BranchCreatedByUs != nil {
		branchCreatedByUs = *data.Worktree.BranchCreatedByUs
	}
	gw, err := git.NewGitWorktreeFromStorage(
		data.Worktree.RepoPath,
		data.Worktree.WorktreePath,
		data.Worktree.SessionName,
		data.Worktree.BranchName,
		data.Worktree.BaseCommitSHA,
		data.Worktree.ExternalWorktree,
		branchCreatedByUs,
	)
	if err != nil {
		log.WarningLog.Printf("ghost session %q: failed to load worktree for cleanup: %v", title, err)
		return
	}
	if err := gw.Cleanup(); err != nil {
		log.WarningLog.Printf("ghost session %q: worktree cleanup failed: %v", title, err)
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
			} else {
				log.ErrorLog.Printf("waitForReady timed out. Last pane content: %s", content)
			}
			return fmt.Errorf("timed out waiting for program to start (%s)", waitForReadyTimeout)
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
