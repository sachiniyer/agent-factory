package session

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// The filesystem half of a same-agent account swap's conversation carry
// (#4367). Both provider stores are append-only JSONL, so a carry never has to
// merge anything: the destination either already holds the whole conversation
// (a retried launch may have appended to it), holds a stale prefix of it (an
// A→B→A return trip), holds nothing, or holds something else entirely — and
// only the last is a refusal.
//
// Every path is walked one component at a time through directory descriptors
// opened with O_NOFOLLOW, the same discipline readCodexRollout applies to its
// final component. An account home is an owner-only credential root; a symlink
// planted anywhere beneath it must not redirect a write outside it, and a
// symlinked source must not smuggle another file's contents into it.

const (
	carryDirMode  = 0o700
	carryFileMode = 0o600
	// carryTreeMaxDepth and carryTreeMaxEntries bound the per-session auxiliary
	// directory walk. Claude's tool-result and subagent trees are shallow; a
	// pathological tree is a reason to stop copying extras, never to hang a swap.
	carryTreeMaxDepth   = 8
	carryTreeMaxEntries = 10000
)

// carryError pairs a clause the replacement's notice can quote with the
// underlying cause, which only the daemon log needs.
type carryError struct {
	reason string
	err    error
}

func (e *carryError) Error() string {
	if e.err == nil {
		return e.reason
	}
	return e.reason + ": " + e.err.Error()
}

func (e *carryError) Unwrap() error { return e.err }

func carryFailure(reason string, err error) error {
	return &carryError{reason: reason, err: err}
}

// carryFailureReason returns the notice-safe clause describing err.
func carryFailureReason(err error) string {
	var carryErr *carryError
	if errors.As(err, &carryErr) {
		return carryErr.reason
	}
	return "copying it into the new account failed"
}

// carryRoots are the only top-level entries a carry may touch: Claude's
// projects/ tree and Codex's sessions/ tree. Credential files live beside
// them, never beneath them.
var carryRoots = map[string]bool{"projects": true, "sessions": true}

