package agentaccount

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// TestLoginTmuxSessionName_MatchesTmuxSanitization is the drift test for the
// loginTmuxPrefix / loginTmuxStableRune mirror. LoginTmuxSessionName reproduces
// session/tmux.toTmuxName's title fold and the 'af_' prefix in this package
// (which deliberately does not import session/tmux), so if either the prefix or
// the rune policy in session/tmux ever changes, this test goes red in
// agentaccount rather than letting a registration guard fall silently out of
// sync with the names adopt() actually keys on.
//
// It is the direct analogue of TestAccountCredentialArtifactsMatchTheMountedCredentials:
// a mirror across the one-way import boundary, held equal by a test in the
// package that CAN cross it.
func TestLoginTmuxSessionName_MatchesTmuxSanitization(t *testing.T) {
	// Names cover the punctuation nameRule admits (".", "_", "-"), the precise
	// '.' vs '_' collision the guard exists for, mixed punctuation, a dotless
	// name, and a case-variant pair (case is PRESERVED by the fold, so it must
	// NOT collapse — that is refuseCaseCollision's job, not this guard's).
	cases := []struct {
		agent string
		name  string
	}{
		{"codex", "work"},
		{"codex", "proj.test"},
		{"codex", "proj_test"},
		{"codex", "a.b.c"},
		{"codex", "v2.api"},
		{"codex", "team.eu"},
		{"codex", "stage_dev"},
		{"codex", "Weird.Name"},
		{"claude", "my-work"},
		{"gemini", "build.env"},
	}
	for _, tc := range cases {
		got := LoginTmuxSessionName(tc.agent, tc.name)
		want := tmux.NewTmuxSession(LoginSessionName(tc.agent, tc.name), "").SanitizedName()
		if got != want {
			t.Fatalf("LoginTmuxSessionName(%q, %q) = %q; want the real tmux-sanitized name %q\n"+
				"the mirror in agentaccount has drifted from session/tmux.toTmuxName",
				tc.agent, tc.name, got, want)
		}
		// Sanity: the sanitized name lives in the af_ namespace every cleanup path
		// recognizes, and contains only tmux-stable runes after the fold.
		if !strings.HasPrefix(got, "af_") {
			t.Fatalf("LoginTmuxSessionName(%q, %q) = %q; lost the af_ prefix", tc.agent, tc.name, got)
		}
	}
}

// TestLoginTmuxSessionName_DotAndUnderscoreFoldToTheSameName pins the precise
// collision the bug report names: two distinct, nameRule-valid names that differ
// only by '.' vs '_' sanitize to ONE tmux login-pane name, which is the
// precondition adopt() needs to hand the second account's login to the first
// account's pane. The registration guard built on this derivation is what breaks
// the pair; this test documents why the guard must fire.
func TestLoginTmuxSessionName_DotAndUnderscoreFoldToTheSameName(t *testing.T) {
	for _, tc := range []struct{ a, b string }{
		{"proj.test", "proj_test"},
		{"a.b.c", "a_b_c"},
		{"v2.api", "v2_api"},
		// A '.'/'_' pair with an extra dot still collapses, because every '.'
		// folds independently.
		{"a..b", "a__b"},
	} {
		dot := LoginTmuxSessionName("codex", tc.a)
		unders := LoginTmuxSessionName("codex", tc.b)
		if dot != unders {
			t.Fatalf("%q and %q derive to distinct tmux names %q and %q; "+
				"the dot/underscore fold is the collision this guard exists for",
				tc.a, tc.b, dot, unders)
		}
	}
}

// TestLoginTmuxSessionName_PreservesCase prevents the sanitization guard from
// over-reaching into refuseCaseCollision's territory. toTmuxName preserves
// letters, so `work` and `Work` keep distinct login-pane names; the case
// collision is a different hazard handled by a different guard, and this
// derivation must not conflate the two.
func TestLoginTmuxSessionName_PreservesCase(t *testing.T) {
	lower := LoginTmuxSessionName("codex", "work")
	upper := LoginTmuxSessionName("codex", "Work")
	if lower == upper {
		t.Fatalf("case-variant names collapsed to one tmux name %q; "+
			"case is preserved by tmux sanitization and is refuseCaseCollision's concern, "+
			"not this guard's", lower)
	}
}

// TestLoginTmuxSessionName_ScopesByAgent keeps the agent segment of the name
// load-bearing: codex/work and claude/work are different accounts and must get
// different login-pane names, so a collision guard scoped to one agent never
// refuses a name another agent already owns.
func TestLoginTmuxSessionName_ScopesByAgent(t *testing.T) {
	codex := LoginTmuxSessionName("codex", "work")
	claude := LoginTmuxSessionName("claude", "work")
	if codex == claude {
		t.Fatalf("codex/work and claude/work share the tmux login name %q; "+
			"the agent segment must keep per-agent namespaces apart", codex)
	}
}
