package quotahost

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// agentName exists because of a bug that only appeared when the command was
// run against a real AF home: InstanceData.Program is the RESOLVED command, not
// the agent enum, so with program_overrides configured the table grew a row
// titled "/home/…/claude --dangerously-skip-permissions" beside the real
// "claude" row — the same agent, reported twice, each undercounted.
func TestAgentName_CollapsesResolvedCommandsToTheAgentEnum(t *testing.T) {
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
			if got := agentName(tc.program); got != tc.want {
				t.Fatalf("agentName(%q) = %q, want %q", tc.program, got, tc.want)
			}
		})
	}
}

// Archived and tombstoned rows are not running an agent, so they must not pad
// the session counts the report presents as current usage.
func TestSessionStates_ExcludesInertRowsAndCarriesLimitState(t *testing.T) {
	reset := time.Unix(1_700_000_000, 0).UTC()
	observed := time.Unix(1_690_000_000, 0).UTC()
	raw, err := json.Marshal([]session.InstanceData{
		{Title: "live", Program: "claude", Liveness: session.LiveRunning},
		{Title: "parked", Program: "codex", Liveness: session.LiveLimitReached,
			LimitResetAt: reset, LimitObservedAt: observed},
		{Title: "archived", Program: "claude", Liveness: session.LiveArchived},
		{Title: "killed", Program: "claude", Liveness: session.LiveRunning, UserKilled: true},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	states, unreadable := sessionStates(map[string]json.RawMessage{"repo": raw})
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
			if !state.ObservedAt.Equal(observed) {
				t.Fatalf("ObservedAt = %v, want %v carried through (#4361)", state.ObservedAt, observed)
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
func TestSessionStates_ExcludesVanishedAgentsAndLegacyArchivedRecords(t *testing.T) {
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

	states, _ := sessionStates(map[string]json.RawMessage{"repo": raw})
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
func TestAgentIsRunning_UnsetLivenessIsNotRunning(t *testing.T) {
	if agentIsRunning(session.LivenessUnset) {
		t.Fatal("an unresolvable liveness was treated as a running agent; it is not evidence of anything")
	}
	if !agentIsRunning(session.LiveLimitReached) {
		t.Fatal("a limit-reached session has an agent parked at the wall and must count")
	}
}

// A record blob that will not parse is COUNTED, never silently dropped: it might
// have been the parked one, and a report that hides it reads as authoritative.
func TestSessionStates_CountsUnparseableRecordsRatherThanHidingThem(t *testing.T) {
	states, unreadable := sessionStates(map[string]json.RawMessage{
		"broken": json.RawMessage(`{"not":"an array"}`),
	})
	if unreadable != 1 {
		t.Fatalf("unreadable = %d, want 1: an unparseable record must be reported, not swallowed", unreadable)
	}
	if len(states) != 0 {
		t.Fatalf("states = %d, want 0", len(states))
	}
}

// Report reads real on-disk records end to end: the daemon RPC and the local
// CLI both consume this, so a skipped or unparseable repo must ride the answer
// as a caveat rather than letting the report look complete.
func TestReport_CarriesUnderReadCaveatsAndObservationTimes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("AGENT_FACTORY_HOME", home)

	observed := time.Unix(1_690_000_000, 0).UTC()
	reset := observed.Add(6 * 24 * time.Hour)
	good, err := json.Marshal([]session.InstanceData{
		{Title: "parked", Program: "codex", Liveness: session.LiveLimitReached,
			LimitResetAt: reset, LimitObservedAt: observed},
		{Title: "live", Program: "claude", Liveness: session.LiveRunning},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := config.SaveRepoInstances("goodrepo", good); err != nil {
		t.Fatalf("SaveRepoInstances: %v", err)
	}
	// A record file that will not parse must surface as a caveat, not silence.
	// SaveRepoInstances validates content, so the corrupt fixture is written
	// directly at the path the loader reads.
	brokenDir := filepath.Join(home, "instances", "brokenrepo")
	if err := os.MkdirAll(brokenDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(brokenDir, "instances.json"),
		[]byte(`{"not":"an array"}`), 0o600); err != nil {
		t.Fatalf("write broken instances: %v", err)
	}

	result, err := Report(time.Unix(1_800_000_000, 0))
	if err != nil {
		t.Fatalf("Report: %v", err)
	}
	if len(result.Caveats) == 0 {
		t.Fatal("an unparseable repo must produce a caveat; the report is INCOMPLETE")
	}
	var foundCodex, foundClaude bool
	for _, row := range result.Rows {
		switch row.Agent {
		case "codex":
			foundCodex = true
			if row.Observed != "limit reached" {
				t.Fatalf("codex observed = %q, want limit reached", row.Observed)
			}
			if !strings.Contains(row.Detail, observed.Format(time.RFC3339)) {
				t.Fatalf("codex detail must name the sighting time (#4361), got %q", row.Detail)
			}
		case tmux.ProgramClaude:
			foundClaude = true
			if row.Observed != "no limit seen" {
				t.Fatalf("claude observed = %q, want no limit seen", row.Observed)
			}
		}
	}
	if !foundCodex || !foundClaude {
		t.Fatalf("rows = %+v, want codex and claude rows", result.Rows)
	}
	if result.Note == "" {
		t.Fatal("the report must carry its framing note for surfaces to render")
	}
}
