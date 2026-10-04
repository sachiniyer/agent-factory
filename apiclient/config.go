package apiclient

import (
	"fmt"

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
// guarded twin routes: EVERY write goes to /v1/SetConfigValueGuarded /
// /v1/UnsetConfigValueGuarded — same request shape, same daemon handler —
// because no write is provably safe on a daemon that predates the refusal. Its
// writer ends in Manager.ApplyConfig, which loads the WHOLE file, and the
// write→apply gap is not atomic: a hand edit after the write's file-lock
// release lands in the apply regardless of which key was sent — so even a
// safe-forcing write (the token coming on, a loopback listen_addr) only SEEMS
// safe, and the digest check can only report the swap after the unsafe apply.
// The pre-refusal daemon has no endpoint that couples a write to refusal
// enforcement, so the only fail-closed answer is to send it nothing at all.
//
// Route selection, not a health preflight, is the capability check: the guarded
// path only exists on daemons that enforce the refusal, so the write and the
// proof arrive in the SAME request — a health-then-write pair could land its
// second leg on a different, older daemon after a restart or behind a
// mixed-version front. A 404 on the guarded route is the fail-closed answer:
// the write never happened.
func (c *Client) SetConfigValue(req daemon.SetConfigValueRequest) (daemon.SetConfigValueResponse, error) {
	var resp daemon.SetConfigValueResponse
	if err := c.call("SetConfigValueGuarded", req, &resp); err != nil {
		return daemon.SetConfigValueResponse{}, refusalCapableRouteError(req.Key, err)
	}
	return resp, nil
}

// UnsetConfigValue clears one globally unsettable migrated setting on the
// targeted daemon.
func (c *Client) UnsetConfigValue(req daemon.UnsetConfigValueRequest) (daemon.UnsetConfigValueResponse, error) {
	var resp daemon.UnsetConfigValueResponse
	if err := c.call("UnsetConfigValueGuarded", req, &resp); err != nil {
		return daemon.UnsetConfigValueResponse{}, refusalCapableRouteError(req.Key, err)
	}
	return resp, nil
}

// refusalCapableRouteError translates the one guarded-route failure whose raw
// form is misleading: a 404 there does not mean the route is missing in some
// generic sense — it means the answering daemon predates the #5137 refusal and
// would have served the exposure the write could have created. Naming that —
// and that nothing was written — beats a bare "daemon does not serve" line.
// Everything else passes through unchanged.
func refusalCapableRouteError(key string, err error) error {
	if !IsRouteNotServed(err) {
		return err
	}
	return fmt.Errorf(
		"the daemon at %s predates af's unauthenticated-listener refusal (#5137): accepting this %s write "+
			"triggers its whole-file apply, which binds whatever listen_addr the file holds — the control "+
			"API, including DeliverPrompt, served unauthenticated if that posture is tokenless — so the "+
			"write is refused rather than risk it — nothing was written. Upgrade af on that host and restart "+
			"its daemon, then retry; to accept the exposure deliberately, edit config.toml on the host "+
			"instead — the refusal guards remote writes, not local file edits",
		RemoteTargetURL(), config.CanonicalConfigKey(key))
}
