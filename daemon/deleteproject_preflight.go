package daemon

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// The fail-closed gates deleteProject runs before its first durable mutation:
// - liveSessionsTouchingRootLocked is the path evidence a derived-id delete
//   uses to show no live session can belong to the recorded root (#7829 R2).
// - preflightDeleteProjectTaskTargets refuses while an enabled task bound to
//   any identity the delete fences targets a session it removes (#7829 R1).
// - preflightRecordedRootTaskTargets refuses while an enabled task recorded
//   at the recorded root targets the root session whose opt-in it sweeps, but
//   is bound to an identity the recorded path can no longer resolve.

// liveSessionsTouchingRootLocked returns the titles of live sessions whose
// recorded paths touch recordedRoot — the workspace the create was scoped to,
// the worktree it placed, or the repo path its identity was recorded under —
// at, beneath, or CONTAINING it. It is the path evidence a derived-id delete
// uses to show no live session can belong to the recorded root (#7829 R2):
// such a row's sessions may be keyed under an identity the path cannot
// recompute (a bare repository's common dir for a linked-worktree root), so
// none of the delete's keyed scans can see them — only their paths name them.
// Both directions of containment count: a session whose workspace contains
// the root may have the root as one of its own worktrees, which is the same
// unknowable identity. resolvedRoot is the root resolved for comparison
// (deleteProjectTarget.carryWorkspace), supplied so the locked walk never
// touches the filesystem; it and the raw spelling are both checked. m.mu held.
func (m *Manager) liveSessionsTouchingRootLocked(recordedRoot, resolvedRoot string) []string {
	recordedRoot = filepath.Clean(recordedRoot)
	touches := func(candidate string) bool {
		if candidate == "" {
			return false
		}
		c := filepath.Clean(candidate)
		return pathutil.IsAtOrInside(c, recordedRoot) ||
			pathutil.IsAtOrInside(recordedRoot, c) ||
			(resolvedRoot != "" && (pathutil.IsAtOrInside(c, resolvedRoot) || pathutil.IsAtOrInside(resolvedRoot, c)))
	}
	titles := make(map[string]struct{})
	for key, inst := range m.instances {
		_, restoring := m.restoresInFlight[key]
		if inst == nil || (inst.GetLiveness() == session.LiveArchived && !restoring) {
			continue
		}
		data := inst.ToInstanceData()
		if touches(data.Path) || touches(data.Worktree.WorktreePath) || touches(data.Worktree.RepoPath) {
			_, title := splitDaemonInstanceKey(key)
			titles[title] = struct{}{}
		}
	}
	for _, pending := range m.pendingCreates {
		if touches(pending.Path) || touches(pending.Worktree.WorktreePath) || touches(pending.Worktree.RepoPath) {
			titles[pending.Title] = struct{}{}
		}
	}
	out := make([]string, 0, len(titles))
	for title := range titles {
		out = append(out, title)
	}
	return out
}

