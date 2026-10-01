package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"golang.org/x/sys/unix"
)

// This file holds the recursive cross-device tree copier. Its counterparts:
// worktree_copy_fidelity.go holds the property-preservation primitives (modes,
// mtimes, hole-preserving content), and worktree_copy_cleanup.go holds the
// identity bookkeeping and the removal paths that consume the manifest this
// copier builds.

// copyTree recursively copies the directory rooted at src to dest, preserving
// regular files (contents + permission bits), subdirectories, and symlinks
// (copied as links, never followed). Permission bits are applied explicitly
// rather than left to the mode passed at creation, because that mode is
// subtracted by the process umask and because a directory has to stay writable
// until its own contents are in place — see preserveSourceMode and
// workingDirectoryMode.
// Traversal stays anchored to open directory descriptors: a worktree process can
// replace any pathname after it is inspected, so every source open is
// nonblocking and no-follow, and every destination node is created exclusively.
// This is only reached on the cross-device fallback.
func copyTree(src, dest string) error {
	copied, err := copyTreeWithIdentities(src, dest)
	if copied != nil {
		copied.close()
	}
	return err
}

type copiedTreeIdentities struct {
	source                    *os.File
	sourceIdentity            pathIdentity
	destinationParent         *os.File
	destinationParentIdentity pathIdentity
	destination               *os.File
	destinationIdentity       pathIdentity
	root                      copiedDirectory
	skipped                   []ArchiveSkippedEntry
}

type copiedDirectory struct {
	entries []copiedEntry
}

type copiedEntry struct {
	name        string
	source      pathIdentity
	destination pathIdentity
	directory   *copiedDirectory
	state       copiedEntryState
	reason      ArchiveSkipReason
}

type copiedDirectoryRoute struct {
	parent    int
	entry     copiedEntry
	directory *copiedDirectory
	depth     int
}

const maxArchiveTreeDepth = 64

type pathIdentity struct {
	device   uint64
	inode    uint64
	fileType uint32
}

func (identity pathIdentity) same(other pathIdentity) bool {
	return identity == other
}

func (copied *copiedTreeIdentities) close() {
	_ = copied.destination.Close()
	_ = copied.destinationParent.Close()
	_ = copied.source.Close()
}

func (copied *copiedTreeIdentities) validateSource(path string) error {
	current, err := identityFromFile(copied.source)
	if err != nil {
		return err
	}
	if !copied.sourceIdentity.same(current) {
		return fmt.Errorf("source root identity changed at %s", path)
	}
	return validateCopiedTree(copied.source, copied.root, true, path)
}

func (copied *copiedTreeIdentities) validateDestination(path string) error {
	current, err := identityFromFile(copied.destination)
	if err != nil {
		return err
	}
	if !copied.destinationIdentity.same(current) {
		return fmt.Errorf("destination root identity changed at %s", path)
	}
	return validateCopiedTree(copied.destination, copied.root, false, path)
}

func openDirectoryRoute(
	root *os.File,
	rootPath string,
	components []copiedEntry,
	source bool,
) (*os.File, error) {
	role := "destination"
	if source {
		role = "source"
	}
	current, _, err := openDirectoryAt(root, ".", rootPath, role)
	if err != nil {
		return nil, err
	}
	currentPath := rootPath
	for _, component := range components {
		currentPath = filepath.Join(currentPath, component.name)
		next, _, err := openDirectoryAt(current, component.name, currentPath, role)
		_ = current.Close()
		if err != nil {
			return nil, err
		}
		current = next
		identity, err := identityFromFile(current)
		if err != nil {
			_ = current.Close()
			return nil, err
		}
		expected := component.destination
		if source {
			expected = component.source
		}
		if !expected.same(identity) {
			_ = current.Close()
			return nil, fmt.Errorf("%s directory identity changed at %s", role, currentPath)
		}
	}
	return current, nil
}

func directoryRoutePath(rootPath string, components []copiedEntry) string {
	path := rootPath
	for _, component := range components {
		path = filepath.Join(path, component.name)
	}
	return path
}

