package git

import "testing"

// TestClaimCollision pins the rule the daemon's admission and the TUI's naming
// pre-check share (#4539). "#x" and "-x" derive one branch under "proj-"
// ("proj-x") and two under "global/" ("global/x", "global/-x").
func TestClaimCollision(t *testing.T) {
	local := TitleNaming{Prefix: "proj-", GlobalPrefix: "global/", Local: true}
	offBox := TitleNaming{Prefix: "global/", GlobalPrefix: "global/", Local: false}
	tests := []struct {
		name       string
		title      string
		naming     TitleNaming
		claim      BranchClaim
		wantBranch string
		want       bool
	}{
		{
			name:  "recorded branch from an older prefix does not collide",
			title: "-x", naming: local,
			claim: BranchClaim{Title: "#x", Branch: "global/x", Local: true},
		},
		{
			name:  "recorded branch equal to the derived one collides",
			title: "-x", naming: local,
			claim:      BranchClaim{Title: "#x", Branch: "proj-x", Local: true},
			wantBranch: "proj-x", want: true,
		},
		{
			name:  "recorded branch compares case-insensitively",
			title: "-x", naming: local,
			claim:      BranchClaim{Title: "#x", Branch: "Proj-X", Local: true},
			wantBranch: "proj-x", want: true,
		},
		{
			name:  "no recorded branch is derived under the create's prefix",
			title: "-x", naming: local,
			claim:      BranchClaim{Title: "#x", Local: true},
			wantBranch: "proj-x", want: true,
		},
		{
			name:  "titles that differ only in case always collide",
			title: "Foo", naming: local,
			claim: BranchClaim{Title: "foo", Branch: "global/foo", Local: true},
			want:  true,
		},
		{
			name:  "an archived session renamed off a title does not hold the branch it left behind",
			title: "foo", naming: TitleNaming{Prefix: "global/", GlobalPrefix: "global/", Local: true},
			claim: BranchClaim{Title: "foo (archived)", Branch: "global/foo", Local: true, Relinquished: true},
		},
		{
			name:  "a title that derives a defended recorded branch collides",
			title: "-x", naming: local,
			// The claim's title derives "proj-zzz", but the row still records
			// "proj-x" — a lane checked out on a branch its title never
			// derived. A create titled "-x" would adopt that ref out from
			// under the record (#4562 review).
			claim:      BranchClaim{Title: "zzz", Branch: "proj-x", Local: true},
			wantBranch: "proj-x", want: true,
		},
		{
			name:  "the recorded-branch check compares case-insensitively",
			title: "-X", naming: local,
			claim:      BranchClaim{Title: "zzz", Branch: "Proj-X", Local: true},
			wantBranch: "proj-x", want: true,
		},
		{
			name:  "an off-box create is judged under the global prefix",
			title: "-x", naming: offBox,
			claim: BranchClaim{Title: "#x", Local: false},
		},
		{
			name:  "a local create against an off-box claim is judged under the global prefix",
			title: "-x", naming: local,
			claim: BranchClaim{Title: "#x", Branch: "sandbox/x", Local: false},
		},
		{
			name:  "an off-box reservation keeps the prefix it pinned, not the live global",
			title: "-x",
			// The global prefix moved to "new-" after "#x" reserved "global/x";
			// "-x" derives "new-x", which nothing holds. The reservation's
			// pinned name is its whole answer — no sandbox ref exists yet to
			// collide with (#4562 review).
			naming: TitleNaming{Prefix: "new-", GlobalPrefix: "new-", Local: false},
			claim:  BranchClaim{Title: "#x", Branch: "global/x", Local: false, Pinned: true},
		},
		{
			name:  "an off-box pair still collides where it did before per-project prefixes",
			title: "A B", naming: offBox,
			claim:      BranchClaim{Title: "a-b", Local: false},
			wantBranch: "global/a-b", want: true,
		},
		{
			name:  "an off-box recorded branch under a foreign prefix keeps the sanitizer collision",
			title: "a-b", naming: offBox,
			// The settled row's sandbox derived "sandbox/a-b"; the host checks
			// under "global/", so the recorded-arm compare misses — but a new
			// sandbox with the same config derives "sandbox/a-b" again, so the
			// title-level collision must still refuse (#4562 review).
			claim:      BranchClaim{Title: "A B", Branch: "sandbox/a-b", Local: false},
			wantBranch: "global/a-b", want: true,
		},
		{
			name:  "a local recorded branch under a foreign prefix is judged by that branch alone",
			title: "a-b", naming: local,
			// Contrast with the off-box row above: a host-local record holds
			// "old/a-b" as a real ref, "a-b" derives "proj-a-b" — distinct
			// refs — so re-deriving its title under the live prefix is an
			// artifact, not a collision.
			claim: BranchClaim{Title: "A B", Branch: "old/a-b", Local: true},
		},
		{
			name:  "a relinquished claim still holds its sanitizer-level title",
			title: "foo-archived", naming: TitleNaming{Prefix: "global/", GlobalPrefix: "global/", Local: true},
			// Relinquished drops only the recorded-branch defense; a new title
			// sanitizing to the renamed row's title still collides.
			claim:      BranchClaim{Title: "foo (archived)", Branch: "global/foo", Local: true, Relinquished: true},
			wantBranch: "global/foo-archived", want: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			branch, got := ClaimCollision(tt.title, tt.naming, tt.claim)
			if got != tt.want || branch != tt.wantBranch {
				t.Fatalf("ClaimCollision(%q, %+v, %+v) = (%q, %v), want (%q, %v)",
					tt.title, tt.naming, tt.claim, branch, got, tt.wantBranch, tt.want)
			}
		})
	}
}
