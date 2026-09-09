package doctor

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDoctorReportsWorktreeIntegrityDangerWithoutFix(t *testing.T) {
	report := &Report{}
	checkWorktreeIntegrityRows(report, []session.SessionWorktreeInspection{{
		Title:   "idle-lane",
		Warning: "DANGER: 2063 staged paths and zero unstaged paths; HEAD moved without a worktree-local reflog entry",
	}})

	rows := make([]CheckResult, 0)
	for _, row := range report.Checks {
		if row.Name == "worktree-integrity" {
			rows = append(rows, row)
		}
	}
	require.Len(t, rows, 1)
	assert.Equal(t, StatusFail, rows[0].Status)
	assert.True(t, rows[0].Problem)
	assert.Contains(t, rows[0].Detail, "idle-lane")
	assert.Contains(t, rows[0].Remediation, "does not reset or clean")
	assert.Empty(t, report.Findings, "the read-only check must never carry a --fix action")
}

func TestDoctorPassesCleanWorktreeIntegrityScan(t *testing.T) {
	report := &Report{}
	checkWorktreeIntegrityRows(report, []session.SessionWorktreeInspection{{Title: "editing-lane"}})
	require.Len(t, report.Checks, 1)
	assert.Equal(t, StatusPass, report.Checks[0].Status)
}