// preflightDeleteProjectTaskTargets checks every identity the delete fences —
// the requested repoID plus each extraFenced id (#7829 R1): a derived-id
// delete also removes the historical path-hash identity's opt-in and sweeps
// its sessions' project, so an enabled task bound to EITHER id can strand.
// repoID remains the id errors are phrased against.
func (m *Manager) preflightDeleteProjectTaskTargets(repoID string, extraFenced ...string) (map[string][]task.Task, error) {
	repoIDs := append([]string{repoID}, extraFenced...)
	m.mu.Lock()
	titleSet := make(map[string]struct{})
	hasRoot := make(map[string]bool, len(repoIDs))
	for key, instance := range m.instances {
		rid, title := splitDaemonInstanceKey(key)
		if instance == nil || instance.GetLiveness() == session.LiveArchived {
			continue
		}
		for _, id := range repoIDs {
			if rid == id {
				titleSet[title] = struct{}{}
				hasRoot[id] = hasRoot[id] || title == session.RootSessionTitle
			}
		}
	}
	m.mu.Unlock()
	// An enabled root is part of the project's effective live-session set even
	// while its process is momentarily absent: the ensure loop will recreate it,
	// and task delivery waits for that same promise. Include the reserved target
	// before the empty-roster return, or DeleteProject can suppress re-creation
	// and strand a root-targeted task behind a title ordinary auto-create refuses.
	// For this consumer UNKNOWN behaves like yes (#3264): a fail-closed repo
	// (unloadable personal config, unlistable registry) reports
	// willMaterialize=false, but deleting through that answer would drop the
	// root_agents opt-in and leave the enabled task stranded the moment the
	// config becomes readable again — the exact hazard this preflight refuses.
	// Each fenced identity gets the verdict under ITS OWN id: a legacy row's
	// path-keyed opt-in materializes under the historical hash, not the derived
	// id the request was addressed by.
	for _, id := range repoIDs {
		if hasRoot[id] {
			continue
		}
		switch m.rootAgentMaterializeVerdictFor(id).reason {
		case rootAgentWillMaterialize, rootAgentRegistryUnreadable, rootAgentPersonalUnreadable, rootAgentProjectUnresolved, rootAgentRecordsUnreadable, rootAgentAttributionPending, rootAgentLegacyProbeUnanswered, rootAgentProjectRootProbeUnanswered:
			titleSet[session.RootSessionTitle] = struct{}{}
		}
	}
	titles := make([]string, 0, len(titleSet))
	for title := range titleSet {
		titles = append(titles, title)
	}
	sort.Strings(titles)
	if len(titles) == 0 {
		return make(map[string][]task.Task), nil
	}

	// Merge each fenced identity's task bindings into one target map: a task
	// bound to the historical id strands exactly like one bound to the derived
	// id once this delete removes the session or sweeps the root's opt-in.
	taskTargets := make(map[string][]task.Task)
	for _, id := range repoIDs {
		targets, err := m.loadEnabledTaskTargets(id)
		if err != nil {
			return nil, fmt.Errorf("delete project %s: could not determine whether enabled tasks target its sessions; nothing was changed: %w", repoID, err)
		}
		for title, targeted := range targets {
			taskTargets[title] = append(taskTargets[title], targeted...)
		}
	}

	var blockers []string
	for _, title := range titles {
		targeted := taskTargets[title]
		if len(targeted) > 0 {
			blockers = append(blockers, fmt.Sprintf("session %q: %s", title, describeTargetTasks(targeted)))
		}
	}
	if len(blockers) > 0 {
		return nil, fmt.Errorf("delete project %s: enabled task(s) target session(s) it must remove: %s; disable or retarget them, then delete the project again; nothing was changed", repoID, strings.Join(blockers, "; "))
	}
	return taskTargets, nil
}

// preflightRecordedRootTaskTargets is the unknowable-identity half of the
// task preflight (#7829 R1/R2): a derived-id delete sweeps the root_agents
// opt-in spelled as the recorded root via the path's hash, but the row's real
// identity — the bare-repo id its sessions and tasks may be keyed under —
// cannot be enumerated, so loadEnabledTaskTargets cannot see those tasks. An
// enabled task RECORDED at the recorded root and targeting the reserved root
// session is the one thing left that can still be attributed by path: bound
// to a fenced id it was already a blocker above; bound to anything else it
// belongs to an identity this delete cannot reach, and sweeping the opt-in
// out from under it strands it the same way. Refuse and name it.
func (m *Manager) preflightRecordedRootTaskTargets(repoID, recordedRoot, resolvedRoot string, fenced []string) error {
	recordedRoot = filepath.Clean(recordedRoot)
	fencedSet := make(map[string]struct{}, len(fenced))
	for _, id := range fenced {
		fencedSet[id] = struct{}{}
	}
	tasks, err := task.LoadTasks()
	if err != nil {
		return fmt.Errorf("delete project %s: could not determine whether enabled tasks recorded at %s target the root session it sweeps; nothing was changed: %w", repoID, recordedRoot, err)
	}
	atRoot := func(path string) bool {
		if path == "" {
			return false
		}
		p := filepath.Clean(path)
		return pathutil.IsAtOrInside(p, recordedRoot) ||
			pathutil.IsAtOrInside(recordedRoot, p) ||
			(resolvedRoot != "" && (pathutil.IsAtOrInside(p, resolvedRoot) || pathutil.IsAtOrInside(resolvedRoot, p)))
	}
	var blockers []string
	for _, t := range tasks {
		if !t.Enabled || task.CanonicalTargetSession(t.TargetSession) != session.RootSessionTitle {
			continue
		}
		if _, fenced := fencedSet[t.RepoID]; t.RepoID == "" || fenced {
			continue
		}
		if atRoot(t.ProjectPath) {
			blockers = append(blockers, fmt.Sprintf("task %q (%s) bound to %s", t.Name, t.ID, t.RepoID))
		}
	}
	if len(blockers) > 0 {
		sort.Strings(blockers)
		return fmt.Errorf("delete project %s: enabled task(s) recorded at %s target the root session whose opt-in it must sweep — %s; they are bound to an identity the recorded path can no longer resolve, so af cannot tell whether they belong to this project — disable or retarget them, restore the checkout's git metadata at %s so af can record the project's identity, then delete; nothing was changed", repoID, recordedRoot, strings.Join(blockers, "; "), recordedRoot)
	}
	return nil
}
