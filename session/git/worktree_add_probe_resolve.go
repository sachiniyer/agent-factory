package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
)

// symlinkResolveTimeout bounds the complete symlink-prefix walk the way
// probeTimeout bounds a single fsprobe flight. Without a shared budget a path
// with N components past the first nonexistent entry on a slow filesystem
// could block the synchronous restore poll for N × probeTimeout, because each
// prefix EvalSymlinks invocation restarts the per-dir bound. The whole walk
// shares one deadline instead, so the total time the poll spends here is
// bounded the same way a single lookup is.
const symlinkResolveTimeout = 2 * time.Second

// resolveSymlinkPrefixBounded resolves the longest existing prefix of path
// through symlinks the way the kernel's chdir resolves a relative worktree
// argument against the repoPath the git runner sets as cmd.Dir, then walks the
// remaining non-existent tail component by component the same way. filepath.Join
// only lexically cleans `..`, so a worktree argument like `link/../wt` (where
// `<repo>/link` is a symlink to /mnt/dir) would collapse to `<repo>/wt` and
// probe the wrong filesystem; git follows `link` first and writes /mnt/wt. The
// prefix walk follows symlinks at each existing prefix with
// filepath.EvalSymlinks before `..` is collapsed, and the tail walk
// (resolveSymlinkTail) continues the same component-by-component resolution
// past creatable components, following symlinks the kernel's chdir would
// follow so a traversal like `missing/../link/../wt` resolves to the
// filesystem git actually writes to rather than the lexical collapse. The
// destination git is about to create may not exist yet, so the longest
// existing prefix is resolved and the non-existent tail is walked; if no
// prefix resolves the lexical path is the honest answer (the probe then
// reports the same errno git's child surfaces from the missing path).
//
// Each prefix and tail lookup runs through the bounded fsprobe.EvalSymlinks
// and fsprobe.Lstat, the same per-dir flight shape statSpaceProbe uses — a
// stalled network/FUSE mount that never answers a Readlink or Stat would
// otherwise wedge the synchronous restore poll the probe informs (#5295
// review). The whole walk shares a single deadline (symlinkResolveTimeout)
// rather than restarting the per-dir bound for every component, so a path
// with many components past the first nonexistent entry cannot block for
// N × probeTimeout. A timed-out lookup returns the bounded non-answer
// (ErrProbeTimedOut + context.DeadlineExceeded) so the caller joins it into
// the annotation rather than silently falling back to the lexical path; a
// non-timeout ENOENT (the normal pre-creation answer) walks up to the next
// shorter prefix, while any other resolution error (EACCES, ETIMEDOUT,
// EIO, ...) is returned — it is environmental evidence about the filesystem
// git's child is about to write to, and rejoining the tail lexically would
// remove a symlinked component like `link/..` and let the subsequent probes
// succeed, misclassifying the original git failure as structural.
func resolveSymlinkPrefixBounded(path string) (resolved string, timedOut bool, err error, creatableParents []string) {
	if path == "" {
		return path, false, nil, nil
	}
	// Run the whole prefix walk under one deadline so a slow filesystem
	// cannot block the restore poll for N × probeTimeout — each prefix
	// EvalSymlinks invocation already runs in its own per-dir bounded flight,
	// but without a shared budget a path with many components past the first
	// nonexistent entry could restart the deadline for every prefix. The
	// caller returns at the shared bound the way a single timed-out flight
	// does; the walk's goroutine is canceled at the same bound so it does
	// not keep issuing metadata calls (and accumulating background goroutines)
	// after the caller has returned — the in-flight per-dir flight still
	// drains on its own per-dir timeout, but no new prefix lookup starts.
	type walkResult struct {
		resolved         string
		timedOut         bool
		err              error
		creatableParents []string
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan walkResult, 1)
	go func() {
		r, to, e, cp := resolveSymlinkPrefixWalk(ctx, path)
		done <- walkResult{r, to, e, cp}
	}()
	timer := time.NewTimer(symlinkResolveTimeout)
	defer timer.Stop()
	select {
	case res := <-done:
		return res.resolved, res.timedOut, res.err, res.creatableParents
	case <-timer.C:
		// The whole walk did not answer within the shared budget — surface
		// the typed non-answer the same way a single timed-out flight does,
		// so the classifier reads the bounded environmental class rather than
		// bare structural silence. Cancel the walk's context so it stops
		// issuing new prefix lookups; the in-flight per-dir flight drains on
		// its own timeout, but no further prefix is probed.
		cancel()
		// The tail walk accumulates the existing parent of every creatable
		// component it advances past, and returns those parents on the
		// context-cancellation path (resolveSymlinkTail's ctx.Done branch
		// returns creatableParents rather than nil). select choosing timer.C
		// over done loses those parents when the worker had already handed
		// them to the buffered channel, and the worker continues to send
		// them after cancellation once the in-flight per-dir flight drains on
		// its own timeout — git wrote the missing directories into those
		// parents before the deadline fired, so a full filesystem on the
		// mount they sit on is the resource errno the classifier must read.
		// Wait for the worker to hand them over rather than returning a
		// hard-coded nil that leaves the creatable-parent probes out of
		// annotateGitWriteError: the in-flight per-dir flight the deadline
		// interrupted already has its own probeTimeout bound, so the receive
		// drains a bounded flight (it does not start a new one) and the total
		// stays within symlinkResolveTimeout plus one in-flight probeTimeout
		// rather than the N × probeTimeout the shared deadline prevents.
		res := <-done
		return "", true, fmt.Errorf("symlink prefix walk of %s did not answer within %s: %w: %w",
			path, symlinkResolveTimeout, fsprobe.ErrProbeTimedOut, context.DeadlineExceeded), res.creatableParents
	}
}

