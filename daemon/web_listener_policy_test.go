package daemon

import (
	"encoding/json"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/stretchr/testify/require"
)

// TestIsLoopbackListenAddr pins the bind classification that governs the
// loopback token exemption: only a loopback-bound listener is eligible for it,
// because on a network bind a same-host reverse proxy's 127.0.0.1 connection is
// indistinguishable from a real local user.
func TestIsLoopbackListenAddr(t *testing.T) {
	loopback := []string{"127.0.0.1:8443", "127.0.0.1:0", "localhost:8443", "LocalHost:80", "[::1]:8443", "127.0.0.1"}
	for _, a := range loopback {
		require.Truef(t, isLoopbackListenAddr(a), "%q must be classified loopback", a)
	}
	network := []string{"0.0.0.0:8443", ":8443", "192.168.1.5:8443", "100.64.0.1:8443", "[::]:8443", "10.0.0.7:8443", ""}
	for _, a := range network {
		require.Falsef(t, isLoopbackListenAddr(a), "%q must be classified network (token enforced)", a)
	}
}

// TestWebListenerPolicy is the SECURITY-fix wiring proof: the loopback exemption
// is granted ONLY on a loopback bind. On a network bind it is withheld even with
// the default require_loopback_token=false, so a same-host reverse proxy (which
// connects from 127.0.0.1) cannot bypass the token.
func TestWebListenerPolicy(t *testing.T) {
	cases := []struct {
		name                 string
		listen               string
		requireToken         bool
		requireLoopbackToken bool
		wantTokenDisabled    bool
		wantLoopbackExempt   bool
	}{
		{"loopback bind, defaults", "127.0.0.1:8443", true, false, false, true},
		{"loopback bind, require_loopback_token", "127.0.0.1:8443", true, true, false, false},
		{"network bind, require_token (proxy-bypass closed)", "0.0.0.0:8443", true, false, false, false},
		{"network bind, all-interfaces empty host", ":8443", true, false, false, false},
		{"tailscale IP bind, require_token", "100.64.0.1:8443", true, false, false, false},
		{"network bind, require_token=false stays open", "0.0.0.0:8443", false, false, true, false},
		{"loopback bind, require_token=false stays open", "127.0.0.1:8443", false, false, true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.DefaultConfig()
			cfg.ListenAddr = tc.listen
			cfg.RequireToken = tc.requireToken
			cfg.RequireLoopbackToken = tc.requireLoopbackToken
			got := webListenerPolicy(cfg)
			require.Equal(t, tc.wantTokenDisabled, got.tokenDisabled, "tokenDisabled")
			require.Equal(t, tc.wantLoopbackExempt, got.loopbackExempt, "loopbackExempt")
		})
	}
}

// TestTCPListener_NetworkBindDeniesLoopbackWithoutToken is the end-to-end
// proxy-bypass proof over a REAL socket: a NETWORK-bound listener (0.0.0.0) with
// require_token rejects a loopback-origin request that carries no token — closing
// the same-host reverse-proxy bypass — while the same request WITH the token is
// allowed. The policy is derived from config via webListenerPolicy, so this
// exercises the full bind→policy→gate wiring, not a hand-set exemption.
func TestTCPListener_NetworkBindDeniesLoopbackWithoutToken(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	cfg := config.DefaultConfig()
	cfg.ListenAddr = "0.0.0.0:0" // NETWORK bind (all interfaces), reachable via loopback
	require.True(t, cfg.RequireToken, "default require_token must be true")
	require.False(t, isLoopbackListenAddr(cfg.ListenAddr), "0.0.0.0 is a network bind")

	m, err := NewManager(cfg)
	require.NoError(t, err)
	cs := &controlServer{manager: m, scheduler: newTaskScheduler()}

	policy := webListenerPolicy(cfg)
	require.False(t, policy.loopbackExempt, "a network bind must NOT exempt loopback (proxy-bypass fix)")

	closeTCP, info, err := startTCPListener(newHTTPMux(cs), cfg, policy)
	require.NoError(t, err)
	defer func() { _ = closeTCP() }()
	require.NotEmpty(t, info.Token)

	// Connect over loopback to the network-bound socket — exactly what a same-host
	// reverse proxy does.
	_, port, err := net.SplitHostPort(info.Addr)
	require.NoError(t, err)
	base := "http://127.0.0.1:" + port
	client := &http.Client{Timeout: 5 * time.Second}

	// No token → 401 even though the peer is loopback (the bypass is closed).
	resp, err := client.Get(base + "/v1/health")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusUnauthorized, resp.StatusCode,
		"a network-bound listener must require the token even from a loopback-origin request")

	// The unauthenticated auth-info probe agrees: this peer needs a token.
	resp, err = client.Get(base + "/v1/auth-info")
	require.NoError(t, err)
	var env struct {
		Data struct {
			AuthRequired bool `json:"auth_required"`
		} `json:"data"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&env))
	_ = resp.Body.Close()
	require.True(t, env.Data.AuthRequired, "a loopback peer of a network-bound listener must be told it needs a token")

	// With the token → 200.
	req, err := http.NewRequest(http.MethodGet, base+"/v1/health", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+info.Token)
	resp, err = client.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "the correct token still reaches the API over a network bind")
}

// TestTCPListener_LoopbackBindStillExemptViaPolicy is the unchanged-behavior
// control: a LOOPBACK-bound listener with the default config still exempts a
// same-machine peer (no token needed), derived through webListenerPolicy.
func TestTCPListener_LoopbackBindStillExemptViaPolicy(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())

	cfg := config.DefaultConfig()
	cfg.ListenAddr = "127.0.0.1:0" // loopback bind
	m, err := NewManager(cfg)
	require.NoError(t, err)
	cs := &controlServer{manager: m, scheduler: newTaskScheduler()}

	policy := webListenerPolicy(cfg)
	require.True(t, policy.loopbackExempt, "a loopback bind with defaults must exempt loopback")

	closeTCP, info, err := startTCPListener(newHTTPMux(cs), cfg, policy)
	require.NoError(t, err)
	defer func() { _ = closeTCP() }()

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + info.Addr + "/v1/health")
	require.NoError(t, err)
	_ = resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode, "a loopback peer of a loopback-bound listener still connects with no token")
}
