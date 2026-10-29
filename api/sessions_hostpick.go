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
	// A sandbox credential is not admitted to /v1/health (the scope table denies
	// it), so probing it first would be refused for exactly the caller that most
	// needs `sessions create --host`. Its capabilities ride on Snapshot instead — a
	// route it IS admitted to — and appear only on a sandbox credential's response,
	// so a bearer-token holder asks that first and everyone else (or a daemon that
	// predates the field) falls through to the health probe.
	if apiclient.HasDaemonToken() {
		snap, snapErr := c.SnapshotWithAlarmsCtx(ctx, daemon.SnapshotRequest{})
		if snapErr == nil && snap.Caller != "" && snap.Capabilities != nil {
			return daemon.PingResponse{
				OK: true, HostPick: snap.Capabilities.HostPick,
				DeliverHostPick: snap.Capabilities.DeliverHostPick, TaskHost: snap.Capabilities.TaskHost,
			}, nil
		}
	}
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
