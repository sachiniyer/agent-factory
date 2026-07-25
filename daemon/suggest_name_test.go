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

// TestSuggestSessionName_BuildsTakenFromLiveTitles pins the daemon wiring
// DETERMINISTICALLY (the random generator cannot be forced into an observable
// collision, so a "call it 50 times and hope" test passes even with the avoidance
// deleted). It captures the collision predicate the handler hands the generator and
// asserts it: a live session's title (any case) reads as taken, a name no session
// holds reads as free. If the Snapshot projection stopped carrying Title, or the
// ToLower/TrimSpace key stopped matching, this fails instead of flaking.
func TestSuggestSessionName_BuildsTakenFromLiveTitles(t *testing.T) {
	t.Setenv("AGENT_FACTORY_HOME", testguard.SocketTempDir(t))
	m, err := NewManager(config.DefaultConfig())
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	const repo = "/tmp/suggest-name-repo"
	rid := config.RepoIDFromRoot(repo)
	// A mixed-case title so the case-insensitive match is actually exercised.
	startedLocalTabInstance(t, m, rid, repo, "Brave-Otter", "af_brave_otter_agent")

	var captured func(string) bool
	orig := suggestName
	suggestName = func(taken func(string) bool) string {
		captured = taken
		return "captured-stub"
	}
	t.Cleanup(func() { suggestName = orig })

	var resp SuggestSessionNameResponse
	if err := (&controlServer{manager: m}).SuggestSessionName(SuggestSessionNameRequest{}, &resp); err != nil {
		t.Fatalf("SuggestSessionName: %v", err)
	}
	if resp.Name != "captured-stub" {
		t.Fatalf("handler did not return the generator's output: %q", resp.Name)
	}
	if captured == nil {
		t.Fatal("handler never called the generator")
	}
	if !captured("brave-otter") {
		t.Error("a live session's title must read as taken (the collision set is empty or unwired)")
	}
	if !captured("BRAVE-OTTER") {
		t.Error("the collision check must be case-insensitive")
	}
	if captured("some-unused-name") {
		t.Error("a name no live session holds must read as free")
	}
}