func copyTreeWithIdentities(src, dest string) (*copiedTreeIdentities, error) {
	return copyTreeWithPolicy(src, dest, refuseUnreadable)
}

// copyTreeWithPolicy is the only permissive copier entrypoint. Its ordinary
// wrapper above deliberately supplies REFUSE, so permission to omit a source
// file can only arrive from the archive operation that names it explicitly.
func copyTreeWithPolicy(src, dest string, policy unreadablePolicy) (*copiedTreeIdentities, error) {
	source, sourceInfo, err := openDirectoryPath(src, "source")
	if err != nil {
		return nil, err
	}
	if err := copyTreeBeforeSourceOpen(src); err != nil {
		_ = source.Close()
		return nil, err
	}
	sourceIdentity, err := identityFromFile(source)
	if err != nil {
		_ = source.Close()
		return nil, err
	}

	destParentPath := filepath.Dir(dest)
	destParent, _, err := openDirectoryPathFollowingLinks(destParentPath, "destination parent")
	if err != nil {
		_ = source.Close()
		return nil, err
	}
	destinationParentIdentity, err := identityFromFile(destParent)
	if err != nil {
		_ = destParent.Close()
		_ = source.Close()
		return nil, err
	}
	destName := filepath.Base(dest)
	if err := unix.Mkdirat(int(destParent.Fd()), destName, workingDirectoryMode(sourceInfo.Mode())); err != nil {
		_ = destParent.Close()
		_ = source.Close()
		return nil, fmt.Errorf("cannot move worktree across filesystems: failed to create destination directory %s exclusively: %w", dest, err)
	}
	// Identify the staging root before anything else can fail: cleanup below
	// unlinks it, and unlinkat() takes a name rather than an inode.
	createdIdentity, err := identityAt(destParent, destName)
	if err != nil {
		_ = destParent.Close()
		_ = source.Close()
		return nil, fmt.Errorf(
			"cannot move worktree across filesystems: failed to identify destination directory %s after creating it (leaving it in place): %w",
			dest, err,
		)
	}
	destination, _, err := openDirectoryAt(destParent, destName, dest, "destination")
	if err != nil {
		cleanupErr := removeCreatedDirectory(destParent, destParentPath, destName, createdIdentity)
		_ = destParent.Close()
		_ = source.Close()
		return nil, errors.Join(err, cleanupErr)
	}
	destinationIdentity, err := identityFromFile(destination)
	if err != nil {
		cleanupErr := removeCreatedDirectory(destParent, destParentPath, destName, createdIdentity)
		_ = destination.Close()
		_ = destParent.Close()
		_ = source.Close()
		return nil, errors.Join(err, cleanupErr)
	}
	copied := &copiedTreeIdentities{
		source:                    source,
		sourceIdentity:            sourceIdentity,
		destinationParent:         destParent,
		destinationParentIdentity: destinationParentIdentity,
		destination:               destination,
		destinationIdentity:       destinationIdentity,
	}

	copied.root, err = copyDirectoryContents(source, destination, src, dest, policy, &copied.skipped)
	if err != nil {
		partialManifest := destinationCleanupManifest(copied.root)
		cleanupErr := removeOpenedDirectory(destParent, destName, dest, destination, &partialManifest)
		copied.close()
		if cleanupErr != nil {
			return nil, errors.Join(err, fmt.Errorf("failed to clean partial destination tree %s: %w", dest, cleanupErr))
		}
		return nil, err
	}
	return copied, nil
}

