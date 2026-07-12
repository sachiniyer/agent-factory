package session

import (
	"testing"
)

// validFingerprint is a well-formed (64-hex) SHA-256 pin — enough for
// construction, which validates the shape without dialing.
const validFingerprint = "aa11bb22cc33dd44ee55ff66007788990011223344556677889900aabbccddee"

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
			URL:         "wss://127.0.0.1:1",
			Token:       "secret",
			Fingerprint: validFingerprint,
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
// construction (no dial): a plaintext scheme, a malformed fingerprint, and a
// missing title are all rejected up front, so the AgentServer() factory can stay
// infallible.
func TestNewRemoteAgentServer_ValidatesEndpoint(t *testing.T) {
	cases := []struct {
		name  string
		ep    AgentServerEndpoint
		title string
	}{
		{"plaintext scheme", AgentServerEndpoint{URL: "ws://host:1", Fingerprint: validFingerprint}, "t"},
		{"http scheme", AgentServerEndpoint{URL: "http://host:1", Fingerprint: validFingerprint}, "t"},
		{"no host", AgentServerEndpoint{URL: "wss://", Fingerprint: validFingerprint}, "t"},
		{"bad fingerprint", AgentServerEndpoint{URL: "wss://host:1", Fingerprint: "nope"}, "t"},
		{"empty title", AgentServerEndpoint{URL: "wss://host:1", Fingerprint: validFingerprint}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewRemoteAgentServer(tc.ep, tc.title); err == nil {
				t.Fatalf("NewRemoteAgentServer(%+v, %q): want error, got nil", tc.ep, tc.title)
			}
		})
	}

	// A well-formed endpoint constructs (still no dial — the URL is never reached).
	as, err := NewRemoteAgentServer(AgentServerEndpoint{URL: "wss://127.0.0.1:1", Token: "tok", Fingerprint: validFingerprint}, "probe")
	if err != nil {
		t.Fatalf("NewRemoteAgentServer(valid): %v", err)
	}
	if _, ok := as.(*remoteAgentServer); !ok {
		t.Fatalf("expected *remoteAgentServer, got %T", as)
	}
}
