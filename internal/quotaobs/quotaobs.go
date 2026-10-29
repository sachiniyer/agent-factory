// Package quotaobs is the one records→Report read every quota surface shares:
// it loads this af home's repo instance records, projects them onto the states
// the report counts, and hands back the Report plus what a renderer must know
// about its completeness. `af quota`, the daemon's QuotaReport RPC, and through
// it the TUI and web all feed on this same path, so the surfaces cannot
// disagree about what af has observed — the drift the parity audit exists to
// catch (#2983).
//
// Keeping the collector out of quota/ preserves that package's stated purity —
// it depends on no session/config types and stays a pure report builder.
package quotaobs

import (
	"encoding/json"
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/quota"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Result is the report plus what a renderer needs to know about its
// completeness: a skipped or unparseable record may be hiding exactly the
// parked session the report exists to find, so the caveat rides with the data
// rather than in a log line nobody assembling the report reads.
type Result struct {
	Report quota.Report
	// SkippedRepos are the repo IDs whose record files could not be read.
	SkippedRepos []string
	// UnreadableRecords counts record blobs that failed to parse.
	UnreadableRecords int
}

// Warnings renders the completeness caveats as sentences. Every surface prints
// this same wording — a silent omission is how a partial table gets mistaken
// for the truth.
func (r Result) Warnings() []string {
	var warnings []string
	if len(r.SkippedRepos) > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%d project record file(s) could not be read, so this report is INCOMPLETE; sessions in them are not counted: %v",
			len(r.SkippedRepos), r.SkippedRepos))
	}
	if r.UnreadableRecords > 0 {
		warnings = append(warnings, fmt.Sprintf(
			"%d project record file(s) could not be parsed and were skipped, so this report is INCOMPLETE",
			r.UnreadableRecords))
	}
	return warnings
}

// Collect reads every repo's instance records and builds the quota report —
// the same read `af quota` makes. A store-level failure is returned as the
// error; per-file failures are reported through the Result, never silently
// dropped.
func Collect() (Result, error) {
	records, skipped, err := config.LoadAllRepoInstancesReportingSkips()
	if err != nil {
		return Result{}, fmt.Errorf("cannot read session records: %w", err)
	}
	states, unreadable := sessionStates(records)
	return Result{
		Report:            quota.Build(tmux.SupportedPrograms, states),
		SkippedRepos:      skipped,
		UnreadableRecords: unreadable,
	}, nil
}

// sessionStates projects the raw per-repo record blobs onto the states Build
// consumes. A blob that will not parse is COUNTED, not dropped: it might have
// been the parked session, and a report that hides it reads as authoritative.
func sessionStates(raw map[string]json.RawMessage) (states []quota.SessionState, unreadable int) {
	for _, blob := range raw {
		var instances []session.InstanceData
		if err := json.Unmarshal(blob, &instances); err != nil {
			unreadable++
			continue
		}
		for _, data := range instances {
			// EffectiveLiveness runs the pre-#1195 rollforward: an old record
			// carries the zero Liveness while its real state lives in the
			// legacy status field, and dropping it under-reports real sessions.
			liveness := session.EffectiveLiveness(data)
			if !agentIsRunning(liveness) || data.UserKilled {
				continue
			}
			states = append(states, quota.SessionState{
				Program:      agentName(data.Program),
				LimitReached: liveness == session.LiveLimitReached,
				ResetAt:      data.LimitResetAt,
			})
		}
	}
	return states, unreadable
}

// agentIsRunning reports whether a session record means an agent process is
// still present — the states the report counts as current usage.
//
// LiveLimitReached counts: a parked agent still HAS its runtime and is the
// strongest usage-limit signal af has. LiveLost/LiveDead do not — counting
// them turns a box full of dead rows into "N session(s) running".
// LivenessUnset is a deliberate exclusion, not a "treated as not running": a
// state nobody resolved is not evidence of a healthy account, which is why the
// check is an allowlist rather than an exclusion list.
func agentIsRunning(l session.Liveness) bool {
	switch l {
	case session.LiveRunning, session.LiveReady, session.LiveLimitReached:
		return true
	default:
		return false
	}
}

// agentName maps a record's Program back to the agent enum.
//
// InstanceData.Program is the RESOLVED command, not the enum: with
// program_overrides configured, "claude" arrives as
// "/home/…/claude --dangerously-skip-permissions", and reporting it verbatim
// grows a second row for the same agent — each undercounted. DetectAgentFromCommand
// folds overrides, wrappers and env prefixes back onto the program name.
//
// A command that maps to no agent is kept verbatim rather than dropped: the
// session is real, and hiding it under-reports.
func agentName(program string) string {
	if agent := tmux.DetectAgentFromCommand(program); agent != "" {
		return agent
	}
	return program
}
