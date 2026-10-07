package app

import (
	"testing"

	sessiontmux "github.com/sachiniyer/agent-factory/session/tmux"
)

// teardownMayHitOwnTTY is the scope gate for the SIGHUP shield (#5182): only a
// teardown whose target family contains this TUI's pane may hold the signal.
// An unresolvable identity must still answer true — a TUI that cannot prove
// its own session is disjoint from the target is a TUI that may die
// mid-reply.
func TestTeardownMayHitOwnTTY(t *testing.T) {
	cases := []struct {
		name   string
		own    string
		target string
		mayHit bool
	}{
		{name: "exact match is self", own: "af_abc12345_work", target: "af_abc12345_work", mayHit: true},
		{name: "nested TUI in target's tab", own: "af_abc12345_work__shell", target: "af_abc12345_work", mayHit: true},
		{name: "different session is disjoint", own: "af_abc12345_work", target: "af_abc12345_other", mayHit: false},
		{name: "prefix-similar session is disjoint", own: "af_abc12345_work", target: "af_abc12345_wo", mayHit: false},
		{name: "tab of another session is disjoint", own: "af_abc12345_work", target: "af_abc12345_other__shell", mayHit: false},
		{name: "closing own tab hits self", own: "af_abc12345_work__shell", target: "af_abc12345_work__shell", mayHit: true},
		{name: "unknown own shields", own: "", target: "af_abc12345_work", mayHit: true},
		{name: "unknown target shields", own: "af_abc12345_work", target: "", mayHit: true},
		{name: "both unknown shields", own: "", target: "", mayHit: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(sessiontmux.EnvMarkerSession, tc.own)
			if got := teardownMayHitOwnTTY(tc.target); got != tc.mayHit {
				t.Fatalf("teardownMayHitOwnTTY(%q) with AF_SESSION=%q = %v, want %v", tc.target, tc.own, got, tc.mayHit)
			}
		})
	}
}

// teardownOwnSessionInRepo is the delete-project half of the shield gate
// (#5182): the project that owns this TUI's pane is the one AF_SESSION's
// repo-hash names, not whichever project the TUI is currently viewing —
// viewing B while living inside A must still shield A's delete.
func TestTeardownOwnSessionInRepo(t *testing.T) {
	ownRoot := "/repos/alpha"
	otherRoot := "/repos/beta"
	ownName := sessiontmux.SanitizedNameForRepo("work", ownRoot)
	tabName := ownName + "__shell"
	otherName := sessiontmux.SanitizedNameForRepo("other", otherRoot)

	cases := []struct {
		name   string
		own    string
		target string
		mayHit bool
	}{
		{name: "session in deleted repo", own: ownName, target: ownRoot, mayHit: true},
		{name: "tab of session in deleted repo", own: tabName, target: ownRoot, mayHit: true},
		{name: "session in other repo", own: otherName, target: ownRoot, mayHit: false},
		{name: "same title other repo", own: sessiontmux.SanitizedNameForRepo("work", otherRoot), target: ownRoot, mayHit: false},
		{name: "unknown own shields", own: "", target: ownRoot, mayHit: true},
		{name: "empty target shields", own: ownName, target: "", mayHit: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(sessiontmux.EnvMarkerSession, tc.own)
			if got := teardownOwnSessionInRepo(tc.target); got != tc.mayHit {
				t.Fatalf("teardownOwnSessionInRepo(%q) with AF_SESSION=%q = %v, want %v", tc.target, tc.own, got, tc.mayHit)
			}
		})
	}
}
