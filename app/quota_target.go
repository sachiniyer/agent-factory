package app

import (
	"context"
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
	"github.com/sachiniyer/agent-factory/quotahost"
)

// Usage follows the config pane's attached daemon (#4361): a TUI pointed at a
// remote daemon shows THAT host's usage-limit evidence.
//
// The LOCAL read goes to quotahost in-process, not to the control socket —
// the same shape as the pane's config read (localConfigForEditor loads the
// file itself while a remote target goes over HTTP). quotahost IS the daemon's
// own read model: controlServer.QuotaReport calls it too, so the answers
// cannot drift. Dialling the socket here would only add a failure mode — and
// worse, callDaemon ensures a daemon, which would let a READ spawn one.
//
// A remote failure never falls back to that local read: answering with this
// machine's parked sessions would report the wrong box's walls as the
// target's, the silent-lie shape the remote routing rules exist to close.
func targetedQuotaReportForPane(_ daemon.QuotaReportRequest) (daemon.QuotaReportResponse, error) {
	if !apiclient.IsRemoteTarget() {
		result, err := quotahost.Report(time.Now())
		if err != nil {
			return daemon.QuotaReportResponse{}, err
		}
		return daemon.QuotaReportResponse{
			Rows:    result.Rows,
			Note:    result.Note,
			Caveats: result.Caveats,
		}, nil
	}
	client, err := apiclient.NewTargeted()
	if err != nil {
		return daemon.QuotaReportResponse{}, err
	}
	defer client.CloseIdleConnections()
	resp, err := client.QuotaReport(daemon.QuotaReportRequest{})
	if err != nil {
		return daemon.QuotaReportResponse{}, remoteUsageRefusal(client, err)
	}
	return resp, nil
}

// remoteUsageRefusal passes a remote read failure through untouched, except
// for the one failure whose raw form is unactionable: a daemon that does not
// serve the route at all. Same reasoning as remoteQuotaReportError on the CLI
// side — name the version, say what to do.
func remoteUsageRefusal(client *apiclient.Client, err error) error {
	if !apiclient.IsRouteNotServed(err) {
		return err
	}
	return fmt.Errorf(
		"that daemon (%s) does not serve the QuotaReport route — it predates usage reporting. "+
			"Upgrade the daemon, or view usage on the daemon host",
		client.DaemonVersionPhrase(context.Background()))
}
