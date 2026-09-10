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
	if row.InFlightOp == OpCreating || (row.InFlightOp == OpNone && row.Status == Loading) {
		return false
	}
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

	// Branch correlation is a repository-wide observation, so its validation must
	// be repository-wide too. One `git worktree list` reads every registered
	// path/branch/HEAD relation at a common command boundary. Matching each local
	// status result against that snapshot rejects a checkout that changed after
	// its own probe; unlike independent second passes, it cannot validate peer B
	// and then validate A against a stale view of B.
	repoIndices := make(map[string][]int)
	repoPaths := make(map[string]string)
	for _, index := range inspectable {
		key := repoKeys[index]
		if key == "" {
			continue
		}
		repoIndices[key] = append(repoIndices[key], index)
		if repoPaths[key] == "" {
			repoPaths[key] = eligible[index].Worktree.RepoPath
		}
	}
	repoErrors := observeWorktreeCohorts(ctx, repoIndices, repoPaths, eligible, inspections)

	// One unreadable lane or repository snapshot makes an absent sibling in the
	// readable subset unknown, never clean.
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
	for repoKey, err := range repoErrors {
		if err != nil {
			incompleteRepos[repoKey] = true
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
			cause := repoErrors[repoKeys[index]]
			if cause != nil {
				correlationErrors = append(correlationErrors, fmt.Errorf("cannot safely correlate live branch bindings because the repository cohort could not be completely observed: %w", cause))
			} else {
				correlationErrors = append(correlationErrors, fmt.Errorf("cannot safely correlate live branch bindings because another worktree in this repository could not be inspected"))
			}
		}
		inspections[index].CorrelationErr = errors.Join(correlationErrors...)
	}

	branchGroups := make(map[string][]int)
	pathGroups := make(map[string][]int)
	for index, inspection := range inspections {
		if inspection.Err != nil {
			continue
		}
		pathKey := repoKeys[index] + "\x00path\x00" + pathutil.ResolveForCompare(inspection.WorktreePath)
		pathGroups[pathKey] = append(pathGroups[pathKey], index)
		if inspection.Evidence.Branch != "" {
			branchKey := repoKeys[index] + "\x00branch\x00" + inspection.Evidence.Branch
			branchGroups[branchKey] = append(branchGroups[branchKey], index)
		}
	}
	for index := range inspections {
		inspection := &inspections[index]
		if inspection.Err != nil {
			continue
		}
		var signals []string
		pathKey := repoKeys[index] + "\x00path\x00" + pathutil.ResolveForCompare(inspection.WorktreePath)
		pathSiblings := otherWorktreeLanes(inspections, pathGroups[pathKey], index)
		if len(pathSiblings) > 0 {
			signals = append(signals, fmt.Sprintf("worktree %q is also used by live lane(s) %s", inspection.WorktreePath, strings.Join(pathSiblings, ", ")))
		}
		var branchSiblings []string
		if inspection.Evidence.Branch != "" {
			branchKey := repoKeys[index] + "\x00branch\x00" + inspection.Evidence.Branch
			branchSiblings = otherWorktreeLanes(inspections, branchGroups[branchKey], index)
			if len(branchSiblings) > 0 {
				signals = append(signals, fmt.Sprintf("branch %q is also checked out by live lane(s) %s", inspection.Evidence.Branch, strings.Join(branchSiblings, ", ")))
			}
		}
		// A fully staged large commit has the same raw index shape. Only surface
		// it when a duplicate live binding or the worktree-local reflog also says
		// this checkout may have followed a sibling's ref move.
		corroboratedTakeover := len(pathSiblings) > 0 || len(branchSiblings) > 0 || inspection.Evidence.HeadMovedWithoutReflog
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

func observeWorktreeCohorts(
	ctx context.Context,
	repoIndices map[string][]int,
	repoPaths map[string]string,
	rows []InstanceData,
	inspections []SessionWorktreeInspection,
) map[string]error {
	errs := make(map[string]error)
	for repoKey, indices := range repoIndices {
		bindings, err := sessiongit.WorktreeBranchBindingsContext(ctx, repoPaths[repoKey])
		if err != nil {
			errs[repoKey] = err
			continue
		}
		byPath := make(map[string]sessiongit.WorktreeBranchBinding, len(bindings))
		for _, binding := range bindings {
			byPath[pathutil.ResolveForCompare(binding.Path)] = binding
		}
		for _, index := range indices {
			if inspections[index].Err != nil {
				continue
			}
			binding, ok := byPath[pathutil.ResolveForCompare(rows[index].Worktree.WorktreePath)]
			switch {
			case !ok:
				errs[repoKey] = errors.Join(errs[repoKey], fmt.Errorf("worktree %q is absent from the repository-wide branch snapshot", rows[index].Worktree.WorktreePath))
			case !resolveAmbiguousBranchHead(&inspections[index].Evidence, binding):
				errs[repoKey] = errors.Join(errs[repoKey], fmt.Errorf("worktree %q changed before repository-wide branch correlation", rows[index].Worktree.WorktreePath))
			case binding.HeadSHA != inspections[index].Evidence.HeadSHA:
				errs[repoKey] = errors.Join(errs[repoKey], fmt.Errorf("worktree %q changed before repository-wide branch correlation", rows[index].Worktree.WorktreePath))
			}
		}
	}
	return errs
}

func resolveAmbiguousBranchHead(evidence *sessiongit.WorktreeIntegrity, binding sessiongit.WorktreeBranchBinding) bool {
	if !evidence.BranchHeadAmbiguous {
		return !binding.Detached && binding.Branch == evidence.Branch
	}
	switch {
	case binding.Detached && binding.Branch == "":
		evidence.Branch = ""
		evidence.BranchHeadAmbiguous = false
		return true
	case !binding.Detached && binding.Branch == "(detached)":
		evidence.BranchHeadAmbiguous = false
		return true
	default:
		return false
	}
}

func otherWorktreeLanes(inspections []SessionWorktreeInspection, group []int, self int) []string {
	seenLanes := map[string]bool{worktreeInspectionLaneIdentity(inspections[self]): true}
	var lanes []string
	for _, index := range group {
		identity := worktreeInspectionLaneIdentity(inspections[index])
		if index == self || seenLanes[identity] {
			continue
		}
		seenLanes[identity] = true
		lanes = append(lanes, fmt.Sprintf("%q", inspections[index].Title))
	}
	sort.Strings(lanes)
	if len(lanes) > maxNamedWorktreeSiblings {
		remaining := len(lanes) - maxNamedWorktreeSiblings
		lanes = append(lanes[:maxNamedWorktreeSiblings], fmt.Sprintf("and %d more", remaining))
	}
	return lanes
}

func worktreeInspectionLaneIdentity(inspection SessionWorktreeInspection) string {
	if inspection.InstanceID != "" {
		return "id\x00" + inspection.InstanceID
	}
	// Legacy rows have no stable ID. Title plus normalized worktree path is the
	// same fallback identity used when doctor merges live and persisted copies.
	// The path is deliberately NOT identity on its own: two --here lanes can
	// share one worktree, and that is a duplicate live branch binding to report.
	return "legacy\x00" + inspection.Title + "\x00" + pathutil.ResolveForCompare(inspection.WorktreePath)
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

// WorktreeInspectionReconciliation is one row in a correlated worktree scan.
type WorktreeInspectionReconciliation struct {
	Instance   *Instance
	Snapshot   InstanceData
	Warning    string
	Incomplete error
}

// ReconcileWorktreeInspectionCohortIfCurrent atomically validates and applies a
// correlated scan. A clean result for one lane depends on every peer that was
// correlated against, so the cohort -- not the row being written -- is the CAS
// unit: every instance identity and every external dependency must remain fixed
// from validation through application, or no warning changes at all.
//
// holdDependencies runs before any instance is locked. When non-nil, it must
// atomically validate the scan's non-instance dependencies and return a release
// function that keeps them stable through application. It must not inspect or
// mutate an Instance. Taking the dependency lock first lets callers preserve
// their established outer-to-instance lock order.
func ReconcileWorktreeInspectionCohortIfCurrent(
	updates []WorktreeInspectionReconciliation,
	holdDependencies func() (current bool, release func()),
) (changed []bool, applied bool) {
	if holdDependencies != nil {
		current, release := holdDependencies()
		if release != nil {
			defer release()
		}
		if !current || release == nil {
			return nil, false
		}
	}

	type lockTarget struct {
		instance *Instance
		key      string
	}
	targets := make([]lockTarget, 0, len(updates))
	seen := make(map[*Instance]bool, len(updates))
	for _, update := range updates {
		if update.Instance == nil || seen[update.Instance] {
			return nil, false
		}
		seen[update.Instance] = true
		targets = append(targets, lockTarget{
			instance: update.Instance,
			key:      fmt.Sprintf("%p", update.Instance),
		})
	}
	sort.Slice(targets, func(left, right int) bool {
		return targets[left].key < targets[right].key
	})
	for _, target := range targets {
		target.instance.mu.Lock()
	}
	defer func() {
		for index := len(targets) - 1; index >= 0; index-- {
			targets[index].instance.mu.Unlock()
		}
	}()

	for _, update := range updates {
		if !SameWorktreeInspectionIdentity(update.Snapshot, update.Instance.toInstanceDataLocked()) {
			return nil, false
		}
	}
	changed = make([]bool, len(updates))
	for index, update := range updates {
		changed[index] = update.Instance.reconcileWorktreeInspectionLocked(update.Warning, update.Incomplete)
	}
	return changed, true
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