func copyDirectoryContents(
	source, destination *os.File,
	sourcePath, destinationPath string,
	policy unreadablePolicy,
	skipped *[]ArchiveSkippedEntry,
) (copiedDirectory, error) {
	root := copiedDirectory{}
	// Source inode -> where its bytes first landed AND what inode they landed
	// on. Scoped to one copy, so it can never name a path from another tree.
	links := map[pathIdentity]copiedFileLink{}
	// The retained comparison descriptors die with the copy that opened them. Every
	// exit from this function passes here, including the error returns below, so a
	// failed copy cannot leak them either (#3063).
	defer func() {
		for _, link := range links {
			if link.reader != nil {
				_ = link.reader.Close()
			}
		}
	}()
	// Same scoping as links above: whether this DESTINATION holds extended
	// attributes is learned once per copy and never leaks into another one.
	xattrs := &xattrDestination{}
	routes := []copiedDirectoryRoute{{parent: -1, directory: &root}}
	for index := 0; index < len(routes); index++ {
		job := routes[index]
		components := copiedDirectoryRouteComponents(routes, index)
		sourceDirectory, err := openDirectoryRoute(source, sourcePath, components, true)
		if err != nil {
			return root, err
		}
		destinationDirectory, err := openDirectoryRoute(destination, destinationPath, components, false)
		if err != nil {
			_ = sourceDirectory.Close()
			return root, err
		}
		err = copyDirectoryLevel(
			sourceDirectory,
			destinationDirectory,
			directoryRoutePath(sourcePath, components),
			directoryRoutePath(destinationPath, components),
			job.directory,
			destination,
			relativeRoutePath(components),
			links,
			xattrs,
			policy,
			skipped,
		)
		if err == nil {
			// The level is complete, and nothing is ever written directly into
			// this directory again — descendants are filled through their own
			// descriptors, which need only the search bit here. So this is the
			// first moment the real mode can be applied, and the last moment it
			// is free: a read-only source directory takes its mode now rather
			// than blocking its own contents (#2872).
			err = applyCopiedDirectoryMode(
				sourceDirectory, destinationDirectory, directoryRoutePath(destinationPath, components), xattrs,
			)
		}
		_ = destinationDirectory.Close()
		_ = sourceDirectory.Close()
		if err != nil {
			return root, err
		}
		if job.depth >= maxArchiveTreeDepth && copiedDirectoryHasChildren(job.directory) {
			return root, fmt.Errorf(
				"cannot move worktree across filesystems: maximum supported depth of %d exceeded at %s",
				maxArchiveTreeDepth, directoryRoutePath(sourcePath, components),
			)
		}
		routes = appendCopiedDirectoryChildren(routes, index)
	}
	return root, nil
}

