// Metadata-snapshot manifest for the upgrade transaction, split from
// storage.go when that file approached the 1000-line limit (#1145).
//
// The journal's Metadata field is the rollback manifest for the user's
// metadata files: at prepare time each planned path is snapshotted (bytes
// into the transaction directory, mode and parent-directory modes into the
// journal), and on rollback each entry is restored — recreating parents
// that vanished, stamping back bytes and modes, and removing what the
// candidate created. This file holds that layer end to end: the
// metadata-path primitives that confine a path to the upgrade home and
// enumerate its parents, the prepare-time snapshotters, and the
// rollback-time restorers.
package upgradetxn

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func snapshotMetadata(home, txnDir string, paths []string) ([]MetadataSnapshot, error) {
	seen := make(map[string]struct{}, len(paths))
	snapshots := make([]MetadataSnapshot, 0, len(paths))
	metadataDir := filepath.Join(txnDir, "metadata")
	for index, path := range paths {
		relative, target, err := validateMetadataPath(home, path)
		if err != nil {
			return nil, err
		}
		if _, exists := seen[relative]; exists {
			return nil, fmt.Errorf("metadata path %q is listed more than once", relative)
		}
		seen[relative] = struct{}{}
		parents, err := snapshotMetadataParents(home, relative)
		if err != nil {
			return nil, err
		}

		info, err := os.Lstat(target)
		if errors.Is(err, os.ErrNotExist) {
			snapshots = append(snapshots, MetadataSnapshot{Path: relative, Parents: parents})
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect metadata %s: %w", relative, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("metadata %s is not a regular file", relative)
		}
		data, err := os.ReadFile(target)
		if err != nil {
			return nil, fmt.Errorf("read metadata %s: %w", relative, err)
		}
		snapshotPath := filepath.Join(metadataDir, fmt.Sprintf("%04d.snapshot", index))
		if err := durableAtomicWriteFile(snapshotPath, data, info.Mode().Perm()); err != nil {
			return nil, fmt.Errorf("snapshot metadata %s: %w", relative, err)
		}
		snapshots = append(snapshots, MetadataSnapshot{
			Path:         relative,
			Existed:      true,
			Mode:         uint32(info.Mode().Perm()),
			SHA256:       digest(data),
			SnapshotPath: snapshotPath,
			Parents:      parents,
		})
	}
	return snapshots, nil
}

func snapshotMetadataParents(home, relative string) ([]MetadataParentSnapshot, error) {
	paths := metadataParentPaths(relative)
	parents := make([]MetadataParentSnapshot, 0, len(paths))
	for _, path := range paths {
		info, err := os.Lstat(filepath.Join(home, path))
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("inspect metadata parent %s: %w", path, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, fmt.Errorf("metadata parent %s is not a directory", path)
		}
		parents = append(parents, MetadataParentSnapshot{Path: path, Mode: uint32(info.Mode().Perm())})
	}
	return parents, nil
}

func prepareMetadataParents(home string, parents []MetadataParentSnapshot) (int, error) {
	for index, parent := range parents {
		path := filepath.Join(home, parent.Path)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			if err := validateDirectoryNoSymlink(filepath.Dir(path)); err != nil {
				return index, fmt.Errorf("validate parent of %s: %w", parent.Path, err)
			}
			if err := os.Mkdir(path, os.FileMode(parent.Mode)|0o700); err != nil {
				return index, fmt.Errorf("recreate metadata parent %s: %w", parent.Path, err)
			}
			if err := os.Chmod(path, os.FileMode(parent.Mode)|0o700); err != nil {
				return index + 1, fmt.Errorf("make recreated metadata parent %s writable: %w", parent.Path, err)
			}
			if err := syncTransactionDirectory(path); err != nil {
				return index + 1, fmt.Errorf("sync recreated metadata parent %s: %w", parent.Path, err)
			}
			if err := syncTransactionDirectory(filepath.Dir(path)); err != nil {
				return index + 1, fmt.Errorf("sync parent after recreating %s: %w", parent.Path, err)
			}
		} else if err != nil {
			return index, fmt.Errorf("inspect metadata parent %s: %w", parent.Path, err)
		} else if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return index, fmt.Errorf("metadata parent %s is not a real directory", parent.Path)
		}
		if err := os.Chmod(path, os.FileMode(parent.Mode)|0o700); err != nil {
			return index + 1, fmt.Errorf("make metadata parent %s writable for restoration: %w", parent.Path, err)
		}
	}
	return len(parents), nil
}

