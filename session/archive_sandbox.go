package session

import (
	"errors"
	"fmt"

	"github.com/sachiniyer/agent-factory/log"
)

// Archive/restore for the disposable sandbox backends (#1592 Phase 4 PR6),
// written ONCE against the Runtime interface so docker and ssh share it. The
// model (epic decision 4): GitHub is the durable workspace store, so —
//
//   - Archive = push the session branch to origin (durability), then tear the
//     sandbox down (the container/remote is disposable, the branch is not).
//   - Restore/Recover = re-provision a FRESH sandbox that clones the pushed
//     branch back, then relaunch the agent.
//
// Both go through the same two Runtime primitives every sandbox runtime already
// implements — Provision (with RestoreBranch on restore) and the Teardown/Kill
// reap — so there is no per-backend archive logic here and no locality branch.
// The local runtime keeps its worktree-move archive (daemon/archive.go), untouched.

// pushBranchForArchive commits any uncommitted work and pushes this instance's
// branch to origin, returning the pushed branch (#1592 Phase 4 PR6). It runs
// INSIDE a sandbox as the local agent-server's Archive implementation
// (agentserver_local.go), where the git worktree it pushes actually lives.
func (i *Instance) pushBranchForArchive() (string, error) {
	i.mu.RLock()
	gw := i.gitWorktree
	i.mu.RUnlock()
	if gw == nil {
		return "", fmt.Errorf("cannot archive session %q: no worktree to push", i.Title)
	}
	return gw.SnapshotAndPushBranch()
}

// ArchiveSandbox makes an off-box session (docker/ssh/hook) durable and reaps
// its sandbox (#1592 Phase 4 PR6/PR7): it pushes the branch to origin over the
// agent-server, then tears the in-sandbox workspace down and reaps the sandbox
// (the AgentServer.Kill path), and finally clears the now-dead remote wiring so a
// later restore rebuilds it. Returns the pushed branch, which the caller records
// so restore knows which branch to clone back.
//
// This is the raw mechanic the daemon's ArchiveSession wraps with its locks +
// state transitions + persistence (and which the round-trip test drives
// directly, like Start/Kill). It is a no-op-and-error for a session with no
// remote runtime — a local session archives by relocating its worktree, not here.
func (i *Instance) ArchiveSandbox() (string, error) {
	i.mu.RLock()
	isRemote := i.remoteClient != nil
	i.mu.RUnlock()
	if !isRemote {
		return "", fmt.Errorf("session %q is not a sandbox session; cannot archive via push/teardown", i.Title)
	}

	as := i.AgentServer()
	// 1. Push the branch to origin while the sandbox is still alive — the workspace
	//    state must be durable on GitHub before anything is torn down.
	branch, err := as.Archive()
	if err != nil {
		return "", fmt.Errorf("failed to push branch for session %q before archive: %w", i.Title, err)
	}
	// 2. Record the branch the INSTANT it is durable on origin — before the teardown
	//    below, which can fail (#1781). Durable-on-origin is the point of no return:
	//    from here the branch is the only handle on the user's work, so it belongs on
	//    the record whatever happens to the sandbox afterwards. Recording it only on
	//    the all-succeeded path loses it exactly when it matters most — a sandbox
	//    session's daemon-side i.Branch is otherwise EMPTY (the only other writer,
	//    backend_local.go's provision, runs INSIDE the sandbox and never mutates this
	//    Instance; the branch reaches the daemon solely as this Archive() return), and
	//    a failed Kill sends the session to Lost + recovery-eligible via the caller's
	//    AbortArchiveToLost (daemon/archive.go). The Lost-restore loop then re-provisions
	//    from i.Branch (reprovisionRemote → ProvisionSpec.RestoreBranch), and an empty
	//    RestoreBranch makes the docker/ssh runtimes SKIP the restore fetch and clone the
	//    repo's DEFAULT branch — a "successful" recovery onto the wrong branch that
	//    silently strands the work this push just made durable.
	i.mu.Lock()
	i.Branch = branch
	i.mu.Unlock()
	// 3. Tear the in-sandbox workspace down over REST and reap the sandbox itself
	//    (container rm / remote dir cleanup + tunnel close) — the branch is durable,
	//    the sandbox is disposable.
	if err := as.Kill(); err != nil {
		if TeardownStateUnknown(err) {
			i.markRuntimeCleanupStateUnknown()
		}
		return branch, fmt.Errorf("pushed branch %q but failed to tear the sandbox down for session %q: %w", branch, i.Title, err)
	}
	// 4. Drop the dead remote wiring so the instance is an inert archived record
	//    until restore re-provisions it. The backend stays (its Type()/Capabilities
	//    keep the session classified as a remote sandbox for load + restore).
	i.resetRemoteRuntime()
	i.mu.Lock()
	i.started = false
	i.mu.Unlock()
	return branch, nil
}

