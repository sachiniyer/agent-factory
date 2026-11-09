package daemon

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/proctree"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/session"
)

type controlServer struct {
	// createSessionFn replaces createSession in the sandbox-scoped CreateSession
	// handler. Nil in production; a test sets it to exercise the response boundary
	// on a successful create without provisioning a host.
	createSessionFn func(ctx context.Context, req CreateSessionRequest, resp *CreateSessionResponse) error
	manager         *Manager
	scheduler       *taskScheduler
	watchers        *watcherSupervisor
	shutdownCh      chan struct{}
	// shutdownOnce is shared by every per-connection controlServer
	// startControlServer builds: one Shutdown must fire once for the whole
	// listener, not once per connection. It is nil on servers that cannot shut
	// anything down (the HTTP server, most tests) — Shutdown returns before it
	// is read.
	shutdownOnce *sync.Once
	httpRequests *httpRequestDrain
	// requester is the kernel-verified identity of the process on the other
	// end of THIS connection: the SO_PEERCRED/LOCAL_PEERPID pid, resolved to
	// its (pid, start-stamp) instance by the accept loop while the peer
	// provably still exists and holds the socket. Resolving at accept — not
	// in the handler — is what keeps a peer that exited and had its pid
	// recycled mid-request from lending a stranger the exemption (#5182).
	// Teardown handlers register it so the reaper exempts the caller still
	// blocked on their reply. nil on read failure (the pre-#5182 posture).
	// The HTTP server shares ONE controlServer across connections, so it
	// leaves this nil and the per-request context carries the same identity
	// instead (see httpPeerRequesterContextKey).
	requester *proctree.Process
	// pendingReplies holds this connection's requester unregistrations until
	// the reply they belong to is written — each is keyed by its call's reply
	// pointer, which gobServerCodec.WriteResponse hands back as the body it
	// just serialized, so only that call's response releases its exemption
	// (Close drains whatever a dead connection leaves — see
	// trackTeardownRequester, #5182). nil on the shared HTTP controlServer,
	// where the per-request context drains instead.
	pendingReplies *pendingUntracks
}

const (
	taskAddCommittedErrorPrefix    = "task add committed, but failed to reload task schedules:"
	taskUpdateCommittedErrorPrefix = "task update committed, but failed to reload task schedules:"
	taskRemoveCommittedErrorPrefix = "task removal committed, but failed to reload task schedules:"
)

func (s *controlServer) Ping(_ PingRequest, resp *PingResponse) error {
	resp.OK = true
	resp.AccountHandoff = true
	resp.HostPick = true
	resp.DeliverHostPick = true
	resp.TaskHost = true
	resp.ConfigHostsEdit = true
	resp.Version = Version()
	resp.PID = os.Getpid()
	if s.manager != nil && s.manager.lifecycle != nil {
		state := s.manager.lifecycle.snapshot()
		resp.BootID = state.bootID
		resp.TransactionID = state.transactionID
		resp.Phase = state.phase
		resp.Listeners = state.listeners
	}
	resp.RefusedHosts = hostRefusals.snapshot()
	if s.manager != nil && s.manager.cfg != nil {
		resp.BootConfig = &DaemonBootConfig{
			ListenAddr:           s.manager.cfg.ListenAddr,
			RequireToken:         s.manager.cfg.RequireToken,
			RequireLoopbackToken: s.manager.cfg.RequireLoopbackToken,
		}
		// The #5183 upgrade-probation floor is enforcement OUTSIDE the file:
		// while it holds, the control listener's gate demands the bearer token
		// even though the loaded config reads tokenless. Report what the socket
		// enforces — otherwise status and doctor would call a floored socket
		// unauthenticated. Read from the same paired publication the gate reads
		// (authPosturePair), never a separate flag.
		if s.manager.tokenFloorArmed() {
			resp.BootConfig.RequireToken = true
		}
	}
	return nil
}

