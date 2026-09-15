package commands

import (
	"context"
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/quota"
	"github.com/sachiniyer/agent-factory/quotahost"
	"github.com/spf13/cobra"
)

// quotaCmd is `af quota` (#2983 Part 1, remote-capable since #4361): what af can
// honestly say about each agent CLI's usage, and — just as importantly — what
// it cannot.
//
// Read-only. Locally it reads this machine's session records through
// quotahost.Report; with --daemon-url / AF_DAEMON_URL it asks THAT daemon for
// its report over /v1/QuotaReport — the same read model the daemon owns, so the
// two answers cannot drift. It starts nothing, contacts no provider, and
// touches no session.
//
// The command exists because the answer a user wants ("how much is left?") is
// currently unobtainable without leaving af, and the tempting way to provide it
// is to estimate — summing token counts out of provider transcripts, say. That
// measures SPEND, not entitlement, and presenting it as a quota would be a
// confident answer to a question nothing here can answer. So this reports the
// ceiling as "not reported" for every provider, and puts the real signal af
// does have — its own sessions parked at a usage wall, with the reset time and
// the sighting it recorded — on a separate axis that cannot be mistaken for it.
var quotaCmd = &cobra.Command{
	Use:   "quota",
	Short: "Show usage-limit status for each agent CLI",
	Long: `Show what af knows about each agent CLI's usage limits.

Two different things, kept apart on purpose:

  QUOTA     what the provider reports about the account's ceiling. af has no
            quota API for any supported agent today, so every row reads
            "not reported". That is af declining to guess, not a ceiling of zero.

  OBSERVED  what af has seen in its OWN sessions — a session parked at a usage
            wall, the reset time recorded with it, and when af recorded it, so
            an old observation reads as old. Real signal even where the
            provider exposes nothing — and never proof the account is healthy.

Read-only: it reads session records and starts nothing. With --daemon-url it
reports the targeted daemon's host instead of this machine's.`,
	RunE: func(cmd *cobra.Command, _ []string) error {
		var rows []quota.Row
		var caveats []string
		if apiclient.IsRemoteTarget() {
			resp, err := quotaReportRemote()
			if err != nil {
				return err
			}
			rows, caveats = resp.Rows, resp.Caveats
		} else {
			result, err := quotahost.Report(time.Now())
			if err != nil {
				return err
			}
			rows, caveats = result.Rows, result.Caveats
		}
		if err := quota.RenderRows(cmd.OutOrStdout(), rows); err != nil {
			return err
		}

		// An under-read report must never look complete. A repo whose records
		// could not be read may hold sessions parked at a limit, so staying
		// silent would render a confident "no limit seen" built on a failed
		// read — the exact shape this repo keeps paying for. Reported after
		// the table so the caveat attaches to what was just shown.
		for _, caveat := range caveats {
			fmt.Fprintf(cmd.ErrOrStderr(), "\nwarning: %s\n", caveat)
		}
		return nil
	},
}

// quotaReportRemote asks the targeted daemon for ITS usage report. A remote
// target never falls back to reading this machine's records — that fallback
// would hand an operator a valid-looking report for the wrong host, the silent
// lie the remote routing rules exist to close (#3678's shape on the read side).
// Every failure is therefore a refusal, with the one whose raw form is
// unactionable — a daemon too old to serve the route — rewritten into a
// sentence that says what to do.
func quotaReportRemote() (daemon.QuotaReportResponse, error) {
	client, err := apiclient.NewTargeted()
	if err != nil {
		return daemon.QuotaReportResponse{}, err
	}
	defer client.CloseIdleConnections()
	resp, err := client.QuotaReport(daemon.QuotaReportRequest{})
	if err != nil {
		return daemon.QuotaReportResponse{}, remoteQuotaReportError(client, err)
	}
	return resp, nil
}

// remoteQuotaReportError passes a remote read failure through untouched, except
// for the one failure whose raw form is unactionable: a daemon that does not
// serve the route at all.
//
// Every daemon deployed before this route exists answers its 404 — this is the
// common case on day one, not an edge. `daemon does not serve /v1/QuotaReport
// (it answered 404: …)` names a path the operator never typed and says nothing
// about what to do; and the tempting repair — "fall back to the local records,
// like a daemonless local read" — would report THIS machine's sessions as that
// host's, which is precisely the silent wrong-machine answer the remote rules
// forbid. So: name the daemon, name its version, and give the two ways forward.
func remoteQuotaReportError(client *apiclient.Client, err error) error {
	if !apiclient.IsRouteNotServed(err) {
		return err
	}
	return fmt.Errorf(
		"af quota cannot read the daemon at %s: that daemon (%s) does not serve the QuotaReport route. "+
			"Nothing was read locally — af never falls back to this machine's records for a remote target. "+
			"Upgrade the daemon, or run af quota on the daemon host",
		apiclient.RemoteTargetURL(), client.DaemonVersionPhrase(context.Background()))
}
