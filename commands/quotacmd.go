package commands

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/quota"
	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/spf13/cobra"
)

// quotaCmd is `af quota` (#2983 Part 1): what af can honestly say about each
// agent CLI's usage, and — just as importantly — what it cannot.
//
// Read-only. It reads the local session records and reports; it starts nothing,
// contacts no provider, and touches no session.
//
// The command exists because the answer a user wants ("how much is left?") is
// currently unobtainable without leaving af, and the tempting way to provide it
// is to estimate — summing token counts out of provider transcripts, say. That
// measures SPEND, not entitlement, and presenting it as a quota would be a
// confident answer to a question nothing here can answer. So this reports the
// ceiling as "not reported" for every provider, and puts the real signal af does
// have — its own sessions parked at a usage wall, with the reset time it
// recorded — on a separate axis that cannot be mistaken for it.
var quotaCmd = &cobra.Command{
	Use:   "quota",
	Short: "Show usage-limit status for each agent CLI",
	Long: `Show what af knows about each agent CLI's usage limits.

Two different things, kept apart on purpose:

  QUOTA     what the provider reports about the account's ceiling. af has no
            quota API for any supported agent today, so every row reads
            "not reported". That is af declining to guess, not a ceiling of zero.

  OBSERVED  what af has seen in its OWN sessions — a session parked at a usage
            wall, and the reset time recorded with it. Real signal even where the
            provider exposes nothing.

Read-only: it reads local session records and starts nothing.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		// --daemon-url / AF_DAEMON_URL promises to target a REMOTE daemon. This
		// command reads local record files, which describe this host and no other,
		// so honouring the flag by ignoring it would hand an operator a
		// valid-looking quota report for the wrong machine — the precise failure
		// this command exists to avoid, arriving through the target rather than
		// through a guessed number. Refuse instead. apiclient.IsRemoteTarget is the
		// established seam for exactly this: its doc names suppressing the local
		// disk fallback, because a remote read has no local disk to fall back to.
		//
		// Refusing rather than fetching is a scope choice, not a verdict on the
		// feature: serving this remotely needs the daemon to expose the report, and
		// an honest error today beats a wrong answer today (#2983).
		if apiclient.IsRemoteTarget() {
			return fmt.Errorf("af quota reads this machine's session records and cannot report on a remote daemon; " +
				"unset --daemon-url/AF_DAEMON_URL to report on this host, or run af quota on the daemon's host")
		}
		records, skipped, err := config.LoadAllRepoInstancesReportingSkips()
		if err != nil {
			return fmt.Errorf("cannot read session records: %w", err)
		}

		states, unreadable := quotaSessionStates(records)
		report := quota.Build(tmux.SupportedPrograms, states)
		if err := quota.Render(cmd.OutOrStdout(), report, time.Now()); err != nil {
			return err
		}

		// An under-read report must never look complete. A repo whose records
		// could not be read may hold sessions parked at a limit, so staying silent
		// would render a confident "no limit seen" built on a failed read — the
		// exact shape this repo keeps paying for. Reported after the table so the
		// caveat attaches to what was just shown.
		if len(skipped) > 0 {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"\nwarning: %d project record file(s) could not be read, so this report is INCOMPLETE; "+
					"sessions in them are not counted above: %v\n", len(skipped), skipped)
		}
		if unreadable > 0 {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"\nwarning: %d project record file(s) could not be parsed and were skipped, so this "+
					"report is INCOMPLETE\n", unreadable)
		}
		return nil
	},
}

// quotaSessionStates projects the on-disk records into the minimal shape the
// quota package needs, and reports how many record blobs could not be parsed.
//
// The count is returned rather than swallowed for the same reason the skip list
// is: a record that would not parse might have been the parked one, and a report
// that quietly drops it reads as authoritative.
func quotaSessionStates(records map[string]json.RawMessage) ([]quota.SessionState, int) {
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
			if !quotaAgentIsRunning(liveness) || instance.UserKilled {
				continue
			}
			states = append(states, quota.SessionState{
				Program:      quotaAgentName(instance.Program),
				LimitReached: liveness == session.LiveLimitReached,
				ResetAt:      instance.LimitResetAt,
			})
		}
	}
	return states, unreadable
}

// quotaAgentIsRunning reports whether a record still has an agent process behind
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
func quotaAgentIsRunning(liveness session.Liveness) bool {
	switch liveness {
	case session.LiveRunning, session.LiveReady, session.LiveLimitReached:
		return true
	default:
		return false
	}
}

// quotaAgentName maps a session's recorded program to the canonical agent enum.
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
func quotaAgentName(program string) string {
	if agent := tmux.DetectAgentFromCommand(program); agent != "" {
		return agent
	}
	return program
}
