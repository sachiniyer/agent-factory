package daemon

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/internal/testguard"
)

// isAdjectiveNoun reports whether name looks like a namegen suggestion: two
// non-empty lowercase [a-z] words joined by a single hyphen.
func isAdjectiveNoun(name string) bool {
	parts := strings.Split(name, "-")
	if len(parts) != 2 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		if strings.ToLower(p) != p {
			return false
		}
	}
	return true
}

// TestSuggestSessionName_ReturnsReadableName pins that the RPC serves a readable
// adjective-noun name end-to-end (#2470): the daemon owns the wordlist and the
// handler always answers, even with no sessions and no readiness gate.
func TestSuggestSessionName_ReturnsReadableName(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	m, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}

	var resp SuggestSessionNameResponse
	if err := (&controlServer{manager: m}).SuggestSessionName(SuggestSessionNameRequest{}, &resp); err != nil {
		t.Fatalf("SuggestSessionName: %v", err)
	}
	if !isAdjectiveNoun(resp.Name) {
		t.Fatalf("SuggestSessionName = %q, want an adjective-noun name", resp.Name)
	}
}

// TestSuggestSessionName_ExercisesLiveTitles runs the suggestion with a live
// session present, so the Snapshot→taken-set→namegen path is walked with a
// non-empty title set. The name stays well-formed and never echoes the live
// title (the collision-avoidance itself is pinned deterministically in
// internal/namegen; here it is the daemon wiring that is under test).
func TestSuggestSessionName_ExercisesLiveTitles(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	m, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	const repo = "/tmp/suggest-name-repo"
	rid := config.RepoIDFromRoot(repo)
	startedLocalTabInstance(t, m, rid, repo, "brave-otter", "af_brave_otter_agent")

	cs := &controlServer{manager: m}
	for i := 0; i < 50; i++ {
		var resp SuggestSessionNameResponse
		if err := cs.SuggestSessionName(SuggestSessionNameRequest{}, &resp); err != nil {
			t.Fatalf("SuggestSessionName: %v", err)
		}
		if !isAdjectiveNoun(resp.Name) {
			t.Fatalf("SuggestSessionName = %q, want an adjective-noun name", resp.Name)
		}
		if strings.EqualFold(resp.Name, "brave-otter") {
			t.Fatalf("SuggestSessionName returned the live title %q", resp.Name)
		}
	}
}
