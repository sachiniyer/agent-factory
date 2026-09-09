package session

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/sachiniyer/agent-factory/internal/pathutil"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

const (
	maxConcurrentWorktreeInspections = 4
	maxNamedWorktreeSiblings         = 5
)

// SessionWorktreeInspection is the read-only integrity result for one live,
// local session workspace. Warning is empty when no dangerous shape was found;
// Err means the checkout could not be inspected and is not treated as clean.
type SessionWorktreeInspection struct {
	InstanceID   string
	Title        string
	WorktreePath string
	Warning      string
	Evidence     sessiongit.WorktreeIntegrity
	Err          error
}

// InspectSessionWorktrees checks each live local worktree independently, then
// correlates their actual branches to expose a duplicate binding by lane name.
// Archived rows are intentionally excluded: their retained worktrees are inert.
func InspectSessionWorktrees(rows []InstanceData) []SessionWorktreeInspection {
	eligible := make([]InstanceData, 0, len(rows))
	for _, row := range rows {
		if IsArchivedData(row) || !row.UsesLocalTmux() || row.Worktree.WorktreePath == "" {
			continue
		}
		eligible = append(eligible, row)
	}
	inspections := make([]SessionWorktreeInspection, len(eligible))
	repoKeys := make([]string, len(eligible))
	workers := min(maxConcurrentWorktreeInspections, len(eligible))
	jobs := make(chan int)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				row := eligible[index]
				evidence, err := sessiongit.InspectWorktreeIntegrity(row.Worktree.WorktreePath)
				inspections[index] = SessionWorktreeInspection{
					InstanceID: row.ID, Title: row.Title, WorktreePath: row.Worktree.WorktreePath,
					Evidence: evidence, Err: err,
				}
				repoKeys[index] = pathutil.ResolveForCompare(row.Worktree.RepoPath)
			}
		}()
	}
	for index := range eligible {
		jobs <- index
	}
	close(jobs)
	wg.Wait()

	groups := make(map[string][]int)
	for index, inspection := range inspections {
		if inspection.Err != nil || inspection.Evidence.Branch == "" {
			continue
		}
		key := repoKeys[index] + "\x00" + inspection.Evidence.Branch
		groups[key] = append(groups[key], index)
	}
	for index := range inspections {
		inspection := &inspections[index]
		if inspection.Err != nil {
			continue
		}
		var signals []string
		key := repoKeys[index] + "\x00" + inspection.Evidence.Branch
		if siblings := otherWorktreeLanes(inspections, groups[key], index); len(siblings) > 0 {
			signals = append(signals, fmt.Sprintf("branch %q is also checked out by live lane(s) %s", inspection.Evidence.Branch, strings.Join(siblings, ", ")))
		}
		if inspection.Evidence.MassRevert {
			signals = append(signals, fmt.Sprintf("the index has %d staged paths and zero unstaged paths (the mass-revert shape)", inspection.Evidence.StagedPaths))
		}
		if inspection.Evidence.HeadMovedWithoutReflog {
			signals = append(signals, fmt.Sprintf("HEAD is %s but this worktree's latest HEAD reflog entry is %s", shortOID(inspection.Evidence.HeadSHA), shortOID(inspection.Evidence.ReflogHeadSHA)))
		}
		if len(signals) > 0 {
			inspection.Warning = "DANGER: " + strings.Join(signals, "; ") + ". Stop work in this lane and inspect it manually; do not commit, reset, clean, or check out anything."
		}
	}
	return inspections
}

func otherWorktreeLanes(inspections []SessionWorktreeInspection, group []int, self int) []string {
	seenPaths := map[string]bool{pathutil.ResolveForCompare(inspections[self].WorktreePath): true}
	var lanes []string
	for _, index := range group {
		path := pathutil.ResolveForCompare(inspections[index].WorktreePath)
		if index == self || seenPaths[path] {
			continue
		}
		seenPaths[path] = true
		lanes = append(lanes, fmt.Sprintf("%q", inspections[index].Title))
	}
	sort.Strings(lanes)
	if len(lanes) > maxNamedWorktreeSiblings {
		remaining := len(lanes) - maxNamedWorktreeSiblings
		lanes = append(lanes[:maxNamedWorktreeSiblings], fmt.Sprintf("and %d more", remaining))
	}
	return lanes
}

func shortOID(oid string) string {
	if len(oid) > 12 {
		return oid[:12]
	}
	return oid
}

// ReconcileWorktreeWarning mirrors the daemon's read-only worktree-integrity
// result onto an existing client projection without changing durable state.
func (i *Instance) ReconcileWorktreeWarning(warning string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.worktreeWarning == warning {
		return false
	}
	i.worktreeWarning = warning
	return true
}
