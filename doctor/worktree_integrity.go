package doctor

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/internal/pathutil"
	"github.com/sachiniyer/agent-factory/session"
)

// completeWorktreeInventory joins the daemon's freshest projections with every
// persisted row. The daemon intentionally skips records it cannot materialize;
// those rows still own worktrees and must remain in a safety correlation.
func completeWorktreeInventory() ([]session.InstanceData, error) {
	live, liveErr := daemonSessionInventory()
	persisted, persistedErr := persistedWorktreeInventory()
	// Preserve every definite observation from either source. The caller carries
	// every failed half alongside the merged rows, so readable evidence can prove
	// danger while an incomplete inventory can never prove safety.
	var incomplete []error
	if liveErr != nil {
		incomplete = append(incomplete, fmt.Errorf("could not read live daemon session inventory: %w", liveErr))
	}
	if persistedErr != nil {
		incomplete = append(incomplete, fmt.Errorf("could not read persisted session inventory: %w", persistedErr))
	}
	return mergeWorktreeInventories(live, persisted), errors.Join(incomplete...)
}

func mergeWorktreeInventories(live, persisted []session.InstanceData) []session.InstanceData {
	merged := append([]session.InstanceData(nil), persisted...)
	positions := make(map[string]int, len(merged))
	for index, row := range merged {
		positions[worktreeInventoryIdentity(row)] = index
	}
	for _, row := range live {
		key := worktreeInventoryIdentity(row)
		if index, ok := positions[key]; ok {
			merged[index] = row
			continue
		}
		positions[key] = len(merged)
		merged = append(merged, row)
	}
	return merged
}

func worktreeInventoryIdentity(row session.InstanceData) string {
	if row.ID != "" {
		return "id\x00" + row.ID
	}
	return "legacy\x00" + pathutil.ResolveForCompare(row.Worktree.RepoPath) + "\x00" +
		pathutil.ResolveForCompare(row.Worktree.WorktreePath) + "\x00" + row.Title
}

func checkWorktreeIntegrity(ctx *scanContext, report *Report, health daemon.HealthStatus) {
	inventory := ctx.opts.worktreeInventory
	inventoryName := "live session"
	if worktreeInventoryInapplicable(ctx, health) {
		// A stopped daemon leaves local tmux sessions running for the next daemon
		// to re-adopt. Disk is therefore the authoritative fallback, not evidence
		// that there are no live lanes.
		inventory = persistedWorktreeInventory
		inventoryName = "persisted session"
	}
	rows, err := inventory()
	if err != nil && len(rows) == 0 {
		report.markIncomplete("worktree-integrity")
		report.Warn(sectionProcesses, "worktree-integrity",
			fmt.Sprintf("could not read the %s inventory, so worktree safety is unknown: %v", inventoryName, err),
			"restore access to the session inventory and rerun `af doctor`; no worktree was changed", true)
		return
	}
	inspections := session.InspectSessionWorktrees(rows)
	if err != nil {
		// A partial inventory may still contain positive danger evidence. Append
		// the observation gap to the same result set: warnings remain failures,
		// while a clean live subset remains unknown rather than passing.
		inspections = append(inspections, session.SessionWorktreeInspection{
			Title:          inventoryName + " inventory",
			CorrelationErr: err,
		})
	}
	checkWorktreeIntegrityRows(report, inspections)
}

func persistedWorktreeInventory() ([]session.InstanceData, error) {
	records, skipped, err := config.LoadAllRepoInstancesReportingSkipDetails()
	if err != nil {
		return nil, err
	}
	var incomplete []error
	if len(skipped) > 0 {
		incomplete = append(incomplete, fmt.Errorf("persisted session inventory is incomplete: %s", config.DescribeRepoInstancesSkips(skipped)))
	}
	repoIDs := make([]string, 0, len(records))
	for repoID := range records {
		repoIDs = append(repoIDs, repoID)
	}
	sort.Strings(repoIDs)
	var rows []session.InstanceData
	for _, repoID := range repoIDs {
		var repoRows []session.InstanceData
		if err := json.Unmarshal(records[repoID], &repoRows); err != nil {
			incomplete = append(incomplete, fmt.Errorf("could not parse persisted sessions for repository %s: %w", repoID, err))
			continue
		}
		rows = append(rows, repoRows...)
	}
	return rows, errors.Join(incomplete...)
}

func checkWorktreeIntegrityRows(report *Report, inspections []session.SessionWorktreeInspection) {
	var dangers []string
	var unreadable []string
	for _, inspection := range inspections {
		if incomplete := inspection.IncompleteError(); incomplete != nil {
			unreadable = append(unreadable, fmt.Sprintf("%q (%v)", inspection.Title, incomplete))
		}
		if inspection.Warning != "" {
			dangers = append(dangers, fmt.Sprintf("lane %q: %s", inspection.Title, inspection.Warning))
		}
	}
	if len(unreadable) > 0 {
		report.markIncomplete("worktree-integrity")
	}
	if len(dangers) > 0 {
		detail := strings.Join(dangers, " | ")
		if len(unreadable) > 0 {
			detail += " | inspection was also incomplete for " + strings.Join(unreadable, "; ")
		}
		report.Fail(sectionProcesses, "worktree-integrity", detail,
			"stop work in the named lane(s) and inspect Git state manually before committing; doctor only reports this condition and does not reset or clean anything")
		return
	}
	if len(unreadable) > 0 {
		report.Warn(sectionProcesses, "worktree-integrity",
			"could not inspect every live local worktree: "+strings.Join(unreadable, "; "),
			"inspect those paths manually and rerun `af doctor`; no worktree was changed", true)
		return
	}
	report.Pass(sectionProcesses, "worktree-integrity", fmt.Sprintf("no dangerous shape in %d live local worktree(s)", len(inspections)))
}

// worktreeInventoryInapplicable distinguishes a stopped daemon from an
// unreachable one. A missing socket alone is not enough: a same-home daemon or
// an owned daemon whose home could not be read may still have live lanes.
func worktreeInventoryInapplicable(ctx *scanContext, health daemon.HealthStatus) bool {
	if health.SocketErr != nil || health.SocketExists || health.PingErr == nil || health.PIDVerified || ctx.snapErr != nil {
		return false
	}
	activeHome := normalizeHome(ctx.opts.ConfigDir)
	for _, candidate := range ctx.daemonProcs() {
		if candidate.ownedByUs && (!candidate.homeKnown || candidate.home == activeHome) {
			return false
		}
	}
	return true
}