// RestoreSandbox re-provisions a fresh sandbox for an archived sandbox session,
// cloning the pushed branch back, and relaunches the agent (#1592 Phase 4 PR6).
// It is the raw restore mechanic (the daemon wraps it with locks + the restore
// transition; the round-trip test drives it directly). The session resumes from
// the pushed branch state — the code survives via GitHub; a fresh agent runs on
// it (the pre-archive conversation lived only in the disposed sandbox).
func (i *Instance) RestoreSandbox() error {
	if err := i.reprovisionRemote(); err != nil {
		return err
	}
	if err := i.Start(true); err != nil {
		// The sandbox is up but the agent failed to launch on it — reap the
		// freshly provisioned sandbox and clear its wiring so a retry re-provisions
		// from a clean state instead of stranding a container/remote (#1726).
		if cleanupErr := i.teardownAfterStartFailure(); cleanupErr != nil {
			return fmt.Errorf("agent start failed and the replacement sandbox's cleanup state is unknown: %w",
				errors.Join(err, cleanupErr))
		}
		return err
	}
	return nil
}

// recoverSandbox re-establishes a Lost/restoring sandbox session in place: it
// re-provisions a fresh sandbox (cloning the branch back), relaunches, and flips
// the session live (#1592 Phase 4 PR6). It is the remote analogue of
// LocalBackend.respawn — the guard-free core the docker/ssh backends' Recover and
// Respawn both call, so the daemon's Lost-restore loop and the archive-restore
// path (RestoreFromArchive → backend.Recover) reuse it unchanged. The caller's
// transition owns the liveness precondition; ConfirmLive clears the restore fence
// on success, exactly as the local respawn does.
func recoverSandbox(i *Instance) error {
	if err := i.reprovisionRemote(); err != nil {
		return err
	}
	if err := i.Start(true); err != nil {
		// Same leak guard as RestoreSandbox: reap the fresh sandbox and reset the
		// remote wiring on a Start failure so the Lost-restore loop's next retry
		// re-provisions cleanly rather than stacking a second sandbox on the first
		// still-running one (#1726).
		if cleanupErr := i.teardownAfterStartFailure(); cleanupErr != nil {
			return fmt.Errorf("agent start failed and the replacement sandbox's cleanup state is unknown: %w",
				errors.Join(err, cleanupErr))
		}
		return err
	}
	_ = i.Transition(ConfirmLive())
	return nil
}

// reprovisionRemote provisions a FRESH sandbox for this instance and rebinds the
// instance to it (#1592 Phase 4 PR6). It resolves the runtime from the instance's
// persisted backend kind and provisions with RestoreBranch set to the session's
// branch, so the new sandbox clones the pushed state back; on success the new
// backend + remote agent-server endpoint + teardown replace the old (dead) ones.
func (i *Instance) reprovisionRemote() error {
	i.mu.RLock()
	backend := i.backend
	spec := ProvisionSpec{
		RepoRoot:              i.Path,
		Title:                 i.Title,
		Program:               i.Program,
		CloneURL:              originRemoteURL(i.Path),
		RestoreBranch:         i.Branch,
		SessionEnvPassthrough: configuredSessionEnvPassthrough(i.sessionEnvPassthrough),
	}
	i.mu.RUnlock()
	if backend == nil {
		return fmt.Errorf("cannot re-provision session %q: no backend on record", i.Title)
	}
	kind, err := backendKindForType(backend.Type())
	if err != nil {
		return fmt.Errorf("cannot re-provision session %q: %w", i.Title, err)
	}
	rt, err := ResolveRuntime(kind)
	if err != nil {
		return fmt.Errorf("cannot re-provision session %q: %w", i.Title, err)
	}
	// A failed archive/recovery can leave the old sandbox wiring live. Reap it
	// before provisioning a replacement so bindProvisionResult never discards its
	// only cleanup handle. An unknown outcome keeps the old wiring installed for a
	// real retry instead of provisioning a second sandbox on a guess.
	if err := i.reapRemoteRuntimeForReplacement(); err != nil {
		return fmt.Errorf("cannot re-provision session %q: previous sandbox cleanup state is unknown: %w", i.Title, err)
	}
	res, err := rt.Provision(spec)
	if err != nil {
		return fmt.Errorf("failed to re-provision sandbox for session %q: %w", i.Title, err)
	}
	if err := i.bindProvisionResult(res); err != nil {
		// The sandbox is up but its endpoint could not be wired — reap it so a
		// bad restore never leaks a container/remote. An unknown reap keeps the
		// new backend + cleanup handle installed (without its invalid client) so
		// the next restore retries that cleanup before provisioning again.
		if res.Teardown != nil {
			if cleanupErr := res.Teardown(); cleanupErr != nil {
				if TeardownStateUnknown(cleanupErr) {
					i.retainProvisionResultCleanup(res)
					return fmt.Errorf("failed to bind re-provisioned sandbox for session %q and its cleanup state is unknown: %w",
						i.Title, errors.Join(err, cleanupErr))
				}
				log.WarningLog.Printf("session %q: unbound replacement sandbox teardown reported a completed error: %v", i.Title, cleanupErr)
			}
		}
		return fmt.Errorf("failed to bind re-provisioned sandbox for session %q: %w", i.Title, err)
	}
	return nil
}

