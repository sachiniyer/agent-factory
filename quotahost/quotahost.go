// Package quotahost owns the usage-limit report every surface renders (#4361):
// the daemon's QuotaReport RPC, `af quota`, the TUI's Usage section, and the
// web's. `af quota` grew the read model first (#2983); lifting its projection
// here keeps the policy — which records count as running, what an unreadable
// one means, when a wall was last seen — in exactly one place, so a remote
// daemon answers about ITS sessions with the same reading the local command
// gives this machine's.
//
// The package reports what af OBSERVED and nothing else: provider entitlement
// stays "not reported" on every row, and each parked session carries when af
// recorded its wall so a stale claim shows its age. The durable per-account
// evidence a router needs is a different read (session.AccountLimitObservations
// and the daemon's account-limit ledger) — this report is the per-agent surface.
package quotahost

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/quota"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
)

// Result is the report plus the framing and caveats that keep an under-read
// honest. Rows are the already-rendered cells — surfaces display them verbatim
// rather than re-interpreting the policy. Note is the standing warning about
// the two axes; Caveats names how the read was incomplete, when it was.
type Result struct {
	// Rows is one rendered row per agent, the same cells `af quota` prints.
	Rows []quota.Row `json:"rows"`
	// Note is the report's standing framing (quota.ReportNote).
	Note string `json:"note"`
	// Caveats reports how the read was incomplete — record files that could
	// not be read or parsed. An under-read report must never look complete: a
	// repo whose records failed to load may hold sessions parked at a limit,
	// and staying silent would render a confident "no limit seen" built on a
	// failed read.
	Caveats []string `json:"caveats,omitempty"`
}

// Report reads every repo's durable session records and builds the usage
// report. `now` is what the rows' ages and countdowns are measured against;
// the daemon stamps it at request time so a remote answer is as fresh as a
// local one.
//
// Read-only: it reads record files and starts nothing, contacts no provider,
// and touches no session.
func Report(now time.Time) (Result, error) {
	records, skipped, err := config.LoadAllRepoInstancesReportingSkips()
	if err != nil {
		return Result{}, fmt.Errorf("cannot read session records: %w", err)
	}
	states, unreadable := sessionStates(records)
	result := Result{
		Rows: quota.Build(tmux.SupportedPrograms, states).Rows(now),
		Note: quota.ReportNote,
	}
	if len(skipped) > 0 {
		result.Caveats = append(result.Caveats, fmt.Sprintf(
			"%d project record file(s) could not be read, so this report is INCOMPLETE; "+
				"sessions in them are not counted: %v", len(skipped), skipped))
	}
	if unreadable > 0 {
		result.Caveats = append(result.Caveats, fmt.Sprintf(
			"%d project record file(s) could not be parsed and were skipped, so this report is INCOMPLETE",
			unreadable))
	}
	return result, nil
}

// sessionStates projects the on-disk records into the minimal shape the
// quota package needs, and reports how many record blobs could not be parsed.
//
// The count is returned rather than swallowed for the same reason the skip list
// is: a record that would not parse might have been the parked one, and a report
// that quietly drops it reads as authoritative.
func sessionStates(records map[string]json.RawMessage) ([]quota.SessionState, int) {
	states := []quota.SessionState{}
	unreadable := 0
	for _, raw := range records {
		var data []session.InstanceData
		if err := json.Unmarshal(raw, &data); err != nil {
			unreadable++
			continue
		}
		for _, instance := range data {
			// Resolve the EFFECTIVE liveness rather than reading data.Liveness:
			// a pre-#1195 record carries the zero value there while its real state
			// lives in the legacy status field, so a direct comparison classifies an
			// old archived row as live and counts it as a running session.
			liveness := session.EffectiveLiveness(instance)
			if !agentIsRunning(liveness) || instance.UserKilled {
				continue
			}
			states = append(states, quota.SessionState{
				Program:      agentName(instance.Program),
				LimitReached: liveness == session.LiveLimitReached,
				ResetAt:      instance.LimitResetAt,
				ObservedAt:   instance.LimitObservedAt,
			})
		}
	}
	return states, unreadable
}

// agentIsRunning reports whether a record still has an agent process behind
// it, and is therefore evidence about CURRENT usage.
//
// An allowlist, not a denylist of inert states. The report's claims are
// "N session(s) running" and "no limit seen", so a state this function has not
// considered must not silently become evidence of a healthy account — the same
// reason the quota axis defaults to "not reported". LivenessUnset lands here too:
// a record whose state cannot be resolved is not proof of a running agent.
//
// LiveLimitReached counts as running deliberately: the agent IS there, parked at
// the wall, and that parking is the strongest signal this report has.
// Lost and Dead do not: their agent vanished, so counting them inflates the
// session totals and turns a machine full of dead rows into "no limit seen".
func agentIsRunning(liveness session.Liveness) bool {
	switch liveness {
	case session.LiveRunning, session.LiveReady, session.LiveLimitReached:
		return true
	default:
		return false
	}
}

// agentName maps a session's recorded program to the canonical agent enum.
//
// InstanceData.Program holds the RESOLVED command, not the enum: with
// program_overrides configured it is a whole command line
// ("/usr/local/bin/claude --dangerously-skip-permissions"), so grouping on it
// raw produces one row per flag combination and splits an agent's sessions
// across rows that all read like separate agents. Found by running the command
// against this box rather than by reading the type.
//
// tmux.DetectAgentFromCommand is the existing answer and already handles the
// cases a naive filepath.Base would not — wrapper prefixes (`ionice -c 3
// claude`), leading env assignments, and `env VAR=x claude` — matching only when
// a token's base equals a supported name verbatim, so /opt/claude-wrapper/run
// never matches on substring.
//
// A command it cannot attribute is returned UNCHANGED rather than dropped or
// bucketed as "unknown": those sessions are real, and hiding them would
// under-report exactly the way an invented number over-reports.
func agentName(program string) string {
	if agent := tmux.DetectAgentFromCommand(program); agent != "" {
		return agent
	}
	return program
}
