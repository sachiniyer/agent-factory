package daemon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/session"
	sessiontmux "github.com/sachiniyer/agent-factory/session/tmux"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Session teardown must leave no editor behind. These drive the real
// ArchiveSession/KillSession paths and assert the supervisor entry is gone —
// registering a marker rather than spawning a real code-server, so the test
// proves the LIFECYCLE WIRING (which is the part that rots) without paying for a
// process start. vscode_server_test.go covers actually killing a real child.

// registerVSCodeMarker stands a marker editor in the supervisor for key.
func registerVSCodeMarker(m *Manager, key string) {
	m.mu.Lock()
	instanceID := ""
	if inst := m.instances[key]; inst != nil {
		instanceID = inst.ID
	}
	m.mu.Unlock()
	m.vscode.mu.Lock()
	defer m.vscode.mu.Unlock()
	m.vscode.servers[key] = &vscodeServer{
		worktree: "/nowhere", instanceID: instanceID, exited: make(chan struct{}),
	}
}

func vscodeServerRegistered(m *Manager, key string) bool {
	m.vscode.mu.Lock()
	defer m.vscode.mu.Unlock()
	_, ok := m.vscode.servers[key]
	return ok
}

func writeVSCodeOwnerFixture(t *testing.T, key, instanceID, bootID string, process proctree.Process) string {
	t.Helper()
	pidNamespace, err := proctree.PIDNamespaceID()
	require.NoError(t, err)
	socketPath, err := vscodeSocketPath(key)
	require.NoError(t, err)
	raw, err := json.Marshal(map[string]any{
		"key": key, "instance_id": instanceID, "pid": process.PID,
		"start_id": process.StartID, "boot_id": bootID, "pid_namespace_id": pidNamespace,
		"process_nonce": "test-vscode-owner-nonce",
	})
	require.NoError(t, err)
	path := vscodeOwnerPath(socketPath)
	require.NoError(t, os.WriteFile(path, append(raw, '\n'), 0o600))
	return path
}

func writeUnreadableVSCodeOwnerFixture(t *testing.T, key string) {
	t.Helper()
	socketPath, err := vscodeSocketPath(key)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(vscodeOwnerPath(socketPath), []byte("{"), 0o600))
}

func startOwnedSleep(t *testing.T) (*exec.Cmd, proctree.Process) {
	return startOwnedSleepWithNonce(t, "test-vscode-owner-nonce")
}

func startOwnedSleepWithNonce(t *testing.T, processNonce string) (*exec.Cmd, proctree.Process) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", "exec sleep 60")
	cmd.Env = append(os.Environ(), vscodeOwnerNonceEnv+"="+processNonce)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	require.NoError(t, cmd.Start())
	waited := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(waited)
	}()
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		select {
		case <-waited:
		case <-time.After(2 * time.Second):
			t.Errorf("owned sleep pid %d did not exit", cmd.Process.Pid)
		}
	})
	deadline := time.Now().Add(2 * time.Second)
	var process proctree.Process
	for {
		candidate, lookupErr := proctree.Lookup(cmd.Process.Pid)
		value, status := proctree.LookupEnv(cmd.Process.Pid, vscodeOwnerNonceEnv)
		if lookupErr == nil && candidate.Comm == "sleep" && status == proctree.EnvFound {
			require.Equal(t, processNonce, value)
			process = candidate
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("owned sleep pid %d never stabilized (comm=%q, lookup=%v, environment=%s)",
				cmd.Process.Pid, candidate.Comm, lookupErr, status)
		}
		time.Sleep(time.Millisecond)
	}
	return cmd, process
}

func requireSessionRecordRetained(t *testing.T, manager *Manager, repoID, key string, inst *session.Instance) {
	t.Helper()
	manager.mu.Lock()
	current := manager.instances[key]
	manager.mu.Unlock()
	require.Same(t, inst, current, "editor cleanup uncertainty removed the in-memory retry handle")
	raw, err := config.LoadRepoInstances(repoID)
	require.NoError(t, err)
	require.Contains(t, string(raw), `"title":"`+inst.Title+`"`, "editor cleanup uncertainty deleted the durable retry handle")
}

