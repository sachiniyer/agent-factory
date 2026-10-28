package commands

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/sachiniyer/agent-factory/apiclient"
	"github.com/sachiniyer/agent-factory/apiproto"
	"github.com/sachiniyer/agent-factory/daemon"
)

// remoteDaemonStatusTimeout bounds the health read so an unreachable target
// answers "not responding" promptly instead of hanging the verb.
const remoteDaemonStatusTimeout = 10 * time.Second

// remoteDaemonStatus is `af daemon status` against a remote target (W9.4). It
// reports ONLY what the target's health route answers — never this machine's
// sockets, pid files or autostart unit, which are facts about a different host.
type remoteDaemonStatus struct {
	Target        string                       `json:"target"`
	Running       bool                         `json:"running"`
	Error         string                       `json:"error,omitempty"`
	Version       string                       `json:"version,omitempty"`
	BootID        string                       `json:"boot_id,omitempty"`
	TransactionID string                       `json:"transaction_id,omitempty"`
	Phase         daemon.DaemonPhase           `json:"phase,omitempty"`
	Listeners     *daemon.DaemonListenerStatus `json:"listeners,omitempty"`
}

func collectRemoteDaemonStatus() remoteDaemonStatus {
	info := remoteDaemonStatus{Target: apiclient.RemoteTargetURL()}
	c, err := apiclient.NewTargeted()
	if err != nil {
		info.Error = err.Error()
		return info
	}
	defer c.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(context.Background(), remoteDaemonStatusTimeout)
	defer cancel()
	h, err := c.Health(ctx)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	info.Running = true
	info.Version, info.BootID, info.TransactionID, info.Phase = h.Version, h.BootID, h.TransactionID, h.Phase
	if h.Phase != "" {
		listeners := h.Listeners
		info.Listeners = &listeners
	}
	return info
}

func runRemoteDaemonStatus(cmd *cobra.Command) error {
	info := collectRemoteDaemonStatus()
	if daemonStatusJSON {
		return apiproto.WriteEnvelope(cmd.OutOrStdout(), apiproto.Success(info))
	}
	w := cmd.OutOrStdout()
	label := info.Target
	if label == "" {
		label = "the configured remote daemon"
	}
	if !info.Running {
		fmt.Fprintf(w, "daemon at %s: not responding\n", label)
		if info.Error != "" {
			fmt.Fprintf(w, "  error:          %s\n", info.Error)
		}
		fmt.Fprintln(w, "  (a remote status reads only its health route; start or restart it on the daemon host)")
		return nil
	}
	fmt.Fprintf(w, "daemon at %s: running\n", label)
	if info.Phase != "" {
		fmt.Fprintf(w, "  phase:          %s\n", info.Phase)
	}
	if info.Version != "" {
		fmt.Fprintf(w, "  version:        %s\n", info.Version)
	}
	if info.BootID != "" {
		fmt.Fprintf(w, "  boot id:        %s\n", info.BootID)
	}
	if info.TransactionID != "" {
		fmt.Fprintf(w, "  transaction:    %s\n", info.TransactionID)
	}
	return nil
}
