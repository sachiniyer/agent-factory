package api

import (
	"context"
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
)

// hostPickSupportViaDaemon reports whether the daemon this command targets
// honors CreateSessionRequest.Host, and what version it said it is. A var so
// tests can stand in for an older daemon.
var hostPickSupportViaDaemon = routedHostPickSupport

func routedHostPickSupport() (supported bool, version string, err error) {
	if !apiclient.IsRemoteTarget() {
		resp, err := daemon.PingCapabilities()
		return resp.HostPick, resp.Version, err
	}
	c, err := remoteSessionClient()
	if err != nil {
		return false, "", err
	}
	defer c.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := c.Health(ctx)
	return resp.HostPick, resp.Version, err
}

// requireHostPickSupport fails closed BEFORE a host pick is sent. A daemon that
// predates the field decodes the request without it and creates the session on
// the repo's default backend — possibly local, possibly a different machine —
// while this command would report success. The daemon must affirm the capability
// (Ping.host_pick); one that cannot say is treated as not supporting it, because
// "could not verify" must not mean "send it anyway".
func requireHostPickSupport(host string) error {
	supported, version, err := hostPickSupportViaDaemon()
	if err != nil {
		return fmt.Errorf("--host %q was not sent: could not confirm the daemon supports host picks: %w", host, err)
	}
	if supported {
		return nil
	}
	if version == "" {
		version = "an unknown version that predates version reporting"
	} else {
		version = "v" + version
	}
	return fmt.Errorf("the daemon (%s) does not support --host: it would ignore %q and create the session on the repo's default backend. "+
		"Upgrade it and run `af daemon restart`, then retry; nothing was created", version, host)
}