func TestPersistedVSCodeOwner_FromPreviousBootIsNeverSignaled(t *testing.T) {
	shortAFHome(t)
	_, process := startOwnedSleep(t)
	path := writeVSCodeOwnerFixture(t, "prior-boot", "instance-1", "definitely-not-this-boot", process)
	owner, err := readVSCodeOwner(path)
	require.NoError(t, err)

	v := newVSCodeSupervisor()
	var signaled bool
	v.killGroup = func(pgid int, sig syscall.Signal) error {
		signaled = true
		return syscall.Kill(-pgid, sig)
	}
	require.NoError(t, v.stopPersistedOwner(owner))
	require.False(t, signaled, "an owner record from a previous boot authorized signaling a reused PID")
}

func TestPersistedVSCodeOwner_ProcessNonceRejectsReusedIdentity(t *testing.T) {
	shortAFHome(t)
	_, process := startOwnedSleepWithNonce(t, "live-process-token")
	bootID, err := proctree.BootID()
	require.NoError(t, err)
	pidNamespace, err := proctree.PIDNamespaceID()
	require.NoError(t, err)

	socketPath, err := vscodeSocketPath("reused-process")
	require.NoError(t, err)
	raw, err := json.Marshal(map[string]any{
		"key": "reused-process", "instance_id": "instance-1", "pid": process.PID,
		"start_id": process.StartID, "boot_id": bootID, "pid_namespace_id": pidNamespace,
		"process_nonce": "recorded-owner-token",
	})
	require.NoError(t, err)
	path := vscodeOwnerPath(socketPath)
	require.NoError(t, os.WriteFile(path, append(raw, '\n'), 0o600))
	owner, err := readVSCodeOwner(path)
	require.NoError(t, err)

	v := newVSCodeSupervisor()
	v.groupAlive = func(int) (bool, error) { return false, nil }
	var signals []syscall.Signal
	v.killGroup = func(_ int, sig syscall.Signal) error {
		signals = append(signals, sig)
		return nil
	}
	require.NoError(t, v.stopPersistedOwner(owner))
	require.Empty(t, signals,
		"matching PID/start identity authorized signaling a process without the persisted ownership nonce")
}

func TestPersistedVSCodeOwner_PIDNamespaceRejectsReusedIdentity(t *testing.T) {
	shortAFHome(t)
	_, process := startOwnedSleep(t)
	bootID, err := proctree.BootID()
	require.NoError(t, err)

	socketPath, err := vscodeSocketPath("reused-namespace")
	require.NoError(t, err)
	raw, err := json.Marshal(map[string]any{
		"key": "reused-namespace", "instance_id": "instance-1", "pid": process.PID,
		"start_id": process.StartID, "boot_id": bootID, "pid_namespace_id": "pidns:foreign",
		"process_nonce": "test-vscode-owner-nonce",
	})
	require.NoError(t, err)
	path := vscodeOwnerPath(socketPath)
	require.NoError(t, os.WriteFile(path, append(raw, '\n'), 0o600))
	owner, err := readVSCodeOwner(path)
	require.NoError(t, err)

	v := newVSCodeSupervisor()
	v.groupAlive = func(int) (bool, error) { return false, nil }
	var signals []syscall.Signal
	v.killGroup = func(_ int, sig syscall.Signal) error {
		signals = append(signals, sig)
		return nil
	}
	require.NoError(t, v.stopPersistedOwner(owner))
	require.Empty(t, signals,
		"an owner record from another PID namespace authorized signaling a reused process identity")
}

func TestPersistedVSCodeStop_DoesNotHoldSupervisorLockWhileWaiting(t *testing.T) {
	shortAFHome(t)
	_, process := startOwnedSleep(t)
	bootID, err := proctree.BootID()
	require.NoError(t, err)
	writeVSCodeOwnerFixture(t, "blocked", "instance-1", bootID, process)

	v := newVSCodeSupervisor()
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	v.killGroup = func(pgid int, sig syscall.Signal) error {
		once.Do(func() {
			close(entered)
			<-release
		})
		return syscall.Kill(-pgid, sig)
	}
	stopDone := make(chan error, 1)
	go func() { stopDone <- v.stopForInstance("blocked", "instance-1") }()
	select {
	case <-entered:
	case stopErr := <-stopDone:
		t.Fatalf("persisted owner teardown returned before reaching its blocking signal seam: %v", stopErr)
	case <-time.After(2 * time.Second):
		t.Fatal("persisted owner teardown never reached its blocking signal seam")
	}

	unrelatedDone := make(chan struct{})
	go func() {
		v.stopFor("unrelated")
		close(unrelatedDone)
	}()
	blocked := false
	select {
	case <-unrelatedDone:
	case <-time.After(250 * time.Millisecond):
		blocked = true
	}
	close(release)
	require.NoError(t, <-stopDone)
	if blocked {
		t.Fatal("persisted owner teardown held the supervisor-wide mutex while waiting for a process group")
	}
}

