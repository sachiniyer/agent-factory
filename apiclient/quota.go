package apiclient

import "github.com/sachiniyer/agent-factory/daemon"

// QuotaReport reads the targeted daemon's usage-limit report (#4361) — the
// HTTP twin of the daemon's controlServer.QuotaReport and of the
// control-socket call the TUI makes locally. It is deliberately thin: request
// in, response out, no policy and no local fallback. A remote read describes
// THAT daemon's host; falling back to this machine's records would report the
// wrong box's sessions as its own, the silent-lie class the remote routing
// rules exist to close.
func (c *Client) QuotaReport(req daemon.QuotaReportRequest) (daemon.QuotaReportResponse, error) {
	var resp daemon.QuotaReportResponse
	if err := c.call("QuotaReport", req, &resp); err != nil {
		return daemon.QuotaReportResponse{}, err
	}
	return resp, nil
}
