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
//
// An observation also carries WHEN af recorded it (#4361): a wall seen six days
// ago is weaker evidence than one seen a minute ago, and a reset time carried
// over from another identity can outlive the truth entirely. Every rendering of
// an observation therefore names its time so a stale claim shows its age rather
// than reading as fresh.
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
	// ObservedAt is the LATEST time af recorded a usage-limit wall for this
	// agent, or nil when no parked session carries one. It is what makes a
	// stale claim visibly stale (#4361): the reader can see that af last saw
	// the wall days ago rather than just now. A wall recorded before this
	// field existed is nil and must render as unknown, not as fresh.
	ObservedAt *time.Time
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
	// ObservedAt is when af recorded that wall; zero for evidence written
	// before the field existed. Only read when LimitReached — a running
	// session's own timestamp is not a wall observation and must not freshen
	// the row's evidence.
	ObservedAt time.Time
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
		if !state.ObservedAt.IsZero() && (row.ObservedAt == nil || state.ObservedAt.After(*row.ObservedAt)) {
			// Latest observation wins: it is the freshest evidence af holds
			// about this agent's wall.
			at := state.ObservedAt
			row.ObservedAt = &at
		}
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

// Row is one rendered row of the report — the four cells the CLI prints, as
// strings. It is the wire shape the daemon's QuotaReport response carries, so
// the TUI, the web, and a remote `af quota` all render the SAME wording of the
// same evidence instead of each growing a private reading of the policy
// (#4361).
type Row struct {
	// Agent is the canonical agent name.
	Agent string `json:"agent"`
	// Quota is the rendered provider entitlement ("not reported" today).
	Quota string `json:"quota"`
	// Observed is the rendered af observation ("limit reached", "no limit
	// seen", "no sessions").
	Observed string `json:"observed"`
	// Detail is the per-row sentence: counts, reset times, and when the
	// observation was made.
	Detail string `json:"detail"`
}

// Rows renders the report into wire cells. `now` is what the age and
// countdown wording in Detail is measured against.
func (r Report) Rows(now time.Time) []Row {
	rows := make([]Row, 0, len(r.Agents))
	for _, agent := range r.Agents {
		rows = append(rows, Row{
			Agent:    agent.Program,
			Quota:    agent.Entitlement.String(),
			Observed: agent.Observation.String(),
			Detail:   detail(agent, now),
		})
	}
	return rows
}

// Render writes the human-readable table.
//
// Every cell is filled. There is no branch that can emit an empty column, which
// is the rendering half of the rule the types enforce: a blank in a quota table
// is read as zero remaining, and af does not know that about any provider.
func Render(w io.Writer, report Report, now time.Time) error {
	return RenderRows(w, report.Rows(now))
}

// RenderRows writes the table for rows that are already rendered cells — what a
// remote daemon's QuotaReport answer hands the CLI, so a remote readout is
// word-for-word a local one. The legend travels with the table because it is
// the standing warning against reading either column as something it is not.
func RenderRows(w io.Writer, rows []Row) error {
	if len(rows) == 0 {
		_, err := fmt.Fprintln(w, "No agent CLIs are configured, so there is nothing to report.")
		return err
	}
	width := len("AGENT")
	for _, row := range rows {
		if len(row.Agent) > width {
			width = len(row.Agent)
		}
	}
	if _, err := fmt.Fprintf(w, "%-*s  %-13s  %-13s  %s\n", width, "AGENT", "QUOTA", "OBSERVED", "DETAIL"); err != nil {
		return err
	}
	for _, row := range rows {
		if _, err := fmt.Fprintf(w, "%-*s  %-13s  %-13s  %s\n",
			width, row.Agent, row.Quota, row.Observed, row.Detail,
		); err != nil {
			return err
		}
	}
	_, err := fmt.Fprint(w, "\n"+ReportNote+"\n")
	return err
}

// ReportNote is the report's standing framing, shared by the wire response and
// every UI section that renders the rows: the two axes must never be conflated,
// and an observation carries its age so a stale one shows (#4361).
const ReportNote = "QUOTA is what the provider reports. af has no quota API for any supported\n" +
	"agent today, so every row reads \"not reported\" — that is af declining to\n" +
	"guess a ceiling, not a ceiling of zero. OBSERVED is what af has seen in its\n" +
	"own sessions, which is real signal even where the provider offers nothing —\n" +
	"and \"no limit seen\" is never a claim that the account is healthy. Each\n" +
	"observation names when af recorded it, so an old one reads as old: a reset\n" +
	"time can outlive the identity it was observed under."

// detail is the per-row sentence. It states what was observed and, when a
// session is parked, when it resets — saying so explicitly when that is unknown
// rather than leaving the column blank.
func detail(agent AgentQuota, now time.Time) string {
	switch agent.Observation {
	case ObservationLimitReached:
		base := fmt.Sprintf("%d of %d session(s) parked at a usage limit", agent.LimitedSessions, agent.Sessions)
		if agent.ResetAt == nil {
			base += "; no reset time was reported with it"
		} else {
			remaining := agent.ResetAt.Sub(now)
			if remaining <= 0 {
				base += fmt.Sprintf("; earliest reset %s has passed, so a retry is due", agent.ResetAt.UTC().Format(time.RFC3339))
			} else {
				base += fmt.Sprintf("; earliest reset %s (in %s)", agent.ResetAt.UTC().Format(time.RFC3339), remaining.Round(time.Minute))
			}
		}
		// When the wall was recorded is what lets a reader distrust an old
		// claim (#4361) — a reset can outlive the identity it was observed
		// under, so the age is part of the evidence, not a footnote.
		if agent.ObservedAt == nil {
			return base + "; af has no record of when this was observed"
		}
		return base + fmt.Sprintf("; observed %s (%s)", agent.ObservedAt.UTC().Format(time.RFC3339), observationAge(*agent.ObservedAt, now))
	case ObservationNoLimitSeen:
		return fmt.Sprintf("%d session(s) running, none parked at a limit", agent.Sessions)
	default:
		return "configured, but af has no sessions running it"
	}
}

// observationAge renders how long ago the wall was recorded, compactly, the
// same scale the tree's pane-churn ages use.
func observationAge(observedAt, now time.Time) string {
	age := now.Sub(observedAt)
	if age <= 0 {
		return "just now"
	}
	if age < time.Minute {
		return "just now"
	}
	if age < time.Hour {
		return fmt.Sprintf("%dm ago", int(age/time.Minute))
	}
	if age < 24*time.Hour {
		return fmt.Sprintf("%dh ago", int(age/time.Hour))
	}
	return fmt.Sprintf("%dd ago", int(age/(24*time.Hour)))
}
