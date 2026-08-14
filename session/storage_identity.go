package session

import "github.com/sachiniyer/agent-factory/config"

// repoIDForStorage returns the durable per-repo key for a checkpoint. A loaded
// instance keeps the key of its containing file; a fresh instance captures its
// resolved identity on first save.
func (i *Instance) repoIDForStorage() string {
	root := i.GetRepoPath()
	if root == "" {
		root = i.Path
	}
	derived := config.RepoIDForPath(root)

	i.mu.Lock()
	defer i.mu.Unlock()
	if i.storageRepoID == "" {
		i.storageRepoID = derived
	}
	return i.storageRepoID
}

func (i *Instance) rememberStorageRepoID(repoID string) {
	i.mu.Lock()
	i.storageRepoID = repoID
	i.mu.Unlock()
}
