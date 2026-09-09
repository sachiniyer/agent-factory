package session

import (
	"context"
	"errors"
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
// Err means the checkout could not be inspected. CorrelationErr means its local
// evidence was read, but another lane in the repository was unreadable, so an
// absent duplicate binding is not evidence that a prior warning cleared.
type SessionWorktreeInspection struct {
	InstanceID     string
	Title          string
	WorktreePath   string
	Warning        string
	Evidence       sessiongit.WorktreeIntegrity
	Err            error
	CorrelationErr error
}

// IncompleteError reports any local or repository-wide observation gap.
func (i SessionWorktreeInspection) IncompleteError() error {
	return errors.Join(i.Err, i.CorrelationErr)
}

// NeedsWorktreeIntegrityInspection distinguishes positively inapplicable rows
// (archived or non-local) from live local rows that must never disappear merely
// because their worktree metadata is incomplete.
func NeedsWorktreeIntegrityInspection(row InstanceData) bool {
	return !IsArchivedData(row) && row.UsesLocalTmux()
}

// InspectSessionWorktrees checks each live local worktree independently, then
// correlates their actual branches to expose a duplicate binding by lane name.
// Archived rows are intentionally excluded: their retained worktrees are inert.
func InspectSessionWorktrees(rows []InstanceData) []SessionWorktreeInspection {
	return InspectSessionWorktreesContext(context.Background(), rows)
}

// InspectSessionWorktreesContext is InspectSessionWorktrees with caller
// cancellation for the daemon's shutdown-aware diagnostic loop.
func InspectSessionWorktreesContext(ctx context.Context, rows []InstanceData) []SessionWorktreeInspection {
	eligible := make([]InstanceData, 0, len(rows))
	for _, row := range rows {
		if !NeedsWorktreeIntegrityInspection(row) {
			continue
		}
		eligible = append(eligible, row)
	}
	inspections := make([]SessionWorktreeInspection, len(eligible))
	repoKeys := make([]string, len(eligible))
	inspectable := make([]int, 0, len(eligible))
	for index, row := range eligible {
		inspections[index] = SessionWorktreeInspection{
			InstanceID: row.ID, Title: row.Title, WorktreePath: row.Worktree.WorktreePath,
		}
		if strings.TrimSpace(row.Worktree.RepoPath) != "" {
			repoKeys[index] = pathutil.ResolveForCompare(row.Worktree.RepoPath)
		}
		switch {
		case strings.TrimSpace(row.Worktree.WorktreePath) == "":
			inspections[index].Err = fmt.Errorf("live local lane has no worktree path; worktree safety is unknown")
		case strings.TrimSpace(row.Worktree.RepoPath) == "":
			inspections[index].Err = fmt.Errorf("live local lane has no repository identity; branch correlation is unknown")
		default:
			inspectable = append(inspectable, index)
		}
	}
	workers := min(maxConcurrentWorktreeInspections, len(inspectable))
	jobs := make(chan int, len(inspectable))
	for _, index := range inspectable {
		jobs <- index
	}
	close(jobs)
	var wg sync.WaitGroup
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for index := range jobs {
				evidence, err := sessiongit.InspectWorktreeIntegrityContext(ctx, eligible[index].Worktree.WorktreePath)
				inspections[index].Evidence = evidence
				inspections[index].Err = err
			}
		}()
	}
	wg.Wait()

	// Correlation is a repository-wide observation. One unreadable lane makes an
	// absent sibling in the readable subset unknown, never clean.
	incompleteRepos := make(map[string]bool)
	unknownRepo := false
	for index, inspection := range inspections {
		if inspection.Err == nil {
			continue
		}
		if repoKeys[index] == "" {
			unknownRepo = true
		} else {
			incompleteRepos[repoKeys[index]] = true
		}
	}
	for index := range inspections {
		if inspections[index].Err != nil {
			continue
		}
		var correlationErrors []error
		if unknownRepo {
			correlationErrors = append(correlationErrors, fmt.Errorf("cannot safely correlate live branch bindings because another live lane has no repository identity"))
		}
		if incompleteRepos[repoKeys[index]] {
			correlationErrors = append(correlationErrors, fmt.Errorf("cannot safely correlate live branch bindings because another worktree in this repository could not be inspected"))
		}
		inspections[index].CorrelationErr = errors.Join(correlationErrors...)
	}

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
		siblings := otherWorktreeLanes(inspections, groups[key], index)
		if len(siblings) > 0 {
			signals = append(signals, fmt.Sprintf("branch %q is also checked out by live lane(s) %s", inspection.Evidence.Branch, strings.Join(siblings, ", ")))
		}
		// A fully staged large commit has the same raw index shape. Only surface
		// it when a duplicate live binding or the worktree-local reflog also says
		// this checkout may have followed a sibling's ref move.
		corroboratedTakeover := len(siblings) > 0 || inspection.Evidence.HeadMovedWithoutReflog
		if inspection.Evidence.MassRevert && corroboratedTakeover {
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

// SameWorktreeInspectionIdentity reports whether two projections describe the
// same safety-scan target. Liveness matters only at the live-local applicability
// boundary; ordinary Ready/Running transitions do not change the checkout.
func SameWorktreeInspectionIdentity(before, after InstanceData) bool {
	return before.ID == after.ID &&
		before.Title == after.Title &&
		before.BackendType == after.BackendType &&
		NeedsWorktreeIntegrityInspection(before) == NeedsWorktreeIntegrityInspection(after) &&
		before.Worktree.RepoPath == after.Worktree.RepoPath &&
		before.Worktree.WorktreePath == after.Worktree.WorktreePath
}

// ReconcileWorktreeWarning mirrors the daemon's read-only worktree-integrity
// result onto an existing client projection without changing durable state.
func (i *Instance) ReconcileWorktreeWarning(warning string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.reconcileWorktreeWarningLocked(warning)
}

func (i *Instance) reconcileWorktreeWarningLocked(warning string) bool {
	i.confirmedWorktreeWarning = warning
	if i.worktreeWarning == warning {
		return false
	}
	i.worktreeWarning = warning
	return true
}

// ReconcileWorktreeInspection applies a complete observation or fails closed
// around an incomplete one. Only a complete scan may clear the last confirmed
// danger; an incomplete scan retains it and says why safety was not established.
func (i *Instance) ReconcileWorktreeInspection(warning string, incomplete error) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.reconcileWorktreeInspectionLocked(warning, incomplete)
}

