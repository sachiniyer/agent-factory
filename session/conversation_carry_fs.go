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

	"github.com/sachiniyer/agent-factory/internal/agentaccount"
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

// carrySymlinkError marks a path af refused because it is a symbolic link, so
// the notice can name the link instead of calling the conversation missing.
type carrySymlinkError struct {
	path string
	err  error
}

func (e *carrySymlinkError) Error() string {
	return e.path + " is a symbolic link: " + e.err.Error()
}

func (e *carrySymlinkError) Unwrap() error { return e.err }

func symlinkCarryReason(path string) string {
	return path + " is a symbolic link, which af does not follow when carrying a conversation"
}

// carrySymlinkPath returns the refused link err names, if any.
func carrySymlinkPath(err error) string {
	var symlinkErr *carrySymlinkError
	if errors.As(err, &symlinkErr) {
		return symlinkErr.path
	}
	return ""
}

// carryFailureAt is carryFailure for an error from opening a path: a refused
// symlink is named rather than described by reason.
func carryFailureAt(reason string, err error) error {
	if path := carrySymlinkPath(err); path != "" {
		return carryFailure(symlinkCarryReason(path), err)
	}
	return carryFailure(reason, err)
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

// carryRoot anchors a provider home. base is opened following a symlink only
// when follow is set; every component below it is opened with O_NOFOLLOW. A
// registered account is anchored at the AF home with the
// accounts/<agent>/<name> components, so an ancestor swapped for a symlink
// after agentaccount.Selected validated it still cannot redirect the carry.
// An ambient source such as ~/.claude is anchored at itself and followed,
// because it may legitimately be a symlink into a dotfiles checkout.
type carryRoot struct {
	base       string
	follow     bool
	components []string
}

// ambientCarryRoot anchors a home af does not own at itself.
func ambientCarryRoot(dir string) carryRoot {
	return carryRoot{base: dir, follow: true}
}

// accountCarryRoot anchors a registered account's home at the AF home. dir
// must be exactly home/accounts/<agent>/<name>, as agentaccount.Dir builds it.
func accountCarryRoot(home, agent, name, dir string) (carryRoot, error) {
	rel, err := filepath.Rel(home, dir)
	if err != nil {
		return carryRoot{}, err
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 3 || parts[0] != agentaccount.DirName || parts[1] != agent || parts[2] != name {
		return carryRoot{}, fmt.Errorf("account directory %s is not %s/%s/%s under %s",
			dir, agentaccount.DirName, agent, name, home)
	}
	return carryRoot{base: home, follow: true, components: parts}, nil
}

// dir is the provider home r names, for messages and path joins.
func (r carryRoot) dir() string {
	return filepath.Join(append([]string{r.base}, r.components...)...)
}

// openCarryAnchor opens the provider home r names. The zero root names no
// home: a committed carry whose source account is gone reads as a missing
// source.
func openCarryAnchor(r carryRoot) (*os.File, error) {
	if r.base == "" {
		return nil, &os.PathError{Op: "open", Path: "", Err: fs.ErrNotExist}
	}
	base, err := openCarryRoot(r.base, r.follow)
	if err != nil || len(r.components) == 0 {
		return base, err
	}
	defer base.Close()
	return openCarryDirChain(base, r.components, false)
}

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
		pathErr := &os.PathError{Op: "open", Path: path, Err: err}
		if !follow && !errors.Is(err, unix.ENOENT) {
			if info, statErr := os.Lstat(path); statErr == nil && info.Mode()&os.ModeSymlink != 0 {
				return nil, &carrySymlinkError{path: path, err: pathErr}
			}
		}
		return nil, pathErr
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
		pathErr := &os.PathError{Op: "openat", Path: path, Err: err}
		if !errors.Is(err, unix.ENOENT) && flags&unix.O_CREAT == 0 {
			var st unix.Stat_t
			if unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW) == nil &&
				st.Mode&unix.S_IFMT == unix.S_IFLNK {
				return nil, &carrySymlinkError{path: path, err: pathErr}
			}
		}
		return nil, pathErr
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

// probeCarryArtifact reports whether rel is a regular file beneath home,
// reachable without following a symlink below home's anchor. Absence is
// (false, nil); any other failure, such as a refused symlink, is returned.
func probeCarryArtifact(home carryRoot, rel string) (bool, error) {
	dirs, name, err := splitCarryPath(rel, true)
	if err != nil {
		return false, err
	}
	root, err := openCarryAnchor(home)
	if err == nil {
		defer root.Close()
		var dir *os.File
		if dir, err = openCarryDirChain(root, dirs, false); err == nil {
			defer dir.Close()
			var file *os.File
			if file, _, err = openCarryFileAt(dir, name); err == nil {
				_ = file.Close()
				return true, nil
			}
		}
	}
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// probeCarryDir returns the error, if any, from opening the directory rel
// beneath home; a missing directory is not an error.
func probeCarryDir(home carryRoot, rel string) error {
	parts, _, err := splitCarryPath(rel, false)
	if err != nil {
		return err
	}
	root, err := openCarryAnchor(home)
	if err == nil {
		defer root.Close()
		var dir *os.File
		if dir, err = openCarryDirChain(root, parts, false); err == nil {
			return dir.Close()
		}
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// carryConversationFile makes dstHome/rel hold the whole append-only
// conversation stored at srcHome/rel. It never writes to srcHome.
//
// requireSource is false only for a carry an identity checkpoint already
// committed: its pre-commit copy landed, so a destination that holds the
// conversation is enough even if the previous account has since lost it. A new
// carry always requires the source, or a stale destination copy would be
// resumed as though it were the whole conversation.
func carryConversationFile(srcHome, dstHome carryRoot, rel string, requireSource bool) error {
	dirs, name, err := splitCarryPath(rel, true)
	if err != nil {
		return carryFailure("af refused an unsafe conversation path", err)
	}
	dstRoot, err := openCarryAnchor(dstHome)
	if err != nil {
		return carryFailureAt("the new account's home could not be opened safely", err)
	}
	defer dstRoot.Close()
	dstDir, err := openCarryDirChain(dstRoot, dirs, true)
	if err != nil {
		return carryFailureAt("the new account's conversation directory is not a plain directory", err)
	}
	defer dstDir.Close()

	var srcDir *os.File
	srcRoot, err := openCarryAnchor(srcHome)
	if err == nil {
		defer srcRoot.Close()
		srcDir, err = openCarryDirChain(srcRoot, dirs, false)
	}
	switch {
	case err == nil:
		defer srcDir.Close()
	case errors.Is(err, fs.ErrNotExist):
	default:
		return carryFailureAt("the conversation could not be read safely from the previous account", err)
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
			return carryFailureAt("the conversation could not be read safely from the previous account", err)
		}
	}
	dst, dstInfo, err := openCarryFileAt(dstDir, name)
	switch {
	case err == nil:
		defer dst.Close()
	case errors.Is(err, fs.ErrNotExist):
		dst = nil
	default:
		return carryFailureAt("the new account holds something other than a plain file where the conversation belongs", err)
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
			// It is kept, so it must stay appendable: the resumed provider
			// writes its next turn into this file.
			if dstInfo.Mode().Perm()&0o200 == 0 {
				if err := dst.Chmod(carryFileMode); err != nil {
					return carryFailure("the new account's copy of this conversation is not writable", err)
				}
			}
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
func carryConversationTree(srcHome, dstHome carryRoot, rel string) error {
	parts, _, err := splitCarryPath(rel, false)
	if err != nil {
		return err
	}
	srcRoot, err := openCarryAnchor(srcHome)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
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
	dstRoot, err := openCarryAnchor(dstHome)
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