func prepareCandidateMetadataParents(
	home, relative string, snapshotted int,
) ([]MetadataParentSnapshot, error) {
	paths := metadataParentPaths(relative)
	prepared := make([]MetadataParentSnapshot, 0, len(paths)-snapshotted)
	for _, relativePath := range paths[snapshotted:] {
		path := filepath.Join(home, relativePath)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			return prepared, fmt.Errorf("inspect candidate-created metadata parent %s: %w", relativePath, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return prepared, fmt.Errorf("candidate-created metadata parent %s is not a real directory", relativePath)
		}
		parent := MetadataParentSnapshot{Path: relativePath, Mode: uint32(info.Mode().Perm())}
		if err := os.Chmod(path, info.Mode().Perm()|0o700); err != nil {
			return prepared, fmt.Errorf("make candidate-created metadata parent %s writable: %w", relativePath, err)
		}
		prepared = append(prepared, parent)
	}
	return prepared, nil
}

func restoreMetadataParentModes(home string, parents []MetadataParentSnapshot) error {
	var result error
	for index := len(parents) - 1; index >= 0; index-- {
		parent := parents[index]
		path := filepath.Join(home, parent.Path)
		if err := os.Chmod(path, os.FileMode(parent.Mode)); err != nil {
			result = errors.Join(result, fmt.Errorf("restore metadata parent mode %s: %w", parent.Path, err))
			continue
		}
		if err := syncTransactionDirectory(path); err != nil {
			result = errors.Join(result, fmt.Errorf("sync metadata parent mode %s: %w", parent.Path, err))
		}
	}
	return result
}

func restoreMetadataEntry(home string, metadata MetadataSnapshot) (retErr error) {
	prepared, err := prepareMetadataParents(home, metadata.Parents)
	defer func() {
		retErr = errors.Join(retErr, restoreMetadataParentModes(home, metadata.Parents[:prepared]))
	}()
	if err != nil {
		return err
	}
	if !metadata.Existed {
		candidateParents, err := prepareCandidateMetadataParents(home, metadata.Path, len(metadata.Parents))
		defer func() {
			retErr = errors.Join(retErr, restoreMetadataParentModes(home, candidateParents))
		}()
		if err != nil {
			return err
		}
	}
	target := filepath.Join(home, metadata.Path)
	if err := ensureNoSymlinkParents(home, target); err != nil {
		return fmt.Errorf("validate rollback path %s: %w", metadata.Path, err)
	}
	if !metadata.Existed {
		if removeErr := os.Remove(target); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
			return fmt.Errorf("restore absence of %s: %w", metadata.Path, removeErr)
		}
		if err := syncMetadataAbsence(home, target); err != nil {
			return fmt.Errorf("sync restored absence of %s: %w", metadata.Path, err)
		}
		return nil
	}
	data, err := readAndVerify(metadata.SnapshotPath, metadata.SHA256)
	if err != nil {
		return fmt.Errorf("verify metadata snapshot %s: %w", metadata.Path, err)
	}
	if err := durableAtomicWriteFile(target, data, os.FileMode(metadata.Mode)); err != nil {
		return fmt.Errorf("restore metadata %s: %w", metadata.Path, err)
	}
	return nil
}

func syncMetadataAbsence(home, target string) error {
	directory := filepath.Dir(target)
	for {
		info, err := os.Lstat(directory)
		if err == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
				return fmt.Errorf("metadata absence parent %s is not a real directory", directory)
			}
			return syncTransactionDirectory(directory)
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if directory == home {
			return errors.New("upgrade home disappeared while restoring metadata absence")
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return errors.New("metadata absence parent escapes the upgrade home")
		}
		directory = parent
	}
}

func metadataParentPaths(relative string) []string {
	dir := filepath.Dir(relative)
	if dir == "." {
		return nil
	}
	components := strings.Split(dir, string(filepath.Separator))
	paths := make([]string, 0, len(components))
	current := ""
	for _, component := range components {
		current = filepath.Join(current, component)
		paths = append(paths, current)
	}
	return paths
}

func resolveMetadataPath(home, path string) (string, string, error) {
	if path == "" || filepath.IsAbs(path) {
		return "", "", fmt.Errorf("metadata path %q must be relative to the upgrade home", path)
	}
	relative := filepath.Clean(path)
	if relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("metadata path %q escapes the upgrade home", path)
	}
	target := filepath.Join(home, relative)
	inside, err := filepath.Rel(home, target)
	if err != nil || inside != relative {
		return "", "", fmt.Errorf("metadata path %q escapes the upgrade home", path)
	}
	return relative, target, nil
}

func validateMetadataPath(home, path string) (string, string, error) {
	relative, target, err := resolveMetadataPath(home, path)
	if err != nil {
		return "", "", err
	}
	if err := ensureNoSymlinkParents(home, target); err != nil {
		return "", "", fmt.Errorf("metadata path %q is unsafe: %w", path, err)
	}
	return relative, target, nil
}

func ensureNoSymlinkParents(home, target string) error {
	relative, err := filepath.Rel(home, filepath.Dir(target))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errors.New("parent escapes the upgrade home")
	}
	current := home
	if relative == "." {
		return nil
	}
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("parent %s is a symlink", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("parent %s is not a directory", current)
		}
	}
	return nil
}
