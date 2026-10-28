package api

import (
	"context"
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

// The surfaces and the refusal live in the daemon package so the CLI, the TUI
// and the web cannot word or decide it differently.
var (
	hostPickOnCreate  = daemon.HostPickOnCreate
	hostPickOnDeliver = daemon.HostPickOnDeliver
	hostPickOnTask    = daemon.HostPickOnTask
)

// requireHostPickSupport fails closed BEFORE a host pick is sent to create.
func requireHostPickSupport(host string) error {
	return requireHostPickOn(hostPickOnCreate, host)
}

func requireHostPickOn(surface daemon.HostPickSurface, host string) error {
	resp, err := pingViaDaemon()
	return surface.Require("--host", resp, err, host)
}