// ReconcileWorktreeInspectionIfCurrent atomically verifies that an observation
// still describes this lane and applies it under the same instance lock. A
// restore or backend/worktree replacement in the gap after a Git scan therefore
// leaves the existing warning untouched.
func (i *Instance) ReconcileWorktreeInspectionIfCurrent(snapshot InstanceData, warning string, incomplete error) (changed, applied bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if !SameWorktreeInspectionIdentity(snapshot, i.toInstanceDataLocked()) {
		return false, false
	}
	return i.reconcileWorktreeInspectionLocked(warning, incomplete), true
}

func (i *Instance) reconcileWorktreeInspectionLocked(warning string, incomplete error) bool {
	if incomplete == nil {
		return i.reconcileWorktreeWarningLocked(warning)
	}
	// A warning is positive evidence even when repository-wide correlation was
	// incomplete. Replace older danger text with the newest definite finding;
	// retain the old finding only when the partial scan found nothing new.
	if warning != "" {
		i.confirmedWorktreeWarning = warning
	}
	rendered := incompleteWorktreeWarning(i.confirmedWorktreeWarning, incomplete)
	if i.worktreeWarning == rendered {
		return false
	}
	i.worktreeWarning = rendered
	return true
}

func incompleteWorktreeWarning(confirmed string, incomplete error) string {
	if confirmed != "" {
		return fmt.Sprintf("%s The latest worktree safety scan could not be verified (%v), so this confirmed danger remains until a complete scan succeeds.", confirmed, incomplete)
	}
	return fmt.Sprintf("DANGER: worktree safety could not be verified: %v. Treat this lane as unsafe until a complete scan succeeds; do not commit, reset, clean, or check out anything.", incomplete)
}