// resolveSymlinkPrefixWalk is the unbounded component walk
// resolveSymlinkPrefixBounded runs under a single shared deadline. It walks
// the un-joined path from the longest prefix down, resolving the longest
// existing prefix through symlinks, then hands the non-existent tail to
// resolveSymlinkTail which follows it component by component the way the
// kernel's chdir does (following symlinks at existing components, treating a
// genuinely missing component as creatable, and surfacing a dangling symlink
// as a resolution error). The context is canceled when the shared deadline
// fires so the walk does not continue issuing prefix or tail lookups after the
// caller has returned.
func resolveSymlinkPrefixWalk(ctx context.Context, path string) (resolved string, timedOut bool, err error, creatableParents []string) {
	parts := strings.Split(path, string(filepath.Separator))
	// On Windows both `/` and `\` are valid path separators, but the split
	// above recognizes only `\`. A restored path written with forward slashes
	// (for example `link/missing/../../dest`) would otherwise stay in one
	// element and the tail walk would lexically collapse the traversal before
	// the symlink is followed, so the probes target the repository filesystem
	// instead of the symlink target where Git creates `missing`. Split the
	// forward-slash elements too so the component walk follows the symlink the
	// way the kernel's chdir does.
	if string(filepath.Separator) != "/" {
		flat := make([]string, 0, len(parts))
		for _, p := range parts {
			flat = append(flat, strings.Split(p, "/")...)
		}
		parts = flat
	}
	// On Windows a UNC destination (\\server\share\dir\..\wt) or a drive-rooted
	// path (C:\dir\..\wt) has a volume prefix the prefix walk must not shorten
	// past: the loop would otherwise reach the bare root (\ or C:) and resolve
	// it against the daemon's current drive, so a tail component above the
	// volume (server, share) would be treated as a creatable directory on the
	// wrong drive and a coincidental ENOSPC there would convert a structural Git
	// failure (Git cannot create a UNC server/share) into an indefinite
	// environmental retry. filepath.VolumeName returns "" on non-Windows, so this
	// bound is a no-op there.
	volume := filepath.VolumeName(path)
	for i := len(parts); i > 0; i-- {
		select {
		case <-ctx.Done():
			// The shared deadline fired — stop walking. The in-flight
			// per-dir flight drains on its own timeout, but no further
			// prefix lookup starts, so the goroutine does not accumulate
			// metadata calls or block waiting on each remaining component.
			return "", false, ctx.Err(), nil
		default:
		}
		prefix := strings.Join(parts[:i], string(filepath.Separator))
		if prefix == "" {
			prefix = string(filepath.Separator)
		}
		// Never search above the volume root: a prefix shorter than the
		// volume name has left the destination's filesystem and reached the
		// current drive, whose ENOSPC is unrelated to the structural Git
		// failure the caller is classifying.
		if volume != "" && len(prefix) < len(volume) {
			break
		}
		r, e := evalSymlinksSpaceProbe(prefix)
		if e == nil {
			tailParts := parts[i:]
			if len(tailParts) == 0 {
				return r, false, nil, nil
			}
			// Walk the non-existent tail component by component the way the
			// kernel's chdir does, following symlinks at each existing
			// component before `..` collapses the path. A lexical
			// filepath.Join would clean `..` first and skip a symlink the
			// kernel follows, so a relative path like `missing/../link/../wt`
			// (git creates `missing`, backs out, follows `link`, backs out,
			// and creates `wt` under the link's target's parent) would collapse
			// to `<repo>/wt` and probe an unrelated filesystem. The walk
			// resolves each existing component (including a symlink) before the
			// `..` that follows it, treats a genuinely missing component as
			// creatable (git creates it and continues), and surfaces a dangling
			// symlink (a symlink whose target does not exist) as a resolution
			// error instead of collapsing past it — git fails resolving the
			// dangling link, so probing a filesystem it never reaches would let
			// a coincidental ENOSPC there stall the lost-restore loop
			// indefinitely on a structural failure.
			return resolveSymlinkTail(ctx, r, tailParts)
		}
		if errors.Is(e, fsprobe.ErrProbeTimedOut) {
			return "", true, e, nil
		}
		// A missing path (ENOENT) is the normal pre-creation answer — walk
		// up to the next shorter prefix to find the longest existing one.
		// A dangling symlink is not a missing path: its entry exists (Lstat
		// succeeds and reports a symlink) but its target does not, so
		// EvalSymlinks returns ENOENT for a prefix that contains it. The
		// prefix walk treats that ENOENT the same as a genuinely absent
		// component and walks up; the tail walk re-examines each component
		// (including the dangling symlink) and surfaces it as a resolution
		// error, so the caller does not collapse the traversal past it and
		// probe a filesystem git never reaches. Any other resolution error
		// (EACCES, ETIMEDOUT, EIO, ...) is environmental evidence about the
		// filesystem git's child is about to write to; rejoining the tail
		// lexically would remove a symlinked component like `link/..` and let
		// the subsequent probes succeed, misclassifying the original git
		// failure as structural. Return it so the caller joins it into the
		// annotation.
		if os.IsNotExist(e) {
			continue
		}
		return "", false, e, nil
	}
	// No prefix resolved through symlinks. The lexical path is the honest
	// answer: the probe reports the same errno git's child surfaces from the
	// missing path.
	return path, false, nil, nil
}

