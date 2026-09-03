package session

import (
	"fmt"
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session/tmux"
)

// TestReservedTitleCollisionCatchesDerivedNames is the #3732 red: the reserved
// guard protected the SPELLING of "root" while tmux keys everything on the
// DERIVED name, which deletes interior whitespace. "ro ot" was therefore
// creatable and minted the reserved session's tmux name.
func TestReservedTitleCollisionCatchesDerivedNames(t *testing.T) {
	for _, title := range []string{
		"ro ot",
		"r o o t",
		"ro\tot",
		"ro ot",  // a non-breaking space is whitespace to unicode.IsSpace too
		" root ", // the spelling rule, trimmed
		"ROOT",   // the spelling rule, folded
	} {
		t.Run(title, func(t *testing.T) {
			if got := ReservedTitleCollision(title); got != RootSessionTitle {
				t.Fatalf("ReservedTitleCollision(%q) = %q, want %q", title, got, RootSessionTitle)
			}
			err := ReservedTitleRefusal(title)
			if err == nil {
				t.Fatalf("ReservedTitleRefusal(%q) admitted a title that claims the reserved name", title)
			}
			// Actionable means: it names the title the caller asked for, the
			// reserved title it collides with, and what to do instead.
			// The title is quoted in the message, so a tab or a non-breaking
			// space appears there in its escaped form — compare the same way.
			for _, want := range []string{fmt.Sprintf("%q", title), RootSessionTitle, "pick another name", "root_agents"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal %q does not mention %q", err, want)
				}
			}
		})
	}
}

// TestReservedDerivedNameIsAReadNameCollision pins the premise the rule rests
// on. If toTmuxName ever stops deleting whitespace, this fails first and says
// why the rule above exists, instead of leaving it as unexplained folklore.
func TestReservedDerivedNameIsARealNameCollision(t *testing.T) {
	const repoPath = "/repo"
	if got, want := tmux.SanitizedNameForRepo("ro ot", repoPath), tmux.SanitizedNameForRepo(RootSessionTitle, repoPath); got != want {
		t.Fatalf("premise gone: %q derives %q, reserved derives %q", "ro ot", got, want)
	}
}

// TestReservedTitleCollisionLeavesDistinctTitlesAlone keeps the widened rule
// from becoming a phantom restriction on ordinary names. Case is NOT folded in
// the derived comparison because tmux session names are case-sensitive: "Ro ot"
// derives "Root", a genuinely different session from "root".
func TestReservedTitleCollisionLeavesDistinctTitlesAlone(t *testing.T) {
	for _, title := range []string{"ro-ot", "root-2", "rooted", "roo t s", "toor"} {
		t.Run(title, func(t *testing.T) {
			if got := ReservedTitleCollision(title); got != "" {
				t.Fatalf("ReservedTitleCollision(%q) = %q, want no collision", title, got)
			}
			if err := ReservedTitleRefusal(title); err != nil {
				t.Fatalf("ReservedTitleRefusal(%q) refused an unrelated title: %v", title, err)
			}
		})
	}
}

// TestIsReservedTitleStaysTheIdentityQuestion is the other half of the split.
// A session titled "ro ot" that predates the admission rule is an ORDINARY
// session: projecting it as the root agent would hand it the root's kill grace,
// limit-resume, archive refusal and IsRoot pin in every client.
func TestIsReservedTitleStaysTheIdentityQuestion(t *testing.T) {
	if IsReservedTitle("ro ot") {
		t.Fatal("IsReservedTitle widened to the derived name; an ordinary session would inherit the root agent's lifecycle")
	}
	if ReservedTitleCollision("ro ot") == "" {
		t.Fatal("admission must still refuse the title the identity question ignores")
	}
}
