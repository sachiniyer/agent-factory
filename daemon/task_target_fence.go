package daemon

import (
	"errors"
	"fmt"
	"sort"

	"github.com/sachiniyer/agent-factory/agentproto"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// This file is both sides of taskTargetMu's fence: the task-writer validation
// that refuses to enable a task whose target cannot receive deliveries, and
// the target snapshot the archive/delete lifecycle shares with it. Split from
// archive.go (#1145 file-length lint) along that concern.

// archiveTargetTasksChecked is a test seam for pinning the interval between the
// task snapshot and the archive fence. Production leaves it as a no-op.
var archiveTargetTasksChecked = func() {}

// loadTasksForRepoID is a test seam for proving lifecycle operations do not
// repeatedly reload and freshly resolve the same project task set.
var loadTasksForRepoID = task.LoadTasksForRepoIDWithBindingUpdates

// enabledTasksTargetingSession returns enabled tasks in repoID whose canonical
// target is exactly title. Names are sorted for a deterministic, complete
// archive refusal. A load error stays unknown and must fail the archive before
// any session mutation.
func (m *Manager) enabledTasksTargetingSession(repoID, title string) ([]task.Task, error) {
	targets, err := m.loadEnabledTaskTargets(repoID)
	if err != nil {
		return nil, err
	}
	return targets[title], nil
}

// loadEnabledTaskTargets loads and freshly scopes a project's task store once,
// then indexes every enabled target. DeleteProject reuses this snapshot for its
// preflight and each subsequent archive while holding taskTargetMu, so projects
// with many sessions do not repeat tasks x path-resolution work.
func (m *Manager) loadEnabledTaskTargets(repoID string) (map[string][]task.Task, error) {
	tasks, bindingUpdates, err := loadTasksForRepoID(repoID)
	// Publish BEFORE propagating: the load returns backfilled bindings alongside
	// a scope error (task.LoadTasksForRepoIDWithBindingUpdates), and those rows
	// were already committed durably. Dropping them on the error branch is not a
	// no-op — nothing re-publishes them, so a push-only client keeps a stale
	// repository scope for a projection the server has made authoritative.
	for _, updated := range bindingUpdates {
		m.publishEvent(agentproto.EventTaskUpdated, updated)
	}
	if err != nil {
		return nil, err
	}
	targets := make(map[string][]task.Task)
	for _, t := range tasks {
		target := task.CanonicalTargetSession(t.TargetSession)
		if t.Enabled && target != "" {
			targets[target] = append(targets[target], t)
		}
	}
	for target := range targets {
		targeted := targets[target]
		sort.Slice(targeted, func(i, j int) bool {
			if targeted[i].Name == targeted[j].Name {
				return targeted[i].ID < targeted[j].ID
			}
			return targeted[i].Name < targeted[j].Name
		})
	}
	return targets, nil
}

// taskTargetValidationContext carries the root-agent reachability verdict that
// can require git resolution. Control callers prepare it while holding
// taskTargetMu but before entering the tasks-file lock; validators must not
// shell out there. The verdict keeps the refusal's CAUSE (#3264), so the
// validator's message can name the thing to fix instead of guessing.
type taskTargetValidationContext struct {
	rootRepoID  string
	rootVerdict rootAgentMaterializeVerdict
}

func (m *Manager) prepareTaskTargetValidation(repoID, target string, enabled bool) taskTargetValidationContext {
	ctx := taskTargetValidationContext{}
	if enabled && repoID != "" && session.IsReservedTitle(target) {
		ctx.rootRepoID = repoID
		ctx.rootVerdict = m.rootAgentMaterializeVerdictFor(repoID)
	}
	return ctx
}

// validateEnabledTaskTarget is the task-writer side of taskTargetMu's fence.
// Missing ordinary targets remain legal (delivery auto-creates them), but a
// reserved target needs a proven daemon materialization path. A known target
// that is archiving or already archived would retry forever and is refused
// before the task-store commit. RepoID has already been derived by the checked
// add path or supplied from a freshly resolved legacy update. Unknown identity
// stays unknown for enabled targeted writes; accepting it would bypass every
// repository-scoped lifecycle check.
func (m *Manager) validateEnabledTaskTarget(t task.Task, ctx taskTargetValidationContext) error {
	target := task.CanonicalTargetSession(t.TargetSession)
	if !t.Enabled || target == "" {
		return nil
	}
	// During restore, absence from m.instances is unknown rather than proof that
	// the target does not exist. Keep the historical warm-up allowance for
	// disabled/untargeted task writes, but fail target-dependent writes closed.
	if !m.Ready() {
		return errDaemonStarting()
	}
	if t.RepoID == "" {
		return fmt.Errorf("cannot determine project identity for enabled task %q target %q; nothing was changed", t.ID, target)
	}
	// Reserved root delivery is safe only while the daemon owns its future, not
	// merely because a process happens to exist now. A disabled root-agent policy
	// deliberately leaves a surviving live root alone, but will not recreate it
	// after the next kill/outage; accepting a task there would defer the permanent
	// failure until that disappearance.
	if session.IsReservedTitle(target) {
		if target != session.RootSessionTitle {
			return fmt.Errorf("cannot enable task %q: reserved target session %q cannot materialize under that spelling; use %q exactly; nothing was changed", t.ID, target, session.RootSessionTitle)
		}
		if ctx.rootRepoID != t.RepoID {
			return fmt.Errorf("cannot enable task %q: could not determine whether reserved target session %q will materialize because its project identity changed during validation; retry; nothing was changed", t.ID, target)
		}
		if ctx.rootVerdict.reason != rootAgentWillMaterialize {
			// The refusal names its cause (#3264): "af could not establish
			// that the root will materialize" with a guess-list sent users
			// re-registering projects whose actual blocker was an unparseable
			// personal config or an unlistable registry.
			return fmt.Errorf("cannot enable task %q: target session %q is reserved for the daemon-managed root agent, and %s; or choose a different target; nothing was changed", t.ID, target, rootAgentUnavailableDetail(ctx.rootVerdict))
		}
	}
	m.mu.Lock()
	instance := m.instances[daemonInstanceKey(t.RepoID, target)]
	m.mu.Unlock()
	loaded := instance != nil
	var state session.InstanceData
	if loaded {
		state = instance.ToInstanceData()
	} else {
		persisted, _, err := findInstanceDataByTitle(target, t.RepoID)
		if errors.Is(err, errSessionNotFound) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("cannot enable task %q: could not determine target session %q state from storage; nothing was changed: %w", t.ID, target, err)
		}
		state = *persisted
	}
	switch {
	case state.InFlightOp == session.OpArchiving:
		return fmt.Errorf("cannot enable task %q: target session %q is being archived; wait for the archive, then restore it or choose a different target", t.ID, target)
	case state.Liveness == session.LiveArchived:
		return fmt.Errorf("cannot enable task %q: target session %q is archived; restore it or choose a different target", t.ID, target)
	case !loaded:
		return fmt.Errorf("cannot enable task %q: could not determine whether target session %q can receive prompts; it is recorded on disk but unavailable in the daemon — repair or remove that session, or choose a different target; nothing was changed", t.ID, target)
	default:
		return nil
	}
}
