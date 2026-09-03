package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sachiniyer/agent-factory/log"
	"golang.org/x/sys/unix"
)

// atomicWriteInOpenDir replaces name inside an ALREADY-OPEN directory: it
// stages a temp file with openat(2) and renames it into place with renameat(2),
// both relative to dirFD, then fsyncs that same fd.
//
// The open fd is the whole point. Every path-based writer re-resolves its
// directory at each syscall, so one symlink anywhere above the file can be
// repointed between the temp create and the rename and send the bytes into a
// directory the caller never locked, checked, or contained. An fd names an
// inode rather than a path: once it is open, nothing an external actor does to
// the path can move where these bytes land.
//
// Both of this package's directory-pinned writers share this body and differ
// only in how they OPEN that fd, which is the decision each of them is about:
//
//   - the in-repo config writer opens O_NOFOLLOW, because a checked-in
//     .agent-factory directory belongs to a repository a clone does not control
//     and must not be a link at all (#1092, #3672);
//   - the followed global-config lock opens WITHOUT it, because a symlinked
//     AGENT_FACTORY_HOME is a supported arrangement — the fd pins whichever
//     directory it resolved to, and a retarget afterwards is refused by the
//     identity re-check rather than by the open (#3697).
//
// dir is carried for messages only; nothing here resolves it again.
func atomicWriteInOpenDir(dirFD int, dir, name string, data []byte, perm os.FileMode) error {
	// The rename is relative to dirFD, so a name carrying any separator would
	// silently escape the directory this writer promises to stay inside.
	if name == "" || filepath.Base(name) != name {
		return fmt.Errorf("invalid config file name %q", name)
	}

	tmp, tmpName, err := createTempFileInOpenDir(dirFD, name)
	if err != nil {
		return err
	}
	success := false
	defer func() {
		if !success {
			_ = unix.Unlinkat(dirFD, tmpName, 0)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to write temp file: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to chmod temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to close temp file: %w", err)
	}

	if err := unix.Renameat(dirFD, tmpName, dirFD, name); err != nil {
		return fmt.Errorf("failed to rename temp file to %s: %w", filepath.Join(dir, name), err)
	}
	success = true

	// The rename is what makes the data visible, and it has happened. A failed
	// directory fsync costs crash durability only, so it is logged rather than
	// returned — the same contract the path-based writer keeps.
	if err := unix.Fsync(dirFD); err != nil {
		log.WarningLog.Printf("AtomicWriteFile: failed to fsync directory %s after rename of %s: %v", dir, filepath.Join(dir, name), err)
	}
	return nil
}

func createTempFileInOpenDir(dirFD int, base string) (*os.File, string, error) {
	for range 32 {
		var suffix [8]byte
		if _, err := rand.Read(suffix[:]); err != nil {
			return nil, "", fmt.Errorf("failed to generate temp file name: %w", err)
		}
		name := "." + base + ".tmp." + hex.EncodeToString(suffix[:])
		fd, err := unix.Openat(dirFD, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC, 0600)
		if err == nil {
			return os.NewFile(uintptr(fd), name), name, nil
		}
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		return nil, "", fmt.Errorf("failed to create temp file: %w", err)
	}
	return nil, "", fmt.Errorf("failed to create temp file: exhausted random names")
}

// sameInode reports whether two stat results name one file — a directory the
// lock was taken in, or the lock file itself.
//
// Dev is uint64 on Linux and int32 on Darwin, so both sides go through the same
// conversion rather than being compared field-by-field per platform.
func sameInode(a, b *unix.Stat_t) bool {
	return uint64(a.Dev) == uint64(b.Dev) && uint64(a.Ino) == uint64(b.Ino)
}
