package apiclient

import (
	"context"

	"github.com/sachiniyer/agent-factory/daemon"
)

// GitCredential asks the daemon for the push credential of this sandbox
// session's own origin (#5571 W7.8). Only a sandbox session's own callback
// credential, presented on the dedicated sandbox endpoint, is ever answered;
// the in-sandbox git credential helper and gh shim are its only callers. The
// returned password is a secret: callers hand it straight to git or gh and
// never log or store it.
func (c *Client) GitCredential(ctx context.Context, req daemon.GitCredentialRequest) (daemon.GitCredentialResponse, error) {
	var resp daemon.GitCredentialResponse
	if err := c.callCtx(ctx, "GitCredential", req, &resp); err != nil {
		return daemon.GitCredentialResponse{}, err
	}
	return resp, nil
}
