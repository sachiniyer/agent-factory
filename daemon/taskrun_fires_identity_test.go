package daemon

import (
	"testing"

	"github.com/sachiniyer/agent-factory/session"
	"github.com/sachiniyer/agent-factory/session/tmux"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestTaskFireForeignRowReasonTmuxIdentity pins #5696: a row owns the fire's
// tmux name only through its tmux identity fields, never because its raw title
// (stored in Worktree.SessionName) happens to equal the sanitized name.
func TestTaskFireForeignRowReasonTmuxIdentity(t *testing.T) {
	root := "/repo/identity"
	fire := &taskFireMarker{Title: "foo"}
	sanitized := tmux.SanitizedNameForRepo("foo", root)

	t.Run("title equal to the sanitized name does not own it", func(t *testing.T) {
		stranger := session.InstanceData{
			Title:    sanitized,
			TmuxName: tmux.SanitizedNameForRepo(sanitized, root),
			Worktree: session.GitWorktreeData{SessionName: sanitized},
		}
		assert.Equal(t, "", taskFireForeignRowReason(&stranger, fire, root, sanitized, "", ""))
	})
	t.Run("a true TmuxName owner still blocks", func(t *testing.T) {
		owner := session.InstanceData{Title: "other", TmuxName: sanitized}
		assert.Equal(t, "tmux session name", taskFireForeignRowReason(&owner, fire, root, sanitized, "", ""))
	})
	t.Run("a tab's tmux name owns it", func(t *testing.T) {
		owner := session.InstanceData{Title: "other", Tabs: []session.TabData{{TmuxName: sanitized}}}
		assert.Equal(t, "tmux session name", taskFireForeignRowReason(&owner, fire, root, sanitized, "", ""))
	})
	t.Run("a legacy row without tmux fields owns the name its title sanitizes to", func(t *testing.T) {
		slashFire := &taskFireMarker{Title: "api/refactor"}
		slashName := tmux.SanitizedNameForRepo(slashFire.Title, root)
		legacy := session.InstanceData{Title: "api_refactor"}
		require.Equal(t, slashName, tmux.SanitizedNameForRepo(legacy.Title, root), "precondition: the sanitizer collapses these titles")
		assert.Equal(t, "tmux session name", taskFireForeignRowReason(&legacy, slashFire, root, slashName, "", ""))
		unrelated := session.InstanceData{Title: "other"}
		assert.Equal(t, "", taskFireForeignRowReason(&unrelated, slashFire, root, slashName, "", ""))
	})
	t.Run("the fire's own leftover record matches by TmuxName", func(t *testing.T) {
		leftover := session.InstanceData{Title: "foo-leftover", TmuxName: sanitized, Worktree: session.GitWorktreeData{SessionName: sanitized}}
		assert.Equal(t, "tmux session name", taskFireForeignRowReason(&leftover, fire, root, sanitized, "", ""))
	})
	t.Run("title, worktree and branch still block", func(t *testing.T) {
		assert.Equal(t, "title", taskFireForeignRowReason(&session.InstanceData{Title: "foo"}, fire, root, sanitized, "", ""))
		assert.Equal(t, "worktree path", taskFireForeignRowReason(&session.InstanceData{Title: "x", Worktree: session.GitWorktreeData{WorktreePath: "/w"}}, fire, root, sanitized, "/w", ""))
		assert.Equal(t, "branch", taskFireForeignRowReason(&session.InstanceData{Title: "x", Branch: "b"}, fire, root, sanitized, "", "b"))
	})
}
