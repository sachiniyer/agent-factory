package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/task"
)

// The fail-closed gates deleteProject runs before its first durable mutation:
// - liveSessionsBelongingToRecordedRootLocked is the path-and-identity
//   evidence a derived-id delete uses to show no live session can belong to
//   the recorded root (#7829 R2, PR #7853 review R6/R7).
// - preflightDeleteProjectTaskTargets refuses while an enabled task bound to
//   any identity the delete fences targets a session it removes (#7829 R1).
// - preflightRecordedRootTaskTargets refuses while an enabled task recorded
//   at the recorded root targets the root session whose opt-in it sweeps, but
//   is bound to an identity the recorded path can no longer resolve.

// recordedRootSessionEvidence is everything the belonging gate learns from the
// filesystem. It is gathered WITHOUT m.mu because it does disk work — path
// resolution and git worktree records — and then consumed under the caller's
// m.mu against a FRESH session set. That two-phase shape is why three sets
// exist: an identity root the gather never saw belongs to a session that
// arrived mid-gather, and an identity root whose records could not be read is
// one ownership could not be established for — both stay fail-closed.
type recordedRootSessionEvidence struct {
	// owning holds identity roots whose own worktrees/*/gitdir records name
	// <recordedRoot>/.git: the repository the session is keyed under kept the
	// recorded root as a linked worktree — the session needs no path of its
	// own near the root to belong to it (PR #7853 review R6).
	owning map[string]struct{}
	// unproven holds identity roots whose records could not be established —
	// the root itself is gone or its worktrees listing or a listed gitdir
	// could not be read — so nothing rules them out (R6's fail-closed leg).
	unproven map[string]struct{}
	// scanned holds every identity root the gather attempted, clean or not.
	// A session keyed under an UNscanned root arrived between the gather and
	// the verdict, so it is unvetted — and unvetted stays fail-closed.
	scanned map[string]struct{}
	// resolved maps each session candidate path seen at gather time to its
	// filesystem-resolved form, so the verdict can compare both sides in
	// resolved form without touching disk under m.mu.
	resolved map[string]string
}

// recordedRootSession is one snapshotted session's share of the gather: the
// paths the containment check compares and the identity root the git worktree
// records are read from.
type recordedRootSession struct {
	candidates []string
	identity   string
}

// gatherRecordedRootSessionEvidence collects, under a short m.mu snapshot, the
// paths and identity roots of every session the belonging gate can see — live
// instances, restoring archives, and pending creates — then reads, WITHOUT the
// lock, what the verdict needs from disk. recordedRoot is the matched row's
// recorded root (present but unresolvable); resolvedRoot is its resolved form
// (deleteProjectTarget.carryWorkspace).
func (m *Manager) gatherRecordedRootSessionEvidence(recordedRoot, resolvedRoot string) recordedRootSessionEvidence {
	m.mu.Lock()
	sessions := make([]recordedRootSession, 0, len(m.instances)+len(m.pendingCreates))
	snapshot := func(data session.InstanceData) {
		identity := data.Worktree.RepoPath
		if identity == "" {
			identity = data.Path
		}
		if identity != "" {
			identity = filepath.Clean(identity)
		}
		sessions = append(sessions, recordedRootSession{
			candidates: []string{data.Path, data.Worktree.WorktreePath, data.Worktree.RepoPath},
			identity:   identity,
		})
	}
	for key, inst := range m.instances {
		_, restoring := m.restoresInFlight[key]
		if inst == nil || (inst.GetLiveness() == session.LiveArchived && !restoring) {
			continue
		}
		snapshot(inst.ToInstanceData())
	}
	for _, pending := range m.pendingCreates {
		snapshot(pending)
	}
	m.mu.Unlock()

	ev := recordedRootSessionEvidence{
		owning:   make(map[string]struct{}),
		unproven: make(map[string]struct{}),
		scanned:  make(map[string]struct{}),
		resolved: make(map[string]string),
	}
	gitFile := filepath.Join(filepath.Clean(recordedRoot), ".git")
	resolvedGitFile := ""
	if resolvedRoot != "" {
		resolvedGitFile = filepath.Join(resolvedRoot, ".git")
	}
	for _, s := range sessions {
		for _, c := range s.candidates {
			if c == "" {
				continue
			}
			if _, done := ev.resolved[c]; !done {
				ev.resolved[c] = pathutil.ResolveForCompare(c)
			}
		}
		if s.identity == "" {
			continue
		}
		if _, done := ev.scanned[s.identity]; done {
			continue
		}
		ev.scanned[s.identity] = struct{}{}
		if _, err := os.Stat(s.identity); err != nil {
			// The identity root itself cannot be reached, so its records can
			// never be established — nothing here rules out ownership.
			ev.unproven[s.identity] = struct{}{}
			continue
		}
		// The common dir answers both layouts: a bare repository holds
		// worktrees/ directly, an ordinary checkout's common dir is <root>/.git.
		for _, dir := range []string{filepath.Join(s.identity, "worktrees"), filepath.Join(s.identity, ".git", "worktrees")} {
			entries, err := os.ReadDir(dir)
			if err != nil {
				// A missing worktrees dir means no linked-worktree records exist
				// — ownership is ruled out, not unproven. ENOTDIR is the same
				// answer: a path component that is not a directory (a .git file
				// rather than a common dir) can hold no records either.
				if !os.IsNotExist(err) && !errors.Is(err, syscall.ENOTDIR) {
					ev.unproven[s.identity] = struct{}{}
				}
				continue
			}
			for _, entry := range entries {
				content, err := os.ReadFile(filepath.Join(dir, entry.Name(), "gitdir"))
				if err != nil {
					// A listed worktree whose gitdir cannot be read is a record
					// ownership could not be established from.
					ev.unproven[s.identity] = struct{}{}
					continue
				}
				gitdir := filepath.Clean(strings.TrimSpace(string(content)))
				if gitdir == gitFile || (resolvedGitFile != "" && (gitdir == resolvedGitFile || pathutil.ResolveForCompare(gitdir) == resolvedGitFile)) {
					ev.owning[s.identity] = struct{}{}
				}
			}
		}
	}
	return ev
}