// ReleaseUpgradeProbation lifts this daemon's upgrade probation once its
// previous-binary supervisor has validated it (#2212 R2). A release for a
// non-matching or non-probationary transaction is refused; one that arrives
// while the instance restore is still running is accepted and takes effect when
// the restore completes (#5693). It is deliberately NOT
// gated on requireMutationAdmission — probation is exactly what it lifts, so
// gating it on admission would deadlock. (Authority rests on the control socket
// being owner-only and already privileged, NOT on the transaction id being
// secret: Ping reports that id, `af daemon status` prints it, and it rides argv
// visible in /proc — it is an identity check, never a capability.)
func (s *controlServer) ReleaseUpgradeProbation(req ReleaseUpgradeProbationRequest, resp *ReleaseUpgradeProbationResponse) error {
	if s.manager == nil || s.manager.lifecycle == nil {
		return fmt.Errorf("daemon has no lifecycle to release from upgrade probation")
	}
	if err := s.manager.lifecycle.releaseUpgradeProbation(req.TransactionID); err != nil {
		return err
	}
	resp.OK = true
	return nil
}

// PauseStatusPoll pauses the daemon's capture-pane liveness poll for one
// attached session (#1160). Deliberately NOT gated on requireManagerReady:
// it is a lightweight, lease-bounded map write on the dedicated pausedMu (not
// m.mu), independent of the instance restore — a pause that lands during
// warm-up is honored once the instance is restored, and it can never corrupt
// state the way a create/kill racing the restore could. A nil manager (some
// test control servers) is a no-op ack. Upgrade probation is different: every
// mutation is closed there, so requireMutationAdmission still applies.
func (s *controlServer) PauseStatusPoll(req PauseStatusPollRequest, resp *PauseStatusPollResponse) error {
	if err := s.requireMutationAdmission(); err != nil {
		return err
	}
	if s.manager != nil {
		s.manager.PauseStatusPollFor(req.RepoID, req.Title, req.ID, req.Holder)
	}
	resp.OK = true
	return nil
}

// ResumeStatusPoll clears a pause set by PauseStatusPoll (#1160). Same restore
// independence and probation gate as PauseStatusPoll.
func (s *controlServer) ResumeStatusPoll(req ResumeStatusPollRequest, resp *ResumeStatusPollResponse) error {
	if err := s.requireMutationAdmission(); err != nil {
		return err
	}
	if s.manager != nil {
		s.manager.ResumeStatusPollFor(req.RepoID, req.Title, req.ID, req.Holder)
	}
	resp.OK = true
	return nil
}

// reloadTaskSchedules re-arms the daemon's cron scheduler and watcher
// supervisor from tasks.json. It is the ReloadTasks poke's own entry point, and
// the only FULL watcher re-arm reachable over the control plane: the task CRUD
// RPCs (Add/Update/RemoveTask) share the locked half below but scope it to the
// row they wrote, so one daemon call still owns both steps and no separate
// ReloadTasks poke is needed for CRUD (#3837). The steps are not transactional:
// persistence happens first, and update callers receive a machine-readable
// committed outcome if the refresh fails.
//
// During warm-up (#829) the scheduler and watcher supervisor have not started
// yet; RunDaemon reloads both from tasks.json right after the restore completes,
// so a change just written is picked up then. It returns nil (nothing to
// reload) instead of erroring — the write is already durable.
func (s *controlServer) reloadTaskSchedules() error {
	unlock, err := s.lockTaskControl()
	if err != nil {
		return err
	}
	defer unlock()
	return s.reloadTaskSchedulesLocked(everyWatchTask())
}

func (s *controlServer) lockTaskControl() (func(), error) {
	if s.scheduler == nil {
		return nil, fmt.Errorf("this daemon does not host a task scheduler")
	}
	s.scheduler.controlMu.Lock()
	return s.scheduler.controlMu.Unlock, nil
}

// scope is how much of the watcher supervisor this refresh may touch: the CRUD
// callers name the one task they wrote, ReloadTasks and daemon start pass the
// full re-arm. See watchScope (#3837).
func (s *controlServer) reloadTaskSchedulesLocked(scope watchScope) error {
	if s.scheduler == nil {
		return fmt.Errorf("this daemon does not host a task scheduler")
	}
	if s.manager != nil && !s.manager.Ready() {
		return nil
	}
	if s.manager != nil {
		refused, err := reloadTaskAutomation(s.manager, s.scheduler, s.watchers, scope)
		if err != nil {
			return err
		}
		for _, refusal := range refused {
			log.WarningLog.Printf("task automation reload: %v", refusal)
		}
		return nil
	}
	if err := s.scheduler.Reload(); err != nil {
		return err
	}
	// Nil only in tests that exercise the scheduler alone.
	if s.watchers != nil {
		return s.watchers.ReloadScoped(scope)
	}
	return nil
}