// resolveSymlinkTail walks the non-existent tail of a worktree parent path —
// the components after the longest existing prefix resolveSymlinkPrefixWalk
// resolved — the way the kernel's chdir resolves a relative worktree argument
// inside git, following symlinks at each existing component before `..`
// collapses the path and treating a genuinely missing component as creatable
// (git creates it and continues). A lexical filepath.Join would clean `..`
// first and skip a symlink the kernel follows, so a relative path like
// `missing/../link/../wt` (where `link` -> /mnt/dir) would collapse to
// `<repo>/wt` and probe an unrelated filesystem: git creates `missing`, backs
// out to the repo, follows `link` to /mnt/dir, backs out to /mnt, and creates
// `wt` there, so a full /mnt is the real ENOSPC git's write would hit but a
// lexical probe would never see it.
//
// Each component is resolved through the bounded fsprobe seams the prefix walk
// already uses, so a stalled mount surfaces the typed non-answer rather than
// wedging the synchronous restore poll, and the whole walk shares the prefix
// walk's context so the shared deadline still bounds it. A `..` component
// backs out of the current path the way chdir does — after a symlink is
// followed, `..` is the symlink target's parent, not the lexical parent of the
// link entry, and after a creatable component, `..` backs out the way git's
// chdir does once it has created the entry.
//
// A genuinely missing component (Lstat reports ENOENT for a non-symlink entry,
// or the prefix walk already reached this tail because EvalSymlinks reported
// ENOENT for the longest existing prefix) is creatable: git makes the
// directory and continues, so the walk advances past it without following a
// symlink (there is nothing to follow). A dangling symlink (Lstat reports a
// symlink whose EvalSymlinks returns ENOENT) is NOT creatable — git fails
// resolving it the way it fails a missing path, but the symlink entry itself
// is present, so collapsing the traversal past it would probe a filesystem git
// never reaches. The walk surfaces it as a resolution error so the caller
// preserves the structural failure rather than write-probing a guessed lexical
// destination.
// tailIsDir reports whether the resolved component the tail walk is about to
// back out of with `..` is a directory, using the bounded lstat flight the walk
// already uses for tail components. The kernel does not traverse `..` through a
// non-directory — `cd file/..` (or a symlink that resolved to a regular file
// followed by `..`) fails with ENOTDIR before writing anywhere — so the tail
// walk must not apply filepath.Dir to a non-directory `cur` and probe the
// unrelated filesystem it would back out to. A coincidental ENOSPC on that
// filesystem would otherwise be joined to a structural ENOTDIR git failure
// and stall the lost-restore loop indefinitely on a filesystem freeing
// it cannot fix. The bound and non-answer shape match the other tail lookups
// so a stalled mount surfaces the typed non-answer rather than wedging the
// synchronous restore poll.
func tailIsDir(path string) (bool, error) {
	info, err := lstatSpaceProbe(path)
	if err != nil {
		return false, err
	}
	return info.IsDir(), nil
}

