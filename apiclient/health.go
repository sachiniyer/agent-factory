package apiclient

import (
	"context"
	"fmt"
	"net/http"

	"github.com/sachiniyer/agent-factory/daemon"
)

// Health reads GET /v1/health — the daemon's liveness probe, which answers with
// a full daemon.PingResponse: the build version, the boot/transaction identity,
// the lifecycle phase, and the bound listeners.
//
// It is the ONE non-POST route this client speaks, which is why it does not go
// through call(): call() POSTs a JSON body to /v1/<Method>, and health takes no
// body. Everything after the request is built is shared with call() through
// roundTrip, so the headers, the bearer token, the transport-error
// classification and the envelope decode are the same code, not a second copy.
//
// The first caller is `af config set`/`unset` against a remote daemon (#3679):
// when the write route comes back RouteNotServedError, the refusal has to name
// the version of the daemon that is missing it, and Ping is where a daemon
// reports its version (#1044). A daemon built before that field existed answers
// "", which is itself a positive skew signal — see PingResponse.Version.
func (c *Client) Health(ctx context.Context) (daemon.PingResponse, error) {
	if c.requestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.requestTimeout)
		defer cancel()
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, c.httpBase+"/v1/health", nil)
	if err != nil {
		return daemon.PingResponse{}, fmt.Errorf("apiclient: build request: %w", err)
	}
	var resp daemon.PingResponse
	if err := c.roundTrip(httpReq, &resp); err != nil {
		return daemon.PingResponse{}, err
	}
	return resp, nil
}
