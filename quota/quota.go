// Package quota answers "how much is left, and on which account?" for the agent
// CLIs af is configured to run (#2983 Part 1).
//
// The whole design turns on one rule, which is why it is enforced by the types
// rather than by the renderer: NO PROVIDER'S ENTITLEMENT MAY BE GUESSED. af has
// no quota endpoint for any supported agent today, so the honest answer to
// "what is your ceiling?" is "not reported" — and this repo's recurring failure
// shape is precisely the other thing, a fabricated number or a blank cell that a
// reader scans as zero (see the fabricated-negative class: a denied read, an
// unanswerable probe, and a missing datum have all been rendered here as
// confident answers).
//
// So Entitlement is not a number with a "valid" flag, and it is not a string
// that might be empty. It is a closed enum whose zero value is EntitlementNotReported,
// which means a caller that forgets to set it, or a provider added later with no
// integration, degrades to the truthful answer instead of to zero.
//
// What af CAN say honestly is what it has OBSERVED: its own sessions parking at a
// usage-limit wall, and the reset time it recorded when they did. That is real
// signal even where the provider offers no endpoint, and it is reported on a
// separate axis so it is never confused with entitlement.
package quota

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// Entitlement is what a provider says about the account's ceiling.
//
// One value today, deliberately. Adding a provider integration means adding a
// value here AND the code that can produce it; until then every provider reports
// the zero value, which is the truth. A bool would have invited "false means
// zero"; an int would have invited a rendered 0.
type Entitlement int

const (
	// EntitlementNotReported: af has no way to read this provider's ceiling. The
	// ZERO VALUE on purpose — an unhandled provider tells the truth by default.
	EntitlementNotReported Entitlement = iota
)

// String renders the entitlement for humans. It never returns an empty string,
// because a blank cell in a quota table reads as zero remaining.
func (e Entitlement) String() string {
	switch e {
	case EntitlementNotReported:
		return "not reported"
	default:
		return "not reported"
	}
}

// Observation is what af itself has seen about an agent, independent of anything
// the provider exposes. It is a separate axis from Entitlement so a reader can
// never mistake "af watched a session hit a wall" for "the provider told us the
// quota".
type Observation int

const (
	// ObservationNone: af has no sessions for this agent, so it has observed
	// nothing either way. The zero value, and NOT the same as "healthy" — an
	// agent nobody has run is not evidence of available quota.
	ObservationNone Observation = iota
	// ObservationNoLimitSeen: af has run sessions on this agent and none is
	// currently parked at a usage limit. Evidence about af's sessions, never a
	// claim about the account's ceiling.
	ObservationNoLimitSeen
	// ObservationLimitReached: at least one session is parked at this agent's
	// usage-limit wall right now. The strongest signal af has, and it comes from
	// af's own watching rather than from the provider.
	ObservationLimitReached
)

func (o Observation) String() string {
	switch o {
	case ObservationLimitReached:
		return "limit reached"
	case ObservationNoLimitSeen:
		return "no limit seen"
	default:
		return "no sessions"
	}
}

// AgentQuota is one row: everything af can honestly say about one agent CLI.
type AgentQuota struct {
	// Program is the canonical agent name (claude, codex, …).
	Program string
	// Entitlement is what the provider reports. See the package comment.
	Entitlement Entitlement
	// Observation is what af has seen for itself.
	Observation Observation
	// Sessions is how many of af's sessions currently use this agent.
	Sessions int
	// LimitedSessions is how many of those are parked at a usage limit.
	LimitedSessions int
	// ResetAt is the EARLIEST recorded reset time among the limited sessions, or
	// nil when none was parseable. Nil is meaningful and must render as unknown:
	// a limit with no reset time is common (not every provider states one), and
	// showing a zero time would invent a deadline in 1970.
	ResetAt *time.Time
}

// Report is the full answer, one row per configured agent.
type Report struct {
	Agents []AgentQuota
}

// SessionState is the minimal projection Build needs, so this package depends on
// no session/daemon types and stays trivially testable.
type SessionState struct {
	// Program is the agent the session runs.
	Program string
	// LimitReached is whether the session is parked at a usage-limit wall.
	LimitReached bool
	// ResetAt is the recorded reset time for that limit; zero when none was
	// parsed. Only read when LimitReached.
	ResetAt time.Time
}

