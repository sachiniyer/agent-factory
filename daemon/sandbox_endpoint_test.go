package daemon

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// The endpoint admits only a per-session sandbox credential, on routes the
// sandbox scope already opts in, and tells the handler whose it is (#5571 W3.4).
func TestSandboxEndpointAdmitsOnlySandboxCredentials(t *testing.T) {
	var allowed string
	for path := range sandboxAllowedPaths {
		allowed = path
		break
	}
	require.NotEmpty(t, allowed, "the scope must admit at least one route")
	var denied string
	for _, r := range servedHTTPRoutes() {
		if !sandboxAllowedPaths[r.Path] {
			denied = r.Path
			break
		}
	}
	require.NotEmpty(t, denied, "the scope must withhold at least one route")

	var reg sandboxTokenRegistry
	token, err := reg.mint("sess-a")
	require.NoError(t, err)

	var gotOwner string
	var gotIsSandbox bool
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotOwner, gotIsSandbox = sandboxOwner(r.Context())
		w.WriteHeader(http.StatusOK)
	})
	sock := filepath.Join(testguard.SocketTempDir(t), "cb.sock")
	ep, err := startSandboxEndpoint(mux, &reg, sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ep.close() })

	info, err := os.Stat(sock)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o600), info.Mode().Perm(), "other host users must not reach the socket")

	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
	do := func(path, bearer string) int {
		req, err := http.NewRequest(http.MethodPost, "http://sandbox"+path, nil)
		require.NoError(t, err)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		resp, err := client.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	require.Equal(t, http.StatusUnauthorized, do(allowed, ""), "no credential: the loopback-like unix peer is NOT exempt")
	require.Equal(t, http.StatusUnauthorized, do(allowed, "not-a-token"), "an unknown bearer is refused")
	require.Equal(t, http.StatusUnauthorized, do(denied, token), "a sandbox credential is refused off-scope")
	require.Equal(t, http.StatusOK, do(allowed, token))
	require.True(t, gotIsSandbox)
	require.Equal(t, "sess-a", gotOwner)

	reg.revoke("sess-a")
	require.Equal(t, http.StatusUnauthorized, do(allowed, token), "revocation takes effect on this endpoint too")
}
