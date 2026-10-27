//go:build !windows

package sessionenv

import (
	"slices"
	"strings"
	"testing"
)

// The W1.1 runtime-identity credential boundary (#5571, approved design):
//
//   - AF_TOKEN_REF is the ONLY name that may carry the token reference, and it
//     reaches the agent-server through the per-invocation extras channel —
//     never commonNames, which is ALSO the agent/shell pane allowlist, so a
//     name in it is inherited by every pane the agent-server spawns.
//   - AF_AGENT_SERVER_TOKEN must not exist at all: the bearer token lives in a
//     0600 file the remote server locates beside its own binary, so neither
//     the token nor its path can enter an environment anything inherits.
func TestAgentServerRuntimeIdentityEnvBoundary(t *testing.T) {
	for _, name := range []string{"AF_TOKEN_REF", "AF_AGENT_SERVER_TOKEN"} {
		if _, ok := commonNames[name]; ok {
			t.Fatalf("%s must never join commonNames — that set is the pane allowlist and would leak it into every pane the agent-server spawns", name)
		}
		for agent := range agentNames {
			if _, ok := agentNames[agent][name]; ok {
				t.Fatalf("%s must never join the %s allowlist — spawned panes inherit it", name, agent)
			}
		}
	}

	// The agent-server's own ambient environment carries the ref (provisioning
	// sources it before launch). The extras channel is what admits it — and
	// only it: without the explicit grant the filter strips it.
	ambient := []string{"AF_TOKEN_REF=v1.nonce", "AF_AGENT_SERVER_TOKEN=secret", "PATH=/usr/bin"}
	kept := FilterForCommand(ambient, "", "", []string{"AF_TOKEN_REF"})
	if !slices.Contains(kept, "AF_TOKEN_REF=v1.nonce") {
		t.Fatal("the extras channel must carry AF_TOKEN_REF into the agent-server's exec-time environment")
	}
	if slices.Contains(kept, "AF_AGENT_SERVER_TOKEN=secret") {
		t.Fatal("no channel may admit a bearer token into an environment")
	}
	if slices.Contains(FilterForCommand(ambient, "", "", nil), "AF_TOKEN_REF=v1.nonce") {
		t.Fatal("AF_TOKEN_REF survived filtering without an extras grant — it must be per-invocation policy only")
	}

	// And the proof the approval requires: a pane the agent-server spawns runs
	// through the SAME filter with its own extras — the ref does not ride along
	// from the server's ambient environment.
	pane := FilterForCommand(ambient, "claude", "claude", nil)
	if slices.Contains(pane, "AF_TOKEN_REF=v1.nonce") || slices.Contains(pane, "AF_AGENT_SERVER_TOKEN=secret") {
		t.Fatal("a spawned pane inherited runtime-identity material — the ref and any token must die at the filter")
	}

	// The handoff itself carries only NAMES: the wrapped command must name
	// AF_TOKEN_REF in its extras segment while containing neither the ref's
	// value nor any token.
	serverArgs := []string{"agent-server", "--listen", "127.0.0.1:0", "--repo", "/r", "--title", "t"}
	wrapped, err := WrapAgentServerCommand("/x/af", []string{"AF_TOKEN_REF"}, serverArgs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(wrapped, AgentServerExecMarker) || !strings.Contains(wrapped, "AF_TOKEN_REF") {
		t.Fatalf("handoff lost the ref name: %q", wrapped)
	}
	if strings.Contains(wrapped, "v1.nonce") || strings.Contains(wrapped, "secret") {
		t.Fatalf("handoff argv carried an identity VALUE — only names may travel in argv: %q", wrapped)
	}
}
