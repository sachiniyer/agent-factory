package daemon

import (
	"fmt"
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
	return nil
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
	if err := req.Validate(); err != nil {
		return nil, err
	}
	if !req.hasFilters() {
		return instances, nil
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

	filtered := make([]session.InstanceData, 0, len(instances))
	for _, instance := range instances {
		liveness := session.RecordedLiveness(instance)
		if req.Live && liveness == session.LiveArchived {
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
		filtered = append(filtered, instance)
		if req.Limit != nil && len(filtered) == *req.Limit {
			break
		}
	}
	return filtered, nil
}

func (r SnapshotRequest) hasFilters() bool {
	return r.Live || len(r.Statuses) > 0 || !r.CreatedAfter.IsZero() || r.Limit != nil
}