func (s *controlServer) ReloadTasks(_ ReloadTasksRequest, resp *ReloadTasksResponse) error {
	if err := s.requireMutationAdmission(); err != nil {
		return err
	}
	if err := s.reloadTaskSchedules(); err != nil {
		return err
	}
	resp.OK = true
	return nil
}

// GetConfig returns the config manifest zipped with the user's live values: the
// single description of config that the web editor renders its form from, and
// the same one config.ManifestWithValues hands the TUI in-process.
//
// Like ListTasks, it is deliberately NOT gated on requireManagerReady: config
// lives on disk and is read fresh here, so the answer is safe and current even
// while the daemon is warming up. Reading it fresh (rather than returning the
// manager's startup snapshot, manager.cfg) is the deliberate choice — the
// snapshot is what the daemon is RUNNING, while the file is what the user is
// EDITING. An editor must show the file, or a value edited twice in one session
// would appear to revert.
//
// The gap between those two is exactly why every entry carries RequiresRestart.
//
// A RepoPath read (#7009) returns the project's effective values with scope
// markers instead. A project that will not resolve degrades rather than fails:
// the editor still opens on the global rows it can edit, and ProjectError says
// why the markers are missing — a refused read would leave the user unable to
// edit anything because one selected project went away.
func (s *controlServer) GetConfig(req GetConfigRequest, resp *GetConfigResponse) error {
	cfg, err := config.LoadConfig()
	if err != nil {
		return fmt.Errorf("cannot read config: %w", err)
	}
	configDir, err := config.GetConfigDir()
	if err != nil {
		return err
	}
	resp.Path = filepath.Join(configDir, config.TomlConfigFileName)
	if req.RepoPath == "" {
		resp.Entries = config.ManifestWithValues(cfg)
		return nil
	}
	entries, root, err := projectEditorEntries(req.RepoPath, cfg)
	if err != nil {
		resp.Entries = config.ManifestWithValues(cfg)
		resp.ProjectError = config.ProjectScopeError(req.RepoPath, err)
		return nil
	}
	resp.Entries = entries
	resp.ProjectRoot = root
	return nil
}

// projectEditorEntries resolves a caller-supplied project path on this host and
// reads its editor rows against the global snapshot GetConfig already loaded,
// so the markers and global_value describe the same generation as Path.
func projectEditorEntries(requested string, global *config.Config) ([]config.ConfigEntry, string, error) {
	repo, err := configProjectFromRequest(requested)
	if err != nil {
		return nil, "", err
	}
	return config.EditorEntriesForRepo(repo, global)
}

// configProjectFromRequest turns a config read's repo_path into the repository
// it names. The path is normalized the way every RPC-boundary path is — the
// daemon has no caller cwd, so a relative path is refused rather than resolved
// against the daemon's own (#4821).
func configProjectFromRequest(requested string) (*config.RepoContext, error) {
	path, err := config.ResolveDaemonHostPath(requested)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", config.ErrProjectPathUnusable, err)
	}
	return config.RepoFromPath(path)
}

// ExplainConfig returns one global config key's provenance — the same
// config.ResolvedValue `af config get <key> --explain` prints — so the TUI and
// web config surfaces answer "which layer supplied this value" through the
// identical resolver rather than growing a second precedence implementation
// (#4803, the UI half of #2216's explain ask).
//
// Like GetConfig it is deliberately NOT gated on requireManagerReady: the trace
// is resolved from the on-disk sources fresh per call, so it describes the file
// as it is now — including a hand-edit made since the daemon started — and the
// answer stays available while the daemon warms up. That is also why
// RunningValueChecked reports false: the explanation is about the FILE the user
// edits, the same contract the CLI prints ("running daemon value not checked").
//
// A RepoPath explain that cannot resolve the project is refused, unlike
// GetConfig's degrade: the rows can honestly fall back to global values under a
// banner, but a global trace answering a project question would be a wrong
// answer presented as the right one.
func (s *controlServer) ExplainConfig(req ExplainConfigRequest, resp *ExplainConfigResponse) error {
	if req.RepoPath == "" {
		value, err := config.ExplainGlobalValue(req.Key)
		if err != nil {
			return err
		}
		resp.Explanation = value
		resp.Scope = "global"
		return nil
	}
	repo, err := configProjectFromRequest(req.RepoPath)
	if err != nil {
		return errors.New(config.ProjectScopeError(req.RepoPath, err))
	}
	value, err := config.ExplainRepoValue(repo, req.Key)
	if err != nil {
		// An unknown key is the caller's own input and safe to echo; anything
		// else came out of the resolver and may name host paths.
		if errors.Is(err, config.ErrUnknownConfigKey) {
			return err
		}
		return errors.New(config.ProjectScopeError(req.RepoPath, err))
	}
	resp.Explanation = value
	resp.Scope = "project"
	return nil
}

