package daemon

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/sachiniyer/agent-factory/session"
)

// TestDockerReapProtectedSlugs: the orphan sweep's protected set covers live
// instances AND still-provisioning pending creates (the #2549 window where a
// container exists but no Instance does yet), each as its af.session title slug —
// so the sweep can never reap a container whose session is alive or mid-create.
func TestDockerReapProtectedSlugs(t *testing.T) {
	repoID := "repo1"
	m := &Manager{
		instances:      make(map[string]*session.Instance),
		pendingCreates: make(map[string]session.InstanceData),
	}
	m.instances[daemonInstanceKey(repoID, "live one")] = &session.Instance{Title: "live one"}
	m.pendingCreates[daemonInstanceKey(repoID, "mid create")] = session.InstanceData{Title: "mid create"}

	got := m.dockerReapProtectedSlugs()

	assert.True(t, got[session.Slugify("live one")], "a live instance's slug must be protected")
	assert.True(t, got[session.Slugify("mid create")], "a mid-create pending session's slug must be protected (#2549)")
	assert.Len(t, got, 2)
}

// TestDockerReapProtectedSlugsSettledPendingNotDoubleCounted: a pending row that
// has already settled into a real instance is not processed twice.
func TestDockerReapProtectedSlugsSettledPendingNotDoubleCounted(t *testing.T) {
	key := daemonInstanceKey("repo1", "worker")
	m := &Manager{
		instances:      map[string]*session.Instance{key: {Title: "worker"}},
		pendingCreates: map[string]session.InstanceData{key: {Title: "worker"}},
	}
	got := m.dockerReapProtectedSlugs()
	assert.True(t, got[session.Slugify("worker")])
	assert.Len(t, got, 1, "a settled pending create must not be counted twice")
}
