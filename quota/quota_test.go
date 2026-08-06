package quota

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

// The constraint this package exists to hold (#2983): a provider that exposes no
// quota surface must render as "not reported" — never a guessed number, and
// never a blank that a reader scans as zero remaining.
//
// These tests are written against the RENDERED output rather than the struct,
// because the failure mode is what a user sees. A type can be impeccable and
// still print an empty cell.

func TestRender_NeverEmitsABlankOrZeroQuotaCell(t *testing.T) {
	report := Build([]string{"claude", "codex", "aider"}, nil)
	var out bytes.Buffer
	if err := Render(&out, report, time.Unix(1_700_000_000, 0)); err != nil {
		t.Fatalf("Render: %v", err)
	}
	rendered := out.String()

	for _, program := range []string{"claude", "codex", "aider"} {
		line := lineFor(t, rendered, program)
		if !strings.Contains(line, "not reported") {
			t.Fatalf("row for %q does not say \"not reported\": %q", program, line)
		}
		// The specific failure this guards: a number where af has no data.
		if strings.Contains(line, " 0 ") || strings.Contains(line, "0%") {
			t.Fatalf("row for %q rendered a zero-looking quota, which reads as exhausted: %q", program, line)
		}
	}
	if strings.Contains(rendered, "  \n") {
		t.Fatal("a row ended in blank columns; an empty quota cell reads as zero remaining")
	}
}

// A configured agent nobody has run must still appear. An absent row is
// indistinguishable from "nothing to report", and answering for every configured
// CLI is the point of the command.
func TestBuild_ConfiguredAgentWithNoSessionsStillGetsARow(t *testing.T) {
	report := Build([]string{"claude", "gemini"}, []SessionState{{Program: "claude"}})
	if len(report.Agents) != 2 {
		t.Fatalf("agents = %d, want a row per configured agent", len(report.Agents))
	}
	gemini := agentNamed(t, report, "gemini")
	if gemini.Observation != ObservationNone {
		t.Fatalf("gemini observation = %v, want ObservationNone", gemini.Observation)
	}
	// Never conflate "nobody ran it" with "it has quota".
	if gemini.Observation == ObservationNoLimitSeen {
		t.Fatal("an agent nobody has run was reported as having seen no limit, which reads as healthy")
	}
	if gemini.Entitlement != EntitlementNotReported {
		t.Fatalf("gemini entitlement = %v, want EntitlementNotReported", gemini.Entitlement)
	}
}

// The observation axis is what af can honestly say, and it must come from af's
// own sessions rather than from anything claimed about the provider.
func TestBuild_ParkedSessionSurfacesAsAnObservationWithItsEarliestReset(t *testing.T) {
	early := time.Unix(1_700_000_000, 0)
	late := early.Add(2 * time.Hour)
	report := Build([]string{"codex"}, []SessionState{
		{Program: "codex", LimitReached: true, ResetAt: late},
		{Program: "codex", LimitReached: true, ResetAt: early},
		{Program: "codex"},
	})
	codex := agentNamed(t, report, "codex")
	if codex.Observation != ObservationLimitReached {
		t.Fatalf("observation = %v, want ObservationLimitReached", codex.Observation)
	}
	if codex.Sessions != 3 || codex.LimitedSessions != 2 {
		t.Fatalf("sessions=%d limited=%d, want 3 and 2", codex.Sessions, codex.LimitedSessions)
	}
	if codex.ResetAt == nil || !codex.ResetAt.Equal(early) {
		t.Fatalf("ResetAt = %v, want the EARLIEST (%v): it is the soonest anything can resume", codex.ResetAt, early)
	}
	// And the observation must never be mistaken for entitlement.
	if codex.Entitlement != EntitlementNotReported {
		t.Fatal("observing a limit was promoted into a claim about the provider's ceiling")
	}
}

// A limit with no parseable reset time is common. It must say so rather than
// render a zero time, which would invent a deadline in 1970.
func TestRender_LimitWithoutResetTimeSaysSoRatherThanShowingZero(t *testing.T) {
	report := Build([]string{"amp"}, []SessionState{{Program: "amp", LimitReached: true}})
	var out bytes.Buffer
	if err := Render(&out, report, time.Unix(1_700_000_000, 0)); err != nil {
		t.Fatalf("Render: %v", err)
	}
	line := lineFor(t, out.String(), "amp")
	if !strings.Contains(line, "no reset time was reported") {
		t.Fatalf("a limit with no reset time must say so, got: %q", line)
	}
	if strings.Contains(line, "1970") {
		t.Fatalf("a missing reset time rendered as the zero time: %q", line)
	}
}

// The zero value of Entitlement is the truthful answer, so a provider added
// later with no integration degrades to "not reported" rather than to zero.
func TestEntitlementZeroValueIsNotReported(t *testing.T) {
	var unset Entitlement
	if unset != EntitlementNotReported {
		t.Fatal("the zero Entitlement is not EntitlementNotReported; an unhandled provider would not default to the truth")
	}
	if got := unset.String(); got != "not reported" {
		t.Fatalf("zero Entitlement renders %q, want \"not reported\"", got)
	}
	if strings.TrimSpace(unset.String()) == "" {
		t.Fatal("Entitlement rendered empty; a blank quota cell reads as zero remaining")
	}
}

// A session naming an agent outside the configured list is real and must not be
// hidden — under-reporting is the same class of dishonesty as over-reporting.
func TestBuild_UnconfiguredAgentWithLiveSessionsIsStillReported(t *testing.T) {
	report := Build([]string{"claude"}, []SessionState{{Program: "devin"}})
	if agentNamed(t, report, "devin").Sessions != 1 {
		t.Fatal("a running agent outside the configured list was dropped from the report")
	}
}

func agentNamed(t *testing.T, report Report, program string) AgentQuota {
	t.Helper()
	for _, agent := range report.Agents {
		if agent.Program == program {
			return agent
		}
	}
	t.Fatalf("no row for %q in %+v", program, report.Agents)
	return AgentQuota{}
}

func lineFor(t *testing.T, rendered, program string) string {
	t.Helper()
	for _, line := range strings.Split(rendered, "\n") {
		if strings.HasPrefix(line, program) {
			return line
		}
	}
	t.Fatalf("no rendered row for %q in:\n%s", program, rendered)
	return ""
}
