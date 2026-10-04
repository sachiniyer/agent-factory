package apiclient

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/daemon"
)

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

// The #5137 unauthenticated-listener refusal reaches REMOTE writes through the
// guarded twin routes: SetConfigValue and UnsetConfigValue select
// /v1/SetConfigValueGuarded / /v1/UnsetConfigValueGuarded — same request shape,
// same daemon handler — for any write that could CREATE the refused posture
// (config.ListenerPostureWriteExposure / ListenerPostureUnsetExposure answer
// that), because a pre-#5137 daemon's writer has no listenerWriteRefusal and
// would accept the write and bind the very listener this build declines to.
//
// Route selection, not a health preflight, is the capability check: the guarded
// path only exists on daemons that enforce the refusal, so the write and the
// proof arrive in the SAME request — a health-then-write pair could land its
// second leg on a different, older daemon after a restart or behind a
// mixed-version front. A 404 on the guarded route is the fail-closed answer:
// the write never happened. Safe writes (require_token on, a loopback
// listen_addr, revoking the opt-in) keep the plain routes — an old daemon
// applies them safely and they are exactly the remediation an old daemon needs.
func (c *Client) SetConfigValue(req daemon.SetConfigValueRequest) (daemon.SetConfigValueResponse, error) {
	var resp daemon.SetConfigValueResponse
	method := "SetConfigValue"
	if config.ListenerPostureWriteExposure(req.Key, req.Value) {
		method += "Guarded"
	}
	if err := c.call(method, req, &resp); err != nil {
		return daemon.SetConfigValueResponse{}, refusalCapableRouteError(method, req.Key, err)
	}
	return resp, nil
}

// UnsetConfigValue clears one globally unsettable migrated setting on the
// targeted daemon.
func (c *Client) UnsetConfigValue(req daemon.UnsetConfigValueRequest) (daemon.UnsetConfigValueResponse, error) {
	var resp daemon.UnsetConfigValueResponse
	method := "UnsetConfigValue"
	if config.ListenerPostureUnsetExposure(req.Key) {
		method += "Guarded"
	}
	if err := c.call(method, req, &resp); err != nil {
		return daemon.UnsetConfigValueResponse{}, refusalCapableRouteError(method, req.Key, err)
	}
	return resp, nil
}

// refusalCapableRouteError translates the one guarded-route failure whose raw
// form is misleading: a 404 there does not mean the route is missing in some
// generic sense — it means the answering daemon predates the #5137 refusal and
// would have served the exposure the write was about to create. Naming that —
// and that nothing was written — beats a bare "daemon does not serve" line.
// Everything else passes through unchanged, including the plain-route 404 a
// pre-#3679 daemon still gives for UnsetConfigValue itself.
func refusalCapableRouteError(method, key string, err error) error {
	if !strings.HasSuffix(method, "Guarded") || !IsRouteNotServed(err) {
		return err
	}
	return fmt.Errorf(
		"the daemon at %s predates af's unauthenticated-listener refusal (#5137): it would accept this %s write "+
			"and serve the control API — including DeliverPrompt — to anyone who can reach the address, "+
			"so nothing was written. Upgrade af on that host and restart its daemon, then retry; to accept the "+
			"exposure deliberately, edit config.toml on the host instead — the refusal guards remote writes, not "+
			"local file edits",
		RemoteTargetURL(), config.CanonicalConfigKey(key))
}
