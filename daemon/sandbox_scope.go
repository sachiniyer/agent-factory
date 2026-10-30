package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// The sandbox credential's project-bounded scope (#5571 W3.5).
//
// A sandbox session's in-sandbox `af` gets the verbs a local agent's af has,
// bounded to the PROJECT its session belongs to. The route table
// (HTTPRoute.sandboxAllowed) only admits a path; every handler below also
// enforces its constraint from sandboxOwner(ctx), and sandboxConstrainedRoutes
// names each one so a test pins both directions.
//
// Two properties hold for everything admitted:
//
//   - Project bound. The target is resolved the way the unscoped handler would
//     resolve it, then required to live in the owner's project (the repo ID of the
//     owner's own row). The sandbox never names a repo: its request's repo_id and
//     repo_path are replaced by the owner's, because a sandbox does not know (and
//     must not learn) the daemon host's layout.
//   - Nothing executes on the daemon box. Prompts and archives are accepted only
//     for sessions that themselves run off-box; created sessions go to a listed
//     host; tasks must deliver a prompt to such a session. Watch tasks (whose
//     scripts run on the daemon host), other accounts, and local sessions are out.
//
// The operator (unix socket, operator token) carries no sandbox owner and is
// delegated to the unscoped handler untouched.

var (
	errSandboxOutOfScope = errors.New("not permitted for a sandbox credential")
)

// sandboxRefusal is the one constructor of a refusal for a sandbox credential. Its
// text is a CONSTANT, never formatted: a refusal that reflected what the caller
// sent (a backend, a title, a path) would hand the caller a probe of the daemon
// host's layout, because the response boundary withholds layout strings and the
// reflected guess then comes back withheld only if it is a real one (#6014). The
// constant texts are registered as they are used, which is how the response
// boundary tells a typed refusal from any other error (see sandboxRefusalText).
func sandboxRefusal(text string) error {
	sandboxRefusalTexts.Store(text, struct{}{})
	return fmt.Errorf("%w: %s", errSandboxOutOfScope, text)
}

// sandboxRefusalTexts is the closed set of refusal texts sandboxRefusal has been
// given. Bounded: every call site passes a constant.
var sandboxRefusalTexts sync.Map

// sandboxRefusalMessage reports whether msg is exactly a typed refusal.
func sandboxRefusalMessage(msg string) bool {
	text, ok := strings.CutPrefix(msg, errSandboxOutOfScope.Error()+": ")
	if !ok {
		return false
	}
	// A refusal raised before a send carries the not-delivered marker (a pre-flight
	// tag, constant text) after it.
	text = strings.TrimSuffix(text, "; "+notDeliveredMarker)
	_, known := sandboxRefusalTexts.Load(text)
	return known
}

// instanceBackendType is the persisted backend type of a tracked session. A
// package var so a test can place sessions on a backend without provisioning one.
var instanceBackendType = func(i *session.Instance) string { return i.ToInstanceData().BackendType }

// offBoxBackendType reports whether a persisted backend type runs its workspace
// off the daemon box.
func offBoxBackendType(t string) bool {
	switch t {
	case "ssh", "sandbox", "docker", "remote":
		return true
	}
	return false
}

// sandboxProject is the project a credential owner's session belongs to.
type sandboxProject struct {
	repoID string
	path   string
}

// sandboxProjectOf finds the owner's own tracked row and returns its project.
func (m *Manager) sandboxProjectOf(owner string) (sandboxProject, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	inst, rid := m.trackedSessionByIDLocked(owner)
	if inst == nil {
		return sandboxProject{}, sandboxRefusal("the credential's session is no longer tracked")
	}
	return sandboxProject{repoID: rid, path: inst.Path}, nil
}

