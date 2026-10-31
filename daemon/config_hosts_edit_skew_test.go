package daemon

import (
	"net/rpc"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// #6096: `af hosts remove --clear-default` must report exactly what it wrote.
// The default clear and the removal are ONE HostsEdit, so against a daemon that
// cannot apply it atomically the client refuses before sending anything — no
// separate default_host write exists that could land and then be disowned.

type skewControl struct {
	mu          sync.Mutex
	hostsEdit   bool // the daemon's ConfigHostsEdit capability bit
	setRequests []SetConfigValueRequest
}

func (c *skewControl) Ping(_ PingRequest, resp *PingResponse) error {
	resp.Version = "test"
	resp.ConfigHostsEdit = c.hostsEdit
	return nil
}

func (c *skewControl) SetConfigValue(req SetConfigValueRequest, resp *SetConfigValueResponse) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setRequests = append(c.setRequests, req)
	return nil
}

func TestEditGlobalHosts_ClearDefaultAcrossDaemonVersions(t *testing.T) {
	edit := config.HostsEdit{Op: config.HostsEditRemove, Name: "box1", ClearDefault: true}

	for _, tc := range []struct {
		name      string
		hostsEdit bool
		wantErr   string
		wantCalls int
	}{
		{name: "a daemon without the host-edit capability is refused before any write", hostsEdit: false,
			wantErr: "nothing was written", wantCalls: 0},
		{name: "a daemon with the capability gets the clear and the removal as one request", hostsEdit: true, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
			ctrl := &skewControl{hostsEdit: tc.hostsEdit}
			srv := rpc.NewServer()
			require.NoError(t, srv.RegisterName(controlServiceName, ctrl))
			_, cleanup := startFakeControlListener(t, srv)
			t.Cleanup(cleanup)

			_, err := EditGlobalHosts(edit)

			ctrl.mu.Lock()
			defer ctrl.mu.Unlock()
			require.Len(t, ctrl.setRequests, tc.wantCalls,
				"the daemon must see no write at all from a refused edit, and exactly one otherwise")
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				require.ErrorContains(t, err, "af daemon restart", "the refusal must say how to fix it")
				return
			}
			require.NoError(t, err)
			got := ctrl.setRequests[0]
			require.NotNil(t, got.HostsEdit)
			require.Equal(t, edit, *got.HostsEdit)
			require.Equal(t, "hosts", got.Key, "no separate default_host write rides alongside")
		})
	}
}