// SetConfigValue writes one config key on the caller's behalf, through
// config.SetGlobalConfigValue — the identical validated, file-locked, atomic
// path a daemonless `af config set` uses. The daemon adds nothing: no second
// validator, no second writer, no reordering.
//
// Since #3231 this is the one write path every first-class surface routes
// through while a daemon is running — the web form posts it, and `af config
// set` and the TUI editor call it over the control socket — so the mutation
// admission gate above answers "is a config write admissible right now"
// identically for all of them, and a refusal lands before anything reaches
// disk. The daemon does not own config.toml (see the control_types.go note);
// the file lock inside SetGlobalConfigValue is what makes this safe against a
// concurrent hand-edit or daemonless CLI write, so this method must not
// pre-read, cache, or merge anything around it.
//
// Errors (unknown key, invalid value) propagate verbatim so the web form shows
// the validator's own message, which is the one the CLI prints.
func (s *controlServer) SetConfigValue(req SetConfigValueRequest, resp *SetConfigValueResponse) error {
	if err := s.requireMutationAdmission(); err != nil {
		return err
	}
	result, wroteDigest, err := writeGlobalConfig(req)
	if err != nil {
		return err
	}
	resp.Result = result
	// Apply the write to the running daemon in place (#2480) so the web form need
	// not tell the user to restart for a hot-reloadable key. Best-effort: the write
	// already succeeded on disk; report a live apply failure separately so the
	// operator knows the running daemon has not adopted the saved config.
	// A nil manager is the in-daemon "no daemon was reached" answer; it is stated
	// explicitly because an unset DaemonApply reports unknown, not no-daemon
	// (#4482).
	outcome := config.ApplyOutcome{DaemonApply: config.DaemonApplyNotReached}
	if s.manager != nil {
		if applied, aerr := s.manager.ApplyConfig(); aerr == nil {
			resp.Applied = applied.Applied
			resp.Pending = applied.Pending
			resp.Warnings = applied.Warnings
			outcome = config.ApplyOutcome{DaemonApply: config.DaemonApplyApplied, FailedListenerKeys: applied.FailedListenerKeys}
			// A successful apply claims "applied" only if the daemon loaded THIS
			// save's file: a competing write can land between the writer's
			// file-lock release and the apply's load (#4247). Both digests are
			// taken in this process — one by the write, one by the apply's own
			// load — so the comparison needs nothing read back.
			confirmSavedConfigDigest(&outcome, &resp.Warnings, wroteDigest, applied.Digest)
		} else {
			resp.Warnings = append(resp.Warnings, "saved config, but live apply failed: "+aerr.Error())
			outcome.DaemonApply = config.DaemonApplyFailed
		}
	}
	resp.Warnings = completeConfigSaveWarnings(outcome, result.Warnings, resp.Warnings)
	resp.ApplyOutcome = outcome.StatusForKey(result.Key)
	// The per-key effect notice (#2480). A socket key (network.listen_addr /
	// network.preview_listen_addr, #2480 PR2) whose live rebind FAILED did not apply —
	// the daemon kept the old listener — so it must not be reported as applied; the
	// address + reason ride in resp.Warnings. That used to be an override branch
	// right here, which is how the unset surfaces came to be written without it
	// (#3397); passing the whole outcome moves the decision into config.EffectNotice,
	// which is the only place it is now made.
	resp.RestartNotice = config.EffectNotice(result.Key, outcome)
	// Where the daemon is accepting NOW for a listener key (#3722) — read after the
	// apply, so it reports the rebind's actual outcome rather than the request's
	// intent. For a listener key this is the reply's most actionable field: the
	// caller may well have been talking over the listener it just moved.
	if s.manager != nil {
		resp.ListenerAddr = s.manager.ListenerAddress(result.Key)
	}
	return nil
}