// authorizeSandboxSession resolves the session a sandbox request names and
// returns its canonical identity, refusing anything outside the owner's project,
// anything not running off-box, and (selfOnly) anything but the owner itself.
func (m *Manager) authorizeSandboxSession(owner, id, title string, selfOnly bool) (sandboxProject, *session.Instance, error) {
	proj, err := m.sandboxProjectOf(owner)
	if err != nil {
		return sandboxProject{}, nil, err
	}
	if id == "" && strings.TrimSpace(title) == "" {
		// A request naming nothing means "this session" to the in-sandbox CLI
		// (whoami, archive --self). Never "any".
		id = owner
	}
	// ONE answer for everything outside the credential's scope: a session that
	// does not exist, one in another project, one that runs on the daemon host,
	// an ambiguous title. A different message (or a different "not found") per
	// case would be an existence oracle across projects.
	unavailable := func() error {
		return sandboxRefusal("that session is not available to this credential")
	}
	inst, repoID, _, _, _, err := m.resolveActionSession(id, title, proj.repoID)
	if err != nil || inst == nil || repoID != proj.repoID {
		return sandboxProject{}, nil, unavailable()
	}
	if selfOnly && inst.ID != owner {
		return sandboxProject{}, nil, sandboxRefusal("this verb acts on the credential's own session only")
	}
	if !offBoxBackendType(instanceBackendType(inst)) {
		return sandboxProject{}, nil, unavailable()
	}
	return proj, inst, nil
}

// --- session verbs -----------------------------------------------------------

// testHookSandboxAfterAuthorize runs between a scoped session verb's
// authorization and its action, so a test can interleave a replacement or rebind
// there. Nil in production.
var testHookSandboxAfterAuthorize func()

func (s *controlServer) sendPromptScoped(ctx context.Context, req SendPromptRequest, resp *SendPromptResponse) error {
	owner, ok := sandboxOwner(ctx)
	if !ok {
		return s.SendPrompt(req, resp)
	}
	proj, inst, err := s.manager.authorizeSandboxSession(owner, req.ID, req.Title, false)
	if err != nil {
		return err
	}
	req.ID, req.Title, req.RepoID = inst.ID, inst.Title, proj.repoID
	req.TaskOrigin = false
	if testHookSandboxAfterAuthorize != nil {
		testHookSandboxAfterAuthorize()
	}
	// Addressed by the authorized session's ID, never re-resolved by title, and
	// re-validated under the target's op lock on the instance the send reaches:
	// authorization is atomic with the action.
	return s.sendPrompt(req, resp, s.manager.sandboxSessionGuard(inst.ID, proj.repoID))
}

func (s *controlServer) archiveSessionScoped(ctx context.Context, req *ArchiveSessionRequest, resp *ArchiveSessionResponse) error {
	owner, ok := sandboxOwner(ctx)
	if !ok {
		return s.archiveSession(ctx, req, resp)
	}
	proj, inst, err := s.manager.authorizeSandboxSession(owner, req.ID, req.Title, false)
	if err != nil {
		return err
	}
	req.ID, req.Title, req.RepoID = inst.ID, inst.Title, proj.repoID
	if testHookSandboxAfterAuthorize != nil {
		testHookSandboxAfterAuthorize()
	}
	id := inst.ID
	guard := func(current *session.Instance) error {
		// Inside the archive fence, on the instance the manager holds right now.
		return sandboxSessionStillAuthorized(current, id)
	}
	return s.archiveSessionGuarded(ctx, req, resp, guard)
}

// --- tab verbs: the credential's own session only ---------------------------

func (s *controlServer) createTabScoped(ctx context.Context, req CreateTabRequest, resp *CreateTabResponse) error {
	if owner, ok := sandboxOwner(ctx); ok {
		// A process or shell tab runs inside the sandbox. A web tab's localhost
		// target is proxied by the daemon and a VS Code tab is served by a daemon
		// managed code-server — both land on the daemon host — and a docs tab
		// reads the daemon's proposal store. None is admitted.
		if err := checkSandboxTabKind(req); err != nil {
			return err
		}
		proj, inst, err := s.manager.authorizeSandboxSession(owner, req.ID, req.Title, true)
		if err != nil {
			return err
		}
		req.ID, req.Title, req.RepoID = inst.ID, inst.Title, proj.repoID
	}
	return s.CreateTab(req, resp)
}

// checkSandboxTabKind admits process and shell tabs only.
func checkSandboxTabKind(req CreateTabRequest) error {
	if k := strings.TrimSpace(req.Kind); (k != "" && k != "shell" && k != "process") ||
		req.URL != "" || req.Port != 0 || req.Proposal != "" {
		return sandboxRefusal("only process and shell tabs can be created from a sandbox (web, VS Code and docs tabs are served by the daemon host)")
	}
	return nil
}

