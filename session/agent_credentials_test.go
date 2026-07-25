package session

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// TestAgentCredentialMounts_OnlyTheGivenAgent is the core P1-b contract: the
// helper mounts ONLY the named agent's credential file(s), never another
// agent's — even when every agent's file exists on disk. A codex session must
// not receive the Claude token.
func TestAgentCredentialMounts_OnlyTheGivenAgent(t *testing.T) {
	home := "/home/tester"
	allExist := func(string) bool { return true }

	codex := agentCredentialMounts(tmux.ProgramCodex, home, allExist)
	if want := []string{"-v", "/home/tester/.codex/auth.json:/root/.codex/auth.json:ro"}; !reflect.DeepEqual(codex, want) {
		t.Fatalf("codex session mounts:\n got %#v\nwant %#v", codex, want)
	}
	// The Claude credential must never appear for a codex session.
	for _, a := range codex {
		if a == "/home/tester/.claude/.credentials.json:/root/.claude/.credentials.json:ro" {
			t.Fatal("codex session was given the Claude credential")
		}
	}

	claude := agentCredentialMounts(tmux.ProgramClaude, home, allExist)
	if want := []string{"-v", "/home/tester/.claude/.credentials.json:/root/.claude/.credentials.json:ro"}; !reflect.DeepEqual(claude, want) {
		t.Fatalf("claude session mounts:\n got %#v\nwant %#v", claude, want)
	}
	// P2: ~/.claude.json is NOT a credential and must not be mounted.
	for _, a := range claude {
		if a == "/home/tester/.claude.json:/root/.claude.json:ro" {
			t.Fatal(".claude.json must not be mounted (it is the config/privacy blob, not a credential)")
		}
	}
}

// TestAgentCredentialMounts_OnlyExisting: for an agent with several candidate
// filenames (gemini, whose OAuth file name has varied), only the ones present on
// disk are mounted — never an empty -v source docker would auto-create.
func TestAgentCredentialMounts_OnlyExisting(t *testing.T) {
	home := "/home/tester"
	present := map[string]bool{
		filepath.Join(home, ".gemini/gemini-credentials.json"): true,
		// oauth_creds.json and google_accounts.json absent.
	}
	got := agentCredentialMounts(tmux.ProgramGemini, home, func(p string) bool { return present[p] })
	want := []string{"-v", "/home/tester/.gemini/gemini-credentials.json:/root/.gemini/gemini-credentials.json:ro"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("gemini mounts (only existing):\n got %#v\nwant %#v", got, want)
	}
}

// TestAgentCredentialMounts_None covers the "mount nothing" paths: an unknown
// agent, no candidate present, and an unresolved (empty) home. All return nil.
func TestAgentCredentialMounts_None(t *testing.T) {
	if got := agentCredentialMounts("aider", "/home/x", func(string) bool { return true }); got != nil {
		t.Errorf("aider (env-only, no file): want nil, got %#v", got)
	}
	if got := agentCredentialMounts(tmux.ProgramCodex, "/home/x", func(string) bool { return false }); got != nil {
		t.Errorf("no files present: want nil, got %#v", got)
	}
	if got := agentCredentialMounts(tmux.ProgramCodex, "", func(string) bool { return true }); got != nil {
		t.Errorf("empty home: want nil, got %#v", got)
	}
}

// TestAgentCredentialMounts_AllReadOnlyUnderContainerHome guards the invariants
// that hold across every agent and any future candidate: each mount is read-only
// (:ro), the host path is absolute, and the container target is under
// dockerContainerHome (so it lands where the container's clean-env agent looks).
func TestAgentCredentialMounts_AllReadOnlyUnderContainerHome(t *testing.T) {
	home := "/home/tester"
	for agent := range agentCredentialFiles {
		got := agentCredentialMounts(agent, home, func(string) bool { return true })
		for i := 0; i < len(got); i += 2 {
			if got[i] != "-v" {
				t.Fatalf("agent %q arg %d = %q, want -v", agent, i, got[i])
			}
			spec := got[i+1]
			if want := ":ro"; spec[len(spec)-len(want):] != want {
				t.Errorf("agent %q mount %q is not read-only", agent, spec)
			}
			host, container := splitMountSpec(t, spec)
			if !filepath.IsAbs(host) {
				t.Errorf("agent %q host path %q is not absolute", agent, host)
			}
			if want := dockerContainerHome + "/"; container[:len(want)] != want {
				t.Errorf("agent %q container target %q is not under %q", agent, container, dockerContainerHome)
			}
		}
	}
}

// splitMountSpec parses a host:container:ro bind spec. Host and container are
// absolute unix paths (no colons), so splitting on the last two colons is exact.
func splitMountSpec(t *testing.T, spec string) (host, container string) {
	t.Helper()
	body := spec[:len(spec)-len(":ro")]
	sep := -1
	for i := len(body) - 1; i >= 0; i-- {
		if body[i] == ':' {
			sep = i
			break
		}
	}
	if sep < 0 {
		t.Fatalf("malformed mount spec %q", spec)
	}
	return body[:sep], body[sep+1:]
}
