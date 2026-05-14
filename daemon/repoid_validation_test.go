package daemon

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
)

// TestValidateRPCRepoID covers the daemon-side gate that drops malicious
// RepoID values before they reach config.LoadRepoInstances and friends
// (#515). Empty is allowed by design — it tells findSession to search
// across every repo.
func TestValidateRPCRepoID(t *testing.T) {
	t.Run("empty allowed (search-all-repos sentinel)", func(t *testing.T) {
		if err := validateRPCRepoID(""); err != nil {
			t.Fatalf("expected empty RepoID to be allowed, got %v", err)
		}
	})

	rejected := []string{
		"..",
		"../../../etc/passwd",
		"foo/../bar",
		"/etc/passwd",
		"foo/bar",
		"foo\\bar",
		".",
		".hidden",
		"foo\x00bar",
	}
	for _, id := range rejected {
		t.Run("rejected_"+id, func(t *testing.T) {
			err := validateRPCRepoID(id)
			if err == nil {
				t.Fatalf("expected %q to be rejected", id)
			}
			if !strings.Contains(err.Error(), "rejected RPC request") {
				t.Fatalf("error %q did not include rejection prefix", err.Error())
			}
		})
	}

	accepted := []string{
		config.RepoIDFromRoot("/some/path"),
		"test-repo",
		"abc123def456",
	}
	for _, id := range accepted {
		t.Run("accepted_"+id, func(t *testing.T) {
			if err := validateRPCRepoID(id); err != nil {
				t.Fatalf("expected %q to be accepted, got %v", id, err)
			}
		})
	}
}

// TestControlServer_KillSession_RejectsTraversal goes through the actual
// RPC handler entrypoint to lock in the network-boundary defense.
func TestControlServer_KillSession_RejectsTraversal(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	server := &controlServer{manager: manager}

	var resp KillSessionResponse
	err = server.KillSession(KillSessionRequest{
		Title:  "anything",
		RepoID: "../../../etc/passwd",
	}, &resp)
	if err == nil {
		t.Fatalf("expected KillSession to reject traversal RepoID")
	}
	if !strings.Contains(err.Error(), "rejected RPC request") {
		t.Fatalf("KillSession error %q did not include rejection prefix", err.Error())
	}
}

// TestControlServer_SendPrompt_RejectsTraversal covers the other RPC entry
// that accepts a RepoID over the wire.
func TestControlServer_SendPrompt_RejectsTraversal(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", t.TempDir())
	installInstantBackend(t)

	manager, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	server := &controlServer{manager: manager}

	var resp SendPromptResponse
	err = server.SendPrompt(SendPromptRequest{
		Title:  "anything",
		RepoID: "foo/../bar",
		Prompt: "hi",
	}, &resp)
	if err == nil {
		t.Fatalf("expected SendPrompt to reject traversal RepoID")
	}
	if !strings.Contains(err.Error(), "rejected RPC request") {
		t.Fatalf("SendPrompt error %q did not include rejection prefix", err.Error())
	}
}