func TestPersistedVSCodeOwner_DoesNotEscalateAfterLeaderExit(t *testing.T) {
	shortAFHome(t)
	cmd, process := startOwnedSleep(t)
	bootID, err := proctree.BootID()
	require.NoError(t, err)
	path := writeVSCodeOwnerFixture(t, "leader-exit", "instance-1", bootID, process)
	owner, err := readVSCodeOwner(path)
	require.NoError(t, err)

	v := newVSCodeSupervisor()
	v.stopGrace = 20 * time.Millisecond
	// Model the old numeric PGID being observed as live after the recorded leader
	// exits. It may be a surviving old child or a reused unrelated group; neither
	// fact authorizes a signal once the stable leader identity is gone.
	v.groupAlive = func(int) (bool, error) { return true, nil }
	var signals []syscall.Signal
	v.killGroup = func(pgid int, sig syscall.Signal) error {
		signals = append(signals, sig)
		if sig == syscall.SIGTERM {
			require.NoError(t, syscall.Kill(-pgid, sig))
			deadline := time.Now().Add(time.Second)
			for time.Now().Before(deadline) {
				if _, lookupErr := proctree.Lookup(cmd.Process.Pid); lookupErr != nil {
					break
				}
				time.Sleep(time.Millisecond)
			}
		}
		return nil
	}
	err = v.stopPersistedOwner(owner)
	require.Error(t, err, "a visible group without the recorded leader must remain unknown")
	require.Equal(t, []syscall.Signal{syscall.SIGTERM}, signals,
		"persisted teardown escalated after losing the stable leader identity")
}

func TestPersistedVSCodeOwner_DoesNotSignalGroupWithoutRecordedLeader(t *testing.T) {
	bootID, err := proctree.BootID()
	require.NoError(t, err)
	if proctree.BootIDIsFallback(bootID) {
		t.Skip("requires a strong kernel boot identity to exercise the unsafe same-boot path")
	}
	pidNamespace, err := proctree.PIDNamespaceID()
	require.NoError(t, err)
	owner := vscodeOwnerRecord{
		Key: "absent-leader", InstanceID: "instance-1", PID: 99_999_999,
		StartID: 1, BootID: bootID, PIDNamespace: pidNamespace, ProcessNonce: "nonce",
	}
	v := newVSCodeSupervisor()
	v.stopGrace = 10 * time.Millisecond
	v.groupAlive = func(int) (bool, error) { return true, nil }
	var signals []syscall.Signal
	v.killGroup = func(_ int, sig syscall.Signal) error {
		signals = append(signals, sig)
		return nil
	}

	err = v.stopPersistedOwner(owner)
	require.Error(t, err, "a group without the recorded leader must remain unknown")
	require.Empty(t, signals, "persisted teardown signalled a numeric group after stable leader identity was already gone")
}

func TestVSCodeSupervisor_StopReapsEditorOwnedByPreviousSupervisor(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	previous := newTestVSCodeSupervisor(t, binary)
	const (
		key        = "previous-daemon"
		instanceID = "instance-1"
	)
	_, err := previous.ensureServerForInstance(key, instanceID, t.TempDir())
	require.NoError(t, err)
	previous.mu.Lock()
	previousEditor := previous.servers[key]
	previous.mu.Unlock()
	require.NotNil(t, previousEditor)
	require.True(t, previousEditor.alive())

	restarted := newVSCodeSupervisor()
	restarted.Stop()
	select {
	case <-previousEditor.exited:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor shutdown ignored an editor owned by the previous daemon")
	}
}

