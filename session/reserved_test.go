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
//
// The folded variants ("Ro ot") derive a case-DISTINCT tmux name — tmux names
// are case-sensitive — but are refused all the same since #4396, because the
// reserved check folds case on the derived name and a session it admits would
// read as the root everywhere else.
func TestReservedTitleCollisionCatchesDerivedNames(t *testing.T) {
	for _, title := range []string{
		"ro ot",
		"r o o t",
		"ro\tot",
		"ro ot",  // a non-breaking space is whitespace to unicode.IsSpace too
		" root ", // the spelling rule, trimmed
		"ROOT",   // the spelling rule, folded
		"Ro ot",  // folded derived name — a distinct tmux name, refused by the fold
		"RO OT",
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
			for _, want := range []string{fmt.Sprintf("%q", title), RootSessionTitle, "pick another name", "[root_agent]"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("refusal %q does not mention %q", err, want)
				}
			}
		})
	}
}

// TestReservedDerivedNameIsARealNameCollision pins the premise the rule rests
// on. If toTmuxName ever stops deleting whitespace, this fails first and says
// why the rule above exists, instead of leaving it as unexplained folklore.
func TestReservedDerivedNameIsARealNameCollision(t *testing.T) {
	const repoPath = "/repo"
	if got, want := tmux.SanitizedNameForRepo("ro ot", repoPath), tmux.SanitizedNameForRepo(RootSessionTitle, repoPath); got != want {
		t.Fatalf("premise gone: %q derives %q, reserved derives %q", "ro ot", got, want)
	}
}

// TestReservedTitleCollisionLeavesDistinctTitlesAlone keeps the widened rule
// from becoming a phantom restriction on ordinary names. Titles whose derived
// tmux name differs from the reserved one under the fold stay admissible —
// including "root!", which collides with "root" on the BRANCH axis (the record
// scan's job, not this rule's) but claims a tmux name of its own.
func TestReservedTitleCollisionLeavesDistinctTitlesAlone(t *testing.T) {
	for _, title := range []string{"ro-ot", "Ro-ot", "root-2", "rooted", "roo t s", "toor", "root!", "root_"} {
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

// TestReservedIdentityAndAdmissionAreOneQuestion pins the #4396 collapse: the
// identity question (is this record the root?) and the admission question (may
// a create claim this title?) are the SAME predicate over ONE normalization —
// the tmux session name the title derives, compared case-folded. They can no
// longer disagree, which is what let "ro ot" sit between them as a creatable
// name claiming the reserved session's runtime name.
//
// The widened identity is deliberate for the shapes it adds. A record titled
// "ro ot" can only predate the #3732 admission rule — the create gate has
// refused it since — and its tmux name already collides with the root's, so
// every tmux-keyed mechanism (markers, generation cohorts, scope prefixes)
// treated it as the same session anyway. Projecting it as the root is the
// coherent read of that record, not a new privilege for a claimable title.
func TestReservedIdentityAndAdmissionAreOneQuestion(t *testing.T) {
	for _, title := range []string{
		"root", "Root", " ROOT ", "ROOT",
		"ro ot", "r o o t", "ro\tot", "Ro ot",
		"worker", "root-2", "rooted", "ro-ot", "Ro-ot", "root!", "root_", "toor", "",
	} {
		if got, want := IsReservedTitle(title), ReservedTitleCollision(title) != ""; got != want {
			t.Fatalf("identity and admission disagree on %q: IsReservedTitle=%v, ReservedTitleCollision=%q",
				title, got, ReservedTitleCollision(title))
		}
	}
	// The widening itself: a whitespace-interleaved spelling claims the root's
	// tmux name, so a record holding one IS the reserved session to af.
	for _, title := range []string{"ro ot", "r o o t", "ro\tot", "Ro ot"} {
		if !IsReservedTitle(title) {
			t.Fatalf("IsReservedTitle(%q) = false; a title claiming the root's derived name must read as the reserved session", title)
		}
	}
}
