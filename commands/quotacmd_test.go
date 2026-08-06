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

// Codex #2996: the filter must exclude every state with no running agent, and
// must resolve liveness through the ROLLFORWARD — a pre-#1195 record carries the
// zero Liveness while its real state lives in the legacy status field, so reading
// data.Liveness directly counts an old archived row as a running session.
func TestQuotaSessionStates_ExcludesVanishedAgentsAndLegacyArchivedRecords(t *testing.T) {
	raw, err := json.Marshal([]session.InstanceData{
		{Title: "running", Program: "claude", Liveness: session.LiveRunning},
		{Title: "ready", Program: "claude", Liveness: session.LiveReady},
		{Title: "parked", Program: "claude", Liveness: session.LiveLimitReached},
		// Agent vanished: counting these turns a box full of dead rows into
		// "N session(s) running, none parked at a limit".
		{Title: "lost", Program: "claude", Liveness: session.LiveLost},
		{Title: "dead", Program: "claude", Liveness: session.LiveDead},
		// Pre-#1195: Liveness unset, real state in the legacy status field. A
		// legacy RUNNING record is the fixture that makes the rollforward
		// load-bearing — a legacy ARCHIVED one is excluded either way by the
		// allowlist, so it proves nothing. Without the rollforward this row
		// resolves to LivenessUnset and is dropped: a real session UNDER-reported,
		// which is the same dishonesty as inventing one. Verified by mutation.
		{Title: "legacy-running", Program: "claude", Status: session.Running},
		{Title: "legacy-archived", Program: "claude", Status: session.Archived},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	states, _ := quotaSessionStates(map[string]json.RawMessage{"repo": raw})
	if len(states) != 4 {
		t.Fatalf("states = %d, want 4 (running, ready, limit-reached, legacy-running): lost/dead have "+
			"no agent, the legacy ARCHIVED row must resolve to archived, and the legacy RUNNING row "+
			"must resolve to running rather than being dropped as unset", len(states))
	}
	parked := 0
	for _, state := range states {
		if state.LimitReached {
			parked++
		}
	}
	if parked != 1 {
		t.Fatalf("parked = %d, want 1: a limit-reached session still HAS an agent and is the report's strongest signal", parked)
	}
}

// A state nobody considered must not become evidence of a healthy account, which
// is why the running check is an allowlist.
func TestQuotaAgentIsRunning_UnsetLivenessIsNotRunning(t *testing.T) {
	if quotaAgentIsRunning(session.LivenessUnset) {
		t.Fatal("an unresolvable liveness was treated as a running agent; it is not evidence of anything")
	}
	if !quotaAgentIsRunning(session.LiveLimitReached) {
		t.Fatal("a limit-reached session has an agent parked at the wall and must count")
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
