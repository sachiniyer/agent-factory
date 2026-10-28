package daemon

import (
	"encoding/json"
	"fmt"

	"github.com/sachiniyer/agent-factory/config"
	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// loadArchiveOwnerData keeps the read behind the destination reservation so
// admission uses fresh owners, but bounds the filesystem read with the same
// deadline and per-path stalled-worker latch as the adjacent directory scan.
func loadArchiveOwnerData(repoID string) ([]session.InstanceData, error) {
	raw, err := config.LoadRepoInstancesWithReader(repoID, sessiongit.BoundedReadFile)
	if err != nil {
		return nil, fmt.Errorf("cannot read archive owners; check that the filesystem is responsive, then retry archive: %w", err)
	}
	var data []session.InstanceData
	if err := json.Unmarshal(raw, &data); err != nil {
		return nil, fmt.Errorf("failed to parse existing instances: %w", err)
	}
	return data, nil
}
