package daemon

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/log"
	"github.com/sachiniyer/agent-factory/task"
)

// warnLegacyBareCloneSessions makes the #3358 identity transition explicit.
// Old rows cannot be migrated safely: the old writer persisted the unrelated
// parent as both Path and Worktree.RepoPath, discarding the linked worktree the
// user originally requested. That parent may itself own real sessions, and
// several bare repositories may share it. Preserve those rows under their old
// key, where all-repo listing and stable-ID actions still reach them, and name
// the compatibility path instead of silently pretending the new identity has
// no history.
func warnLegacyBareCloneSessions(repo *config.RepoContext) {
	legacyRoot, legacyID := repo.LegacyBareRepoIdentity()
	if legacyID == "" || legacyID == repo.ID {
		return
	}
	rows, err := loadRepoInstanceData(legacyID)
	if err != nil {
		log.WarningLog.Printf("bare repository %s now uses repo identity %s, but its pre-#3358 parent-keyed session store %s could not be read: %v; inspect that repo ID before assuming it is empty", repo.IdentityPath(), repo.ID, legacyID, err)
		return
	}
	if len(rows) == 0 {
		return
	}
	log.WarningLog.Printf("bare repository %s now uses repo identity %s; preserving %d pre-#3358 session record(s) under former parent identity %s (%s) because those records discarded the requesting worktree and cannot be re-attributed safely — they remain available through all-repo listing and stable session IDs", repo.IdentityPath(), repo.ID, len(rows), legacyID, legacyRoot)
}

// warnLegacyBareCloneTasks is the automation half of the same #3358 transition.
// A task created from a bare linked worktree before the fix retained the
// unrelated parent in BOTH ProjectPath and RepoID, so the corrected project no
// longer lists it — while an enabled cron/watch task keeps firing under the old
// identity. When that parent is itself a repository, each delivery keeps making
// sessions in it, invisible from the bare project the user now works in.
//
// Sessions are inert once preserved; automation is not, so this names the tasks
// and what to do about them. Rebinding them here would be the same unsafe
// re-attribution the session rows are preserved to avoid: the old rows discarded
// the requesting worktree, several bare clones can share one parent, and a real
// repository may live there and legitimately own these tasks.
//
// The scan is a pure read of the task file — no ProjectPath resolution and no
// binding backfill (LoadTasksForRepoID durably rewrites bindings and hands the
// caller a publish obligation, neither of which belongs on a create path).
// Matching is therefore textual: the retained RepoID, or a legacy row whose
// RepoID was never written and whose ProjectPath still spells the old parent.
func warnLegacyBareCloneTasks(repo *config.RepoContext) {
	legacyRoot, legacyID := repo.LegacyBareRepoIdentity()
	if legacyID == "" || legacyID == repo.ID {
		return
	}
	all, err := loadTasksForLegacyScan()
	if err != nil {
		log.WarningLog.Printf("bare repository %s now uses repo identity %s, but its pre-#3358 tasks could not be read: %v; inspect `af tasks list --all` for tasks still bound to former parent identity %s (%s) before assuming there are none", repo.IdentityPath(), repo.ID, err, legacyID, legacyRoot)
		return
	}
	var stranded []task.Task
	for _, t := range all {
		if t.RepoID != "" {
			if t.RepoID == legacyID {
				stranded = append(stranded, t)
			}
			continue
		}
		if t.ProjectPath != "" && filepath.Clean(t.ProjectPath) == filepath.Clean(legacyRoot) {
			stranded = append(stranded, t)
		}
	}
	if len(stranded) == 0 {
		return
	}
	enabled := 0
	for _, t := range stranded {
		if t.Enabled {
			enabled++
		}
	}
	log.WarningLog.Printf("bare repository %s now uses repo identity %s; %d pre-#3358 task(s) (%d enabled) remain bound to former parent identity %s (%s) and are NOT listed by this project: %s; enabled cron/watch deliveries keep creating sessions under that identity — inspect them with `af tasks list --all`, then disable or explicitly rebind each one to this project", repo.IdentityPath(), repo.ID, len(stranded), enabled, legacyID, legacyRoot, describeLegacyTasks(stranded))
}

// describeLegacyTasks names the stranded tasks so the warning is actionable
// without a second lookup: an id is what `af tasks` acts on, a name is what the
// user recognizes.
func describeLegacyTasks(tasks []task.Task) string {
	parts := make([]string, 0, len(tasks))
	for _, t := range tasks {
		if t.Name != "" {
			parts = append(parts, fmt.Sprintf("%s (%q)", t.ID, t.Name))
			continue
		}
		parts = append(parts, t.ID)
	}
	return strings.Join(parts, ", ")
}

// loadTasksForLegacyScan is a package var so the warning above can be tested
// without a task file on disk. Production never reassigns it.
var loadTasksForLegacyScan = task.LoadTasks