// liveSessionsBelongingToRecordedRootLocked returns the titles of live
// sessions a derived-id delete cannot rule out for the recorded root (#7829
// R2): a present root whose .git is gone cannot say WHICH identity its
// sessions were keyed under, so the keyed scans cannot see them — a root that
// was a linked worktree of a bare repository keys them under the bare common
// dir's id, which the recorded path cannot recompute.
//
// The evidence comes in three kinds, all supplied by ev:
//   - a session path AT OR BENEATH the root (workspace, worktree, or recorded
//     repo path, in raw or resolved form). The reverse direction does NOT
//     count for plain paths (PR #7853 review R7): an enclosing workspace is
//     just a bigger project, and blaming it refuses deletes master completes
//     while naming an unrelated project.
//   - a session keyed under an identity root that OWNS the root — git's own
//     worktrees records name <root>/.git — which is the only way an enclosing
//     or sibling path legitimately belongs.
//   - a session keyed under an identity root ownership could not be
//     established for — unscanned, unreadable, or unrecorded — which stays
//     fail-closed.
//
// m.mu held; the verdict runs against the fresh session set, so sessions that
// arrived between the gather and this lock are evaluated by the same rules.
func (m *Manager) liveSessionsBelongingToRecordedRootLocked(ev recordedRootSessionEvidence, recordedRoot, resolvedRoot string, covered ...string) []string {
	recordedRoot = filepath.Clean(recordedRoot)
	coveredSet := make(map[string]struct{}, len(covered))
	for _, id := range covered {
		coveredSet[id] = struct{}{}
	}
	touches := func(candidate string) bool {
		if candidate == "" {
			return false
		}
		c := filepath.Clean(candidate)
		if pathutil.IsAtOrInside(c, recordedRoot) {
			return true
		}
		if resolvedRoot == "" {
			return false
		}
		if pathutil.IsAtOrInside(c, resolvedRoot) {
			return true
		}
		if r, ok := ev.resolved[candidate]; ok {
			return pathutil.IsAtOrInside(r, resolvedRoot)
		}
		return false
	}
	// cleanIdentity reports whether ownership could be ruled out for the
	// session's keying identity root: it was scanned and is neither proven
	// nor unproven.
	cleanIdentity := func(identity string) bool {
		if identity == "" {
			return false
		}
		identity = filepath.Clean(identity)
		if _, ok := ev.scanned[identity]; !ok {
			return false
		}
		if _, ok := ev.unproven[identity]; ok {
			return false
		}
		_, ok := ev.owning[identity]
		return !ok
	}
	belongs := func(data session.InstanceData) bool {
		if touches(data.Path) || touches(data.Worktree.WorktreePath) || touches(data.Worktree.RepoPath) {
			return true
		}
		identity := data.Worktree.RepoPath
		if identity == "" {
			identity = data.Path
		}
		return !cleanIdentity(identity)
	}
	titles := make(map[string]struct{})
	for key, inst := range m.instances {
		_, restoring := m.restoresInFlight[key]
		if inst == nil || (inst.GetLiveness() == session.LiveArchived && !restoring) {
			continue
		}
		if belongs(inst.ToInstanceData()) {
			_, title := splitDaemonInstanceKey(key)
			titles[title] = struct{}{}
		}
	}
	for _, pending := range m.pendingCreates {
		if belongs(pending) {
			titles[pending.Title] = struct{}{}
		}
	}
	// A reserved title is an admitted create that has not projected its
	// pendingCreates row yet, so it carries no paths or identity root to vet —
	// unvetted stays fail-closed. Reservations under an id the keyed gates
	// already cover are named there instead.
	for key := range m.reservedTitles {
		rid, title := splitDaemonInstanceKey(key)
		if _, covered := coveredSet[rid]; !covered {
			titles[title] = struct{}{}
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
// out from under it strands it the same way. Refuse and name it. Only
// at-or-inside counts — a task's project path is a plain workspace, so an
// enclosing one is a bigger project, not this root's owner (R7).
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
		if pathutil.IsAtOrInside(p, recordedRoot) {
			return true
		}
		if resolvedRoot == "" {
			return false
		}
		return pathutil.IsAtOrInside(p, resolvedRoot) ||
			pathutil.IsAtOrInside(pathutil.ResolveForCompare(p), resolvedRoot)
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