// resolveSymlinkTail walks the non-existent tail of a worktree parent path —
// the components after the longest existing prefix resolveSymlinkPrefixWalk
// resolved — component by component the way the kernel's chdir resolves a
// relative worktree argument against the repoPath the git runner sets as
// cmd.Dir. The final component is preserved by the caller (filepath.Split) so
// the existing-destination check sees it un-resolved; this walk only resolves
// the parent the way chdir does, following symlinks at existing components
// before a `..` collapses them and treating a genuinely missing component as
// creatable the way git's safe_create_leading_directories does.
//
// It returns the resolved tail, whether the walk timed out, an error for a
// non-ENOENT failure the caller must join (a dangling symlink, ENOTDIR on a
// non-directory `..`, or a non-timeout lstat/eval error the caller preserves
// as environmental evidence), and the existing parent of every creatable
// component the walk advanced past — the directory git creates that component
// in. When a creatable component sits on a mounted symlink target a later
// `..` escapes (for example `link/missing/../../dest` with `link -> /mnt/x`,
// where git creates `missing` under `/mnt/x` before the final worktree lands
// under a different filesystem), the existing-parent walk above
// (filepath.Dir(worktreePath) upward) never reaches that mount: it ascends
// from the final destination's parent toward the root, but the creatable
// parent is a sibling branch the tail walk followed. Returning the creatable
// parents lets the caller write-probe each one so a full filesystem on the
// mount git actually wrote the missing directory into surfaces the resource
// errno the subprocess's opaque ExitError drops, instead of probing only the
// final parent and the common dir.
func resolveSymlinkTail(ctx context.Context, cur string, tailParts []string) (string, bool, error, []string) {
	// Track whether the current resolved component is a directory. The
	// kernel's chdir does not traverse `..` through a non-directory — a
	// symlink that resolved to a regular file (or a regular file component)
	// followed by `..` fails with ENOTDIR before writing there — so the tail
	// walk surfaces ENOTDIR instead of applying filepath.Dir and probing an
	// unrelated filesystem git never reaches. A coincidental ENOSPC on that
	// filesystem would otherwise be joined to a structural ENOTDIR git failure
	// and stall the lost-restore loop indefinitely on a filesystem freeing
	// it cannot fix.
	curIsDir, err := tailIsDir(cur)
	if err != nil {
		if errors.Is(err, fsprobe.ErrProbeTimedOut) {
			return "", true, err, nil
		}
		// A non-timeout lstat error (EACCES, EIO, ...) on the resolved prefix
		// is environmental evidence about the filesystem the probe is about to
		// write to; return it the way a non-ENOENT lstat on a tail component is
		// returned so the caller joins it rather than probing a guessed
		// lexical destination.
		return "", false, err, nil
	}
	var creatableParents []string
	for _, part := range tailParts {
		select {
		case <-ctx.Done():
			// The shared deadline fired — stop walking. The in-flight per-dir
			// flight drains on its own timeout, but no further component lookup
			// starts, so the goroutine does not accumulate metadata calls or
			// block waiting on each remaining component.
			return "", false, ctx.Err(), creatableParents
		default:
		}
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			if !curIsDir {
				// The current resolved component is a regular file (for
				// example, a symlink that resolved to a regular file), and the
				// kernel does not traverse `..` through a non-directory —
				// `git worktree add link/../wt` fails with "Not a directory"
				// before writing there. filepath.Dir would lexically back out
				// to the parent and the subsequent probe would target a
				// filesystem git never reaches, so a coincidental ENOSPC there
				// would stall the lost-restore loop indefinitely on a structural
				// ENOTDIR failure. Surface ENOTDIR the way the kernel does so
				// the caller probes only the independently known locations and
				// joins the structural error. The creatable parents the walk
				// already accumulated are still independently known locations
				// git wrote missing directories into before the unresolvable
				// component, so return them for the caller to probe alongside
				// the structural error.
				return "", false, &os.PathError{Op: "stat", Path: cur, Err: syscall.ENOTDIR}, creatableParents
			}
			cur = filepath.Dir(cur)
			curIsDir = true
			continue
		}
		next := filepath.Join(cur, part)
		if !curIsDir {
			// The current resolved component is not a directory (a symlink
			// that resolved to a regular file, or a regular file). The kernel
			// rejects descending into a child of a non-directory with ENOTDIR,
			// so a child lookup is not a creatable component — `git worktree
			// add file/child` fails before writing there. On Windows an Lstat
			// below a regular file returns ERROR_PATH_NOT_FOUND, which Go maps
			// to both syscall.ENOTDIR and os.IsNotExist, so the ENOENT branch
			// below would otherwise treat the child as creatable even though
			// the parent is known not to be a directory, marking `curIsDir`
			// true and probing an unrelated filesystem the kernel never
			// reaches. Surface ENOTDIR the way the `..` branch does so the
			// caller joins the structural error and probes only the
			// independently known locations; the creatable parents accumulated
			// before the non-directory are still independently known locations
			// git wrote missing directories into, so return them for the
			// caller to probe.
			return "", false, &os.PathError{Op: "stat", Path: next, Err: syscall.ENOTDIR}, creatableParents
		}
		info, err := lstatSpaceProbe(next)
		if err != nil {
			if errors.Is(err, fsprobe.ErrProbeTimedOut) {
				return "", true, err, creatableParents
			}
			if os.IsNotExist(err) {
				// A genuinely missing component — git creates it and
				// continues. There is nothing to follow, so the walk advances
				// without resolving a symlink; a creatable component is not a
				// dangling symlink, and the `..` after it backs out the way
				// chdir does once git has made the entry. Retain the existing
				// parent as a creatable prefix: git's
				// safe_create_leading_directories writes the missing entry
				// under it, so a full filesystem on that mount is the resource
				// errno the lost-restore classifier must read. When a later
				// `..` escapes a symlink target the existing-parent walk above
				// (filepath.Dir(worktreePath) upward) never reaches that mount,
				// so the caller write-probes the creatable parents this walk
				// retains the way it probes the final parent and the common
				// dir.
				creatableParents = append(creatableParents, cur)
				cur = next
				curIsDir = true
				continue
			}
			// A non-ENOENT, non-timeout lstat error (EACCES, EIO, ...) is
			// environmental evidence about the filesystem git's child is about
			// to write to. Return it so the caller joins it rather than
			// collapsing the tail lexically and probing an unrelated filesystem.
			// The creatable parents accumulated before the error are still
			// independently known locations git wrote missing directories into,
			// so return them for the caller to probe alongside the error.
			return "", false, err, creatableParents
		}
		if info.Mode()&os.ModeSymlink == 0 {
			// A regular component — continue through it the way chdir does.
			// A regular file is not a directory, so a `..` after it surfaces
			// ENOTDIR the way the kernel does rather than backing out
			// lexically.
			cur = next
			curIsDir = info.IsDir()
			continue
		}
		// A symlink. Resolve it the way the kernel's chdir does: EvalSymlinks
		// follows the target. A dangling symlink (the entry exists but its
		// target does not) is NOT a creatable component — git fails resolving
		// it, so collapsing the traversal past it would probe a filesystem git
		// never reaches and a coincidental ENOSPC there would stall the
		// lost-restore loop indefinitely on a structural failure. Distinguish
		// it from a genuinely absent component (Lstat reported ENOENT) and
		// surface the resolution error so the caller preserves the structural
		// failure rather than write-probing a guessed lexical destination.
		resolved, rErr := evalSymlinksSpaceProbe(next)
		if rErr != nil {
			if errors.Is(rErr, fsprobe.ErrProbeTimedOut) {
				return "", true, rErr, creatableParents
			}
			if os.IsNotExist(rErr) {
				// A dangling symlink makes the final destination unresolvable,
				// but the creatable parents the walk already accumulated (git
				// wrote the missing directories that precede the dangling link)
				// are still independently known locations the caller must probe —
				// a full filesystem on the mount git wrote them into is the
				// resource errno the lost-restore classifier reads even when a
				// later component fails. Return them so the caller write-probes
				// them alongside the structural error.
				return "", false, fmt.Errorf("dangling symlink %s: %w", next, rErr), creatableParents
			}
			// A non-ENOENT, non-timeout resolution error on a symlink is
			// environmental evidence about the filesystem the symlink's target
			// sits on; return it the way the prefix walk returns a non-ENOENT
			// EvalSymlinks error. The creatable parents accumulated before the
			// error are still independently known locations git wrote missing
			// directories into, so return them for the caller to probe.
			return "", false, rErr, creatableParents
		}
		// Track whether the resolved symlink target is a directory: a `..`
		// after a symlink to a regular file must surface ENOTDIR the way the
		// kernel does, rather than backing out lexically and probing the
		// target's parent filesystem.
		curIsDir, err = tailIsDir(resolved)
		if err != nil {
			if errors.Is(err, fsprobe.ErrProbeTimedOut) {
				return "", true, err, creatableParents
			}
			// A non-timeout lstat on the resolved target is environmental
			// evidence about the filesystem git's child is about to write to;
			// return it the way a non-ENOENT tail lstat is returned so the
			// caller joins it rather than probing a guessed lexical destination.
			// The creatable parents accumulated before the error are still
			// independently known locations git wrote missing directories into,
			// so return them for the caller to probe.
			return "", false, err, creatableParents
		}
		cur = resolved
	}
	return cur, false, nil, creatableParents
}

