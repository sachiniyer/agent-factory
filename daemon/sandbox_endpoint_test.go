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
	"github.com/sachiniyer/agent-factory/session"
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
	thisProcessCarries(t, &reg, "sess-a") // this test's connections are sess-a's own forward

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

// dialUnix returns an HTTP client whose every request goes to sock.
func sandboxUnixClient(sock string) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", sock)
		},
	}}
}

// The operator's token is refused on the sandbox endpoint, on every route — the
// endpoint has no operator credential at all, and a unix peer is not a loopback
// exemption (#5571 W3.4, owner-approved constraint).
func TestSandboxEndpointRefusesTheOperatorToken(t *testing.T) {
	const operatorToken = "operator-token-0123456789abcdef"
	var reg sandboxTokenRegistry
	sandboxToken, err := reg.mint("sess-a")
	require.NoError(t, err)
	thisProcessCarries(t, &reg, "sess-a")

	reached := false
	mux := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	sock := filepath.Join(testguard.SocketTempDir(t), "cb.sock")
	ep, err := startSandboxEndpoint(mux, &reg, sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ep.close() })
	client := sandboxUnixClient(sock)

	do := func(path, bearer string) int {
		req, err := http.NewRequest(http.MethodPost, "http://sandbox"+path, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := client.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	// The gate itself: even a gate that is handed the operator token's value
	// elsewhere in the process has none configured here.
	gate := sandboxOnlyGate(&reg)
	expected, err := gate.expectedToken()
	require.NoError(t, err)
	require.Empty(t, expected, "the sandbox-only gate must hold no operator credential")

	for _, r := range servedHTTPRoutes() {
		reached = false
		require.Equalf(t, http.StatusUnauthorized, do(r.Path, operatorToken), "operator token on %s", r.Path)
		require.Falsef(t, reached, "the handler for %s must not run for the operator token", r.Path)
	}
	// Anti-vacuous: the sandbox credential is admitted on an in-scope route.
	var allowed string
	for path := range sandboxAllowedPaths {
		allowed = path
		break
	}
	require.Equal(t, http.StatusOK, do(allowed, sandboxToken))
	require.True(t, reached)
}

// A session-A credential cannot act on session B (#5571 W3.4, owner-approved
// constraint): the endpoint identifies the caller by credential alone, and the
// owner constraint hands each handler only that session's own row.
func TestSandboxEndpointCredentialIsBoundToItsOwnSession(t *testing.T) {
	var reg sandboxTokenRegistry
	tokenA, err := reg.mint("sess-a")
	require.NoError(t, err)
	tokenB, err := reg.mint("sess-b")
	require.NoError(t, err)
	require.NotEqual(t, tokenA, tokenB)
	carrier := thisProcessCarries(t, &reg, "sess-a")

	rows := []session.InstanceData{{ID: "sess-a", Title: "a", Path: "/host/a"}, {ID: "sess-b", Title: "b", Path: "/host/b"}}
	var seen []string
	var seenOwner string
	mux := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		owner, _ := sandboxOwner(r.Context())
		seenOwner, seen = owner, nil
		for _, d := range onlyOwnedBySandbox(rows, owner) {
			seen = append(seen, d.ID)
		}
		w.WriteHeader(http.StatusOK)
	})
	sock := filepath.Join(testguard.SocketTempDir(t), "cb.sock")
	ep, err := startSandboxEndpoint(mux, &reg, sock)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ep.close() })
	client := sandboxUnixClient(sock)

	var allowed string
	for path := range sandboxAllowedPaths {
		allowed = path
		break
	}
	call := func(bearer string) {
		req, err := http.NewRequest(http.MethodPost, "http://sandbox"+allowed, nil)
		require.NoError(t, err)
		req.Header.Set("Authorization", "Bearer "+bearer)
		resp, err := client.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}

	call(tokenA)
	require.Equal(t, "sess-a", seenOwner)
	require.Equal(t, []string{"sess-a"}, seen, "session A's credential must see only session A")

	// Through B's forward, A's credential is not accepted at all (#5571 W7.8).
	carrier.carry("sess-b")
	wrong, err := http.NewRequest(http.MethodPost, "http://sandbox"+allowed, nil)
	require.NoError(t, err)
	wrong.Header.Set("Authorization", "Bearer "+tokenA)
	wrongResp, err := client.Do(wrong)
	require.NoError(t, err)
	_ = wrongResp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, wrongResp.StatusCode, "a credential sent through another session's forward")

	call(tokenB)
	require.Equal(t, "sess-b", seenOwner)
	require.Equal(t, []string{"sess-b"}, seen, "session B's credential must see only session B")

	// Re-minting for A voids A's old credential and leaves B's alone. Asked
	// through A's own forward, so the refusal is the revocation's, not the
	// forward's.
	newA, err := reg.mint("sess-a")
	require.NoError(t, err)
	carrier.carry("sess-a")
	req, err := http.NewRequest(http.MethodPost, "http://sandbox"+allowed, nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+tokenA)
	resp, err := client.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	call(newA)
	carrier.carry("sess-b")
	call(tokenB)
}
