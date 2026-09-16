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

// TestReservedIdentityStaysInsideAdmission pins the #4396 relationship after
// the case-variant review fix: admission is a strict SUPERSET of identity.
// Every title the record-identity predicate calls the root is refused at
// create — no admitted session can be projected as the root, the incoherence
// the admission rule exists to prevent — while admission additionally refuses
// the case variants whose derived tmux name is distinct.
func TestReservedIdentityStaysInsideAdmission(t *testing.T) {
	for _, title := range []string{
		"root", "Root", " ROOT ", "ROOT",
		"ro ot", "r o o t", "ro\tot", "Ro ot", "RO OT",
		"worker", "root-2", "rooted", "ro-ot", "Ro-ot", "root!", "root_", "toor", "",
	} {
		if IsReservedTitle(title) && ReservedTitleCollision(title) == "" {
			t.Fatalf("IsReservedTitle(%q) = true but admission would accept it — a session could be created that the daemon projects as the root", title)
		}
	}
}

// TestIsReservedTitleCoversExactlyWhatARecordCanClaim enumerates the identity
// predicate itself: the byte-exact derived-name claim plus the shapes the
// pre-#4396 spelling rule caught — and NOT the case variants admission widened
// past it.
func TestIsReservedTitleCoversExactlyWhatARecordCanClaim(t *testing.T) {
	// Reserved: whitespace-interleaved spellings whose derived tmux name is
	// af_root byte-for-byte — the claim is real, two records cannot own one
	// tmux session — plus the legacy trim-and-fold shapes.
	for _, title := range []string{"root", "Root", "ROOT", " root ", "ro ot", "r o o t", "ro\tot", "ro ot"} {
		if !IsReservedTitle(title) {
			t.Fatalf("IsReservedTitle(%q) = false, want reserved", title)
		}
	}
	// Ordinary: a whitespace-interleaved CASE VARIANT ("Ro ot") derives a
	// case-distinct tmux name (af_Root — tmux names are case-sensitive), so it
	// was admissible before #4396 and a stored record under it is a real
	// session, not the root — unarchivable/unrecoverable were it projected as
	// reserved. Admission still refuses it going forward (pinned above); the
	// record keeps the identity its tmux name actually claims.
	for _, title := range []string{"Ro ot", "RO OT", "r Oo t", "rO OT"} {
		if IsReservedTitle(title) {
			t.Fatalf("IsReservedTitle(%q) = true: a case-distinct derived name (af_Root) is not the root's af_root", title)
		}
		if ReservedTitleCollision(title) == "" {
			t.Fatalf("ReservedTitleCollision(%q) = no collision: admission must still refuse the lookalike", title)
		}
	}
}

// TestIsReservedRecordTitleScopesDerivedNameToLocalTmux pins the record-identity
// boundary: the derived-tmux-name clause exists because two records cannot own
// one local tmux session, so it only applies to a record that claims a name in
// the local tmux namespace. A pre-#3732 remote record titled "ro ot" has no
// local tmux name to collide with af_root and keeps ordinary identity; the
// spelling clause still marks a remote "root" as reserved.
func TestIsReservedRecordTitleScopesDerivedNameToLocalTmux(t *testing.T) {
	for _, backendType := range []string{"docker", "ssh", "sandbox", "remote"} {
		for _, title := range []string{"ro ot", "r o o t", "ro\tot", "ro  ot"} {
			if IsReservedRecordTitle(title, backendType) {
				t.Fatalf("IsReservedRecordTitle(%q, %q) = true: a provisioned-backend record claims no local tmux name", title, backendType)
			}
		}
		for _, title := range []string{"root", "Root", " ROOT "} {
			if !IsReservedRecordTitle(title, backendType) {
				t.Fatalf("IsReservedRecordTitle(%q, %q) = false: the reserved spelling holds on any backend", title, backendType)
			}
		}
	}
	for _, backendType := range []string{"", "local"} {
		for _, title := range []string{"ro ot", "r o o t"} {
			if !IsReservedRecordTitle(title, backendType) {
				t.Fatalf("IsReservedRecordTitle(%q, %q) = false: a local record deriving af_root IS the reserved session", title, backendType)
			}
		}
	}
}

// TestIsReservedTitleSpellingExcludesDerivedNames pins the recovery question's
// boundary (#4407 review). Ordinary Lost/Dead recovery withholds itself from a
// record so the root ensure loop can heal it — but that loop only finds the
// exact "root" key, and cannot re-create while a derived-name record holds
// af_root. So the spelling predicate must be a strict subset of record
// identity: every spelled root is reserved identity on every backend, and the
// local derived-name records are reserved identity WITHOUT being spelled.
func TestIsReservedTitleSpellingExcludesDerivedNames(t *testing.T) {
	for _, title := range []string{"root", "Root", "ROOT", " root ", " ROOT ", "rOoT"} {
		if !IsReservedTitleSpelling(title) {
			t.Fatalf("IsReservedTitleSpelling(%q) = false, want the reserved spelling", title)
		}
		for _, backendType := range []string{"", "local", "docker", "ssh", "sandbox", "remote"} {
			if !IsReservedRecordTitle(title, backendType) {
				t.Fatalf("IsReservedRecordTitle(%q, %q) = false: a spelled root must be reserved identity everywhere", title, backendType)
			}
		}
	}
	for _, title := range []string{"ro ot", "r o o t", "ro\tot", "ro  ot"} {
		if !IsReservedRecordTitle(title, "local") {
			t.Fatalf("premise: IsReservedRecordTitle(%q, local) = false, want the derived-name identity", title)
		}
		if IsReservedTitleSpelling(title) {
			t.Fatalf("IsReservedTitleSpelling(%q) = true: a derived-name record is not spelled as the root, and withholding its recovery strands it", title)
		}
	}
	for _, title := range []string{"Ro ot", "worker", "root-2", "ro-ot", "rooted", ""} {
		if IsReservedTitleSpelling(title) {
			t.Fatalf("IsReservedTitleSpelling(%q) = true, want an ordinary title", title)
		}
	}
}