func copyDirectoryLevel(
	source, destination *os.File,
	sourcePath, destinationPath string,
	directory *copiedDirectory,
	destinationRoot *os.File,
	relativeDirectory string,
	links map[pathIdentity]copiedFileLink,
	xattrs *xattrDestination,
	policy unreadablePolicy,
	skipped *[]ArchiveSkippedEntry,
) error {
	names, err := source.Readdirnames(-1)
	if err != nil {
		return fmt.Errorf("cannot move worktree across filesystems: failed to enumerate source directory %s: %w", sourcePath, err)
	}
	sort.Strings(names)
	directory.entries = make([]copiedEntry, 0, len(names))
	for _, name := range names {
		childSourcePath := filepath.Join(sourcePath, name)
		childDestinationPath := filepath.Join(destinationPath, name)
		if err := copyTreeBeforeSourceOpen(childSourcePath); err != nil {
			return err
		}
		stat, err := statAt(source, name)
		if err != nil {
			return fmt.Errorf("cannot move worktree across filesystems: failed to inspect source entry %s: %w", childSourcePath, err)
		}
		inspected := identityFromStat(stat)
		// The inspect/open boundary: from here the entry's type is a stale fact
		// about a pathname, and a worktree process can replace what that name
		// resolves to. Every branch below re-establishes the type from the object
		// it actually opened; this seam lets a test substitute a node in exactly
		// this window (#2708).
		if err := copyTreeAfterSourceInspect(childSourcePath); err != nil {
			return err
		}

		var entry copiedEntry
		switch inspected.fileType {
		case unix.S_IFDIR:
			entry, err = copyDirectoryEntry(source, destination, name, childSourcePath, childDestinationPath, inspected)
		case unix.S_IFLNK:
			entry, err = copySymlinkEntry(source, destination, name, childSourcePath, childDestinationPath, inspected)
		case unix.S_IFREG:
			// Every successfully copied regular inode is recorded, not just the
			// ones observed with nlink > 1. A live worktree process can hard-link
			// an already-copied file AFTER it was seen with nlink == 1, and the
			// later sighting would then miss the map and be copied as a separate
			// inode — silently publishing two unrelated files while source
			// validation passed, because pathIdentity excludes the link count.
			// A hit means the same inode was already copied. It does NOT mean the
			// inode still holds what that copy captured: a rewrite in place keeps
			// device/inode/type, so linking here would reproduce the earlier
			// sighting's bytes at this path. Compare what actually changes with
			// content, and when it has moved, copy afresh instead.
			//
			// Falling back rather than failing is deliberate. A concurrent write is
			// benign and must not fail the archive; a fresh copy costs disk and the
			// link semantics for this one pair, which is strictly better than an
			// archive holding bytes that never existed at that path. Hard-linking
			// is an optimisation, and one that can corrupt content is not worth its
			// saving (#3046).
			first, seen := links[inspected]
			var retained *os.File
			if seen && first.reader != nil {
				retained = first.reader.file
			}
			if seen && sourceMatchesCopiedFile(source, name, destinationRoot, first.path, first.identity, inspected, retained) {
				entry, err = linkCopiedFile(destination, destinationRoot, first, name, childDestinationPath, inspected)
			} else {
				// Asked for only when the source was ALREADY seen with more than one
				// link: those are the inodes a later sighting can hit, so the retained-fd
				// count is bounded by genuinely shared inodes rather than by tree size and
				// EMFILE does not become a new failure mode. An inode linked after this
				// copy is still recorded (see above) and simply falls back to reopening,
				// which is exactly today's behaviour.
				var reader *retainedLinkReader
				var retain func(fd int)
				// PROCESS-WIDE BOUNDED (#3063 review). A valid static tree can hold more distinct
				// hard-linked inodes than the process has spare descriptors, and an
				// unbounded cache does not merely waste them: once dup starts failing, the
				// later groups get a nil reader, cannot be reopened, and are copied as
				// separate inodes — reintroducing the exact fidelity loss this change
				// exists to remove, silently and only on big trees.
				//
				// Past the budget af simply does not retain, which is today's behaviour
				// (reopen, and fall back to a fresh copy if the mode denies it). Degrading
				// to the old path is acceptable; degrading to it unpredictably at whatever
				// point the fd table happens to fill is not.
				if stat.Nlink > 1 {
					retain = func(fd int) {
						// dup(2), not a reopen: the point is a descriptor that predates the
						// mode narrowing, and a reopen would hit the very denial this avoids.
						// A dup failure is not fatal — the comparison falls back to reopening.
						// F_DUPFD_CLOEXEC, never unix.Dup: dup(2) CLEARS FD_CLOEXEC on the new
						// descriptor even though the original was opened O_CLOEXEC (#3063
						// review). The daemon forks hooks and session processes while a copy is
						// running, and any of them would inherit an O_RDWR handle to a staging
						// inode — able to read or modify archived content past the preserved
						// mode and past this function's own close. Setting the flag after the
						// dup would leave that window open between the two calls.
						reader = retainLinkReader(fd, childDestinationPath)
					}
				}
				entry, err = copyRegularFileAtWithIdentity(
					source, destination, name, childSourcePath, childDestinationPath, &inspected, xattrs, retain)
				switch {
				case err == nil:
					// The entry being REPLACED owns a descriptor, and overwriting the map
					// value would strand it: the deferred cleanup only sees the final value,
					// so an inode rewritten between sightings would leak one descriptor per
					// fallback and exhaust the table before any finalizer ran (#3063 review).
					if previous, seen := links[inspected]; seen && previous.reader != nil {
						_ = previous.reader.Close()
					}
					links[inspected] = copiedFileLink{
						path:     filepath.Join(relativeDirectory, name),
						identity: entry.destination,
						reader:   reader,
					}
				case reader != nil:
					// Nothing will record it, so nothing else can close it.
					_ = reader.Close()
				}
			}
		default:
			err = unsupportedSourceTypeError(childSourcePath, uint32(stat.Mode))
		}
		// A skipped file is still IN the manifest. Its source identity and reason
		// are the evidence validateSource consumes; its state is the evidence
		// validateDestination consumes. Omitting the entry is the attempt-1 bug:
		// strict source name-set validation correctly rejected that tree (#3066).
		var unreadable *unreadableSourceError
		if policy == skipUnreadable && errors.As(err, &unreadable) {
			entry = copiedEntry{
				name:   name,
				source: inspected,
				state:  copiedEntryKnownAbsent,
				reason: ArchiveSkipPermissionDenied,
			}
			*skipped = append(*skipped, newArchiveSkippedEntry(
				filepath.Join(relativeDirectory, name), ArchiveSkipPermissionDenied,
			))
			err = nil
		}
		// A helper names its entry as soon as it has created the destination
		// node and learned its identity, so record it even when the entry
		// later fails. Cleanup can only remove what the manifest describes;
		// dropping a node this process created makes it read as an unexpected
		// entry and strands the whole partial tree.
		if entry.name != "" {
			directory.entries = append(directory.entries, entry)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// relativeRoutePath renders a BFS route as a path relative to the copy root,
// which is what linkCopiedFile needs: the root descriptor outlives every level,
// so a link can be made against it without reopening the route the first copy
// landed in.
func relativeRoutePath(components []copiedEntry) string {
	parts := make([]string, 0, len(components))
	for _, component := range components {
		parts = append(parts, component.name)
	}
	return filepath.Join(parts...)
}

// copiedFileLink is where a source inode's bytes first landed: the path to link
// against, and the destination inode that path resolved to at the time. Both are
// needed — the path to make the link, the identity to prove the link landed on
// the right inode.

func copyDirectoryEntry(
	source, destination *os.File,
	name, sourcePath, destinationPath string,
	inspected pathIdentity,
) (copiedEntry, error) {
	sourceChild, sourceInfo, err := openDirectoryAt(source, name, sourcePath, "source")
	if err != nil {
		return copiedEntry{}, err
	}
	defer sourceChild.Close()
	sourceIdentity, err := identityFromFile(sourceChild)
	if err != nil {
		return copiedEntry{}, err
	}
	if !inspected.same(sourceIdentity) {
		return copiedEntry{}, fmt.Errorf("cannot move worktree across filesystems: source directory %s changed before it was opened", sourcePath)
	}
	if err := unix.Mkdirat(int(destination.Fd()), name, workingDirectoryMode(sourceInfo.Mode())); err != nil {
		return copiedEntry{}, fmt.Errorf("cannot move worktree across filesystems: failed to create destination directory %s exclusively: %w", destinationPath, err)
	}
	// Identify the node right after creating it: every later step can fail, and
	// the caller can only record what it can name and identify.
	destinationIdentity, err := identityAt(destination, name)
	if err != nil {
		return copiedEntry{}, fmt.Errorf("cannot move worktree across filesystems: failed to identify destination directory %s after creating it: %w", destinationPath, err)
	}
	created := copiedEntry{
		name:        name,
		source:      sourceIdentity,
		destination: destinationIdentity,
		directory:   &copiedDirectory{},
	}
	if err := copyTreeAfterDestCreate(destinationPath); err != nil {
		return created, err
	}
	destinationChild, _, err := openDirectoryAt(destination, name, destinationPath, "destination")
	if err != nil {
		return created, err
	}
	defer destinationChild.Close()
	openedIdentity, err := identityFromFile(destinationChild)
	if err != nil {
		return created, err
	}
	if !destinationIdentity.same(openedIdentity) {
		return created, fmt.Errorf("cannot move worktree across filesystems: destination directory %s changed after it was created", destinationPath)
	}
	return created, nil
}

func copySymlinkEntry(
	source, destination *os.File,
	name, sourcePath, destinationPath string,
	inspected pathIdentity,
) (copiedEntry, error) {
	link, err := readLinkAt(source, name, sourcePath)
	if err != nil {
		return copiedEntry{}, err
	}
	// One lstat serves both the race re-check and the timestamp below: statAt is
	// Fstatat with AT_SYMLINK_NOFOLLOW, so it reads the LINK rather than what it
	// points at, and it is the same call identityAt was already making here.
	sourceStat, err := statAt(source, name)
	if err != nil || !inspected.same(identityFromStat(sourceStat)) {
		return copiedEntry{}, fmt.Errorf("cannot move worktree across filesystems: source symlink %s changed while it was copied", sourcePath)
	}
	if err := unix.Symlinkat(link, int(destination.Fd()), name); err != nil {
		return copiedEntry{}, fmt.Errorf("cannot move worktree across filesystems: failed to create destination symlink %s exclusively: %w", destinationPath, err)
	}
	// Identify the node right after creating it: every later step can fail, and
	// the caller can only record what it can name and identify.
	destinationIdentity, err := identityAt(destination, name)
	if err != nil {
		return copiedEntry{}, fmt.Errorf("cannot move worktree across filesystems: failed to identify destination symlink %s after creating it: %w", destinationPath, err)
	}
	created := copiedEntry{name: name, source: inspected, destination: destinationIdentity}
	if err := copyTreeAfterSymlinkCreate(destinationPath); err != nil {
		return created, err
	}
	destinationLink, err := readLinkAt(destination, name, destinationPath)
	if err != nil {
		return created, err
	}
	confirmedIdentity, err := identityAt(destination, name)
	if err != nil || !destinationIdentity.same(confirmedIdentity) || destinationLink != link {
		return created, fmt.Errorf("cannot move worktree across filesystems: destination symlink %s changed while it was copied", destinationPath)
	}
	// The link's OWN mtime. AT_SYMLINK_NOFOLLOW means this addresses the link
	// rather than following it to its target.
	//
	// The confirmation above does NOT cover this call, which is the correction
	// the review of the duplicate raised: the stamp resolves `name` one more
	// time, so a racer that swaps the entry in between lands our timestamp on an
	// inode we never created. AT_SYMLINK_NOFOLLOW refuses to FOLLOW a symlink; it
	// does not refuse to stamp a hard link, so the reachable damage is a foreign
	// file's mtime — outside the staging tree included.
	//
	// It cannot be prevented here. Holding a symlink open needs O_PATH|O_NOFOLLOW
	// on Linux or O_SYMLINK on darwin, and stamping through that descriptor needs
	// AT_EMPTY_PATH, which is Linux-only — the portability wall this whole item
	// keeps running into. So it is DETECTED instead, and detection means the
	// archive refuses rather than commits, which is what every other replacement
	// check in this function already does.
	if err := copyTreeBeforeSymlinkStamp(destinationPath); err != nil {
		return created, err
	}
	if err := preserveSourceModTime(
		int(destination.Fd()), name, symlinkModTime(sourceStat), destinationPath, "symlink",
	); err != nil {
		return created, err
	}
	stampedIdentity, err := identityAt(destination, name)
	if err != nil || !destinationIdentity.same(stampedIdentity) {
		return created, fmt.Errorf("cannot move worktree across filesystems: destination symlink %s changed while its timestamp was applied", destinationPath)
	}
	return created, nil
}

func copiedDirectoryHasChildren(directory *copiedDirectory) bool {
	for _, entry := range directory.entries {
		if entry.directory != nil {
			return true
		}
	}
	return false
}

func copyRegularFileAtWithIdentity(
	source, destination *os.File,
	name, sourcePath, destinationPath string,
	inspected *pathIdentity,
	support *xattrDestination,
	retainCopiedFD func(fd int),
) (copiedEntry, error) {
	fd, err := unix.Openat(
		int(source.Fd()), name,
		unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		0,
	)
	if err != nil {
		// A PERMISSION failure is a file this process cannot read, not a broken
		// copy. Classify it so the walker can apply the operation's policy; every
		// other open failure still aborts, because it means something is wrong with
		// the copy rather than with one file's mode (#3066).
		if errors.Is(err, os.ErrPermission) {
			return copiedEntry{}, &unreadableSourceError{path: sourcePath, err: err}
		}
		return copiedEntry{}, fmt.Errorf("cannot move worktree across filesystems: failed to open source file %s without following links: %w", sourcePath, err)
	}
	in := os.NewFile(uintptr(fd), sourcePath)
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return copiedEntry{}, err
	}
	if !info.Mode().IsRegular() {
		return copiedEntry{}, unsupportedSourceTypeError(sourcePath, uint32(info.Mode()))
	}
	sourceIdentity, err := identityFromFile(in)
	if err != nil {
		return copiedEntry{}, err
	}
	if inspected != nil && !inspected.same(sourceIdentity) {
		return copiedEntry{}, fmt.Errorf("cannot move worktree across filesystems: source file %s changed before it was opened", sourcePath)
	}
	outFD, err := unix.Openat(
		int(destination.Fd()), filepath.Base(destinationPath),
		// O_RDWR, not O_WRONLY: a descriptor retained past the mode narrowing below is
		// how a later hard-link sighting compares this copy's bytes without reopening a
		// path its own mode may deny (#3063). It works for the reason the mode comment
		// below already gives — an open descriptor keeps the access it was opened with
		// regardless of the new mode — applied to reading.
		unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC,
		workingFileMode(info.Mode()),
	)
	if err != nil {
		return copiedEntry{}, fmt.Errorf("cannot move worktree across filesystems: failed to create destination file %s exclusively: %w", destinationPath, err)
	}
	out := os.NewFile(uintptr(outFD), destinationPath)
	// Identify the node right after creating it: the copy and the close can both
	// fail, and the caller can only record what it can name and identify.
	destinationIdentity, err := identityFromFile(out)
	if err != nil {
		_ = out.Close()
		return copiedEntry{}, err
	}
	created := copiedEntry{name: name, source: sourceIdentity, destination: destinationIdentity}
	if err := copyTreeAfterDestCreate(destinationPath); err != nil {
		_ = out.Close()
		return created, err
	}
	if err := copyContentsPreservingHoles(out, in); err != nil {
		_ = out.Close()
		return created, err
	}
	// The mode comes after the contents AND after every step that needs the inode
	// writable. workingFileMode created this file owner-writable for exactly that
	// reason; preserveSourceMode narrows it to the source's mode here, once there
	// is nothing half-written left to expose. The already-open descriptor keeps
	// its write access regardless of the new mode.
	//
	// Non-ACL attributes go on BEFORE the mode and the prune runs while the inode
	// is still writable, because removing one needs write permission just as
	// setting one does. ACLs go on AFTER it: the access ACL and the mode bits are
	// one state seen two ways, so chmod would silently rewrite an ACL written
	// earlier. applyCopiedDirectoryMode does the same four steps in the same order.
	if err := copyNonACLXattrs(support, int(in.Fd()), outFD, destinationPath, "file"); err != nil {
		_ = out.Close()
		return created, err
	}
	pruneDestinationXattrs(int(in.Fd()), outFD, destinationPath, "file")
	if err := preserveSourceMode(outFD, info.Mode(), destinationPath, "file"); err != nil {
		_ = out.Close()
		return created, err
	}
	if err := copyACLXattrs(support, int(in.Fd()), outFD, destinationPath, "file"); err != nil {
		_ = out.Close()
		return created, err
	}
	// After the contents for the same reason the mode is: writing bytes bumps
	// mtime, so this has to be the last thing that touches the file.
	if err := preserveSourceModTime(
		int(destination.Fd()), filepath.Base(destinationPath), info.ModTime(), destinationPath, "file",
	); err != nil {
		_ = out.Close()
		return created, err
	}
	// SUCCESS ONLY, and before the close: the caller dups this descriptor to compare
	// against later (#3063). A callback rather than a returned file keeps every error
	// path above unchanged — each still closes `out` and returns without a descriptor
	// the caller would have to remember to release.
	if retainCopiedFD != nil {
		retainCopiedFD(outFD)
	}
	if err := out.Close(); err != nil {
		return created, err
	}
	return created, nil
}
