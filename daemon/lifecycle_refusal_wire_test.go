package daemon

import (
	"errors"
	"fmt"
	"net"
	"net/rpc"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// refusalWrappingControl is a daemon whose handlers return the restart refusal
// bare, and wrapped the ways a handler that ran could wrap it.
type refusalWrappingControl struct{}

func (refusalWrappingControl) Bare(_ struct{}, _ *struct{}) error { return errDaemonRestarting() }
func (refusalWrappingControl) SuffixWrap(_ struct{}, _ *struct{}) error {
	return fmt.Errorf("%w; and the handler ran", errDaemonRestarting())
}
func (refusalWrappingControl) Join(_ struct{}, _ *struct{}) error {
	return errors.Join(errDaemonRestarting(), errors.New("and the handler ran"))
}
func (refusalWrappingControl) PrefixOnly(_ struct{}, _ *struct{}) error {
	return errors.New("[daemon_restarting] ")
}
func (refusalWrappingControl) PrefixOnlyTwice(_ struct{}, _ *struct{}) error {
	return errors.New("[daemon_restarting] [daemon_restarting] ")
}
func (refusalWrappingControl) Restacked(_ struct{}, _ *struct{}) error {
	return errors.New("[daemon_restarting] " + errDaemonRestarting().Error() + "; and the handler ran")
}

// serveThroughTheControlCodec serves svc on the daemon's control socket
// through the daemon's own codec, as every control connection is served.
func serveThroughTheControlCodec(t *testing.T, svc any) {
	t.Helper()
	srv := rpc.NewServer()
	require.NoError(t, srv.RegisterName(controlServiceName, svc))
	socketPath, err := DaemonSocketPath()
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(filepath.Dir(socketPath), 0o755))
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go srv.ServeCodec(newGobServerCodec(conn, nil, nil))
		}
	}()
}

// TestOnlyTheGatesOwnRefusalLeadsWithItsCode: over the control socket, only
// the admission gate's own unwrapped refusal reaches the client as one (#7480).
// A wrap of it, by suffix, by join, or with its code stacked again, came from a
// handler that ran: the client reads a plain error and retries nothing, as HTTP
// already answers it.
func TestOnlyTheGatesOwnRefusalLeadsWithItsCode(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	serveThroughTheControlCodec(t, refusalWrappingControl{})

	require.True(t, IsDaemonRestartingErr(callDaemonNoEnsure("Bare", struct{}{}, &struct{}{})),
		"the gate's own refusal must reach the client as one")
	for _, method := range []string{"SuffixWrap", "Join", "Restacked"} {
		t.Run(method, func(t *testing.T) {
			err := callDaemonNoEnsure(method, struct{}{}, &struct{}{})
			require.Error(t, err)
			require.False(t, IsDaemonRestartingErr(err), "a handler that ran must not read as a refusal: %v", err)
			require.False(t, IsDaemonAdmissionRetryable(err))
			require.Contains(t, err.Error(), "and the handler ran", "the handler's own error must still reach the caller")
			require.False(t, strings.HasPrefix(err.Error(), "[daemon_restarting]"), "got %q", err.Error())
		})
	}
}

// TestTheStripNeverEmptiesAnError: an error that is only a refusal's code,
// once or stacked, must still reach the client as an error. Stripped to an
// empty text, net/rpc would deliver it as a success with a zero reply (#7480).
func TestTheStripNeverEmptiesAnError(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	serveThroughTheControlCodec(t, refusalWrappingControl{})

	for _, method := range []string{"PrefixOnly", "PrefixOnlyTwice"} {
		t.Run(method, func(t *testing.T) {
			err := callDaemonNoEnsure(method, struct{}{}, &struct{}{})
			require.Error(t, err, "an error that was only the code arrived as a success")
			require.False(t, IsDaemonRestartingErr(err), "a handler that ran must not read as a refusal: %v", err)
			require.NotEmpty(t, strings.TrimSpace(err.Error()))
		})
	}
}
