package apiclient

import "github.com/sachiniyer/agent-factory/daemon"

// The config write pair (#3679). These are the HTTP twins of the daemon's
// admission-gated controlServer.SetConfigValue / UnsetConfigValue — the same
// handlers the web form posts to (#3231) — so a client pointed at a REMOTE
// daemon administers that daemon's config through exactly the write path its own
// machine would use, with the same lifecycle predicate answering first.
//
// They are deliberately THIN: request in, response out, no key rewriting and no
// fallback. That is what makes them a parity twin rather than a second
// implementation. The two policies a caller needs on top of them —
// canonicalizing the legacy flat alias for an older daemon's allowlist, and
// refusing (never falling back to a local write) when the daemon does not serve
// the route — live one layer up in commands/configremote.go, alongside the
// local/remote target decision that only a CLI can make. daemon/ cannot host
// them: apiclient imports daemon, so the arrow cannot point back.

// SetConfigValue writes one global config key on the targeted daemon.
func (c *Client) SetConfigValue(req daemon.SetConfigValueRequest) (daemon.SetConfigValueResponse, error) {
	var resp daemon.SetConfigValueResponse
	if err := c.call("SetConfigValue", req, &resp); err != nil {
		return daemon.SetConfigValueResponse{}, err
	}
	return resp, nil
}

// UnsetConfigValue clears one globally unsettable migrated setting on the
// targeted daemon.
func (c *Client) UnsetConfigValue(req daemon.UnsetConfigValueRequest) (daemon.UnsetConfigValueResponse, error) {
	var resp daemon.UnsetConfigValueResponse
	if err := c.call("UnsetConfigValue", req, &resp); err != nil {
		return daemon.UnsetConfigValueResponse{}, err
	}
	return resp, nil
}
