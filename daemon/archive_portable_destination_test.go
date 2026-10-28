package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
	"github.com/stretchr/testify/require"
)

func TestArchiveSequentialPortableDestination(t *testing.T) {
	for _, backing := range []string{"directory-and-record", "directory-only", "record-only"} {
		t.Run(backing, func(t *testing.T) {
			m, repoID, instances := legacyArchivePair(t, "x/Σz", "x-ςz", "unrelated")
			first, contender := instances[0], instances[1]
			dest, _, err := m.ArchiveSession(ArchiveSessionRequest{ID: first.ID, RepoID: repoID})
			require.NoError(t, err)
			require.Empty(t, m.reservedArchiveDestinations)
			if backing == "directory-only" {
				// Preserve the actual archive while removing its owner record.
				delete(m.instances, daemonInstanceKey(repoID, first.Title))
				require.NoError(t, config.UpdateRepoInstances(repoID, func(json.RawMessage) (json.RawMessage, error) {
					return json.Marshal([]session.InstanceData{contender.ToInstanceData(), instances[2].ToInstanceData()})
				}))
			}
			if backing == "record-only" {
				require.NoError(t, os.RemoveAll(dest))
				delete(m.instances, daemonInstanceKey(repoID, first.Title))
			}
			before := contender.ToInstanceData()
			old := archiveTeardown
			touched := false
			archiveTeardown = func(inst *session.Instance, dest string, claim sessiongit.RelocationClaim, hook func() error, trust bool) (error, error) {
				if inst == contender {
					touched = true
				}
				return old(inst, dest, claim, hook, trust)
			}
			t.Cleanup(func() { archiveTeardown = old })
			_, _, err = m.ArchiveSession(ArchiveSessionRequest{ID: contender.ID, RepoID: repoID})
			require.ErrorContains(t, err, `collides with existing archive "x-Σz" (same portable name)`)
			require.False(t, touched, "collision must refuse before teardown")
			require.Equal(t, before.Tabs, contender.ToInstanceData().Tabs)
			require.Equal(t, before.Liveness, contender.GetLiveness())
			require.Equal(t, session.OpNone, contender.GetInFlightOp())
			require.Empty(t, m.reservedArchiveDestinations)
			path, _, err := m.ArchiveSession(ArchiveSessionRequest{ID: instances[2].ID, RepoID: repoID})
			require.NoError(t, err)
			require.Equal(t, "unrelated", filepath.Base(path))
		})
	}
}