// splitCarryPath validates rel and splits it into its directory components and
// final name. It must be a clean, local path under one of carryRoots.
func splitCarryPath(rel string, wantFile bool) ([]string, string, error) {
	if rel == "" || filepath.Clean(rel) != rel || !filepath.IsLocal(rel) {
		return nil, "", fmt.Errorf("carry path %q is not a clean relative path", rel)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if !carryRoots[parts[0]] {
		return nil, "", fmt.Errorf("carry path %q is outside the provider's conversation store", rel)
	}
	if !wantFile {
		return parts, "", nil
	}
	if len(parts) < 2 {
		return nil, "", fmt.Errorf("carry path %q names no file inside the conversation store", rel)
	}
	return parts[:len(parts)-1], parts[len(parts)-1], nil
}

func retryEINTR(call func() error) error {
	for {
		if err := call(); !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

// openCarryRoot opens an account home. The destination must itself be a real
// directory (agentaccount.Selected already refused symlinked ancestors); an
// ambient source such as ~/.claude may legitimately be a symlink to a dotfiles
// checkout, so only the source root is followed.
func openCarryRoot(path string, follow bool) (*os.File, error) {
	flags := unix.O_RDONLY | unix.O_DIRECTORY | unix.O_CLOEXEC
	if !follow {
		flags |= unix.O_NOFOLLOW
	}
	var fd int
	err := retryEINTR(func() (err error) {
		fd, err = unix.Open(path, flags, 0)
		return err
	})
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

func openCarryAt(dir *os.File, name string, flags int, mode uint32) (*os.File, error) {
	path := filepath.Join(dir.Name(), name)
	var fd int
	err := retryEINTR(func() (err error) {
		fd, err = unix.Openat(int(dir.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC, mode)
		return err
	})
	if err != nil {
		return nil, &os.PathError{Op: "openat", Path: path, Err: err}
	}
	return os.NewFile(uintptr(fd), path), nil
}

// openCarryDirAt opens the directory name beneath dir without following a
// symlink. With create, a missing directory is made owner-only first.
func openCarryDirAt(dir *os.File, name string, create bool) (*os.File, error) {
	opened, err := openCarryAt(dir, name, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err == nil || !create || !errors.Is(err, fs.ErrNotExist) {
		return opened, err
	}
	if err := unix.Mkdirat(int(dir.Fd()), name, carryDirMode); err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, &os.PathError{Op: "mkdirat", Path: filepath.Join(dir.Name(), name), Err: err}
	}
	return openCarryAt(dir, name, unix.O_RDONLY|unix.O_DIRECTORY, 0)
}

// openCarryDirChain descends components beneath root. The returned directory
// is always a fresh descriptor the caller must close; root stays open.
func openCarryDirChain(root *os.File, components []string, create bool) (*os.File, error) {
	if len(components) == 0 {
		return nil, errors.New("carry path has no directory beneath the account home")
	}
	current := root
	for _, name := range components {
		next, err := openCarryDirAt(current, name, create)
		if current != root {
			_ = current.Close()
		}
		if err != nil {
			return nil, err
		}
		current = next
	}
	return current, nil
}

// openCarryFileAt opens a regular file beneath dir for reading. O_NONBLOCK
// keeps a FIFO planted at the path from hanging the swap; it has no effect on a
// regular file.
func openCarryFileAt(dir *os.File, name string) (*os.File, os.FileInfo, error) {
	file, err := openCarryAt(dir, name, unix.O_RDONLY|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, nil, fmt.Errorf("%s is not a regular file (%s)", file.Name(), info.Mode().Type())
	}
	return file, info, nil
}

// carryArtifactPresent reports whether home/rel is a regular file reachable
// without following a symlink below home.
func carryArtifactPresent(home, rel string) bool {
	dirs, name, err := splitCarryPath(rel, true)
	if err != nil {
		return false
	}
	root, err := openCarryRoot(home, true)
	if err != nil {
		return false
	}
	defer root.Close()
	dir, err := openCarryDirChain(root, dirs, false)
	if err != nil {
		return false
	}
	defer dir.Close()
	file, _, err := openCarryFileAt(dir, name)
	if err != nil {
		return false
	}
	_ = file.Close()
	return true
}

// carryConversationFile makes dstHome/rel hold the whole append-only
// conversation stored at srcHome/rel. It never writes to srcHome.
//
// requireSource is false only for a carry an identity checkpoint already
// committed: its pre-commit copy landed, so a destination that holds the
// conversation is enough even if the previous account has since lost it. A new
// carry always requires the source, or a stale destination copy would be
// resumed as though it were the whole conversation.
func carryConversationFile(srcHome, dstHome, rel string, requireSource bool) error {
	dirs, name, err := splitCarryPath(rel, true)
	if err != nil {
		return carryFailure("af refused an unsafe conversation path", err)
	}
	dstRoot, err := openCarryRoot(dstHome, false)
	if err != nil {
		return carryFailure("the new account's home could not be opened safely", err)
	}
	defer dstRoot.Close()
	dstDir, err := openCarryDirChain(dstRoot, dirs, true)
	if err != nil {
		return carryFailure("the new account's conversation directory is not a plain directory", err)
	}
	defer dstDir.Close()

	var srcDir *os.File
	srcRoot, err := openCarryRoot(srcHome, true)
	if err == nil {
		defer srcRoot.Close()
		srcDir, err = openCarryDirChain(srcRoot, dirs, false)
	}
	switch {
	case err == nil:
		defer srcDir.Close()
	case errors.Is(err, fs.ErrNotExist):
	default:
		return carryFailure("the conversation could not be read safely from the previous account", err)
	}
	return carryFileAt(srcDir, dstDir, name, requireSource)
}

// carryFileAt applies the superset rule to one file. srcDir may be nil when the
// source directory does not exist.
func carryFileAt(srcDir, dstDir *os.File, name string, requireSource bool) error {
	var src *os.File
	var srcInfo os.FileInfo
	if srcDir != nil {
		var err error
		src, srcInfo, err = openCarryFileAt(srcDir, name)
		switch {
		case err == nil:
			defer src.Close()
		case errors.Is(err, fs.ErrNotExist):
			src = nil
		default:
			return carryFailure("the conversation could not be read safely from the previous account", err)
		}
	}
	dst, dstInfo, err := openCarryFileAt(dstDir, name)
	switch {
	case err == nil:
		defer dst.Close()
	case errors.Is(err, fs.ErrNotExist):
		dst = nil
	default:
		return carryFailure("the new account holds something other than a plain file where the conversation belongs", err)
	}

	if src == nil {
		if dst != nil && !requireSource {
			return nil
		}
		return carryFailure("its transcript is missing from the previous account's home", fs.ErrNotExist)
	}
	if dst != nil {
		if os.SameFile(srcInfo, dstInfo) {
			return nil
		}
		srcSize, dstSize := srcInfo.Size(), dstInfo.Size()
		same, err := carryPrefixMatches(src, dst, min(srcSize, dstSize))
		if err != nil {
			return carryFailure("comparing it with the new account's copy failed", err)
		}
		if !same {
			return carryFailure("the new account already holds a different copy of this conversation", nil)
		}
		if dstSize >= srcSize {
			// A replacement that already resumed here appended to this copy.
			// Overwriting it with the shorter source would truncate that work.
			return nil
		}
	}
	return replaceCarryFile(dstDir, name, src, srcInfo.Size())
}

// carryPrefixMatches reports whether the first n bytes of a and b are equal.
func carryPrefixMatches(a, b *os.File, n int64) (bool, error) {
	const chunk = 64 << 10
	left := make([]byte, chunk)
	right := make([]byte, chunk)
	for offset := int64(0); offset < n; {
		size := min(int64(chunk), n-offset)
		if err := readFullAt(a, left[:size], offset); err != nil {
			return false, err
		}
		if err := readFullAt(b, right[:size], offset); err != nil {
			return false, err
		}
		if !bytes.Equal(left[:size], right[:size]) {
			return false, nil
		}
		offset += size
	}
	return true, nil
}

func readFullAt(file *os.File, buf []byte, offset int64) error {
	n, err := file.ReadAt(buf, offset)
	if n == len(buf) {
		return nil
	}
	if err == nil || errors.Is(err, io.EOF) {
		return fmt.Errorf("%s shrank while it was being compared", file.Name())
	}
	return err
}

// replaceCarryFile atomically installs the first size bytes of src as
// dir/name: an owner-only temporary file beside the target, synced, then
// renamed over it. rename replaces a planted symlink rather than writing
// through it.
func replaceCarryFile(dir *os.File, name string, src *os.File, size int64) error {
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return carryFailure("copying it into the new account failed", err)
	}
	tmpName := ".af-carry-" + hex.EncodeToString(nonce[:]) + ".tmp"
	tmp, err := openCarryAt(dir, tmpName, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL, carryFileMode)
	if err != nil {
		return carryFailure("copying it into the new account failed", err)
	}
	installed := false
	defer func() {
		if !installed {
			_ = tmp.Close()
			_ = unix.Unlinkat(int(dir.Fd()), tmpName, 0)
		}
	}()
	copied, err := io.Copy(tmp, io.NewSectionReader(src, 0, size))
	if err == nil && copied != size {
		err = fmt.Errorf("copied %d of %d bytes", copied, size)
	}
	if err == nil {
		// umask may have narrowed the create mode; the provider must still be
		// able to append to its own transcript.
		err = tmp.Chmod(carryFileMode)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if err == nil {
		err = tmp.Close()
	}
	if err == nil {
		err = retryEINTR(func() error {
			return unix.Renameat(int(dir.Fd()), tmpName, int(dir.Fd()), name)
		})
	}
	if err != nil {
		return carryFailure("copying it into the new account failed", err)
	}
	installed = true
	_ = dir.Sync()
	return nil
}

// carryConversationTree copies a per-session directory (Claude's tool-result
// and subagent files) beside a carried transcript. It is best effort: the
// transcript is the conversation, and these files only restore detail. Files
// follow the same superset rule; symlinks and special files are skipped.
func carryConversationTree(srcHome, dstHome, rel string) error {
	parts, _, err := splitCarryPath(rel, false)
	if err != nil {
		return err
	}
	srcRoot, err := openCarryRoot(srcHome, true)
	if err != nil {
		return err
	}
	defer srcRoot.Close()
	srcDir, err := openCarryDirChain(srcRoot, parts, false)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer srcDir.Close()
	dstRoot, err := openCarryRoot(dstHome, false)
	if err != nil {
		return err
	}
	defer dstRoot.Close()
	dstDir, err := openCarryDirChain(dstRoot, parts, true)
	if err != nil {
		return err
	}
	defer dstDir.Close()
	budget := carryTreeMaxEntries
	return copyCarryTree(srcDir, dstDir, 0, &budget)
}

func copyCarryTree(src, dst *os.File, depth int, budget *int) error {
	if depth > carryTreeMaxDepth {
		return fmt.Errorf("%s is nested more than %d directories deep", src.Name(), carryTreeMaxDepth)
	}
	entries, err := src.ReadDir(-1)
	if err != nil {
		return err
	}
	var errs []error
	for _, entry := range entries {
		*budget--
		if *budget < 0 {
			return errors.Join(append(errs, fmt.Errorf("more than %d entries", carryTreeMaxEntries))...)
		}
		name := entry.Name()
		switch {
		case entry.IsDir():
			errs = append(errs, copyCarrySubtree(src, dst, name, depth, budget))
		case entry.Type().IsRegular():
			errs = append(errs, carryFileAt(src, dst, name, true))
		}
	}
	return errors.Join(errs...)
}

func copyCarrySubtree(src, dst *os.File, name string, depth int, budget *int) error {
	srcChild, err := openCarryDirAt(src, name, false)
	if err != nil {
		return err
	}
	defer srcChild.Close()
	dstChild, err := openCarryDirAt(dst, name, true)
	if err != nil {
		return err
	}
	defer dstChild.Close()
	return copyCarryTree(srcChild, dstChild, depth+1, budget)
}
