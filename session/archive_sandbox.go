package session

import (
	"context"
	"errors"
	"fmt"
	"github.com/sachiniyer/agent-factory/config"
	"os"
	"os/exec"
	"strings"
	"time"

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
	// Ask the BACKEND whether this is a sandbox session, not the client pointer.
	// daemon/archive.go routes here on the declared capability
	// (Capabilities().Workspace == WorkspaceRemote); re-deriving the same fact
	// from `remoteClient != nil` lets the two disagree, and they disagree in the
	// states that matter: a sandbox session loaded inert from disk, or one left
	// Lost by a failed recovery, has a remote backend and a nil client (the cases
	// AgentServer() enumerates when it installs deadRemoteAgentServer). The old
	// check called those "not a sandbox session", which is false — it is a
	// sandbox session whose wiring is torn down — and sent the reader looking for
	// a backend-detection bug (#3000, the #2924 class).
	i.mu.RLock()
	isSandbox := i.capabilitiesLocked().Workspace == WorkspaceRemote
	hasClient := i.remoteClient != nil
	i.mu.RUnlock()
	if !isSandbox {
		return "", fmt.Errorf("session %q is not a sandbox session; cannot archive via push/teardown", i.Title)
	}
	if !hasClient {
		// Distinct from the above on purpose: same refusal, different cause, and
		// the caller can act on this one (recover the session, then archive).
		return "", fmt.Errorf("session %q is a sandbox session whose runtime wiring is torn down, "+
			"so there is no live client to push its branch through; recover it before archiving", i.Title)
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
	//    from this recorded checkpoint (reprovisionRemote → ProvisionSpec.RestoreBranch), and an empty
	//    RestoreBranch makes the docker/ssh runtimes SKIP the restore fetch and clone the
	//    repo's DEFAULT branch — a "successful" recovery onto the wrong branch that
	//    silently strands the work this push just made durable.
	i.recordArchivePush(branch)
	// 3. Tear the in-sandbox workspace down over REST and reap the sandbox itself
	//    (container rm / remote dir cleanup + tunnel close) — the branch is durable,
	//    the sandbox is disposable.
	if err := as.Kill(false); err != nil {
		if TeardownStateUnknown(err) {
			i.markRuntimeCleanupStateUnknown()
			return branch, fmt.Errorf("pushed branch %q but failed to tear the sandbox down for session %q: %w", branch, i.Title, err)
		}
		// A KNOWN failure here still means the runtime is gone: remoteAgentServer.Kill
		// tears the in-sandbox workspace down over REST and then reaps the sandbox,
		// and it returns the REST error even when the reap SUCCEEDED — the same
		// asymmetry manager_create.go documents for a failed remote create. So the
		// credential must go with it (#3065 review).
		//
		// The runtime wiring is deliberately NOT reset here, unlike the success path
		// below: the archive aborts to Lost and the retry machinery owns what happens
		// to that wiring. Only the credential is settled, because "the runtime that
		// owned it is gone" is already answered, and leaving it live means a copied
		// token keeps authenticating until a recovery happens to replace it — or
		// until the daemon restarts, if none ever does.
		//
		// The reap answered definitively on this branch (a known error is a
		// definite answer, not an unknown one) and latched, so af's teardown
		// obligation is discharged and the backend's immutable cleanup identity
		// points at a runtime af already reaped. Clear it too, but leave the
		// in-memory wiring (runtimeTeardown/remoteClient/agentSrv) for the
		// abort-to-Lost retry: the in-process re-provision reaps through that
		// closure (reapRemoteRuntimeForReplacement), not this field.
		// resetRemoteRuntime clears the same identity on the success path where
		// the wiring is also dropped; without it here, an ordinary sandbox row's
		// ForStorage projection persists the discharged handle and a restart's
		// FromInstanceData rebuilds a teardown for a runtime af already reaped
		// (#5559 review). The unknown-outcome branch above returns before this
		// point, so only a runtime whose reap answered definitively and latched
		// reaches here.
		i.revokeSandboxCredential()
		i.clearDischargedRuntimeCleanup()
		return branch, fmt.Errorf("pushed branch %q but failed to tear the sandbox down for session %q: %w", branch, i.Title, err)
	}
	// 4. Drop the dead remote wiring so the instance is an inert archived record
	//    until restore re-provisions it. The backend stays (its Type()/Capabilities
	//    keep the session classified as a remote sandbox for load + restore).
	i.resetRemoteRuntime()
	i.mu.Lock()
	if i.started {
		i.started = false
		i.touchLocked()
	}
	i.mu.Unlock()
	return branch, nil
}

// recordArchivePush records the durable branch and raises the process-local
// checkpoint fence for this archive generation in one critical section. A
// restored sandbox can already carry the same Branch, so assigning Branch alone
// cannot prove that this ArchiveSandbox call's push has returned.
func (i *Instance) recordArchivePush(branch string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	changed := i.Branch != branch
	i.Branch = branch
	i.branchUnpushed = false
	// A push archive publishes the branch and reaps the host directory, so the
	// work now lives on origin (#5571 W8.5).
	i.archivedWorkspace = &ArchivedWorkspaceData{Host: i.Host, Pushed: true, Branch: branch}
	if branch != "" && !i.archivePushCompleted {
		i.archivePushCompleted = true
		changed = true
	}
	if changed {
		i.touchLocked()
	}
}

// RestoreSandbox re-provisions a fresh sandbox for an archived sandbox session,
// cloning the pushed branch back, and relaunches the agent (#1592 Phase 4 PR6).
// It is the raw restore mechanic (the daemon wraps it with locks + the restore
// transition; the round-trip test drives it directly). The session resumes from
// the pushed branch state — the code survives via GitHub; a fresh agent runs on
// it (the pre-archive conversation lived only in the disposed sandbox).
func (i *Instance) RestoreSandbox() error {
	return restoreSandboxRuntime(i, false)
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
	return restoreSandboxRuntime(i, true)
}

// restoreSandboxRuntime is the body RestoreSandbox and recoverSandbox share. A row
// whose archive kept its workspace reattaches it (W8.6) and launches the agent
// with its recorded conversation; any other row gets a fresh sandbox.
func restoreSandboxRuntime(i *Instance, confirmLive bool) error {
	return restoreSandboxRuntimeWith(i, confirmLive, i.provisionForRestore, i.Start)
}

// restoreSandboxRuntimeWith is restoreSandboxRuntime with the provisioning and the
// launch injected, so the retry behaviour of a failed adopted launch is testable
// without a host.
func restoreSandboxRuntimeWith(i *Instance, confirmLive bool, provision func() (*ArchivedWorkspaceData, error), start func(firstTimeSetup bool) error) error {
	adopted, err := provision()
	if err != nil {
		return err
	}
	if err := start(adopted == nil); err != nil {
		if adopted != nil {
			// The workspace is the archive's, not this attempt's: keep it. Reaping
			// here is what a fresh sandbox's failed start gets, and would delete
			// the only copy of the work.
			i.rollbackAdoption(adopted)
			return fmt.Errorf("the agent did not start in the reattached workspace at %s, which was left as archived: %w", adopted.Dir, err)
		}
		// The sandbox is up but the agent failed to launch on it — reap the
		// freshly provisioned sandbox and clear its wiring so a retry re-provisions
		// from a clean state instead of stranding a container/remote (#1726).
		if cleanupErr := i.teardownAfterStartFailure(); cleanupErr != nil {
			return fmt.Errorf("agent start failed and the replacement sandbox's cleanup state is unknown: %w",
				errors.Join(err, cleanupErr))
		}
		return err
	}
	if adopted != nil {
		// The work is back on a live agent; the row no longer owes a preserved
		// workspace.
		i.mu.Lock()
		i.archivedWorkspace = nil
		i.touchLocked()
		i.mu.Unlock()
	}
	if !confirmLive {
		return nil
	}
	// A reattach starts a new agent process even though the workspace is the same,
	// and a fresh sandbox is a replacement outright (#4222). Stamp the verdict
	// before the edge so the run-end classifier inside it can interrupt a task run
	// still in flight.
	i.markAgentRuntimeReplaced(true)
	_ = i.Transition(ConfirmLive())
	return nil
}

// provisionForRestore brings up the runtime a restore launches the agent on. It
// returns the preserved workspace record when it ADOPTED that workspace (the
// launch is then a reattach, not a first-time setup) and nil when it provisioned
// a fresh sandbox.
func (i *Instance) provisionForRestore() (*ArchivedWorkspaceData, error) {
	if w := i.preservedWorkspaceOwed(); w != nil {
		// A server af started here and could not confirm stopped is reaped first;
		// no adoption proceeds beside a survivor (W8.6).
		if err := i.confirmStoppedBeforeAdopt(); err != nil {
			return nil, err
		}
		return i.restorePreservedWorkspace(w)
	}
	return nil, i.reprovisionRemoteWith(nil)
}

// reprovisionRemote provisions a FRESH sandbox for this instance and rebinds the
// instance to it (#1592 Phase 4 PR6). It resolves the runtime from the instance's
// persisted backend kind and provisions with RestoreBranch set to the session's
// branch, so the new sandbox clones the pushed state back; on success the new
// backend + remote agent-server endpoint + teardown replace the old (dead) ones.
func (i *Instance) reprovisionRemote() error {
	_, err := i.provisionForRestore()
	return err
}

// reprovisionRemoteWith is reprovisionRemote with the adoption choice made. With
// adopt nil it replaces the runtime; with adopt set it provisions over the
// preserved workspace and reaps nothing.
func (i *Instance) reprovisionRemoteWith(adopt *AdoptSpec) error {
	return i.reprovisionRemoteOpts(adopt, false)
}

// reprovisionRemoteOpts is the one body. replacePreserved marks the replacement of
// a preserved workspace the host has PROVEN gone (#5571 W8.6): the only time a row
// that still owes a preserved workspace may be replaced, and then onto the recorded
// checkpoint or not at all.
func (i *Instance) reprovisionRemoteOpts(adopt *AdoptSpec, replacePreserved bool) error {
	// Backstop for a caller that reaches the replacing path directly: it reaps the
	// host directory, which for a row archived without a push is the only copy of
	// the work (#5571 W8.5). The restore routes go through provisionForRestore.
	if adopt == nil && !replacePreserved && i.preservedWorkspaceOwed() != nil {
		return fmt.Errorf("session %q: its preserved workspace must be reattached, not replaced (#5571 W8.6)", i.Title)
	}
	// Bound the hook's lifetime to this attempt: clear it on every exit so a
	// hook registered for this call cannot survive to fire on a later call.
	// After a successful reap (below) the hook is already taken and fired, so
	// this defer gets nil — a no-op. After a reap failure the explicit clear
	// below is reached first; this defer is then also a no-op. For all
	// pre-reap returns (missing backend, bad kind, unresolvable account,
	// runtime resolution failure, config/drift check) only this defer runs.
	defer i.takeOnSandboxRetired()
	// Code the operator did not place taints the machine it is re-provisioned
	// onto, durably and before any of it runs there (#5571 W7.8).
	if err := i.recordReprovisionPlacement(); err != nil {
		return err
	}
	i.mu.RLock()
	backend := i.backend
	accountName := i.Account
	restoreBranch, unverified := i.restoreCheckpointLocked()
	spec := ProvisionSpec{
		RepoRoot:      i.Path,
		Title:         i.Title,
		Program:       i.Program,
		CloneURL:      originRemoteURL(i.Path),
		RestoreBranch: restoreBranch,
		// The replacement agent-server must land on the SAME branch the fetch
		// just materialized, not re-derive one from its own branch_prefix
		// (#5571 W2.4).
		Branch:                i.Branch,
		Host:                  i.Host,
		SessionEnvPassthrough: configuredSessionEnvPassthrough(i.sessionEnvPassthrough),
		Adopt:                 adopt,
		OperatorCreated:       i.OperatorCreated(),
	}
	if adopt != nil {
		// Nothing is cloned or fetched when adopting.
		spec.RestoreBranch = ""
	}
	i.mu.RUnlock()
	if err := i.settleRestoreCheckpoint(&spec, restoreBranch, unverified, adopt != nil, replacePreserved); err != nil {
		return err
	}
	spec.GitUserName, spec.GitUserEmail = operatorGitIdentity(spec.RepoRoot)
	if backend == nil {
		return fmt.Errorf("cannot re-provision session %q: no backend on record", i.Title)
	}
	policy, perr := agentPolicyFor(spec.RepoRoot, nil)
	if perr != nil {
		return fmt.Errorf("cannot re-provision session %q: %w", i.Title, perr)
	}
	spec.AgentPolicy = policy
	kind, err := backendKindForType(backend.Type())
	if err != nil {
		return fmt.Errorf("cannot re-provision session %q: %w", i.Title, err)
	}
	// Refuse a drifted agent BEFORE anything is reaped or provisioned (#3082) —
	// and before the account resolution below, which would answer a different
	// question first (#4430): the resolver looks the persisted account up under
	// the CURRENTLY resolved agent's registry, so a drifted config either finds
	// no account there or reports the resolved agent "does not support multiple
	// accounts" — roster answers, when the defect is that the session's
	// persisted identity no longer matches what its program will launch.
	//
	// The create path checks this; reprovision did not, so a restore or recovery
	// after program_overrides changed would rebuild the container against the same
	// account while running a different agent — the identity the account names
	// would be reported, and another one spent. Same question, same helper: two
	// places deciding "has the program changed?" separately is how they drift.
	//
	// A refusal must cost nothing: the old runtime is still live and still
	// recoverable, and reaping first would destroy it for a create that is
	// about to be refused anyway.
	// The namespace is the durable selection record, not a fresh parse of the
	// recorded enum: a cross-agent handoff can pin an account whose registry
	// differs from Program, and an override edit must not move it (#4430 review
	// round 4).
	scopedAgent := i.AccountAgent()
	if strings.TrimSpace(accountName) != "" {
		resolved, cfgErr := resolveRepoConfig(i.Path)
		if cfgErr != nil {
			return fmt.Errorf("cannot re-provision session %q: its account cannot be checked against the resolved program: %w", i.Title, cfgErr)
		}
		if err := refuseAccountAgentDrift(accountName, scopedAgent, config.ResolveProgram(&resolved.Config, i.Program)); err != nil {
			return fmt.Errorf("cannot re-provision session %q: %w", i.Title, err)
		}
	}
	// Resolve the persisted account before reaping the old sandbox. A missing or
	// cross-agent account makes the replacement unusable, and that is not a reason
	// to destroy the only runtime the session still has. The drift check above
	// already proved the resolved program's agent is the durable namespace, so
	// selection runs inside it directly — re-deriving from spec.Program's enum
	// would refuse a handoff-scoped pin whose registry is not the recorded
	// enum's (#4430 review round 4), and restore cannot drift onto ambient
	// credentials.
	if accountCarried(kind, i.Path, i.Host) && strings.TrimSpace(accountName) != "" {
		account, accountErr := selectAccountInNamespace(scopedAgent, accountName)
		if accountErr != nil {
			return fmt.Errorf("cannot re-provision session %q with its persisted account: %w", i.Title, accountErr)
		}
		spec.Account = account
		// The agent command is proven against the account BEFORE anything is
		// reaped, like the persisted-account checks around it: a program override
		// that changed since the session was created must leave the recoverable
		// runtime intact, not delete it and then refuse the replacement.
		if kind == BackendSSH {
			rc, rcErr := resolveRepoConfig(i.Path)
			if rcErr != nil {
				return fmt.Errorf("cannot re-provision session %q: its account cannot be checked against the resolved program: %w", i.Title, rcErr)
			}
			check := spec
			if verr := validateRemoteAccountProgram(rc, &check); verr != nil {
				return fmt.Errorf("cannot re-provision session %q: %w", i.Title, verr)
			}
		}
	} else if strings.TrimSpace(accountName) != "" {
		// A session recorded under an account is relaunched under THAT account or
		// not at all (#5571 W7.4). The target can no longer carry it — its host is
		// not in the global host list any more — and replacing the sandbox anyway
		// would run the session on the host's own login (or another account) while
		// the row still reports this one. Refused BEFORE anything is reaped, so the
		// old runtime stays recoverable.
		return fmt.Errorf("cannot re-provision session %q: it runs under account %q, but its target %s is not a host in the global host list, so the account cannot be synced; "+
			"re-add the host under [hosts.<name>] (it is recorded as %q) or clear the session's account, rather than restoring it under another identity",
			i.Title, accountName, string(kind), i.Host)
	}
	rt, err := ResolveRuntime(kind)
	if err != nil {
		return fmt.Errorf("cannot re-provision session %q: %w", i.Title, err)
	}
	i.mu.RLock()
	creds := i.sandboxCreds
	i.mu.RUnlock()

	if adopt == nil {
		// A failed archive/recovery can leave the old sandbox wiring live. Reap it
		// before provisioning a replacement so bindProvisionResult never discards its
		// only cleanup handle. An unknown outcome keeps the old wiring installed for a
		// real retry instead of provisioning a second sandbox on a guess.
		if err := i.reapRemoteRuntimeForReplacement(); err != nil {
			// Retirement failed: clear any registered hook so it is not inherited by a
			// later call or fired at the wrong point.
			i.takeOnSandboxRetired()
			return fmt.Errorf("cannot re-provision session %q: previous sandbox cleanup state is unknown: %w", i.Title, err)
		}
		// The old sandbox is now provably gone: fire the one-shot hook before
		// continuing. This is the earliest point from which a new Recover episode
		// can legitimately begin. Pre-reap failures are excluded because the old
		// sandbox may still be live, and firing there would let repeated pre-reap
		// failures reset the budget indefinitely.
		if fn := i.takeOnSandboxRetired(); fn != nil {
			fn()
		}
	}
	// Mint AFTER the reap, through the same helper the create path uses (#3068).
	//
	// The ordering is the load-bearing part, and it is now enforced by position
	// rather than by care: the reap above revokes the old credential on its way out
	// (resetRemoteRuntime), and it returns early — leaving the old wiring installed
	// and its credential untouched — when the outcome is unknown. So a retained,
	// possibly-live sandbox keeps working, and only a runtime that is provably gone
	// is replaced by a fresh mint. Minting before this point severed callback from
	// exactly that retained runtime, permanently and silently.
	// Mint through the same shared helper the create path uses, which DEGRADES
	// on refusal rather than failing: a restored sandbox without callback is
	// more useful than no restore at all, and the reason rides the result onto
	// the durable record so the surfaces say why instead of the agent
	// discovering af commands silently fail (#5571 W3.1).
	cred := mintForProvision(&spec, kind, creds)
	// ARMED HERE, disarmed only once the replacement is bound and live (#3065
	// review). Every exit below this line abandons a credential minted for a
	// sandbox that either does not exist or cannot be used, and unlike the create
	// path there is no session-level cleanup behind them — the old runtime was
	// already reaped above — so an abandoned credential strands a live token on
	// nothing.
	//
	// Deferred rather than repeated at each return, because repeating it is what
	// this review kept catching: provision-failure, revalidation-failure and
	// bind-failure each needed the same line, and each was added only after a
	// finding named it. A fourth exit added later gets it for free, and gets it
	// FAIL-CLOSED, which is the direction an auth credential should default in.
	replacementLive := false
	defer func() {
		if !replacementLive && cred.Granted() && creds != nil {
			creds.Revoke()
		}
	}()
	res, err := rt.Provision(spec)
	if err != nil {
		if adopt != nil && res.Backend != nil {
			// The adopting provision started a server it could not confirm stopped.
			// Record ITS identity on the row (persisted, unknown) rather than keep
			// only the archived server's: a later restore then checks the right pid
			// and a kill or the orphan sweep can reach the survivor.
			i.retainProvisionResultCleanup(res)
		}
		return fmt.Errorf("failed to re-provision sandbox for session %q: %w", i.Title, err)
	}
	// A runtime-set reason (the mid-provision loss detected before the env
	// commit, PR #5577 review) is more precise than the mint's — keep it.
	if res.CallbackUnavailable == "" {
		res.CallbackUnavailable = cred.UnavailableReason
	}
	// EVERY exit below this line disposes of the replacement through
	// discardUnusableReplacement — the same reason the credential revoke above is a
	// defer rather than a line per branch. What the exits share is not the message
	// but the RULE, and a rule each branch restates is a rule each branch can get
	// wrong: these two got it wrong in opposite directions until #3475, with the
	// bind exit retaining an unconfirmed cleanup handle and the revalidation exit
	// beside it discarding one.
	//
	// Same post-provision revalidation the create path performs — and the same
	// degrade (#5571 W3.1): the replacement is provisioned, runnable and already
	// replacing the old sandbox, so discarding it for a dead callback would trade
	// a working session for a clean failure. The dead grant is revoked for
	// tidiness, the reason overrides the mint's (it is the more specific cause),
	// and binding proceeds into a callback-less session.
	if rerr := revalidateAfterProvision(cred); rerr != nil {
		if creds != nil {
			creds.Revoke()
		}
		res.CallbackUnavailable = rerr.Error()
	}
	// The sandbox is up but its endpoint could not be wired — reap it so a bad
	// restore never leaks a container/remote.
	if bindErr := i.bindProvisionResult(res); bindErr != nil {
		if adopt != nil {
			// Reaping an adopting provision would delete the preserved workspace.
			// Keep its cleanup identity so a kill still reaps it, and say so.
			i.retainProvisionResultCleanup(res)
			return fmt.Errorf("failed to bind the reattached workspace for session %q (left as archived): %w", i.Title, bindErr)
		}
		return fmt.Errorf("failed to bind re-provisioned sandbox for session %q: %w",
			i.Title, i.discardUnusableReplacement(res, bindErr))
	}
	// The replacement is bound and live, so its credential belongs to it now. From
	// here the runtime's own lifetime owns revocation again — resetRemoteRuntime on
	// the next reap, revokeSandboxCredential on a known archive teardown failure.
	replacementLive = true
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
	i.callbackUnavailable = res.CallbackUnavailable
	// The skip list is provision evidence, installed verbatim like the callback
	// reason above: a fresh runtime that placed every declared file clears a
	// skip the old one recorded, and a reattach that provisions nothing passes
	// its prior list back in (#6337).
	i.agentConfigSkipped = append([]string(nil), res.AgentConfigSkipped...)
	i.touchLocked()
	if i.runtimeCleanupStateUnknown {
		i.runtimeCleanupStateUnknown = false
		i.touchLocked()
	}
	i.mu.Unlock()
	i.recordBoundPlacement()
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
	i.touchLocked()
	if !i.runtimeCleanupStateUnknown {
		i.runtimeCleanupStateUnknown = true
		i.touchLocked()
	}
	i.mu.Unlock()
}

// discardUnusableReplacement disposes of a freshly provisioned replacement this
// instance could not adopt, and is THE exit for every post-provision failure in
// reprovisionRemote.
//
// It is the create path's discardUnusableSandbox with the one difference that
// path cannot express: there IS an instance here, so an unconfirmed teardown has
// somewhere to put the handle. Routing the replacement path through the create
// helper is what shipped #3475 — the reap ran, the outcome came back unknown,
// and the handle was dropped anyway, because the helper had nothing to hand it
// to. The previous runtime was already reset by the reap above, so that handle
// was the last pointer to a sandbox that may still be running, and the next
// restore, finding nothing to reap, provisioned a second one beside it.
//
// The credential is deliberately NOT settled here, unlike on the create path:
// reprovisionRemote's replacementLive defer already owns that for every exit
// below the mint, and splitting one decision across two owners is the same
// duplication this function removes, in the other axis.
func (i *Instance) discardUnusableReplacement(res ProvisionResult, cause error) error {
	return reapUnusableSandbox(i.Title, res, cause, i.retainProvisionResultCleanup)
}

// resetRemoteRuntime clears the instance's remote-runtime wiring after its
// sandbox has been reaped (archive), leaving an inert record whose backend still
// classifies it as a remote sandbox (#1592 Phase 4 PR6). A subsequent restore
// rebuilds the wiring via bindProvisionResult.
func (i *Instance) resetRemoteRuntime() {
	// Clear the cache and the runtime fields together under i.mu (#1729), so a
	// concurrent AgentServer() never rebuilds against a half-cleared state.
	i.mu.Lock()
	if i.remoteClient != nil || i.runtimeTeardown != nil {
		i.touchLocked()
	}
	i.agentSrv = nil
	i.remoteClient = nil
	i.runtimeTeardown = nil
	// A confirmed reap discharged the off-box runtime this backend's immutable
	// cleanup identity points at. Clear it under the same lock so the next
	// checkpoint stages no RuntimeCleanup for a runtime af already reaped:
	// otherwise a restart's FromInstanceData would rebuild a teardown handle
	// from it and a later kill/recovery would act on a resource it no longer
	// owns (#5553 review). The unknown-outcome path returns before this point,
	// so only a provably-gone runtime reaches here. The reaper itself is the
	// provisioner's (bound to runtimeTeardown, already nilled above), not this
	// field, so clearing it does not affect a teardown still in flight.
	if provider, ok := i.backend.(runtimeCleanupProvider); ok {
		provider.clearRuntimeCleanupData()
	}
	if i.runtimeCleanupStateUnknown {
		i.runtimeCleanupStateUnknown = false
		i.touchLocked()
	}
	i.mu.Unlock()
	// THE credential's revocation point (#3068). This function is reached only when
	// a runtime was conclusively reaped — teardown succeeded, or failed in a way
	// that still proves it is gone — and never when an unknown outcome retains the
	// old wiring for a retry. So "the credential dies with the runtime it was
	// minted for" holds by construction here, instead of being a rule each of kill,
	// archive, restore and recover has to remember separately.
	//
	// It is what stops the worse of the two failure modes: a token minted for one
	// runtime surviving into its replacement. The replacement mints its own.
	//
	// Called OUTSIDE i.mu — revocation takes the daemon's registry lock, and holding
	// a session lock across another subsystem's lock is how a deadlock gets built.
	i.revokeSandboxCredential()
}

// revokeSandboxCredential drops this session's callback credential without
// touching the runtime wiring.
//
// Separate from resetRemoteRuntime because the two are not always the same
// event: an archive whose sandbox reap SUCCEEDED but whose in-sandbox REST kill
// errored leaves the runtime gone while the wiring is deliberately retained for
// the abort-to-Lost retry path. The credential is settled there; the wiring is
// not (#3065 review).
//
// Called OUTSIDE i.mu — revocation takes the daemon's registry lock, and holding
// a session lock across another subsystem's lock is how a deadlock gets built.
func (i *Instance) revokeSandboxCredential() {
	i.mu.RLock()
	creds := i.sandboxCreds
	i.mu.RUnlock()
	if creds != nil {
		creds.Revoke()
	}
}

// clearDischargedRuntimeCleanup drops the backend's immutable cleanup identity
// after a reap that answered definitively and latched but left the in-memory
// wiring in place for a retry. It is the ArchiveSandbox known-failure branch's
// analogue of resetRemoteRuntime's cleanup clear: the reap returned a known
// error (a definite answer, success or a definite failure) and latched, so
// af's teardown obligation is discharged and the cleanup identity points at a
// runtime af already reaped, but the row aborts to Lost and the in-process
// retry (reapRemoteRuntimeForReplacement) still needs the in-memory
// runtimeTeardown/remoteClient/agentSrv to re-enter the reap. Only the
// persisted identity is cleared, so a restart's FromInstanceData cannot
// rebuild a teardown for the reaped runtime and the next checkpoint stages no
// RuntimeCleanup for it (#5559 review). The unknown-outcome branch returns
// before this is reached, so only a runtime whose reap answered definitively
// and latched discharges here.
func (i *Instance) clearDischargedRuntimeCleanup() {
	i.mu.Lock()
	if provider, ok := i.backend.(runtimeCleanupProvider); ok {
		if provider.runtimeCleanupData() != nil {
			provider.clearRuntimeCleanupData()
			i.touchLocked()
		}
	}
	i.mu.Unlock()
}

// markRuntimeCleanupStateUnknown makes an indeterminate reap durable without
// changing the session's wanted/kill intent. The exact backend identity staged
// by ToInstanceData then crosses storage so a restart must retry this cleanup
// before provisioning a replacement.
func (i *Instance) markRuntimeCleanupStateUnknown() {
	i.mu.Lock()
	if !i.runtimeCleanupStateUnknown {
		i.runtimeCleanupStateUnknown = true
		i.touchLocked()
	}
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
// disposable sandbox runtime (docker/ssh/sandbox/hook) — the runtimes archive/restore
// re-provision (#1592 Phase 4 PR6/PR7).
func isSandboxBackendType(t string) bool {
	_, err := backendKindForType(t)
	return err == nil
}

// lostSandboxRecord reports whether a persisted row is a sandbox session that
// loaded INERT and Lost — the one shape whose started=false says nothing about
// whether the record is disposable. FromInstanceData skips Start() for a
// non-archived sandbox row on purpose: its container/remote did not survive the
// daemon restart and only the branch it pushed to origin did, so the row stays out
// of the poll and restore loops behind the started=false fence until an explicit
// restore re-provisions it.
//
// That makes the record the user's ONLY pointer to real work on origin, so the
// storage checkpoint treats this as a durable retention claim rather than
// never-started junk (#3422). It is scoped to the exact shape the loader produces:
// a LOCAL session that is Lost loads started=true and already survives, and a
// local row that never started is the junk the checkpoint's !Started() skip is for.
func lostSandboxRecord(data InstanceData) bool {
	// EffectiveLiveness, not data.Liveness directly, so a pre-#1195 sandbox row
	// (LivenessUnset with a legacy Status: Lost/Dead) rolls forward to LiveLost
	// exactly as FromInstanceData does on load. Reading data.Liveness here would
	// miss that row while the loader treats it as a lost sandbox record, so the
	// loaded form would release its slot and the raw ghost would still hold it —
	// the cap wedges on the very legacy row the release exists for.
	return isSandboxBackendType(data.BackendType) && EffectiveLiveness(data) == LiveLost
}

// LostSandboxRecord is the exported form of lostSandboxRecord for the raw-row
// cap accounting path (daemon/refresh_support.go's rawTaskRunHoldsSlot), which
// must mirror holdsTaskRunSlot for the row's known materialized form. A
// sandbox-backed LiveLost row always loads inert (started=false, per
// FromInstanceData's sandbox branch), so its live arm releases —
// canAutoRestoreLostSession returns false because ValidateRuntimeAction refuses
// a !Started session. LostSandboxRecord lets the raw arm reach the same verdict
// when the row ghosts and no in-memory Instance exists to consult Started.
func LostSandboxRecord(data InstanceData) bool {
	return lostSandboxRecord(data)
}

// IsSandboxBackendType is the exported form of isSandboxBackendType for the
// raw-row cap accounting path (daemon/refresh_support.go's rawTaskRunHoldsSlot).
// A sandbox-backed row routes its hold/release verdict through the loader's inert
// rewrite (see session.LoadedActivity), so the raw arm must recognize the same
// backend set the loader's sandbox branch does rather than gating on the stored
// liveness, which a row ghosted mid-run has not yet rolled forward to LiveLost.
func IsSandboxBackendType(t string) bool {
	return isSandboxBackendType(t)
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
	case "sandbox":
		return &sandboxBackend{}
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
	case "sandbox":
		return BackendSandbox, nil
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

// restoreCheckpointLocked returns the branch a replacement sandbox may fetch
// from origin, and whether that is still unverified. The displayed i.Branch is
// the NAME the session works on and, for a session created by this af, may never
// have been pushed; a runtime fetches a non-empty RestoreBranch unconditionally,
// so handing it that bare name would fail every replacement and leave the
// session Lost (#5972). A row an older af wrote carries no unpushed mark, and
// there a recorded branch could only have come from a push, so it is returned
// verified. For the marked exception the name is returned with unverified set:
// the caller asks origin, because af merely not having SEEN a push is not
// evidence there was none. The caller holds i.mu.
func (i *Instance) restoreCheckpointLocked() (branch string, unverified bool) {
	return strings.TrimSpace(i.Branch), i.branchUnpushed && strings.TrimSpace(i.Branch) != ""
}

// markBranchPushed records that origin was seen to hold the branch.
func (i *Instance) markBranchPushed() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.branchUnpushed {
		i.branchUnpushed = false
		i.touchLocked()
	}
}

// branchOnOrigin answers whether origin holds refs/heads/<branch>: true when it
// does, false only on a positive "origin lacks it", and an error for anything
// else (no URL, unreachable, auth). A var so tests need no network.
var branchOnOrigin = branchOnOriginViaGit

// lsRemoteArgs is the probe's git invocation. It carries the same
// environment-backed credential helper the session's clone installs
// (githubEnvironmentCredential), so a private origin reachable only through
// GH_TOKEN/GITHUB_TOKEN answers the probe exactly as it answered the clone — a
// plain ls-remote cannot use those variables (#6003). The helper holds variable
// NAMES only; the git child reads them from its environment.
func lsRemoteArgs(originURL, branch string) []string {
	var args []string
	if key, helper, ok := githubEnvironmentCredential(originURL); ok {
		args = append(args, "-c", key+"="+helper)
	}
	return append(args, "ls-remote", "--exit-code", "--heads", originURL, "refs/heads/"+branch)
}

func branchOnOriginViaGit(originURL, branch string) (bool, error) {
	if strings.TrimSpace(originURL) == "" {
		return false, errors.New("the repository has no origin remote")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", lsRemoteArgs(originURL, branch)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	cmd.WaitDelay = dockerWaitDelay
	out, err := cmd.CombinedOutput()
	if err == nil {
		return true, nil
	}
	var exit *exec.ExitError
	// --exit-code makes 2 mean "reached origin, no matching ref"; any other
	// failure says nothing about the ref.
	if errors.As(err, &exit) && exit.ExitCode() == 2 && ctx.Err() == nil {
		return false, nil
	}
	return false, fmt.Errorf("git ls-remote: %w: %s", err, strings.TrimSpace(string(out)))
}

// settleRestoreCheckpoint decides the branch a replacement sandbox may fetch
// (#5972), writing it to spec.RestoreBranch.
//
//   - An adoption fetches nothing, so there is no checkpoint to prove and origin is
//     not consulted: it must not be refused for an origin it never reads.
//   - A verified checkpoint (a legacy row with no unpushed mark) is used as is.
//   - An unverified one is asked of origin: present, it is fetched (and marked
//     pushed); absent, it is discarded and the replacement starts from the default
//     branch — except when replacing a preserved workspace the host proved gone,
//     where a different tree must not stand in for the session's, so it refuses
//     (W8.6); unknown stays unknown and refuses.
func (i *Instance) settleRestoreCheckpoint(spec *ProvisionSpec, restoreBranch string, unverified, adopting, replacePreserved bool) error {
	if !unverified || adopting {
		return nil
	}
	// af never observed this branch being pushed, but a lost response or a crash
	// mid-push leaves exactly that picture with the branch on origin. Ask origin
	// before discarding it (#5972).
	verdict, err := branchOnOrigin(spec.CloneURL, restoreBranch)
	switch {
	case err != nil:
		// Unknown stays unknown: not discarded, not fetched blindly.
		return fmt.Errorf("cannot re-provision session %q: af could not check whether its branch %q is on origin (%v); nothing was changed — fix access to the repository's origin from this machine and retry, or push the branch from the session's host", i.Title, restoreBranch, err)
	case verdict:
		i.markBranchPushed()
	case replacePreserved:
		return fmt.Errorf("cannot restore session %q: its preserved workspace is gone from its host and its branch %q is not on origin, so there is nothing to restore it from; nothing was changed. The work existed only in that workspace; to start over under this name, remove the row (af sessions kill %q) and create a new session", i.Title, restoreBranch, i.Title)
	default:
		restoreBranch = ""
	}
	spec.RestoreBranch = restoreBranch
	return nil
}
