package app

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/quota"
	"github.com/stretchr/testify/assert"
)

// applyQuotaToPane threads resp.Accounts (#6776) from the daemon's
// QuotaReportResponse into the config pane's per-account evidence section.
// This proves the end-to-end TUI path the bug report said was missing: the
// daemon serves accounts, app/quota_load.go maps them, and the pane renders
// them — the same words `af quota` prints.
func TestApplyQuotaToPane_ThreadsAccountsIntoPane(t *testing.T) {
	h := newTestHome(t)
	h.configPane.SetSize(120, 40)
	resp := daemon.QuotaReportResponse{
		Agents: []quota.RenderedAgent{{
			Program: "codex", Quota: "not reported", Observed: "limit reached",
			Sessions: 1, LimitedSessions: 1, Detail: "parked",
		}},
		Accounts: []quota.RenderedAccount{
			{Agent: "codex", Name: "work", Observed: "observed", Windows: "primary 42%", Detail: "recorded 2026-01-01T09:00:00Z"},
			{Agent: "codex", Name: "personal", Observed: "no evidence", Windows: "—", Detail: "no rate_limits claims found"},
		},
		Mover: "shadow",
	}
	h.applyQuotaToPane(resp, nil)

	out := h.configPane.String()
	for _, want := range []string{
		"Account evidence",
		"codex/work",
		"observed",
		"primary 42%",
		"recorded 2026-01-01T09:00:00Z",
		"codex/personal",
		"no evidence",
		"no rate_limits claims found",
		"mover: shadow",
		"ACCOUNTS is what af recorded",
	} {
		assert.True(t, strings.Contains(out, want), "pane is missing %q:\n%s", want, out)
	}
}

// The absent-table shape (no accounts, no mover) renders no per-account
// evidence section — byte-identical to before the machinery existed.
func TestApplyQuotaToPane_AbsentTableRendersNoAccountsSection(t *testing.T) {
	h := newTestHome(t)
	h.configPane.SetSize(120, 40)
	resp := daemon.QuotaReportResponse{
		Agents: []quota.RenderedAgent{{
			Program: "codex", Quota: "not reported", Observed: "no sessions", Detail: "none",
		}},
	}
	h.applyQuotaToPane(resp, nil)

	out := h.configPane.String()
	for _, absent := range []string{"Account evidence", "mover:", "ACCOUNTS is what af recorded"} {
		assert.False(t, strings.Contains(out, absent), "absent-table shape must not render %q:\n%s", absent, out)
	}
}

// The empty-accounts + mover-served case renders the "(no codex accounts
// registered)" line and the mover line — the section exists, the table is empty.
func TestApplyQuotaToPane_MoverOnlyRendersEmptyAccountsLine(t *testing.T) {
	h := newTestHome(t)
	h.configPane.SetSize(120, 40)
	resp := daemon.QuotaReportResponse{
		Agents: []quota.RenderedAgent{{
			Program: "codex", Quota: "not reported", Observed: "no sessions", Detail: "none",
		}},
		Mover: "halted: move-failed since 2026-01-01T09:00:00Z",
	}
	h.applyQuotaToPane(resp, nil)

	out := h.configPane.String()
	assert.Contains(t, out, "(no codex accounts registered)")
	assert.Contains(t, out, "mover: halted: move-failed")
}