func TestStopForInstance_ReconcilesPersistedOwnerAfterInMemoryStop(t *testing.T) {
	shortAFHome(t)
	_, process := startOwnedSleep(t)
	bootID, err := proctree.BootID()
	if err != nil {
		t.Fatalf("BootID: %v", err)
	}
	const (
		key        = "mixed-owners"
		instanceID = "instance-1"
	)
	ownerPath := writeVSCodeOwnerFixture(t, key, instanceID, bootID, process)

	v := newVSCodeSupervisor()
	v.servers[key] = &vscodeServer{instanceID: instanceID, exited: make(chan struct{})}
	v.killGroup = func(pgid int, sig syscall.Signal) error { return syscall.Kill(-pgid, sig) }
	if err := v.stopForInstance(key, instanceID); err != nil {
		t.Fatalf("stopForInstance: %v", err)
	}
	if _, err := os.Stat(ownerPath); !os.IsNotExist(err) {
		t.Fatalf("persisted owner survived successful in-memory teardown, stat error = %v", err)
	}
}

func TestVSCodeSupervisor_StopDoesNotCreateMissingHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "missing-af-home")
	t.Setenv("AGENT_FACTORY_HOME", home)

	newVSCodeSupervisor().Stop()

	_, err := os.Stat(home)
	require.ErrorIs(t, err, os.ErrNotExist, "editor shutdown recreated a missing Agent Factory home")
}

func TestStopForInstance_ReportsUnconfirmedLiveEditorExit(t *testing.T) {
	shortAFHome(t)
	cmd, _ := startOwnedSleep(t)
	v := newVSCodeSupervisor()
	v.servers["stuck"] = &vscodeServer{
		worktree:   "/stuck-editor",
		instanceID: "instance-1",
		cmd:        cmd,
		exited:     make(chan struct{}),
		stopGrace:  20 * time.Millisecond,
		// Model a process group that accepts both signals but never exits. The
		// lifecycle caller must receive UNKNOWN rather than permission to mutate
		// the editor's worktree.
		killGroup: func(int, syscall.Signal) error { return nil },
	}

	err := v.stopForInstance("stuck", "instance-1")
	require.Error(t, err, "an unconfirmed editor exit must block destructive session teardown")
}

// TestArchiveSession_StopsVSCodeEditor: archiving MOVES the worktree, and the
// editor's cwd is that worktree — leaving it running would strand it serving a
// path that no longer exists.
func TestArchiveSession_StopsVSCodeEditor(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerArchivable(t, manager, repoID, repoPath, "worker")
	key := daemonInstanceKey(repoID, "worker")
	registerVSCodeMarker(manager, key)

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)

	require.False(t, vscodeServerRegistered(manager, key),
		"archiving a session left its VS Code editor running against the moved worktree")
}

func TestArchiveSession_InitialVSCodeStopFailurePreservesLiveness(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, origPath := registerArchivable(t, manager, repoID, repoPath, "limit-blocked")
	inst.SetLimitReached(time.Now().Add(time.Hour))
	cmd, _ := startOwnedSleep(t)
	key := daemonInstanceKey(repoID, "limit-blocked")
	manager.vscode.mu.Lock()
	manager.vscode.servers[key] = &vscodeServer{
		worktree: origPath, instanceID: inst.ID, cmd: cmd, exited: make(chan struct{}),
		stopGrace: 10 * time.Millisecond,
		killGroup: func(int, syscall.Signal) error { return nil },
	}
	manager.vscode.mu.Unlock()

	_, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "limit-blocked", RepoID: repoID})
	require.ErrorContains(t, err, "editor teardown could not be confirmed")
	require.Equal(t, session.LiveLimitReached, inst.GetLiveness(),
		"an archive aborted before tmux or the worktree changed must preserve the prior liveness")
	require.Equal(t, session.OpNone, inst.GetInFlightOp(), "the aborted archive fence must be cleared")
	require.Equal(t, origPath, inst.GetWorktreePath(), "the aborted archive moved the worktree")
}

