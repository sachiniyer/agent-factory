package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

// RepoIDsWithReapedRootCarry lists the repositories that currently have a
// parked reaped root carry on disk, sorted.
//
// The carry outlives the record set it was reaped from BY DESIGN — that is what
// makes it survive a daemon restart inside the reap-to-publish window — so it
// also outlives the root_agents entry that would have consumed it. Nothing else
// enumerates it: every other retirement is driven by a candidate the sweep
// still enumerates, and a repository whose last entry was deleted enumerates
// none (Codex on #4400, round 8). A caller reconciling these against the live
// candidate set is the only thing that can retire that carry.
//
// A missing instances directory is no carries, not an error. An unreadable one
// IS an error: a caller that retires what it cannot see would delete on an
// absence it never established.
func RepoIDsWithReapedRootCarry() ([]string, error) {
	dir, err := instancesDirPath()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("failed to read instances directory: %w", err)
	}
	var withCarry []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		repoID := entry.Name()
		path, err := RepoReapedRootCarryPath(repoID)
		if err != nil {
			// Not a legal repo ID, so af never wrote a carry there.
			continue
		}
		if _, err := os.Lstat(path); err == nil {
			withCarry = append(withCarry, repoID)
		}
	}
	sort.Strings(withCarry)
	return withCarry, nil
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