// hasPathTraversal reports whether path contains a `..` component that
// filepath.Dir/Join would lexically collapse, so the symlink-aware walk is
// needed to follow a symlinked component before `..` the way the kernel's
// chdir does. On Windows both `\` and `/` are valid separators.
func hasPathTraversal(path string) bool {
	p := strings.ReplaceAll(path, "/", string(filepath.Separator))
	for _, part := range strings.Split(p, string(filepath.Separator)) {
		if part == ".." {
			return true
		}
	}
	return false
}

// hasFinalSymlinkComponent reports whether an absolute path ends in a separator
// or a final `.` component, so the symlink-aware walk's trailing-separator and
// final-dot preservation must run the way it does for a relative path. Without
// it, a trailing separator makes os.Lstat follow a final symlink (a nonempty
// target looks like a regular directory rather than the symlink the
// existing-destination check must see un-resolved), and a final `.` collapses
// the same way after filepath.Split, so a nonempty symlinked target git rejects
// without writing would be write-probed and a coincidental ENOSPC on its
// filesystem would stall the lost-restore loop on a structural "already
// exists" rejection. `..` is covered by hasPathTraversal; a plain absolute path
// with no trailing separator and no final `.` returns false.
func hasFinalSymlinkComponent(path string) bool {
	if path == "" {
		return false
	}
	if strings.HasSuffix(path, string(filepath.Separator)) {
		return true
	}
	if filepath.Separator != '/' && strings.HasSuffix(path, "/") {
		return true
	}
	_, base := filepath.Split(path)
	return base == "."
}