func TestArchiveSession_FinalVSCodeStopFailureRollsBack(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, origPath := registerArchivable(t, manager, repoID, repoPath, "late-editor")
	inst.SetTmuxSession(sessiontmux.NewTmuxSession("af_late_editor_agent", sessiontmux.ProgramClaude))
	if _, err := inst.AddVSCodeTab("editor"); err != nil {
		t.Fatalf("AddVSCodeTab: %v", err)
	}
	cmd, _ := startOwnedSleep(t)
	key := daemonInstanceKey(repoID, "late-editor")

	origTeardown := archiveTeardown
	archiveTeardown = func(target *session.Instance, dest string, beforeMove func() error) (error, error) {
		hookErr, err := origTeardown(target, dest, beforeMove)
		if err != nil {
			return hookErr, err
		}
		manager.vscode.mu.Lock()
		manager.vscode.servers[key] = &vscodeServer{
			worktree: origPath, instanceID: inst.ID, cmd: cmd, exited: make(chan struct{}),
			stopGrace: 10 * time.Millisecond,
			killGroup: func(int, syscall.Signal) error { return nil },
		}
		manager.vscode.mu.Unlock()
		return hookErr, nil
	}
	t.Cleanup(func() { archiveTeardown = origTeardown })

	dest, err := archivedWorktreePath(repoID, "late-editor")
	if err != nil {
		t.Fatalf("archivedWorktreePath: %v", err)
	}
	if _, _, err := manager.ArchiveSession(ArchiveSessionRequest{Title: "late-editor", RepoID: repoID}); err == nil {
		t.Fatal("archive reported success after its final editor sweep could not confirm exit")
	}
	if inst.GetLiveness() != session.LiveLost {
		t.Fatalf("archive with an unconfirmed late editor left liveness %v, want Lost", inst.GetLiveness())
	}
	if got := inst.GetWorktreePath(); got != origPath {
		t.Fatalf("archive with an unconfirmed late editor left worktree at %q, want rollback to %q", got, origPath)
	}
	if _, err := os.Stat(dest); !os.IsNotExist(err) {
		t.Fatalf("archive destination survived rollback, stat error = %v", err)
	}
}

// TestKillSession_StopsVSCodeEditor: killing removes the worktree, so its editor
// must go with it rather than linger as an orphan holding a loopback port.
func TestKillSession_StopsVSCodeEditor(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	registerArchivable(t, manager, repoID, repoPath, "worker")
	key := daemonInstanceKey(repoID, "worker")
	registerVSCodeMarker(manager, key)

	_, err := manager.KillSession(KillSessionRequest{Title: "worker", RepoID: repoID})
	require.NoError(t, err)

	require.False(t, vscodeServerRegistered(manager, key),
		"killing a session left its VS Code editor running")
}

// TestFinishUserKill_StopsVSCodeEditor covers the post-tombstone retry path:
// a kill interrupted after committing its durable intent must converge to the
// same no-editor end state as the synchronous KillSession tail.
func TestFinishUserKill_StopsVSCodeEditor(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, manager, repoID, repoPath, "worker", session.NewFakeBackend(), true, session.Ready)
	key := daemonInstanceKey(repoID, "worker")
	registerVSCodeMarker(manager, key)
	require.NoError(t, manager.persistKillTombstone(repoID, inst, nil))

	manager.finishUserKill(repoID, inst)

	require.False(t, vscodeServerRegistered(manager, key),
		"finishing a tombstoned kill left its VS Code editor running")
}

