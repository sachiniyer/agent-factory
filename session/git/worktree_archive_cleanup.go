package git

import (
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// cleanupRetainedArchiveTrees consumes every complete source tree owned by this
// report. Kill calls it before deleting the ordinary worktree and session record,
// so retained bytes cannot outlive the destructive action that removed their
// only durable handle.
func (g *GitWorktree) cleanupRetainedArchiveTrees() error {
	for {
		report := g.GetArchiveReport()
		if report.Empty() {
			return nil
		}
		tree := report.RetainedTrees[0]
		if err := removeRetainedArchiveTree(tree); err != nil {
			return err
		}
		g.dropRetainedArchiveTree(tree)
	}
}

func removeRetainedArchiveTree(tree ArchiveRetainedTree) error {
	path := tree.filesystemPath()
	if path == "" || !privateSourceName(filepath.Base(path)) {
		return fmt.Errorf("refusing to remove retained archive tree with invalid private path %q", path)
	}
	if !tree.IdentityKnown {
		return fmt.Errorf("refusing to remove retained archive tree %s without its durable identity", path)
	}
	parentPath := filepath.Dir(path)
	parent, _, err := openDirectoryPathFollowingLinks(parentPath, "retained archive parent")
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer parent.Close()

	name := filepath.Base(path)
	current, err := identityAt(parent, name)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect retained archive tree %s: %w", path, err)
	}
	if !tree.identity().same(current) {
		return fmt.Errorf("refusing to remove retained archive tree %s because its private name identifies different data", path)
	}
	root, _, err := openDirectoryAt(parent, name, path, "retained archive")
	if err != nil {
		return err
	}
	defer root.Close()
	opened, err := identityFromFile(root)
	if err != nil {
		return err
	}
	if !tree.identity().same(opened) {
		return fmt.Errorf("refusing to remove retained archive tree %s because its opened identity changed", path)
	}
	if err := removeOpenedDirectory(parent, name, path, root, nil); err != nil {
		return fmt.Errorf("remove retained archive tree %s: %w", path, err)
	}
	return nil
}

func privateSourceName(name string) bool {
	const prefix = ".af-source-"
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	random := strings.TrimPrefix(name, prefix)
	if len(random) != 32 {
		return false
	}
	_, err := hex.DecodeString(random)
	return err == nil
}

func (g *GitWorktree) dropRetainedArchiveTree(removed ArchiveRetainedTree) {
	g.relocationMu.Lock()
	defer g.relocationMu.Unlock()
	for index, tree := range g.archiveReport.RetainedTrees {
		if sameRetainedArchiveTree(tree, removed) {
			g.archiveReport.RetainedTrees = append(
				g.archiveReport.RetainedTrees[:index], g.archiveReport.RetainedTrees[index+1:]...,
			)
			g.refreshArchiveWarningLocked()
			return
		}
	}
}

func sameRetainedArchiveTree(left, right ArchiveRetainedTree) bool {
	return left.IdentityKnown == right.IdentityKnown &&
		left.identity().same(right.identity()) &&
		left.filesystemPath() == right.filesystemPath()
}