// gitdirFromGitfile reads a `.git` gitfile (the file `git worktree add` writes
// at a worktree root pointing at the worktree's gitdir) and returns the gitdir
// path it names, resolved relative to the gitfile's directory. A non-gitfile
// `.git` entry (not starting with `gitdir:`) or an unreadable file returns ok
// = false. The caller uses this to distinguish a gitfile from a failed `git
// worktree add` (whose gitdir may no longer exist) from one a pre-existing
// registered worktree left behind — the latter is not partial-attempt debris.
//
// maxGitfileBytes caps how much of a `.git` gitfile BoundedReadFileLimit reads.
// A gitdir path is a single short line, so only the small prefix needed for
// parsing is read; a restored symlink destination containing a very large
// regular `.git` file (not a gitfile) cannot make the diagnostic consume
// gigabytes or OOM the daemon, since the deadline only limits how long the
// caller waits and does not cap the worker's allocation. 4 KiB comfortably
// holds a gitdir path on every platform Git supports (PATH_MAX is 4096 on
// Linux, the Windows MAX_PATH is 260, and macOS is 1024) while bounding the read.
const maxGitfileBytes = 4096

// The read runs through BoundedReadFileLimit so a `.git` entry on a stalled
// filesystem returns the bounded deadline's non-answer instead of blocking the
// synchronous lost-restore poll forever, and so a huge regular `.git` file is
// not read in full — only the bounded prefix a gitfile needs is read, with an
// explicit maximum size. A non-regular entry (FIFO, device, socket, or a `.git`
// symlink to a directory) is rejected by the caller before this runs, since the
// unbounded os.ReadFile it replaced would otherwise wedge on such an entry that
// a bounded Lstat accepts merely for being "not a directory". An unreadable
// entry returns the error so the caller preserves it as environmental evidence
// (a bounded read timeout carries the typed deadline the lost-restore
// classifier reads); a non-gitfile entry returns ok = false so the caller
// treats it as pre-existing content.
func gitdirFromGitfile(gitfile string) (string, bool, error) {
	b, err := BoundedReadFileLimit(gitfile, maxGitfileBytes)
	if err != nil {
		return "", false, err
	}
	// strings.TrimPrefix returns the original contents when the prefix is
	// absent, so a nonempty regular `.git` file that is not a gitfile (for
	// example an ordinary file such as README) would be accepted and its
	// contents parsed as a gitdir path. A symlinked nonempty destination
	// containing such a file would then be treated as failed-add debris
	// (the derived path usually stats as missing), turning a structural
	// "already exists" rejection into an indefinite environmental retry on a
	// coincidental ENOSPC. Require the `gitdir:` prefix the way an
	// unreadable or empty file is rejected, so a non-gitfile `.git` entry is
	// pre-existing content git rejected the add over.
	const gitdirPrefix = "gitdir:"
	if !strings.HasPrefix(string(b), gitdirPrefix) {
		return "", false, nil
	}
	dir := strings.TrimSpace(strings.TrimPrefix(string(b), gitdirPrefix))
	if dir == "" {
		return "", false, nil
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(filepath.Dir(gitfile), dir)
	}
	return dir, true, nil
}

