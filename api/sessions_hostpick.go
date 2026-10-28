package api

import (
	"context"
	"fmt"
	"time"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/daemon"
)

// pingViaDaemon returns the Ping reply of the daemon this command targets: its
// capability bits and its version. A var so tests can stand in for an older
// daemon.
var pingViaDaemon = routedPing

func routedPing() (daemon.PingResponse, error) {
	if !apiclient.IsRemoteTarget() {
		return daemon.PingCapabilities()
	}
	c, err := remoteSessionClient()
	if err != nil {
		return daemon.PingResponse{}, err
	}
	defer c.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return c.Health(ctx)
}

// hostPickSurface names one place a host pick is sent and the Ping bit that
// says the daemon honors it there. The create request, send-prompt --create's
// delivery request and a task definition are three different wire fields: a
// daemon that predates one of them drops it silently, so each has its own
// affirmative bit.
type hostPickSurface struct {
	// what is the flag or field as the user typed it, for the message.
	what string
	// ignored says what the daemon would do with a pick it drops.
	ignored string
	bit     func(daemon.PingResponse) bool
}

var (
	hostPickOnCreate = hostPickSurface{
		what:    "--host",
		ignored: "create the session on the repo's default backend",
		bit:     func(r daemon.PingResponse) bool { return r.HostPick },
	}
	hostPickOnDeliver = hostPickSurface{
		what:    "--host",
		ignored: "create the session on the repo's default backend",
		bit:     func(r daemon.PingResponse) bool { return r.DeliverHostPick },
	}
	hostPickOnTask = hostPickSurface{
		what:    "--host",
		ignored: "store the task without a host, so every run would use the repo's default backend",
		bit:     func(r daemon.PingResponse) bool { return r.TaskHost },
	}
)

// requireHostPickSupport fails closed BEFORE a host pick is sent. A daemon that
// predates the field decodes the request without it and creates the session on
// the repo's default backend — possibly local, possibly a different machine —
// while this command would report success. The daemon must affirm the capability
// (a Ping bit); one that cannot say is treated as not supporting it, because
// "could not verify" must not mean "send it anyway".
func requireHostPickSupport(host string) error {
	return requireHostPickOn(hostPickOnCreate, host)
}

func requireHostPickOn(surface hostPickSurface, host string) error {
	resp, err := pingViaDaemon()
	if err != nil {
		return fmt.Errorf("%s %q was not sent: could not confirm the daemon supports host picks: %w", surface.what, host, err)
	}
	if surface.bit(resp) {
		return nil
	}
	version := resp.Version
	if version == "" {
		version = "an unknown version that predates version reporting"
	} else {
		version = "v" + version
	}
	return fmt.Errorf("the daemon (%s) does not support %s here: it would ignore %q and %s. "+
		"Upgrade it and run `af daemon restart`, then retry; nothing was created or changed", version, surface.what, host, surface.ignored)
}