// bindProvisionResult installs a freshly provisioned sandbox's wiring on the
// instance (#1592 Phase 4 PR6): the new backend, a remote agent-server client
// built from the new endpoint, and the new teardown, discarding the cached
// agent-server so AgentServer() rebuilds it against the new client. It mirrors the
// endpoint wiring NewInstance does at create, reused for restore.
func (i *Instance) bindProvisionResult(res ProvisionResult) error {
	var rc *remoteAgentClient
	if res.Endpoint != nil {
		var err error
		rc, err = newRemoteAgentClient(*res.Endpoint, i.Title)
		if err != nil {
			return err
		}
	}
	// Clear the cached agent-server AND swap the runtime fields in ONE i.mu section
	// (#1729): AgentServer() reads the cache and the fields under the same i.mu, so
	// doing the rebind atomically means the poll can never rebuild the cache from a
	// pre-restore snapshot of remoteClient — no stale/torn-down endpoint pinned.
	i.mu.Lock()
	i.agentSrv = nil
	i.backend = res.Backend
	i.remoteClient = rc
	i.runtimeTeardown = res.Teardown
	i.runtimeCleanupStateUnknown = false
	i.mu.Unlock()
	return nil
}

// retainProvisionResultCleanup installs only the identity and cleanup handle of
// a provisioned sandbox whose endpoint could not be bound and whose teardown
// outcome was unknown. The invalid client is deliberately omitted; AgentServer
// rebuilds as a deadRemoteAgentServer whose Kill can retry this exact cleanup.
func (i *Instance) retainProvisionResultCleanup(res ProvisionResult) {
	i.mu.Lock()
	i.agentSrv = nil
	i.backend = res.Backend
	i.remoteClient = nil
	i.runtimeTeardown = res.Teardown
	i.runtimeCleanupStateUnknown = true
	i.mu.Unlock()
}

// resetRemoteRuntime clears the instance's remote-runtime wiring after its
// sandbox has been reaped (archive), leaving an inert record whose backend still
// classifies it as a remote sandbox (#1592 Phase 4 PR6). A subsequent restore
// rebuilds the wiring via bindProvisionResult.
func (i *Instance) resetRemoteRuntime() {
	// Clear the cache and the runtime fields together under i.mu (#1729), so a
	// concurrent AgentServer() never rebuilds against a half-cleared state.
	i.mu.Lock()
	i.agentSrv = nil
	i.remoteClient = nil
	i.runtimeTeardown = nil
	i.runtimeCleanupStateUnknown = false
	i.mu.Unlock()
}

// markRuntimeCleanupStateUnknown makes an indeterminate reap durable without
// changing the session's wanted/kill intent. The exact backend identity staged
// by ToInstanceData then crosses storage so a restart must retry this cleanup
// before provisioning a replacement.
func (i *Instance) markRuntimeCleanupStateUnknown() {
	i.mu.Lock()
	i.runtimeCleanupStateUnknown = true
	i.mu.Unlock()
}

