package commands

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/session"
)

// quotaAgentName exists because of a bug that only appeared when the command was
// run against a real AF home: InstanceData.Program is the RESOLVED command, not
// the agent enum, so with program_overrides configured the table grew a row
// titled "/home/…/claude --dangerously-skip-permissions" beside the real
// "claude" row — the same agent, reported twice, each undercounted.
func TestQuotaAgentName_CollapsesResolvedCommandsToTheAgentEnum(t *testing.T) {
	for _, tc := range []struct {
		name    string
		program string
		want    string
	}{
		{"bare enum", "claude", "claude"},
		{"absolute path with flags", "/home/u/.local/bin/claude --dangerously-skip-permissions", "claude"},
		{"wrapper prefix", "ionice -c 3 codex", "codex"},
		{"env assignment prefix", "FOO=bar gemini", "gemini"},
		// Not attributable: kept verbatim rather than dropped or bucketed. The
		// session is real, and hiding it under-reports.
		{"unrelated command", "/opt/claude-wrapper/run", "/opt/claude-wrapper/run"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := quotaAgentName(tc.program); got != tc.want {
				t.Fatalf("quotaAgentName(%q) = %q, want %q", tc.program, got, tc.want)
			}
		})
	}
}

// Archived and tombstoned rows are not running an agent, so they must not pad
// the session counts the report presents as current usage.
func TestQuotaSessionStates_ExcludesInertRowsAndCarriesLimitState(t *testing.T) {
	reset := time.Unix(1_700_000_000, 0).UTC()
	raw, err := json.Marshal([]session.InstanceData{
		{Title: "live", Program: "claude", Liveness: session.LiveRunning},
		{Title: "parked", Program: "codex", Liveness: session.LiveLimitReached, LimitResetAt: reset},
		{Title: "archived", Program: "claude", Liveness: session.LiveArchived},
		{Title: "killed", Program: "claude", Liveness: session.LiveRunning, UserKilled: true},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	states, unreadable := quotaSessionStates(map[string]json.RawMessage{"repo": raw})
	if unreadable != 0 {
		t.Fatalf("unreadable = %d, want 0", unreadable)
	}
	if len(states) != 2 {
		t.Fatalf("states = %d (%+v), want 2: archived and tombstoned rows run no agent", len(states), states)
	}
	var parked int
	for _, state := range states {
		if state.LimitReached {
			parked++
			if state.Program != "codex" {
				t.Fatalf("parked session program = %q, want codex", state.Program)
			}
			if !state.ResetAt.Equal(reset) {
				t.Fatalf("ResetAt = %v, want %v carried through", state.ResetAt, reset)
			}
		}
	}
	if parked != 1 {
		t.Fatalf("parked = %d, want 1", parked)
	}
}

// A record blob that will not parse is COUNTED, never silently dropped: it might
// have been the parked one, and a report that hides it reads as authoritative.
func TestQuotaSessionStates_CountsUnparseableRecordsRatherThanHidingThem(t *testing.T) {
	states, unreadable := quotaSessionStates(map[string]json.RawMessage{
		"broken": json.RawMessage(`{"not":"an array"}`),
	})
	if unreadable != 1 {
		t.Fatalf("unreadable = %d, want 1: an unparseable record must be reported, not swallowed", unreadable)
	}
	if len(states) != 0 {
		t.Fatalf("states = %d, want 0", len(states))
	}
}
