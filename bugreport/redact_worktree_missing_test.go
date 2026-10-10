package bugreport

import (
	"strings"
	"testing"

	"github.com/sachiniyer/agent-factory/session"
)

// Worktree.MissingReason embeds the vanished worktree path — a user-meaningful
// repo location and the session title af names the directory after — in an
// af-authored sentence (#5102). It must ship with the path collapsed like
// WorktreePath and the sentence intact, so triage can still tell a live
// deletion from one found at archive time.
func TestRedactInstancesJSONRebuildsWorktreeMissingReason(t *testing.T) {
	const wt = "/srv/ConfidentialClient/repo-ProjectKingfisher"
	for _, reason := range []string{
		"tracked worktree path " + wt + " does not exist (deleted outside af)",
		session.WorktreeMissingArchivedReason(wt),
	} {
		r := &redactor{}
		out := redactOneInstance(t, r, session.InstanceData{
			ID:    "abc123",
			Title: "ProjectKingfisher",
			Worktree: session.GitWorktreeData{
				RepoPath:      "/srv/ConfidentialClient/repo",
				WorktreePath:  wt,
				Missing:       true,
				MissingReason: reason,
			},
		})
		for _, secret := range []string{"ProjectKingfisher", "ConfidentialClient"} {
			if strings.Contains(out, secret) {
				t.Errorf("the missing-worktree reason carried %q into the bundle:\n%s", secret, out)
			}
		}
		if !strings.Contains(out, "(deleted outside af)") {
			t.Errorf("the missing-worktree reason lost its af-authored sentence:\n%s", out)
		}
	}
}

// Text that is not one of af's forms has no structure to preserve, so it
// collapses whole rather than shipping on a guess about where its path is.
func TestRedactInstancesJSONCollapsesUnknownWorktreeMissingReason(t *testing.T) {
	r := &redactor{}
	out := redactOneInstance(t, r, session.InstanceData{
		ID: "abc123",
		Worktree: session.GitWorktreeData{
			Missing:       true,
			MissingReason: "gone from /srv/ConfidentialClient/repo-wt",
		},
	})
	if strings.Contains(out, "ConfidentialClient") {
		t.Errorf("an unrecognized missing-worktree reason shipped verbatim:\n%s", out)
	}
	if !strings.Contains(out, `"missing_reason": "`+redactedMarker+`"`) {
		t.Errorf("an unrecognized missing-worktree reason must collapse to the marker:\n%s", out)
	}
}