// orphanedGitfileDebris stat's a regular `.git` file's referenced gitdir and
// returns a non-ENOENT stat error to the caller as environmental evidence. A
// regular `.git` file at a nonempty destination is not, on its own, partial-
// attempt debris from this add: `git worktree add` creates the gitdir before
// the gitfile, so an absent gitdir is a pruned linked worktree (its registration
// was deleted, not a gitfile this add created and then failed), and a present
// gitdir is a live registered worktree git rejected the add over — which could
// just as well predate this invocation (a reachable gitdir does not prove this
// failed add created the gitfile). Either way the destination already existed
// and git did not write to it, so a coincidental ENOSPC on its filesystem
// cannot fix the structural failure and the probe must not write-probe the
// destination — reachability of the referenced gitdir alone cannot establish
// that this invocation created the gitfile. A non-ENOENT stat error (EACCES,
// EIO, ErrProbeTimedOut) is environmental evidence about a live but
// temporarily inaccessible gitdir, not proof of an orphaned gitfile; the error
// is returned so the caller preserves it the way it preserves the destination
// and ancestor stat errors, rather than probing a registered worktree git
// rejected the add over. A bounded read error from the gitfile itself is
// likewise environmental evidence: it is returned so the caller preserves it
// rather than silently treating an unresponsive filesystem as a missing
// gitfile.
func orphanedGitfileDebris(gitFile string) (bool, error) {
	gitDir, ok, rErr := gitdirFromGitfile(gitFile)
	if rErr != nil {
		return false, rErr
	}
	if !ok {
		return false, nil
	}
	if _, dErr := statSpaceProbe(gitDir); os.IsNotExist(dErr) {
		// An absent gitdir is a pruned linked worktree (its registration was
		// deleted), not a gitfile this add created: `git worktree add` creates
		// the gitdir before the gitfile, so a present gitfile with an absent
		// gitdir predates this add. The destination already existed and git
		// rejected the add without writing, so it is not debris the probe
		// covers (a coincidental ENOSPC on its filesystem cannot fix the
		// structural failure).
		return false, nil
	} else if dErr != nil {
		return false, dErr
	}
	// A present gitdir is a live registered worktree git rejected the add over.
	// Reachability does not prove this failed invocation created the gitfile:
	// the worktree could predate this add (including one registered
	// concurrently after the setup cleanup), in which case git rejected the
	// existing destination without writing and a coincidental ENOSPC on its
	// filesystem must not turn that structural rejection into an indefinite
	// environmental retry. The probe must not write-probe the destination.
	return false, nil
}