// Build assembles the report for the given configured agents from af's own
// session state.
//
// programs is the canonical configured list, and every one of them gets a row
// even when no session uses it: an agent missing from the table is
// indistinguishable from an agent with nothing to report, and the point of this
// command is to answer for every configured CLI.
//
// A session naming an agent outside programs still gets a row, because it is
// real and hiding it would under-report. That is the only way rows are added.
func Build(programs []string, sessions []SessionState) Report {
	rows := map[string]*AgentQuota{}
	order := []string{}
	ensure := func(name string) *AgentQuota {
		if row, ok := rows[name]; ok {
			return row
		}
		row := &AgentQuota{Program: name, Entitlement: EntitlementNotReported}
		rows[name] = row
		order = append(order, name)
		return row
	}
	for _, program := range programs {
		if program = strings.TrimSpace(program); program != "" {
			ensure(program)
		}
	}
	for _, state := range sessions {
		program := strings.TrimSpace(state.Program)
		if program == "" {
			continue
		}
		row := ensure(program)
		row.Sessions++
		if !state.LimitReached {
			continue
		}
		row.LimitedSessions++
		if state.ResetAt.IsZero() {
			continue
		}
		// Earliest reset wins: it is the soonest the user could resume anything.
		if row.ResetAt == nil || state.ResetAt.Before(*row.ResetAt) {
			at := state.ResetAt
			row.ResetAt = &at
		}
	}
	report := Report{Agents: make([]AgentQuota, 0, len(order))}
	for _, name := range order {
		row := rows[name]
		switch {
		case row.LimitedSessions > 0:
			row.Observation = ObservationLimitReached
		case row.Sessions > 0:
			row.Observation = ObservationNoLimitSeen
		default:
			row.Observation = ObservationNone
		}
		report.Agents = append(report.Agents, *row)
	}
	sort.SliceStable(report.Agents, func(i, j int) bool {
		return report.Agents[i].Program < report.Agents[j].Program
	})
	return report
}

// Render writes the human-readable table.
//
// Every cell is filled. There is no branch that can emit an empty column, which
// is the rendering half of the rule the types enforce: a blank in a quota table
// is read as zero remaining, and af does not know that about any provider.
func Render(w io.Writer, report Report, now time.Time) error {
	if len(report.Agents) == 0 {
		_, err := fmt.Fprintln(w, "No agent CLIs are configured, so there is nothing to report.")
		return err
	}
	width := len("AGENT")
	for _, agent := range report.Agents {
		if len(agent.Program) > width {
			width = len(agent.Program)
		}
	}
	if _, err := fmt.Fprintf(w, "%-*s  %-13s  %-13s  %s\n", width, "AGENT", "QUOTA", "OBSERVED", "DETAIL"); err != nil {
		return err
	}
	for _, agent := range report.Agents {
		if _, err := fmt.Fprintf(w, "%-*s  %-13s  %-13s  %s\n",
			width, agent.Program, agent.Entitlement, agent.Observation, detail(agent, now),
		); err != nil {
			return err
		}
	}
	_, err := fmt.Fprint(w, "\nQUOTA is what the provider reports. af has no quota API for any supported\n"+
		"agent today, so every row reads \"not reported\" — that is af declining to\n"+
		"guess a ceiling, not a ceiling of zero. OBSERVED is what af has seen in its\n"+
		"own sessions, which is real signal even where the provider offers nothing.\n")
	return err
}

// detail is the per-row sentence. It states what was observed and, when a
// session is parked, when it resets — saying so explicitly when that is unknown
// rather than leaving the column blank.
func detail(agent AgentQuota, now time.Time) string {
	switch agent.Observation {
	case ObservationLimitReached:
		base := fmt.Sprintf("%d of %d session(s) parked at a usage limit", agent.LimitedSessions, agent.Sessions)
		if agent.ResetAt == nil {
			return base + "; no reset time was reported with it"
		}
		remaining := agent.ResetAt.Sub(now)
		if remaining <= 0 {
			return base + fmt.Sprintf("; earliest reset %s has passed, so a retry is due", agent.ResetAt.UTC().Format(time.RFC3339))
		}
		return base + fmt.Sprintf("; earliest reset %s (in %s)", agent.ResetAt.UTC().Format(time.RFC3339), remaining.Round(time.Minute))
	case ObservationNoLimitSeen:
		return fmt.Sprintf("%d session(s) running, none parked at a limit", agent.Sessions)
	default:
		return "configured, but af has no sessions running it"
	}
}