// reapRemoteRuntimeForReplacement reaps the currently bound sandbox before its
// wiring is replaced. Teardown has three outcomes: success and a completed,
// known-state error both follow the runtimes' existing best-effort contract and
// clear the binding; an unknown-state error preserves the binding and handle so
// the next restore can retry instead of stacking another sandbox on it.
//
// The teardown is read under i.mu and invoked outside the lock because docker/SSH
// I/O may block. Each runtime serializes and conditionally latches its own reap.
func (i *Instance) reapRemoteRuntimeForReplacement() error {
	i.mu.RLock()
	teardown := i.runtimeTeardown
	i.mu.RUnlock()
	if teardown == nil {
		return nil
	}
	if err := teardown(); err != nil {
		if TeardownStateUnknown(err) {
			i.markRuntimeCleanupStateUnknown()
			return err
		}
		log.WarningLog.Printf("session %q: previous sandbox teardown reported a completed error; continuing with replacement: %v", i.Title, err)
	}
	i.resetRemoteRuntime()
	return nil
}

// teardownAfterStartFailure applies the same replacement rule to a freshly
// provisioned sandbox whose agent failed to start: clear it after a known
// teardown outcome, but retain an unknown cleanup handle for the next retry.
func (i *Instance) teardownAfterStartFailure() error {
	return i.reapRemoteRuntimeForReplacement()
}

// isSandboxBackendType reports whether a persisted backend Type() names a
// disposable sandbox runtime (docker/ssh/hook) — the runtimes archive/restore
// re-provision (#1592 Phase 4 PR6/PR7).
func isSandboxBackendType(t string) bool {
	_, err := backendKindForType(t)
	return err == nil
}

// newInertSandboxBackend rebuilds a sandbox backend with NO live sandbox handle,
// for a docker/ssh/hook session loaded from disk (#1592 Phase 4 PR6/PR7). Its
// Type() and Capabilities() keep the session classified as its runtime so
// archive/restore route correctly; its reap is nil (nothing live to tear down),
// and restore replaces it wholesale via a fresh Provision. Only ever called for a
// sandbox backend type (the FromInstanceData switch guards it).
func newInertSandboxBackend(t string) Backend {
	switch t {
	case "ssh":
		return &sshBackend{}
	case "remote":
		return &HookBackend{}
	default:
		return &dockerBackend{}
	}
}

// backendKindForType maps a persisted backend Type() discriminator to the
// BackendKind whose Runtime re-provisions it (#1592 Phase 4 PR6/PR7). Only the
// off-box runtimes are re-provisionable (they push/re-clone the durable branch);
// the local runtime never routes here (it relocates a worktree instead), so its
// type is rejected with an actionable error.
func backendKindForType(t string) (BackendKind, error) {
	switch t {
	case "docker":
		return BackendDocker, nil
	case "ssh":
		return BackendSSH, nil
	case "remote":
		return BackendHook, nil
	default:
		return "", fmt.Errorf("backend %q is not a re-provisionable sandbox runtime", t)
	}
}

// RestoreReprovisionsSandbox reports whether a Lost/Dead restore of this session
// re-provisions a FRESH off-box sandbox (cloning the last pushed commit) rather
// than re-spawning it in place. Only the sandbox runtimes (docker/ssh/hook) route
// their Recover through recoverSandbox → reprovisionRemote; a local session
// re-spawns its tmux in the same worktree. A backend-less instance is treated as
// local. This is the backend primitive — RestoreWouldDiscardUnpushedWork gates it
// on the liveness that actually takes the re-provision path.
func (i *Instance) RestoreReprovisionsSandbox() bool {
	b := i.currentBackend()
	if b == nil {
		return false
	}
	_, err := backendKindForType(b.Type())
	return err == nil
}

// RestoreWouldDiscardUnpushedWork reports whether restoring this row re-provisions
// a fresh sandbox and thereby risks discarding work that was never pushed off the
// old one (#1794). It is true only for a Lost/Dead REMOTE session: the daemon's
// restore re-provisions a fresh sandbox when the old one can't be reached
// (daemon/restore.go), losing anything unpushed. A local session re-spawns in
// place, and an archived session (any backend) restores from the branch the
// archive already pushed — neither loses anything. Interactive clients confirm a
// restore for which this is true before triggering it.
func (i *Instance) RestoreWouldDiscardUnpushedWork() bool {
	switch i.GetLiveness() {
	case LiveLost, LiveDead:
		return i.RestoreReprovisionsSandbox()
	default:
		return false
	}
}