// ApplyConfig makes the running daemon reflect the on-disk global config in place
// (#2480), so a config write does not require a manual restart to take effect.
// Reports which changed keys are live vs pending. Since #3231 the first-class
// clients route their writes through SetConfigValue, which applies internally;
// this remains for older CLIs that write the file themselves and poke it after.
//
// Gated on mutation admission like SetConfigValue (#3231): an apply is not
// inert — it swaps the live config, rebinds listeners, and changes auth
// posture — so during upgrade probation it would mutate the very posture the
// supervisor is validating, and during quiescing it would race the hand-off.
// Ordinary warm-up is still admitted (mutationAdmissionError allows it), so a
// write during a normal start keeps applying live.
func (s *controlServer) ApplyConfig(_ ApplyConfigRequest, resp *ApplyConfigResponse) error {
	if err := s.requireMutationAdmission(); err != nil {
		return err
	}
	if s.manager == nil {
		return nil
	}
	result, err := s.manager.ApplyConfig()
	if err != nil {
		return err
	}
	resp.Applied = result.Applied
	resp.Pending = result.Pending
	resp.Warnings = result.Warnings
	resp.FailedListenerKeys = result.FailedListenerKeys
	return nil
}

// Shutdown acknowledges a request to terminate the daemon, then asynchronously
// signals the main loop to tear down after a short grace period. The grace
// lets the RPC response flush back to the caller before the listener closes.
func (s *controlServer) Shutdown(_ ShutdownRequest, resp *ShutdownResponse) error {
	resp.OK = true
	resp.PID = os.Getpid()
	if s.shutdownCh == nil || s.shutdownOnce == nil {
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

// requireMutationAdmission is the probation gate for mutations that are safe
// during ordinary restore (task/config writes and poll leases). A candidate
// blocks them from socket bind onward so rollback can restore one coherent
// metadata snapshot.
func (s *controlServer) requireMutationAdmission() error {
	if s.manager == nil || s.manager.lifecycle == nil {
		return nil
	}
	return s.manager.lifecycle.mutationAdmissionError()
}

// requireStateMutationAdmission composes the existing restored-state barrier
// with upgrade probation. Every session mutation uses this one entrypoint, so
// neither phase can accidentally authorize what the other refuses.
func (s *controlServer) requireStateMutationAdmission() error {
	if err := s.requireManagerReady(); err != nil {
		return err
	}
	return s.requireMutationAdmission()
}

// CreateSession is the net/rpc entrypoint. net/rpc gives no per-call context, so
// it passes Background; the create is still bounded by WaitForReady's internal
// timeout and torn down when this returns. The HTTP route wires the request
// context through createSession so a web/API client disconnect cancels the poll.
func (s *controlServer) CreateSession(req CreateSessionRequest, resp *CreateSessionResponse) error {
	return s.createSession(context.Background(), req, resp)
}

func (s *controlServer) createSession(ctx context.Context, req CreateSessionRequest, resp *CreateSessionResponse) (retErr error) {
	managerDelegated := false
	defer func() {
		if !managerDelegated {
			retErr = taskPreflightError(req.TaskOrigin, req.TaskID, req.TaskRepoID, retErr)
		}
	}()

	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	// Reject a program that runs no recognized agent BEFORE the create proceeds —
	// the one boundary a raw RPC caller can reach without the upstream enum
	// validation every shipped client applies (see validateCreateProgram for why
	// this gate is at the RPC boundary, not Manager.CreateSession, which the
	// root-agent ensure loop calls with command strings that must NOT be trimmed).
	// trimProgramEnumIfBare normalizes only a bare enum with surrounding whitespace
	// into Instance.Program (the form the opaque same-target guard and
	// ResolveProgram's exact program_overrides lookup care about); it preserves
	// arbitrary command strings verbatim so an intentional trailing space survives.
	if err := validateCreateProgram(req.Program); err != nil {
		return err
	}
	// A remote client sends the project path as typed; expand ~ against this daemon's
	// home before anything resolves it. A still-relative path would resolve against
	// the DAEMON's working directory and could silently pick a different checkout (#5743).
	if strings.TrimSpace(req.RepoPath) != "" {
		resolved, err := config.ResolveDaemonHostPath(req.RepoPath)
		if err != nil {
			return err
		}
		req.RepoPath = resolved
	}
	req.Account = strings.TrimSpace(req.Account) // mirror trimProgramEnumIfBare (#5123)
	if req.ValidateFirst {
		if err := validateCreateFirst(req); err != nil {
			return err
		}
	}
	req.Program = trimProgramEnumIfBare(req.Program)
	managerDelegated = true
	data, err := s.manager.CreateSession(ctx, req)
	// A committed retained create (#3233) lands in the envelope with the
	// retained projection in Instance; a genuine failure returns unchanged.
	if !resp.record(err) {
		return err
	}
	resp.Instance = data.ForClients()
	return nil
}

// SpawnConfigAgent starts the config agent and returns its tmux session name.
// net/rpc gives no per-call context, so it passes Background; the spawn is still
// bounded by the readiness timeout inside WaitForReadyOn, and any failure tears
// the session down before returning.
func (s *controlServer) SpawnConfigAgent(req SpawnConfigAgentRequest, resp *SpawnConfigAgentResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	name, socketPath, err := s.manager.SpawnConfigAgent(context.Background(), req)
	if err != nil {
		return err
	}
	resp.SessionName = name
	resp.SocketPath = socketPath
	return nil
}

// ReapConfigAgent tears down a config-agent session. No event is published: a
// config agent is not a session, so nothing on the events plane models it.
func (s *controlServer) ReapConfigAgent(req ReapConfigAgentRequest, _ *ReapConfigAgentResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	return s.manager.ReapConfigAgent(req)
}

func (s *controlServer) CreateTab(req CreateTabRequest, resp *CreateTabResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	created, err := s.manager.CreateTab(req)
	// Assign before recording so the envelope survives the copy: a committed
	// outcome (#3237) carries the minted identity in the response; a genuine
	// failure returns unchanged (net/rpc sends no reply body on error).
	*resp = created
	if !resp.record(err) {
		return err
	}
	return nil
}

func (s *controlServer) CloseTab(req CloseTabRequest, resp *CloseTabResponse) error {
	return s.closeTab(context.Background(), req, resp)
}

func (s *controlServer) closeTab(ctx context.Context, req CloseTabRequest, resp *CloseTabResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	name, err := s.manager.closeTabRequestedBy(req, rpcRequester(ctx))
	if err != nil {
		return err
	}
	resp.Name = name
	return nil
}

func (s *controlServer) RenameTab(req RenameTabRequest, resp *RenameTabResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	name, err := s.manager.RenameTab(req)
	if err != nil {
		return err
	}
	resp.Name = name
	return nil
}

func (s *controlServer) ReorderTab(req ReorderTabRequest, resp *ReorderTabResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	name, index, err := s.manager.ReorderTab(req)
	if err != nil {
		return err
	}
	resp.Name = name
	resp.Index = index
	return nil
}

func (s *controlServer) KillSession(req *KillSessionRequest, resp *KillSessionResponse) error {
	return s.killSession(context.Background(), req, resp)
}

func (s *controlServer) killSession(ctx context.Context, req *KillSessionRequest, resp *KillSessionResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	// A caller tearing down its OWN session is still blocked on this reply
	// inside the pane tree being reaped; register the kernel-verified
	// requester so teardown spares it (#5182). Unregistered on return — the
	// reply is in flight and the exemption is over.
	defer s.trackTeardownRequester(ctx, req)()
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	// KillSession resolves the target (id-first, erroring on a stale/missing id)
	// and returns the stable identity it ACTUALLY killed — so the event names that
	// exact session, never the request's own id, which under a cross-repo title
	// collision could point at a different (or gone) session (#1592 Phase 5 PR5 +
	// follow-up: the write-path analogue of the id-keyed read/stream paths).
	// The HEAD is read before teardown removes the worktree (#6233).
	kept := s.manager.keptBranchBeforeKill(*req)
	killed, err := s.manager.killSessionRequestedBy(*req, rpcRequester(ctx), nil)
	resp.KeptBranch = kept
	if !resp.record(err) {
		return err
	}
	resp.OK = true
	// Publish only when the kill actually completed. A committed-but-unfinished
	// kill (err recorded above) retains its tombstoned row, and session.killed
	// means the durable row and authoritative map entry disappeared — that event
	// is published by finishUserKill/the late ghost worker when the row actually
	// goes (#3234).
	if err == nil {
		s.manager.publishEvent(agentproto.EventSessionKilled, session.InstanceData{ID: killed.ID, Title: killed.Title})
	}
	return nil
}

func (s *controlServer) ArchiveSession(req *ArchiveSessionRequest, resp *ArchiveSessionResponse) error {
	return s.archiveSession(context.Background(), req, resp)
}

func (s *controlServer) RestoreArchived(req RestoreArchivedRequest, resp *RestoreArchivedResponse) error {
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	worktreePath, restored, err := s.manager.RestoreArchived(req)
	if !resp.record(err) {
		return err
	}
	resp.OK = true
	resp.WorktreePath = worktreePath
	// Publish the identity the manager actually resolved, not the request's
	// potentially stale title/repo pair. This is the same one-shot resolution the
	// restore body used, so the event cannot name a same-title sibling.
	// Publish only when the restore actually completed. A committed-but-unfinished
	// restore (err recorded above, a mutationCommittedError) did NOT restore the
	// session — its own marker tells the caller "the session is NOT restored; retry
	// the restore" — so session.restored, which means the synchronous restore call
	// completed, must not fire. Unlike killSession there is no late worker that owns
	// this event, so an abandoned committed-but-unfinished restore yields zero
	// restored events — correct, because the operation did not complete.
	if err == nil {
		s.manager.publishEvent(agentproto.EventSessionRestored, restored)
	}
	return nil
}

func (s *controlServer) RestoreSession(req RestoreSessionRequest, resp *RestoreSessionResponse) error {
	if req.ExpectedDaemonBootID != "" {
		if s.manager == nil || s.manager.lifecycle == nil {
			return fmt.Errorf("cannot verify the daemon process for restore admission")
		}
		if actual := s.manager.lifecycle.snapshot().bootID; actual != req.ExpectedDaemonBootID {
			return fmt.Errorf("daemon process changed before restore admission; refresh and retry")
		}
	}
	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	if err := validateRPCRepoID(req.RepoID); err != nil {
		return err
	}
	worktreePath, restored, err := s.manager.RestoreSession(req)
	if !resp.record(err) {
		return err
	}
	resp.OK = true
	resp.WorktreePath = worktreePath
	// Publish only when the restore actually completed. A committed-but-unfinished
	// restore (err recorded above, a mutationCommittedError) did NOT restore the
	// session — its own marker tells the caller "the session is NOT restored; retry
	// the restore" — so session.restored, which means the synchronous restore call
	// completed, must not fire. Unlike killSession there is no late worker that owns
	// this event, so an abandoned committed-but-unfinished restore yields zero
	// restored events — correct, because the operation did not complete. The auto
	// Lost-restore loop's EndRecoverFence publishes session.updated with the true Lost
	// liveness (restore.go) as the runtime-status substitute.
	if err == nil {
		s.manager.publishEvent(agentproto.EventSessionRestored, restored)
	}
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

func (s *controlServer) DeliverPrompt(req DeliverPromptRequest, resp *DeliverPromptResponse) (retErr error) {
	managerDelegated := false
	defer func() {
		if !managerDelegated {
			retErr = taskPreflightError(req.TaskOrigin, "", req.TaskRepoID, retErr)
		}
	}()

	if err := s.requireStateMutationAdmission(); err != nil {
		return err
	}
	managerDelegated = true
	status, deliveryStatus, promptRetained, err := s.manager.deliverPromptWithOutcome(req)
	// A committed retained auto-create (#3357) — the absent-target branch
	// routed through Manager.CreateSession, which provisioned a sandbox but
	// could not confirm its teardown/startup — lands in the envelope so the
	// marker survives the net/rpc hop that would otherwise flatten it to a
	// plain rpc.ServerError string. A genuine failure returns unchanged.
	if !resp.record(err) {
		return err
	}
	resp.Status = status
	resp.DeliveryStatus = deliveryStatus
	resp.PromptRetained = promptRetained
	return nil
}
