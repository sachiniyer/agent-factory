package app

import (
	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
)

// The Usage report follows the config pane's attached daemon (#3708), exactly
// like the Accounts reads: remote failures never fall back to the local
// control socket — a local answer under a remote target would paint this
// host's sessions under that host's name.
var localQuotaReport = daemon.QuotaReport

func targetedQuotaReport() (daemon.QuotaReportResponse, error) {
	if !apiclient.IsRemoteTarget() {
		return localQuotaReport(daemon.QuotaReportRequest{})
	}
	client, err := apiclient.NewTargeted()
	if err != nil {
		return daemon.QuotaReportResponse{}, err
	}
	defer client.CloseIdleConnections()
	return client.QuotaReport()
}