// TestFinishUserKill_StopsEditorOwnedByPreviousSupervisor models the daemon-crash
// case that creates a tombstone retry. The restarted daemon has an empty
// supervisor map, so the editor can be found only through durable process
// ownership recorded by the daemon that spawned it.
func TestFinishUserKill_StopsEditorOwnedByPreviousSupervisor(t *testing.T) {
	binary := writeFakeVSCodeBinary(t, "code-server", nil)
	manager, _, _, _ := newVSCodeFixture(t, binary)
	const title = "vscodeproxy"
	inst, repoID := vscodeFixtureInstance(t, manager, title)
	key := daemonInstanceKey(repoID, title)

	if _, err := manager.ensureVSCodeServer(inst, repoID, title); err != nil {
		t.Fatalf("ensureVSCodeServer: %v", err)
	}
	manager.vscode.mu.Lock()
	previousEditor := manager.vscode.servers[key]
	manager.vscode.mu.Unlock()
	if previousEditor == nil || !previousEditor.alive() {
		t.Fatal("precondition: previous supervisor did not own a live editor")
	}
	// Keep the session teardown itself inert so the only possible reaper is the
	// restarted supervisor's durable-ownership path. A local backend may discover
	// and reap processes rooted in its worktree, which would let this pass without
	// proving stopFor found the previous daemon's editor.
	inst.SetBackend(session.NewFakeBackend())

	// Simulate the daemon restart: durable session state survives, but the new
	// supervisor has no pointer to the old process.
	manager.vscode = newVSCodeSupervisor()
	require.NoError(t, manager.persistKillTombstone(repoID, inst, nil))
	require.True(t, previousEditor.alive(), "persisting the tombstone unexpectedly reaped the editor")
	manager.finishUserKill(repoID, inst)

	select {
	case <-previousEditor.exited:
	case <-time.After(2 * time.Second):
		t.Fatal("finishing the tombstoned kill could not reap the previous daemon's editor")
	}
}

func TestKillSession_RetainsRecordWhenEditorOwnershipIsUnknown(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, manager, repoID, repoPath, "worker", session.NewFakeBackend(), true, session.Ready)
	key := daemonInstanceKey(repoID, "worker")
	writeUnreadableVSCodeOwnerFixture(t, key)

	_, err := manager.KillSession(KillSessionRequest{Title: "worker", RepoID: repoID})
	require.ErrorContains(t, err, "VS Code editor")
	requireSessionRecordRetained(t, manager, repoID, key, inst)
}

func TestFinishUserKill_RetainsRecordWhenEditorOwnershipIsUnknown(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, manager, repoID, repoPath, "worker", session.NewFakeBackend(), true, session.Ready)
	key := daemonInstanceKey(repoID, "worker")
	writeUnreadableVSCodeOwnerFixture(t, key)
	require.NoError(t, manager.persistKillTombstone(repoID, inst, nil))

	manager.finishUserKill(repoID, inst)
	requireSessionRecordRetained(t, manager, repoID, key, inst)
}

// TestReapDeadRoot_StopsVSCodeEditor is the adjacent automatic teardown path.
// A dead root is removed and recreated under the same session key, but its old
// vscode tabs are not carried forward, so retaining their editor leaks an
// unreachable daemon child into the replacement root's lifetime.
func TestReapDeadRoot_StopsVSCodeEditor(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, manager, repoID, repoPath, session.RootSessionTitle, session.NewFakeBackend(), true, session.Dead)
	key := daemonInstanceKey(repoID, session.RootSessionTitle)
	registerVSCodeMarker(manager, key)

	_, reaped, err := manager.reapDeadRoot(repoID, inst)
	require.NoError(t, err)
	require.True(t, reaped)
	require.False(t, vscodeServerRegistered(manager, key),
		"reaping a dead root left its unreachable VS Code editor running")
}

func TestReapDeadRoot_RetainsRecordWhenEditorOwnershipIsUnknown(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst := registerStarted(t, manager, repoID, repoPath, session.RootSessionTitle, session.NewFakeBackend(), true, session.Dead)
	key := daemonInstanceKey(repoID, session.RootSessionTitle)
	writeUnreadableVSCodeOwnerFixture(t, key)

	_, reaped, err := manager.reapDeadRoot(repoID, inst)
	require.ErrorContains(t, err, "VS Code editor")
	require.False(t, reaped)
	requireSessionRecordRetained(t, manager, repoID, key, inst)
}