func (s *controlServer) closeTabScoped(ctx context.Context, req CloseTabRequest, resp *CloseTabResponse) error {
	if owner, ok := sandboxOwner(ctx); ok {
		proj, inst, err := s.manager.authorizeSandboxSession(owner, req.ID, req.Title, true)
		if err != nil {
			return err
		}
		req.ID, req.Title, req.RepoID = inst.ID, inst.Title, proj.repoID
	}
	return s.closeTab(ctx, req, resp)
}

func (s *controlServer) renameTabScoped(ctx context.Context, req RenameTabRequest, resp *RenameTabResponse) error {
	if owner, ok := sandboxOwner(ctx); ok {
		proj, inst, err := s.manager.authorizeSandboxSession(owner, req.ID, req.Title, true)
		if err != nil {
			return err
		}
		req.ID, req.Title, req.RepoID = inst.ID, inst.Title, proj.repoID
	}
	return s.RenameTab(req, resp)
}

func (s *controlServer) reorderTabScoped(ctx context.Context, req ReorderTabRequest, resp *ReorderTabResponse) error {
	if owner, ok := sandboxOwner(ctx); ok {
		proj, inst, err := s.manager.authorizeSandboxSession(owner, req.ID, req.Title, true)
		if err != nil {
			return err
		}
		req.ID, req.Title, req.RepoID = inst.ID, inst.Title, proj.repoID
	}
	return s.ReorderTab(req, resp)
}

// --- CreateSession: listed hosts only, project default account only ---------

func (s *controlServer) createSessionScoped(ctx context.Context, req CreateSessionRequest, resp *CreateSessionResponse) error {
	if owner, ok := sandboxOwner(ctx); ok {
		proj, err := s.manager.sandboxProjectOf(owner)
		if err != nil {
			return err
		}
		if err := checkSandboxCreate(req); err != nil {
			return err
		}
		// The project is the owner's, never the caller's: the sandbox's own
		// working directory is not a path on this host.
		req.RepoPath = proj.path
		create := s.createSession
		if s.createSessionFn != nil {
			create = s.createSessionFn
		}
		if err := create(ctx, req, resp); err != nil {
			return err
		}
		// The create response is the manager's complete InstanceData: the
		// daemon-host repository path, worktree and tmux name. A sandbox gets the
		// same projection the snapshot paths give it (#5888).
		projectSandboxCreateResponse(resp)
		return nil
	}
	return s.createSession(ctx, req, resp)
}

// projectSandboxCreateResponse narrows a create response to what a sandbox may
// learn about the session it just started (#5888).
func projectSandboxCreateResponse(resp *CreateSessionResponse) {
	resp.Instance = sandboxSafeInstanceData(resp.Instance)
}

// checkSandboxCreate is the create-shape rule for a sandbox caller.
func checkSandboxCreate(req CreateSessionRequest) error {
	if strings.TrimSpace(req.Host) == "" {
		return sandboxRefusal("a sandbox creates sessions on a listed host only (--host); it cannot start one on the daemon box")
	}
	if b := strings.TrimSpace(req.Backend); b != "" && b != "ssh" {
		return sandboxRefusal("a sandbox creates sessions through a listed host (the ssh runtime) only")
	}
	if req.InPlace {
		return sandboxRefusal("--here attaches the daemon host's own working tree")
	}
	if strings.TrimSpace(req.Account) != "" {
		return sandboxRefusal("a sandbox cannot select an account; its sessions use the project default")
	}
	return nil
}

// --- tasks: prompt-delivery tasks on the project's own off-box sessions ------

// stampSandboxTask binds a sandbox-created task to its owner and project. The
// provenance is durable and overwrites anything the caller sent: every later
// delivery re-checks against it (checkSandboxTaskDelivery).
func stampSandboxTask(t task.Task, id, owner string, proj sandboxProject, target string) task.Task {
	// The ID is the daemon's, minted per add and never the caller's: a caller
	// that named an existing task's ID could otherwise shadow or re-arm a task it
	// does not own (an operator watch task, another project's). The store also
	// refuses a duplicate atomically (task.ErrTaskIDExists).
	t.ID = id
	t.TargetSession = target
	t.ProjectPath, t.RepoID, t.Host = proj.path, "", ""
	t.OnComplete = ""
	t.SandboxOwner = owner
	return t
}

