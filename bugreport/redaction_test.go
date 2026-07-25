package bugreport

import (
	"strings"
	"testing"
)

// Username / branch redaction cases (#2533). Split out of bugreport_test.go to
// keep that file under the 1500-line limit (#1145); package and helpers are shared.

// TestScrubRedactsUsernameEndingInNonWordChar is the #2533 regression: an OS
// username ending in a non-word rune (hyphen, dot, …) has no word boundary after
// it, so the old `\b<name>\b` regex never matched it. The branch field
// (`<username>/<session>`, left for the text scrub) then leaked the username in a
// bundle meant to be safe to share. Against master these leak.
func TestScrubRedactsUsernameEndingInNonWordChar(t *testing.T) {
	for _, tc := range []struct {
		name, user, in, leak, want string
	}{
		{"trailing hyphen", "test-", "branch test-/fix-login-bug", "test-/fix-login-bug", userMarker + "/fix-login-bug"},
		{"trailing dot", "svc.", "worktree /srv/svc./repo", "svc./repo", userMarker + "/repo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &redactor{home: "/home/" + tc.user, users: []string{tc.user}}
			out := r.scrub(tc.in)
			if strings.Contains(out, tc.leak) {
				t.Errorf("username ending in a non-word char leaked through scrub: %q\n%s", tc.leak, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("expected %q after scrub:\n%s", tc.want, out)
			}
		})
	}
}

// TestScrubDoesNotOverRedactUsernameInsideAWord guards the boundary from the other
// side: a username must not be blanked when it is only part of a longer word.
func TestScrubDoesNotOverRedactUsernameInsideAWord(t *testing.T) {
	r := &redactor{home: "/home/al", users: []string{"al"}}
	if out := r.scrub("algorithm alerts"); out != "algorithm alerts" {
		t.Errorf("username 'al' over-redacted inside a word: %q", out)
	}
}

// TestRedactInstanceDataBranchUsernameScrubbed is the end-to-end #2533 lock: the
// Branch field is not field-redacted (paths are left for the text scrub by design),
// so a username with a non-word tail must still be blanked out of the rendered
// branch by the scrub the redaction path applies.
func TestRedactInstanceDataBranchUsernameScrubbed(t *testing.T) {
	r := &redactor{home: "/home/test-", users: []string{"test-"}}
	if out := r.scrub(`"branch":"test-/fix-login-bug"`); strings.Contains(out, "test-/fix-login-bug") {
		t.Errorf("branch username leaked through the redaction scrub:\n%s", out)
	}
}

// TestScrubRedactsMixedCaseUsernameLowercasedInBranch is the #2533 mixed-case half:
// config.BranchPrefix lowercases the username, so a mixed-case account carries a
// LOWERCASED branch prefix. Byte-exact matching against the raw username alone
// misses it, so appendUserToken registers the lowercased form too. Against master
// (raw token only) the username ships verbatim.
func TestScrubRedactsMixedCaseUsernameLowercasedInBranch(t *testing.T) {
	r := &redactor{home: "/home/Sachin.Iyer", users: appendUserToken(nil, "Sachin.Iyer")}
	out := r.scrub(`"branch":"sachin.iyer/fix-login-bug"`)
	if strings.Contains(out, "sachin.iyer/fix-login-bug") {
		t.Errorf("lowercased branch username leaked for a mixed-case account:\n%s", out)
	}
	if !strings.Contains(out, userMarker+"/fix-login-bug") {
		t.Errorf("expected [user]/fix-login-bug after scrub:\n%s", out)
	}
}

// TestScrubUsernameLongestFirstAvoidsPrefixShadow is the #2533 ordering invariant:
// a short username that is a prefix of a longer one (raw "jdoe" vs a home basename
// "jdoe.admin") must be redacted longest-first, or redacting "jdoe" first destroys
// the only match for the longer token and strands ".admin" — the same invariant the
// title scrub already keeps. Without the sort this leaks ".admin".
func TestScrubUsernameLongestFirstAvoidsPrefixShadow(t *testing.T) {
	r := &redactor{home: "/home/jdoe.admin", users: []string{"jdoe", "jdoe.admin"}}
	out := r.scrub("branch jdoe.admin/fix-1")
	if strings.Contains(out, ".admin") {
		t.Errorf("shorter username shadowed the longer one, stranding a suffix:\n%s", out)
	}
	if !strings.Contains(out, userMarker+"/fix-1") {
		t.Errorf("expected [user]/fix-1 (longest-first match):\n%s", out)
	}
}
