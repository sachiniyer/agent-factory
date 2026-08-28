package daemon

import (
	"fmt"
	"strings"

	"github.com/sachiniyer/agent-factory/session"
)

var snapshotStatusLiveness = map[string]session.Liveness{
	"running":       session.LiveRunning,
	"ready":         session.LiveReady,
	"lost":          session.LiveLost,
	"dead":          session.LiveDead,
	"archived":      session.LiveArchived,
	"limit-reached": session.LiveLimitReached,
}

const snapshotStatusNames = "running, ready, lost, dead, archived, limit-reached"

// Validate checks the additive Snapshot filters without reading manager state.
func (r SnapshotRequest) Validate() error {
	for _, raw := range r.Statuses {
		status := strings.ToLower(strings.TrimSpace(raw))
		if _, ok := snapshotStatusLiveness[status]; !ok {
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
		statuses[snapshotStatusLiveness[status]] = struct{}{}
	}

	filtered := make([]session.InstanceData, 0, len(instances))
	for _, instance := range instances {
		liveness := snapshotLiveness(instance)
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

func snapshotLiveness(instance session.InstanceData) session.Liveness {
	if instance.Liveness != session.LivenessUnset {
		return instance.Liveness
	}
	return session.LivenessForStatus(instance.Status)
}
