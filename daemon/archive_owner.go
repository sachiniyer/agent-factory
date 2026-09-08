package daemon

import (
	"errors"
	"fmt"
	"os"

	"github.com/sachiniyer/agent-factory/session"
	sessiongit "github.com/sachiniyer/agent-factory/session/git"
)

// archiveDestinationOwner snapshots loaded paths before doing bounded probes, so
// a stalled old home alias does not hold the manager mutex across the deadline.
func (m *Manager) archiveDestinationOwner(repoID string, inst *session.Instance, dest string, destInfo os.FileInfo) (string, error) {
	type recordedOwner struct{ title, path string }
	var owners []recordedOwner
	m.mu.Lock()
	for key, other := range m.instances {
		rid, _ := splitDaemonInstanceKey(key)
		if rid == repoID && other != nil && other != inst {
			owners = append(owners, recordedOwner{other.Title, other.GetWorktreePath()})
		}
	}
	m.mu.Unlock()
	match := func(owner recordedOwner) (bool, error) {
		if owner.path == "" {
			return false, nil
		}
		if owner.path == dest {
			return true, nil
		}
		info, err := sessiongit.BoundedLstat(owner.path)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("cannot inspect recorded path %s for session %q; check the filesystem and retry: %w", owner.path, owner.title, err)
		}
		return os.SameFile(destInfo, info), nil
	}
	for _, owner := range owners {
		same, err := match(owner)
		if err != nil {
			return "", err
		}
		if same {
			return owner.title, nil
		}
	}
	disk, err := loadArchiveOwnerData(repoID)
	if err != nil {
		return "", err
	}
	for _, data := range disk {
		if data.Title == inst.Title {
			continue
		}
		same, err := match(recordedOwner{data.Title, data.Worktree.WorktreePath})
		if err != nil {
			return "", err
		}
		if same {
			return data.Title, nil
		}
	}
	return "", nil
}
