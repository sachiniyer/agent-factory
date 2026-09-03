package apiclient

import "github.com/sachiniyer/agent-factory/daemon"

// The config read/write trio (#3679, #3708). These are the HTTP twins of the
// daemon's controlServer.GetConfig and its admission-gated SetConfigValue /
// UnsetConfigValue — the same handlers the web config form reads and posts to
// (#3231) — so a client pointed at a REMOTE daemon administers that daemon's
// config through exactly the paths its own machine would use, with the same
// lifecycle predicate answering the writes first.
//
// GetConfig joined them for the TUI's `,` editor (#3708), which needs BOTH
// halves routed or neither: a pane that reads machine A and writes machine B
// would overwrite values that were never B's. A form's read and its write are
// one feature, so they are one client surface.
//
// They are deliberately THIN: request in, response out, no key rewriting and no
// fallback. That is what makes them a parity twin rather than a second
// implementation. The two policies a caller needs on top of them —
// canonicalizing the legacy flat alias for an older daemon's allowlist, and
// refusing (never falling back to a local write) when the daemon does not serve
// the route — live one layer up in commands/configremote.go, alongside the
// local/remote target decision that only a CLI can make. daemon/ cannot host
// them: apiclient imports daemon, so the arrow cannot point back.

// GetConfig reads the targeted daemon's config manifest zipped with its live
// values, plus the path on THAT daemon's host they were read from. It is the
// read half of the editor pair: the daemon reads config.toml fresh per call, so
// the answer reflects a hand-edit made since, exactly as the in-process read the
// local path uses does.
func (c *Client) GetConfig(req daemon.GetConfigRequest) (daemon.GetConfigResponse, error) {
	var resp daemon.GetConfigResponse
	if err := c.call("GetConfig", req, &resp); err != nil {
		return daemon.GetConfigResponse{}, err
	}
	return resp, nil
}

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