// checkSandboxTaskShape is the task-shape rule for a sandbox caller.
func checkSandboxTaskShape(t task.Task) error {
	if strings.TrimSpace(t.WatchCmd) != "" {
		return sandboxRefusal("watch tasks run a script on the daemon host and cannot be created from a sandbox")
	}
	if strings.TrimSpace(t.CronExpr) == "" {
		return sandboxRefusal("only cron tasks can be created from a sandbox")
	}
	if strings.TrimSpace(t.TargetSession) == "" {
		return sandboxRefusal("a sandbox task must deliver its prompt to an existing session (target_session); spawning one runs on the daemon host")
	}
	return nil
}

func (s *controlServer) addTaskScoped(ctx context.Context, req AddTaskRequest, resp *AddTaskResponse) error {
	if owner, ok := sandboxOwner(ctx); ok {
		t := req.Task
		if err := checkSandboxTaskShape(t); err != nil {
			return err
		}
		proj, inst, err := s.manager.authorizeSandboxSession(owner, "", t.TargetSession, false)
		if err != nil {
			return err
		}
		req.Actor = fmt.Sprintf("sandbox session %s", owner)
		// A minted ID can still collide (32 random bits) — the store refuses that
		// atomically, and a fresh one is minted for the retry.
		for attempt := 0; attempt < 3; attempt++ {
			var id string
			if id, err = task.GenerateID(); err != nil {
				return fmt.Errorf("could not mint a task id: %w", err)
			}
			req.Task = stampSandboxTask(t, id, owner, proj, inst.Title)
			if err = s.addTask(ctx, req, resp); !errors.Is(err, task.ErrTaskIDExists) {
				// The stored record returned to the caller is the sandbox-safe
				// projection (its id, name, prompt, schedule): the full row carries
				// the daemon's project path and bound repo id.
				if resp.Task != nil {
					safe := sandboxSafeTask(*resp.Task)
					resp.Task = &safe
				}
				return err
			}
		}
		return err
	}
	return s.addTask(ctx, req, resp)
}

// sandboxTaskInProject reports whether t belongs to the owner's project. It keys
// on the task's retained RepoID — the project's identity — and never on its
// path, which can be rebound or reused. A task with no retained RepoID is not
// provably in any project and fails closed.
func sandboxTaskInProject(t task.Task, proj sandboxProject) bool {
	return t.RepoID != "" && t.RepoID == proj.repoID
}

func (s *controlServer) listTasksScoped(ctx context.Context, req ListTasksRequest, resp *ListTasksResponse) error {
	owner, ok := sandboxOwner(ctx)
	if !ok {
		return s.ListTasks(req, resp)
	}
	// Scope first: a credential that is not tracked learns nothing from the
	// scheduler's own state.
	proj, err := s.manager.sandboxProjectOf(owner)
	if err != nil {
		return err
	}
	if err := s.ListTasks(req, resp); err != nil {
		return err
	}
	kept := make([]task.Task, 0, len(resp.Tasks))
	for _, t := range resp.Tasks {
		// Watch tasks are not part of a sandbox's surface at all (their command
		// is a host-side script), and every kept record is projected: the full
		// record carries the daemon's absolute project path and repo id.
		if sandboxTaskInProject(t, proj) && strings.TrimSpace(t.WatchCmd) == "" {
			kept = append(kept, sandboxSafeTask(t))
		}
	}
	resp.Tasks = kept
	return nil
}

// sandboxSafeTask projects a task to what a sandbox may learn about it. Built by
// CONSTRUCTION, like sandboxSafeInstanceData: a field added to task.Task later is
// withheld until someone decides otherwise. Withheld: ProjectPath, RepoID, Host,
// Program, WatchCmd, OnComplete, SandboxOwner and the scheduler's arming/audit
// internals — host layout and operator state.
func sandboxSafeTask(t task.Task) task.Task {
	return task.Task{
		ID:            t.ID,
		Name:          t.Name,
		Prompt:        t.Prompt,
		CronExpr:      t.CronExpr,
		TargetSession: t.TargetSession,
		Enabled:       t.Enabled,
		CreatedAt:     t.CreatedAt,
		LastRunAt:     t.LastRunAt,
		LastRunStatus: sandboxRunStatusHead(t.LastRunStatus),
	}
}

// testHookSandboxRemoveAfterPrecheck runs between a scoped RemoveTask's early
// refusal and the locked removal, so a test can rebind or convert the task there.
var testHookSandboxRemoveAfterPrecheck func()

