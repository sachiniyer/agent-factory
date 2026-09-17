package config

import (
	"os"
	"path/filepath"
)

// ReapedRootCarryFileName is the durable carry a reaped root-agent record hands
// its replacement (#4400): the account pin, conversation, tab roster, notice,
// and pending account swap the deleted record held. The daemon writes it beside
// the repo's instances file immediately before deleting the root record, and
// removes it once a replacement makes it moot.
const ReapedRootCarryFileName = "reaped-root-carry.json"

// RepoReapedRootCarryPath returns the path of repoID's reaped root carry. It
// shares the instances file's directory, so the repo ID validation that path
// resolution performs covers this one too.
func RepoReapedRootCarryPath(repoID string) (string, error) {
	instancesPath, err := repoInstancesPath(repoID)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(instancesPath), ReapedRootCarryFileName), nil
}

// DeleteRepoReapedRootCarry removes repoID's reaped root carry. The carry is
// state of the root record it was reaped from, so a reset that deletes a repo's
// session records deletes it with them (#4400 review round 7): left behind, it
// would restore a stale account pin, conversation, and pending swap on the
// first root create after the reset.
//
// A plain remove, like DeleteRepoInstances and DeleteAllRepoInstances beside
// it: a reset clears state rather than writing through it, and unlinking a
// symlink never touches its target. A missing carry is the common case.
func DeleteRepoReapedRootCarry(repoID string) error {
	path, err := RepoReapedRootCarryPath(repoID)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
