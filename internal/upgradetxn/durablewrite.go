package upgradetxn

// The durable atomic file write every transaction artefact goes through, and its
// group-aware variant. Split out of storage.go (#1145 file-length limit): these are
// one cohesive mechanism — write to a temp inode, establish its ownership and mode
// THERE, fsync, rename, fsync the directory — and reading them together is what
// makes the ordering obvious.

import (
	"fmt"
	"os"
	"path/filepath"
)

func durableAtomicWriteFile(path string, data []byte, mode os.FileMode) error {
	_, err := durableAtomicWriteFileInGroup(path, data, mode, mode, -1)
	return err
}

// durableAtomicWriteFileInGroup is durableAtomicWriteFile that also carries a GROUP,
// established on the temporary inode BEFORE the rename so the published file is
// never briefly — or permanently — wrong.
//
// Doing it after the rename cannot be made safe (#3011 review). On a group-writable
// directory whose owner is not a member of its owning group, the new file takes its
// CREATOR's primary group; a post-rename chown then fails with EPERM and the ledger
// is already published group-WRITABLE to an unrelated group, while the directory's
// authorized writers cannot read it. Swallowing that failure leaves the worst of
// both: not shared with the people who need it, and shared with people who do not.
//
// So the group is set on the temp, and if that cannot be done the file publishes
// with privateMode instead. Reports whether the group was actually established, so
// a caller can say which of the two outcomes it got rather than guessing.
func durableAtomicWriteFileInGroup(
	path string, data []byte, sharedMode, privateMode os.FileMode, gid int,
) (bool, error) {
	shared, err := durableAtomicWriteFileImpl(path, data, sharedMode, privateMode, gid)
	return shared, err
}

func durableAtomicWriteFileImpl(
	path string, data []byte, sharedMode, privateMode os.FileMode, gid int,
) (bool, error) {
	dir := filepath.Dir(path)
	if err := validateDirectoryNoSymlink(dir); err != nil {
		return false, fmt.Errorf("validate durable write directory %s: %w", dir, err)
	}
	temporary, err := os.CreateTemp(dir, filepath.Base(path)+".tmp.*")
	if err != nil {
		return false, fmt.Errorf("create durable temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return false, fmt.Errorf("write durable temporary file: %w", err)
	}
	// Group first, then the mode that depends on it. If the group cannot be
	// established the file must NOT publish group-writable, so it falls back to the
	// private mode rather than being shared with whoever the creator's primary group
	// happens to be.
	mode := sharedMode
	grouped := gid < 0
	if gid >= 0 {
		if err := temporary.Chown(-1, gid); err != nil {
			mode = privateMode
		} else {
			grouped = true
		}
	}
	if err := temporary.Chmod(mode); err != nil {
		_ = temporary.Close()
		return false, fmt.Errorf("set durable temporary file mode: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return false, fmt.Errorf("sync durable temporary file: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return false, fmt.Errorf("close durable temporary file: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return false, fmt.Errorf("install durable file %s: %w", path, err)
	}
	renamed = true
	if err := syncTransactionDirectory(dir); err != nil {
		return false, fmt.Errorf("sync durable file directory %s: %w", dir, err)
	}
	return grouped, nil
}
