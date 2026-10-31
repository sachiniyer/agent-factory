package daemon

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/session"
)

func pageRow(id, repo string, liveness session.Liveness, created time.Time) session.InstanceData {
	d := session.InstanceData{ID: id, Title: id, Liveness: liveness, CreatedAt: created}
	d.Worktree.RepoPath = repo
	return d
}

func pageIDs(rows []session.InstanceData) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.ID)
	}
	return out
}

// The lazy-archive request shape (#6178): the live-only startup read, the summary
// that stands in for the rows it leaves out, and newest-first archived pages that
// walk the whole set exactly once.
func TestFilterSnapshotPageLazyArchive(t *testing.T) {
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	rows := []session.InstanceData{
		pageRow("live-1", "/p/a", session.LiveRunning, base),
		pageRow("old-a", "/p/a", session.LiveArchived, base.Add(1*time.Hour)),
		pageRow("old-b", "/p/b", session.LiveArchived, base.Add(2*time.Hour)),
		pageRow("old-c", "/p/a", session.LiveArchived, base.Add(3*time.Hour)),
		pageRow("tie-1", "/p/a", session.LiveArchived, base.Add(4*time.Hour)),
		pageRow("tie-2", "/p/a", session.LiveArchived, base.Add(4*time.Hour)),
	}

	t.Run("startup read is the live rows plus a summary", func(t *testing.T) {
		got, next, err := FilterSnapshotPage(SnapshotRequest{LazyArchive: true}, rows)
		require.NoError(t, err)
		require.Equal(t, []string{"live-1"}, pageIDs(got))
		require.Nil(t, next)
		total, byProject := archivedSummary(rows)
		require.Equal(t, 5, total)
		require.Equal(t, map[string]int{"/p/a": 4, "/p/b": 1}, byProject)
	})

	t.Run("pages walk the archive newest first exactly once", func(t *testing.T) {
		limit := 2
		req := SnapshotRequest{Statuses: []string{"archived"}, NewestFirst: true, Limit: &limit}
		var seen []string
		for pages := 0; ; pages++ {
			require.Less(t, pages, 10, "paging did not terminate")
			got, next, err := FilterSnapshotPage(req, rows)
			require.NoError(t, err)
			seen = append(seen, pageIDs(got)...)
			if next == nil {
				break
			}
			req.Before = next
		}
		require.Equal(t, []string{"tie-2", "tie-1", "old-c", "old-b", "old-a"}, seen)
	})

	t.Run("a project path scopes the archive", func(t *testing.T) {
		got, _, err := FilterSnapshotPage(SnapshotRequest{Statuses: []string{"archived"}, NewestFirst: true, ProjectPath: "/p/b"}, rows)
		require.NoError(t, err)
		require.Equal(t, []string{"old-b"}, pageIDs(got))
	})

	t.Run("ids fetch a deep-linked row", func(t *testing.T) {
		got, _, err := FilterSnapshotPage(SnapshotRequest{IDs: []string{"old-a"}}, rows)
		require.NoError(t, err)
		require.Equal(t, []string{"old-a"}, pageIDs(got))
	})

	t.Run("before needs newest_first", func(t *testing.T) {
		require.Error(t, SnapshotRequest{Before: &SnapshotCursor{}}.Validate())
	})

	t.Run("a request with none of the new fields is unchanged", func(t *testing.T) {
		got, next, err := FilterSnapshotPage(SnapshotRequest{}, rows)
		require.NoError(t, err)
		require.Equal(t, rows, got)
		require.Nil(t, next)
		limit := 2
		got, next, err = FilterSnapshotPage(SnapshotRequest{Limit: &limit}, rows)
		require.NoError(t, err)
		require.Equal(t, []string{"live-1", "old-a"}, pageIDs(got), "legacy limit keeps the stored order")
		require.Nil(t, next)
	})
}
