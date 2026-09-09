package doctor

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/session"
)

func checkWorktreeIntegrity(ctx *scanContext, report *Report) {
	rows, err := ctx.opts.worktreeInventory()
	if err != nil {
		report.markIncomplete("worktree-integrity")
		report.Warn(sectionProcesses, "worktree-integrity",
			fmt.Sprintf("could not read the live session inventory, so worktree safety is unknown: %v", err),
			"restore daemon access and rerun `af doctor`; no worktree was changed", true)
		return
	}
	checkWorktreeIntegrityRows(report, session.InspectSessionWorktrees(rows))
}

func checkWorktreeIntegrityRows(report *Report, inspections []session.SessionWorktreeInspection) {
	var dangers []string
	var unreadable []string
	for _, inspection := range inspections {
		if inspection.Err != nil {
			unreadable = append(unreadable, fmt.Sprintf("%q (%v)", inspection.Title, inspection.Err))
			continue
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
