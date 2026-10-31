package daemon

import (
	"fmt"
	"sort"
	"strings"

	"github.com/sachiniyer/agent-factory/session"
)

// snapshotStatusNames is the accepted `statuses` vocabulary, rendered for the
// "valid: …" half of a rejection. It is DERIVED from session's canonical
// Liveness ↔ name table rather than restated here (#3631): the same table names
// each row's `liveness_name`, so the word this filter accepts and the word the
// payload reports it by cannot drift apart, and a Liveness value added later
// reaches both at once.
var snapshotStatusNames = strings.Join(session.LivenessNameList(), ", ")

// Validate checks the additive Snapshot filters without reading manager state.
func (r SnapshotRequest) Validate() error {
	for _, raw := range r.Statuses {
		status := strings.ToLower(strings.TrimSpace(raw))
		if _, ok := session.ParseLivenessName(status); !ok {
			return fmt.Errorf("invalid session status %q (valid: %s)", raw, snapshotStatusNames)
		}
	}
	if r.Limit != nil && *r.Limit <= 0 {
		return fmt.Errorf("session list limit must be greater than 0")
	}
	if r.Before != nil && !r.NewestFirst {
		return fmt.Errorf("session list before requires newest_first")
	}
	return nil
}

// archivedSummary counts the archived rows of a repo-scoped listing, in total and
// per worktree repo path. A row recorded without a worktree repo path counts
// toward the total only.
func archivedSummary(instances []session.InstanceData) (int, map[string]int) {
	total := 0
	byProject := map[string]int{}
	for _, instance := range instances {
		if session.RecordedLiveness(instance) != session.LiveArchived {
			continue
		}
		total++
		if instance.Worktree.RepoPath != "" {
			byProject[instance.Worktree.RepoPath]++
		}
	}
	return total, byProject
}

// FilterSnapshotInstances applies a Snapshot request to already repo-scoped,
// stably ordered rows. The daemon calls it before building the response
// envelope, which is the payload-size boundary #3362 requires. The CLI also
// calls it for daemonless disk fallback and as a version-skew safety net when an
// older daemon ignores additive request fields.
//
// With no filters it returns the input slice unchanged. This makes the legacy
// no-flag path explicit: same rows, same order, same nil-versus-empty shape.
func FilterSnapshotInstances(req SnapshotRequest, instances []session.InstanceData) ([]session.InstanceData, error) {
	filtered, _, err := FilterSnapshotPage(req, instances)
	return filtered, err
}

// FilterSnapshotPage is FilterSnapshotInstances that also returns the cursor of
// the next page when Limit cut the listing short (nil when it did not).
func FilterSnapshotPage(req SnapshotRequest, instances []session.InstanceData) ([]session.InstanceData, *SnapshotCursor, error) {
	if err := req.Validate(); err != nil {
		return nil, nil, err
	}
	if !req.hasFilters() {
		return instances, nil, nil
	}

	statuses := make(map[session.Liveness]struct{}, len(req.Statuses))
	for _, raw := range req.Statuses {
		status := strings.ToLower(strings.TrimSpace(raw))
		liveness, ok := session.ParseLivenessName(status)
		if !ok {
			// Unreachable: Validate above rejects the request first. Skipping
			// rather than inserting the zero Liveness keeps a future caller that
			// bypasses Validate from silently filtering on LivenessUnset, which
			// matches nothing and would read as "no such sessions".
			continue
		}
		statuses[liveness] = struct{}{}
	}
	var ids map[string]struct{}
	if len(req.IDs) > 0 {
		ids = make(map[string]struct{}, len(req.IDs))
		for _, id := range req.IDs {
			ids[id] = struct{}{}
		}
	}

	filtered := make([]session.InstanceData, 0, len(instances))
	for _, instance := range instances {
		liveness := session.RecordedLiveness(instance)
		if (req.Live || req.LazyArchive) && liveness == session.LiveArchived {
			continue
		}
		if len(statuses) > 0 {
			if _, ok := statuses[liveness]; !ok {
				continue
			}
		}
		if !req.CreatedAfter.IsZero() && (instance.CreatedAt.IsZero() || instance.CreatedAt.Before(req.CreatedAfter)) {
			continue
		}
		if req.ProjectPath != "" && instance.Worktree.RepoPath != req.ProjectPath {
			continue
		}
		if ids != nil {
			if _, ok := ids[instance.ID]; !ok {
				continue
			}
		}
		if req.Before != nil && !olderThanCursor(instance, *req.Before) {
			continue
		}
		filtered = append(filtered, instance)
		if req.Limit != nil && !req.NewestFirst && len(filtered) == *req.Limit {
			// The legacy limit keeps the first N in the stored order and never
			// reports a next page, so it can stop reading as soon as it has them.
			return filtered, nil, nil
		}
	}
	if !req.NewestFirst {
		return filtered, nil, nil
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		a, b := filtered[i], filtered[j]
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.After(b.CreatedAt)
		}
		return a.ID > b.ID
	})
	if req.Limit == nil || len(filtered) <= *req.Limit {
		return filtered, nil, nil
	}
	filtered = filtered[:*req.Limit]
	last := filtered[len(filtered)-1]
	return filtered, &SnapshotCursor{CreatedAt: last.CreatedAt, ID: last.ID}, nil
}

// olderThanCursor reports whether a row sorts strictly after the cursor in the
// NewestFirst order (created_at descending, id descending).
func olderThanCursor(instance session.InstanceData, c SnapshotCursor) bool {
	if !instance.CreatedAt.Equal(c.CreatedAt) {
		return instance.CreatedAt.Before(c.CreatedAt)
	}
	return instance.ID < c.ID
}

func (r SnapshotRequest) hasFilters() bool {
	return r.Live || r.LazyArchive || len(r.Statuses) > 0 || !r.CreatedAfter.IsZero() || r.Limit != nil ||
		r.NewestFirst || r.Before != nil || r.ProjectPath != "" || len(r.IDs) > 0
}
