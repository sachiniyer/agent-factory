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
			wantBranch: "Proj-X", want: true,
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
			name:  "an off-box pair still collides where it did before per-project prefixes",
			title: "A B", naming: offBox,
			claim:      BranchClaim{Title: "a-b", Local: false},
			wantBranch: "global/a-b", want: true,
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