func (s *controlServer) removeTaskScoped(ctx context.Context, req RemoveTaskRequest, resp *RemoveTaskResponse) error {
	owner, ok := sandboxOwner(ctx)
	if !ok {
		return s.RemoveTask(req, resp)
	}
	unavailable := func() error { return sandboxRefusal("that task is not available to this credential") }
	proj, err := s.manager.sandboxProjectOf(owner)
	if err != nil {
		return err
	}
	// Scope before existence, with ONE answer: a task that does not exist,
	// belongs to another project, or is a watch task is the same refusal, so the
	// id space is not an oracle. This early read only decides which refusal the
	// caller sees; it authorizes nothing.
	admit := func(t task.Task) error {
		if !sandboxTaskInProject(t, proj) || strings.TrimSpace(t.WatchCmd) != "" {
			return unavailable()
		}
		return nil
	}
	t, err := task.GetTask(req.ID)
	if err != nil {
		return unavailable()
	}
	if err := admit(*t); err != nil {
		return err
	}
	if testHookSandboxRemoveAfterPrecheck != nil {
		testHookSandboxRemoveAfterPrecheck()
	}
	// The caller's expectation is never consulted: its mismatch error is composed
	// from the stored record, host path included (#5893). The authorization is the
	// project's identity, re-validated on the record loaded under the same lock
	// that performs the delete (#5889).
	req.Expect = task.ProjectExpectation{}
	if err := s.removeTask(req, resp, admit); err != nil {
		var nf *task.NotFoundError
		if errors.As(err, &nf) {
			return unavailable()
		}
		return err
	}
	return nil
}

// checkSandboxTaskDelivery re-authorizes one firing of a sandbox-created task.
// The target must be an existing, unarchived, off-box session in the project of
// the session that created the task, and that project must be the one the
// delivery resolved. Any failure skips the run with the reason; there is no
// fallback to the task's own project path or to a create.
func (m *Manager) checkSandboxTaskDelivery(owner, deliveryRepoID, title string) (string, error) {
	if strings.TrimSpace(title) == "" {
		return "", sandboxRefusal("the task has no target session")
	}
	proj, inst, err := m.authorizeSandboxSession(owner, "", title, false)
	if err != nil {
		return "", err
	}
	if proj.repoID != deliveryRepoID {
		return "", sandboxRefusal("the target is not in the project the task was created in")
	}
	if inst.IsArchived() {
		return "", sandboxRefusal("the target session is archived")
	}
	return inst.ID, nil
}

// sandboxDeliveryGuard pins a sandbox task's send to the session ID that
// checkSandboxTaskDelivery authorized. It runs under the target's op lock on the
// instance the send is about to reach, so a session that was archived and
// replaced by another with the same title between the check and the send is
// refused rather than reached: authorization is by ID and delivery is by that
// same ID, never a second title resolution.
func (m *Manager) sandboxDeliveryGuard(id, repoID string) func(*session.Instance, string) error {
	base := m.sandboxSessionGuard(id, repoID)
	return func(inst *session.Instance, resolvedRepo string) error {
		if err := base(inst, resolvedRepo); err != nil {
			return err
		}
		if inst.IsArchived() {
			return sandboxRefusal("the target session is archived")
		}
		return nil
	}
}

// sandboxSessionStillAuthorized is the re-check every scoped session mutation
// runs on the instance it is about to act on, under the lock that performs the
// action: it is still the authorized session (by ID, never by title) and still
// runs off-box.
func sandboxSessionStillAuthorized(inst *session.Instance, id string) error {
	switch {
	case inst == nil || id == "" || inst.ID != id:
		return sandboxRefusal("that session is not available to this credential")
	case !offBoxBackendType(instanceBackendType(inst)):
		return sandboxRefusal("that session is not available to this credential")
	}
	return nil
}

// sandboxSessionGuard is sandboxSessionStillAuthorized plus the project check,
// shaped for the send path, which also learns the repo it resolved.
func (m *Manager) sandboxSessionGuard(id, repoID string) func(*session.Instance, string) error {
	return func(inst *session.Instance, resolvedRepo string) error {
		if err := sandboxSessionStillAuthorized(inst, id); err != nil {
			return err
		}
		if resolvedRepo != repoID {
			return sandboxRefusal("that session is not available to this credential")
		}
		return nil
	}
}
