package proposal

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// Agents edit the store folder with their own file tools, so af treats every
// entry in it as untrusted: each open below goes through a directory fd and
// O_NOFOLLOW, so a symlink planted anywhere in the store cannot send a read or
// a write outside it.

const dirOpenFlags = unix.O_RDONLY | unix.O_DIRECTORY | unix.O_NOFOLLOW | unix.O_CLOEXEC

const maxDocSize = 8 << 20

var errSymlink = errors.New("is a symlink or not a regular entry; af does not follow links in the proposal store")

func openDirAt(at int, name, display string) (*os.File, error) {
	fd, err := unix.Openat(at, name, dirOpenFlags, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) || errors.Is(err, unix.ENOTDIR) {
			err = errSymlink
		}
		return nil, &fs.PathError{Op: "open", Path: display, Err: err}
	}
	return os.NewFile(uintptr(fd), display), nil
}

// openStoreDir opens the store folder. Links above it (a symlinked AF home)
// are the user's own arrangement and are followed; the folder itself is not.
func (s *Store) openStoreDir() (*os.File, error) {
	return openDirAt(unix.AT_FDCWD, s.dir, s.dir)
}

// openSlugDir opens the folder of proposal slug without following links.
func (s *Store) openSlugDir(slug string) (*os.File, error) {
	if err := ValidateSlug(slug); err != nil {
		return nil, err
	}
	store, err := s.openStoreDir()
	if err != nil {
		return nil, err
	}
	defer store.Close()
	return openDirAt(int(store.Fd()), slug, s.SlugPath(slug))
}

// openDoc opens doc file of proposal slug for reading. It fails unless the
// file is a regular file reached without following a link.
func (s *Store) openDoc(slug, file string) (*os.File, error) {
	if filepath.Base(file) != file {
		return nil, fmt.Errorf("invalid doc file %q", file)
	}
	dir, err := s.openSlugDir(slug)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	display := filepath.Join(s.SlugPath(slug), file)
	// O_NONBLOCK keeps a planted FIFO from hanging the open.
	fd, err := unix.Openat(int(dir.Fd()), file, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, unix.ELOOP) {
			err = errSymlink
		}
		return nil, &fs.PathError{Op: "open", Path: display, Err: err}
	}
	f := os.NewFile(uintptr(fd), display)
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		f.Close()
		return nil, &fs.PathError{Op: "open", Path: display, Err: errSymlink}
	}
	return f, nil
}

func (s *Store) readDoc(slug, file string) ([]byte, error) {
	f, err := s.openDoc(slug, file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxDocSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxDocSize {
		return nil, fmt.Errorf("%s is larger than %d MiB; af does not read proposal docs that large", f.Name(), maxDocSize>>20)
	}
	return data, nil
}

func (s *Store) readSlugDir(slug string) ([]os.DirEntry, error) {
	dir, err := s.openSlugDir(slug)
	if err != nil {
		return nil, err
	}
	defer dir.Close()
	return dir.ReadDir(-1)
}

// mkSlugDir creates the folder of proposal slug. It fails when any entry of
// that name exists, link or not.
func (s *Store) mkSlugDir(slug string) error {
	store, err := s.openStoreDir()
	if err != nil {
		return err
	}
	defer store.Close()
	if err := unix.Mkdirat(int(store.Fd()), slug, 0o755); err != nil {
		return &fs.PathError{Op: "mkdir", Path: s.SlugPath(slug), Err: err}
	}
	return nil
}

// createDoc writes a new doc file. It fails when any entry of that name exists.
func (s *Store) createDoc(slug, file string, data []byte) error {
	dir, err := s.openSlugDir(slug)
	if err != nil {
		return err
	}
	defer dir.Close()
	display := filepath.Join(s.SlugPath(slug), file)
	fd, err := unix.Openat(int(dir.Fd()), file, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o644)
	if err != nil {
		return &fs.PathError{Op: "create", Path: display, Err: err}
	}
	f := os.NewFile(uintptr(fd), display)
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = unix.Unlinkat(int(dir.Fd()), file, 0)
	}
	return err
}

func (s *Store) removeDoc(slug, file string) error {
	dir, err := s.openSlugDir(slug)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := unix.Unlinkat(int(dir.Fd()), file, 0); err != nil {
		return &fs.PathError{Op: "remove", Path: filepath.Join(s.SlugPath(slug), file), Err: err}
	}
	return nil
}

// writeDoc atomically replaces doc file of proposal slug. The rename replaces
// a link at that name rather than writing through it.
func (s *Store) writeDoc(slug, file string, data []byte) error {
	if filepath.Base(file) != file {
		return fmt.Errorf("invalid doc file %q", file)
	}
	if len(data) > maxDocSize {
		return fmt.Errorf("doc is larger than %d MiB; af does not write proposal docs that large", maxDocSize>>20)
	}
	dir, err := s.openSlugDir(slug)
	if err != nil {
		return err
	}
	defer dir.Close()
	dirFD := int(dir.Fd())
	tmp, tmpName, err := createTempAt(dirFD, s.SlugPath(slug))
	if err != nil {
		return err
	}
	done := false
	defer func() {
		if !done {
			_ = unix.Unlinkat(dirFD, tmpName, 0)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := unix.Renameat(dirFD, tmpName, dirFD, file); err != nil {
		return &fs.PathError{Op: "rename", Path: filepath.Join(s.SlugPath(slug), file), Err: err}
	}
	done = true
	return nil
}

func createTempAt(dirFD int, dirDisplay string) (*os.File, string, error) {
	for range 16 {
		var b [8]byte
		if _, err := rand.Read(b[:]); err != nil {
			return nil, "", err
		}
		name := ".write-" + hex.EncodeToString(b[:])
		fd, err := unix.Openat(dirFD, name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
		if err == nil {
			return os.NewFile(uintptr(fd), filepath.Join(dirDisplay, name)), name, nil
		}
		if !errors.Is(err, unix.EEXIST) {
			return nil, "", &fs.PathError{Op: "create", Path: dirDisplay, Err: err}
		}
	}
	return nil, "", fmt.Errorf("create temp file in %s: exhausted random names", dirDisplay)
}