// TestEnsureVSCodeServer_RefusesInertSessions is the codex P1 gate: the webtab
// proxy resolves (and may spawn) an editor WITHOUT the kill/archive op-lock — it
// must, since a spawn blocks for seconds — so nothing but this check stops a
// stale iframe refresh, or simply selecting an archived row that still has a
// vscode tab, from starting an editor for a session whose worktree is being moved
// or removed.
func TestEnsureVSCodeServer_RefusesInertSessions(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "worker")

	// A live session resolves (it gets as far as needing a binary, which is the
	// point: the gate is not what stops it).
	if _, err := manager.ensureVSCodeServer(inst, repoID, "worker"); err != nil &&
		strings.Contains(err.Error(), "archived") {
		t.Fatalf("a LIVE session was refused as inert: %v", err)
	}

	// Archived: refused, and named actionably.
	if err := inst.Transition(session.BeginArchive()); err != nil {
		t.Fatalf("BeginArchive: %v", err)
	}
	if err := inst.Transition(session.CommitArchive()); err != nil {
		t.Fatalf("CommitArchive: %v", err)
	}
	_, err := manager.ensureVSCodeServer(inst, repoID, "worker")
	if err == nil {
		t.Fatal("ensureVSCodeServer started an editor for an ARCHIVED session; archive must stop it until restore")
	}
	if !strings.Contains(err.Error(), "archived") {
		t.Fatalf("err = %v, want one naming the archived state", err)
	}
}

// TestEnsureVSCodeServer_RefusesMidArchive covers the in-flight window rather than
// the settled state: BeginArchive raises the fence before the worktree moves, and
// a request arriving in that window must not start an editor rooted at a directory
// that is about to be relocated.
func TestEnsureVSCodeServer_RefusesMidArchive(t *testing.T) {
	manager, repoID, repoPath := newStatusTestManager(t)
	inst, _ := registerArchivable(t, manager, repoID, repoPath, "worker")

	if err := inst.Transition(session.BeginArchive()); err != nil {
		t.Fatalf("BeginArchive: %v", err)
	}
	_, err := manager.ensureVSCodeServer(inst, repoID, "worker")
	if err == nil {
		t.Fatal("ensureVSCodeServer started an editor for a session mid-archive")
	}
	if !strings.Contains(err.Error(), "being archived or removed") {
		t.Fatalf("err = %v, want one naming the in-flight teardown", err)
	}
}

// TestKillProbeAlive is #2823. `kill(-pgid, 0)` was classified as
// alive / dead / hard-error, and EPERM fell into the hard-error arm — so a
// recycled process-group id turned an ordinary teardown into
// "confirming previous editor process group N stopped: operation not
// permitted", failing an archive or kill the user could do nothing about.
//
// EPERM is not ambiguity. POSIX returns it only when processes exist in the
// group and the caller may signal NONE of them. The editor we recorded was
// started by this daemon, under this uid, so it was signalable by construction:
// if nothing in the group is, our editor is not in it. The group we were
// waiting on is therefore gone and the numeric id has been reused — which is
// exactly "not alive" for every caller here, and waiting longer cannot make an
// unsignalable group ours.
func TestKillProbeAlive(t *testing.T) {
	otherErr := errors.New("some unrelated failure")
	for _, tc := range []struct {
		name      string
		err       error
		wantAlive bool
		wantErr   error
	}{
		{name: "signal delivered", err: nil, wantAlive: true},
		{name: "no such group", err: syscall.ESRCH},
		{name: "recycled id owned by someone else", err: syscall.EPERM},
		{name: "wrapped EPERM", err: fmt.Errorf("kill: %w", syscall.EPERM)},
		{name: "anything else is still an error", err: otherErr, wantErr: otherErr},
	} {
		t.Run(tc.name, func(t *testing.T) {
			alive, err := killProbeAlive(tc.err)
			assert.Equal(t, tc.wantAlive, alive)
			if tc.wantErr == nil {
				assert.NoError(t, err, "an answerable kill(2) result must not abort teardown")
				return
			}
			assert.ErrorIs(t, err, tc.wantErr,
				"an error we cannot interpret must still surface rather than read as 'gone'")
		})
	}
}

// The wait loop is where a misclassified EPERM actually reached the user, so it
// is pinned through the real classifier rather than only at the seam.
func TestWaitForProcessGroupExitTreatsEPERMAsExited(t *testing.T) {
	v := newVSCodeSupervisor()
	calls := 0
	v.groupAlive = func(int) (bool, error) {
		calls++
		return killProbeAlive(syscall.EPERM)
	}

	exited, err := v.waitForProcessGroupExit(4242, 2*time.Second)
	require.NoError(t, err, "a recycled process-group id must not fail the teardown (#2823)")
	assert.True(t, exited, "the group we recorded is gone once nothing in it is ours to signal")
	assert.Equal(t, 1, calls, "an answered probe must not spin until the deadline")
}
