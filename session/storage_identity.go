package session

import "github.com/sachiniyer/agent-factory/config"

// repoIDForStorage returns the durable per-repo key for a checkpoint. A loaded
// instance keeps the key of its containing file; a fresh instance captures its
// resolved identity on first save.
func (i *Instance) repoIDForStorage() string {
	i.mu.RLock()
	remembered := i.storageRepoID
	i.mu.RUnlock()
	if remembered != "" {
		return remembered
	}

	root := i.GetRepoPath()
	derived := ""
	if root != "" {
		// GitWorktree.repoPath is already the canonical recorded identity.
		// Resolving it again could adopt an enclosing repository if the origin
		// disappears before this fresh row's first checkpoint.
		derived = config.RepoIDForRecordedRoot(root)
	} else {
		// Worktree-less remote rows carry only the requested workspace path,
		// which retains the historical direct-resolution behavior.
		derived = config.RepoIDForPath(i.Path)
	}

	i.mu.Lock()
	defer i.mu.Unlock()
	if i.storageRepoID == "" {
		i.storageRepoID = derived
	}
	return i.storageRepoID
}

// PinStorageRepoID records the authoritative key used by an instance's first
// successful durable write. A later checkpoint must not re-resolve a
// worktree-less row's Path after that checkout disappears or is repurposed.
func (i *Instance) PinStorageRepoID(repoID string) {
	if repoID == "" {
		return
	}
	i.mu.Lock()
	if i.storageRepoID == "" {
		i.storageRepoID = repoID
	}
	i.mu.Unlock()
}
