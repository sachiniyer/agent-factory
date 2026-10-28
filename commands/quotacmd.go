package commands

import (
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/internal/quotaobs"
	"github.com/sachiniyer/agent-factory/quota"
	"github.com/spf13/cobra"
)

// quotaCmd is `af quota` (#2983 Part 1): what af can honestly say about each
// agent CLI's usage, and — just as importantly — what it cannot.
//
// Read-only. It reads the session records and reports; it starts nothing,
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

Read-only: it reads session records and starts nothing. With --daemon-url (or
AF_DAEMON_URL) the report comes from the targeted daemon's host — the daemon
serves the same records read, so the answer describes that machine, not yours.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		// --daemon-url / AF_DAEMON_URL promises to target a REMOTE daemon, so the
		// report must come from that daemon's host. The daemon's QuotaReport RPC
		// serves the same records→Build read this command runs locally, so the
		// answer describes the machine the flag names rather than looking like it
		// does — a valid-looking report for the wrong machine is the precise
		// failure this command exists to avoid. An older daemon without the RPC
		// answers with its own method error, verbatim.
		if apiclient.IsRemoteTarget() {
			client, err := apiclient.NewTargeted()
			if err != nil {
				return err
			}
			defer client.CloseIdleConnections()
			resp, err := client.QuotaReport()
			if err != nil {
				return err
			}
			if err := quota.RenderRows(cmd.OutOrStdout(), resp.Agents); err != nil {
				return err
			}
			printQuotaWarnings(cmd, resp.Warnings)
			return nil
		}

		result, err := quotaobs.Collect()
		if err != nil {
			return err
		}
		if err := quota.Render(cmd.OutOrStdout(), result.Report, time.Now()); err != nil {
			return err
		}
		// An under-read report must never look complete. A repo whose records
		// could not be read may hold sessions parked at a limit, so staying silent
		// would render a confident "no limit seen" built on a failed read — the
		// exact shape this repo keeps paying for. Reported after the table so the
		// caveat attaches to what was just shown.
		printQuotaWarnings(cmd, result.Warnings())
		return nil
	},
}

func printQuotaWarnings(cmd *cobra.Command, warnings []string) {
	for _, warning := range warnings {
		fmt.Fprintf(cmd.ErrOrStderr(), "\nwarning: %s\n", warning)
	}
}
