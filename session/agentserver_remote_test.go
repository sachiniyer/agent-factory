package session

import (
	"testing"
)

// TestAgentServerFactory_SelectsRuntime is the per-runtime factory proof (#1592
// Phase 4 PR2): AgentServer() returns the local in-process impl for a default
// instance, and a remoteAgentServer for one whose runtime exposes a remote
// agent-server endpoint — with the local default provably unchanged.
func TestAgentServerFactory_SelectsRuntime(t *testing.T) {
	local, err := NewInstance(InstanceOptions{Title: "local", Path: t.TempDir()})
	if err != nil {
		t.Fatalf("NewInstance(local): %v", err)
	}
	if _, ok := local.AgentServer().(*localAgentServer); !ok {
		t.Fatalf("default instance must get the local in-process agent-server, got %T", local.AgentServer())
	}

	remote, err := NewInstance(InstanceOptions{
		Title: "remote",
		Path:  t.TempDir(),
		RemoteAgentServer: &AgentServerEndpoint{
			URL:   "http://127.0.0.1:1",
			Token: "secret",
		},
	})
	if err != nil {
		t.Fatalf("NewInstance(remote): %v", err)
	}
	if _, ok := remote.AgentServer().(*remoteAgentServer); !ok {
		t.Fatalf("remote-runtime instance must get the remote agent-server client, got %T", remote.AgentServer())
	}
	// Cached: a second call returns the same instance (the data-plane ring buffer
	// and subscriber set must persist across calls).
	if remote.AgentServer() != remote.AgentServer() {
		t.Fatal("AgentServer() must cache the per-instance server")
	}
}

// TestNewRemoteAgentServer_ValidatesEndpoint proves the endpoint is validated at
// construction (no dial): a TLS scheme (the agent-server is HTTP-only), a missing
// host, and a missing title are all rejected up front, so the AgentServer()
// factory can stay infallible.
func TestNewRemoteAgentServer_ValidatesEndpoint(t *testing.T) {
	cases := []struct {
		name  string
		ep    AgentServerEndpoint
		title string
	}{
		{"tls scheme wss", AgentServerEndpoint{URL: "wss://host:1"}, "t"},
		{"tls scheme https", AgentServerEndpoint{URL: "https://host:1"}, "t"},
		{"no host", AgentServerEndpoint{URL: "http://"}, "t"},
		{"empty title", AgentServerEndpoint{URL: "http://host:1"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewRemoteAgentServer(tc.ep, tc.title); err == nil {
				t.Fatalf("NewRemoteAgentServer(%+v, %q): want error, got nil", tc.ep, tc.title)
			}
		})
	}

	// A well-formed endpoint constructs (still no dial — the URL is never reached).
	// Both http:// and ws:// are accepted interchangeably.
	for _, u := range []string{"http://127.0.0.1:1", "ws://127.0.0.1:1"} {
		as, err := NewRemoteAgentServer(AgentServerEndpoint{URL: u, Token: "tok"}, "probe")
		if err != nil {
			t.Fatalf("NewRemoteAgentServer(%q): %v", u, err)
		}
		if _, ok := as.(*remoteAgentServer); !ok {
			t.Fatalf("expected *remoteAgentServer, got %T", as)
		}
	}
}
