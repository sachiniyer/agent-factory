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

// writeSpaceProbe is the test seam over the shared bounded filesystem probe:
// a Go-side representative write (create + payload + Sync + close) that
// produces a REAL errno from this process, replacing the verdict a failing
// subprocess loses into its stderr text. The fsprobe implementation runs it
// inside a per-dir bounded flight — a filesystem that stops completing
// writes answers with typed deadline evidence instead of wedging the
// synchronous restore path that called it (#5295 review).
var writeSpaceProbe = fsprobe.Write

// statSpaceProbe is the test seam over fsprobe.Stat — the bounded metadata
// lookup that decides whether the worktree destination exists as a dir and
// gets probed itself. It must be bounded for the same reason the write
// probe is: the annotate path runs inside the synchronous restore poll, and
// an unbounded os.Stat on a mount that never answers would wedge the loop
// before any probe is reached (#5295 review). Its non-answer carries the
// fsprobe ErrProbeTimedOut + context.DeadlineExceeded pair, so the joined
// evidence classifies as the bounded environmental non-answer, not bare
// deadline ambiguity.
var statSpaceProbe = fsprobe.Stat

// lstatSpaceProbe is the test seam over fsprobe.Lstat — the bounded metadata
// lookup that reports the final component of the worktree destination itself,
// without following a symlink. statSpaceProbe (os.Stat) follows a final
// symlink to its target, so a destination whose final name is a symlink to an
// existing nonempty directory looks like a directory and the
// destination-directory probe below would write-probe the symlink's target;
// but `git worktree add` rejects a destination that already exists without
// writing through the symlink, so a coincidental ENOSPC on the target's
// filesystem would stall the lost-restore loop indefinitely on a filesystem
// freeing it cannot fix. The lstat sees the symlink itself so the probe can
// skip a symlinked destination the way it skips a missing one. It is bounded
// for the same reason statSpaceProbe is, and its non-answer carries the
// fsprobe ErrProbeTimedOut + context.DeadlineExceeded pair so a stalled mount
// classifies as the bounded environmental non-answer, not bare deadline
// ambiguity.
var lstatSpaceProbe = fsprobe.Lstat

// isEmptyDirSpaceProbe is the test seam over fsprobe.IsEmptyDir — the bounded
// directory-listing lookup that decides whether a symlinked destination's
// target already holds content. statSpaceProbe follows a final symlink and
// reports the target as a directory, but `git worktree add` populates an
// empty destination (even one reached through a symlink) while rejecting a
// nonempty one because it already exists; the lstat gate that skips a
// symlinked destination would otherwise drop the empty case — where git does
// write and whose filesystem can therefore fail with ENOSPC the lost-restore
// classifier must read as environmental. The read follows the symlink the way
// Stat does and counts the target's entries. It is bounded for the same
// reason statSpaceProbe is, and its non-answer carries the fsprobe
// ErrProbeTimedOut + context.DeadlineExceeded pair so a stalled mount
// classifies as the bounded environmental non-answer, not bare deadline
// ambiguity.
var isEmptyDirSpaceProbe = fsprobe.IsEmptyDir

// evalSymlinksSpaceProbe is the test seam over fsprobe.EvalSymlinks — the
// bounded symlink-resolution walk the worktree probe uses to follow symlinks
// in the destination the way the kernel's chdir does. It is bounded for the
// same reason statSpaceProbe is: filepath.EvalSymlinks performs an unbounded
// sequence of Readlink/Stat calls, and the annotate path runs inside the
// synchronous restore poll — an unbounded lookup on a stalled mount would
// wedge the loop before the probe is reached (#5295 review). Its non-answer
// carries the fsprobe ErrProbeTimedOut + context.DeadlineExceeded pair so the
// classifier reads the bounded environmental class, not bare deadline
// ambiguity.
var evalSymlinksSpaceProbe = fsprobe.EvalSymlinks

// ErrCommonDirProbeNoAnswer marks the bounded `rev-parse --git-common-dir`
// lookup's non-answer: runGitLocalCommand returns a bare
// context.DeadlineExceeded when its deadline trips, and the lost-restore
// classifier deliberately accepts deadlines ONLY paired with a recognized
// bounded-probe sentinel — a bare one reads as a hung session operation, not
// an outage. The lookup IS a bounded probe of the repository filesystem —
// a mount that cannot answer rev-parse is a mount stall — so its deadline is
// paired with this sentinel before it joins the annotated error (#5295
// review). A bare sentinel never appears: the pairing is applied only at
// the deadline.
var ErrCommonDirProbeNoAnswer = errors.New("common-dir probe did not answer inside its bound")

// gitCommonDir resolves the repository's Git common directory — the place
// `worktree add` writes its registration (worktrees/<name>/) and `commit`/
// `push` write objects — which may live on a different filesystem than the
// worktree itself (#5295 review). It is a test seam so a probe test can pin
// the second filesystem without a real repository layout.
//
// repoPath is resolved against the daemon launch cwd with the same rule the
// git runner applies: a restored record can carry a RELATIVE repoPath, and a
// relative --git-common-dir result joins onto the resolved repo dir — joining
// onto the un-resolved relative repoPath would leave the probe path relative
// to the daemon's post-chdir AF home and check an unrelated filesystem.
var gitCommonDir = func(g *GitWorktree) (string, error) {
	repoPath := g.repoPath
	if !filepath.IsAbs(repoPath) && daemonLaunchCwd != "" {
		repoPath = filepath.Join(daemonLaunchCwd, repoPath)
	}
	out, err := g.runGitLocalCommand(repoPath, "rev-parse", "--git-common-dir")
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			// The bounded local-git deadline carries no sentinel on its own —
			// pair it so the joined evidence reads as the bounded-probe
			// non-answer class, not a hung session operation (#5295 review).
			err = fmt.Errorf("%w: %w", ErrCommonDirProbeNoAnswer, err)
		}
		return "", err
	}
	dir := strings.TrimSpace(out)
	if dir == "" {
		return "", nil
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(repoPath, dir)
	}
	return dir, nil
}

// annotateGitWriteError re-verifies the environment behind a failed git
// subprocess: a Go-side write probe into each of dirs plus the repository's
// Git common directory. A subprocess failure loses its errno identity —
// ENOSPC arrives only as text on the *exec.ExitError's stderr, so a
// classifier that reads errors.Is through the chain (and never matches
// message text) cannot see disk exhaustion. The probe produces the evidence
// from THIS process instead of trusting git's words, and needs no locale
// assumptions.
//
// Both targets are probed because a worktree write spans them: `worktree add`
// creates the checkout under the worktree root AND registers it under the
// common dir's worktrees/; `add`/`commit` write the index and objects in the
// worktree's gitdir under the common dir. When the worktree parent and the
// common dir sit on different filesystems, probing only one misses a full
// other. If the common dir cannot be resolved at all, that failure is joined
// too — an unanswerable `rev-parse` is itself evidence about the
// environment (a fork that fails carries its resource errno).
//
// The joined error classifies environmental only when a probe reports a
// resource errno: a write, sync, or unlink to that filesystem just failed
// with it. When every probe succeeds the git error returns unchanged, so an
// ordinary failure on writable filesystems stays structural.
//
// False positive: a permanent non-space failure (bad ref, stale registration)
// while a covered filesystem is coincidentally full waits environmentally —
// but no write could have succeeded there anyway, and the first probe to
// succeed routes the same git failure back to the structural budget.
func (g *GitWorktree) annotateGitWriteError(err error, dirs ...string) error {
	if err == nil {
		return nil
	}
	joined := err
	probed := map[string]bool{}
	probe := func(dir string) {
		if dir == "" || probed[dir] {
			return
		}
		probed[dir] = true
		if probeErr := writeSpaceProbe(dir); probeErr != nil {
			joined = errors.Join(joined,
				fmt.Errorf("write probe of %s also failed: %w", dir, probeErr))
		}
	}
	for _, dir := range dirs {
		probe(dir)
	}
	common, cerr := gitCommonDir(g)
	if cerr != nil {
		joined = errors.Join(joined,
			fmt.Errorf("could not resolve the repository's common dir to probe it: %w", cerr))
	} else {
		probe(common)
	}
	return joined
}

// annotateWorktreeAddError is annotateGitWriteError for a failed `git
// worktree add` — the checkout writes under the worktree's parent directory.
// The #5295 play-test proved the gap this closes: `worktree add` was the
// first real write under the filled filesystem, every attempt charged the
// structural budget, and the lost session gave up while the disk stayed full.
//
// The destination path itself is probed too when it already exists as a
// directory — a pre-created mount point or the debris of a partial attempt
// can sit on a different filesystem than the parent that hosts its name.
// Probing inside it is safe for the same reason as the push path: the probe
// unlinks its file before writing, so nothing stageable ever exists. A
// destination whose final component is a symlink to an existing directory is
// probed only when the target is empty: `git worktree add` populates an empty
// destination (even through a symlink) but rejects a nonempty one because it
// already exists, so probing a nonempty symlink target's filesystem would
// target an unrelated filesystem and a coincidental ENOSPC there would stall
// the lost-restore loop indefinitely on a filesystem freeing it cannot fix.
// The lstat check below splits the two cases, and the empty-dir read that
// follows it counts the target's entries. A FAILED stat, lstat, or read-dir
// on the destination is evidence in its own right rather than a skip.
//
// A FAILED stat on the destination is evidence in its own right, not a skip:
// on a dead mount it returns environmental errnos such as ETIMEDOUT that the
// git subprocess only ever surfaced as an opaque ExitError, and discarding it
// would charge mount stalls to the structural budget. The stat itself runs
// bounded — on a mount that never answers at all, an unbounded lookup would
// wedge the restore poll before the probe is reached, and its typed timeout
// is evidence too. ENOENT is the normal pre-creation answer and stays
// silent; anything else joins the chain.
func (g *GitWorktree) annotateWorktreeAddError(err error) error {
	// Resolve the probe destination against the same resolved repo dir the git
	// runner hands its child, mirroring gitCommonDir's daemonLaunchCwd rule for
	// repoPath. The runner joins a relative `-C repoPath` onto daemonLaunchCwd
	// and sets cmd.Dir to that joined dir, so a relative worktreePath argument
	// resolves inside git against that resolved repo dir. Probing the raw
	// worktreePath would resolve against the daemon's post-chdir cwd (the AF
	// home) and verdict an unrelated filesystem — the #5295-review hazard
	// gitCommonDir already documents. Production paths are always absolute, so
	// this only matters for a hand-edited or externally-authored relative
	// record in storage — the input class gitCommonDir, the runner, and
	// bounded removal all defend against.
	repoPath := g.repoPath
	if !filepath.IsAbs(repoPath) && daemonLaunchCwd != "" {
		repoPath = filepath.Join(daemonLaunchCwd, repoPath)
	}
	// Resolve symlinks in the probe path the way the kernel's chdir resolves
	// the worktree argument inside git. The git runner sets cmd.Dir = repoPath,
	// so a relative worktree argument resolves inside git against the
	// resolved (real) repo dir — `git -C /launch/repo worktree add ../wt` with
	// /launch/repo -> /mnt/repo creates /mnt/wt, not /launch/wt — and a
	// worktree argument like `link/../wt` (where `<repo>/link` is a symlink)
	// follows `link` before collapsing `..`, creating /mnt/wt rather than the
	// lexical `<repo>/wt`. filepath.Join only lexically cleans `..`, so a
	// raw join would probe an unrelated filesystem and misclassify ENOSPC or
	// mount failures on the real one. The resolution walks the un-joined path
	// component by component, following symlinks at each existing prefix; the
	// destination git is about to create may not exist yet, so the longest
	// existing prefix is resolved and the remaining non-existent tail is walked
	// component by component the same way, following symlinks the kernel's
	// chdir would follow after creatable components rather than rejoining it
	// lexically.
	//
	// The lookup runs through the bounded fsprobe.EvalSymlinks, the same flight
	// shape statSpaceProbe and gitCommonDir's deadline already defend: an
	// unbounded filepath.EvalSymlinks on a stalled network/FUSE mount would
	// wedge the synchronous restore poll the probe informs. A bounded
	// non-answer joins the chain as typed environmental evidence the way a
	// stat-probe timeout does, instead of silently falling back to the lexical
	// path; a non-ENOENT error on an existing prefix walks up to the next
	// prefix, and if no prefix resolves the lexical join stands (the probe
	// then reports the same errno git's child surfaces from the missing path).
	// A dangling symlink in the tail (a symlink whose target does not exist)
	// is surfaced as a resolution error rather than collapsed past, so the
	// caller does not probe a filesystem git never reaches.
	//
	// The un-joined concatenation preserves `..` in the relative worktreePath
	// so the walk traverses a symlinked component before collapsing it the way
	// git's chdir does; a joined path would lexically clean `..` first and the
	// symlink would never be followed.
	//
	// Only the parent traversal is resolved, and the final component is
	// preserved as the existing-destination check sees it. When the final
	// component is a symlink to a regular file or a nonempty directory, git
	// rejects the add because the destination already exists without writing to
	// the symlink target's parent; resolving the whole destination would make
	// filepath.Dir below point at that unrelated parent, and a full filesystem
	// there would add ENOSPC the lost-restore loop waits on indefinitely even
	// though freeing it cannot fix the structural failure. Resolving the parent
	// and rejoining the final name keeps filepath.Dir on the directory that
	// hosts the destination's name — the filesystem git actually operates in —
	// while the symlinked components the kernel's chdir walks through before the
	// final name are still followed.
	//
	// The split uses filepath.Split rather than filepath.Dir so the parent keeps
	// its `..` for the bounded walk: a relative worktreePath like `link/../wt`
	// resolves `link` before `..` collapses the way git's chdir does, and
	// filepath.Dir would clean the `..` away and lose the symlinked component.
	worktreePath := g.worktreePath
	var creatableParents []string
	if !filepath.IsAbs(worktreePath) {
		worktreePath = repoPath + string(filepath.Separator) + g.worktreePath
		// Trim trailing separators (but preserve a filesystem root) so a
		// trailing slash on the destination does not fold the final
		// component into the parent resolution: filepath.Split("/repo/dest/")
		// returns an empty base, which would make the "parent-only"
		// resolution follow a final symlink the existing-destination check
		// must see un-resolved. A `dest/` whose `dest` is a symlink to a
		// nonempty directory makes git reject the add because the destination
		// already exists without writing to the symlink target's parent;
		// resolving it would let a coincidental ENOSPC there stall the
		// lost-restore loop indefinitely. The guard keeps at least the
		// volume name plus one separator so a root path is not turned into
		// a relative one.
		vol := filepath.VolumeName(worktreePath)
		for len(worktreePath) > len(vol)+1 && strings.HasSuffix(worktreePath, string(filepath.Separator)) {
			worktreePath = strings.TrimSuffix(worktreePath, string(filepath.Separator))
		}
		parent, base := filepath.Split(worktreePath)
		// Trim the parent's trailing separator for the prefix walk, but
		// preserve a filesystem root: when repoPath resolves to "/"
		// (Unix) or "C:\" (Windows) and worktreePath is a relative name,
		// Split returns a root parent ("//") and TrimRight would empty it
		// (Unix) or drop the separator from a volume root (Windows, "C:\"
		// -> "C:", the current directory on that drive rather than its
		// root), making the join relative and probing the daemon's cwd
		// instead of the root Git writes into.
		parent = strings.TrimRight(parent, string(filepath.Separator))
		if parent == "" || parent == vol {
			parent = vol + string(filepath.Separator)
		}
		resolvedParent, timedOut, resolveErr, cp := resolveSymlinkPrefixBounded(parent)
		creatableParents = cp
		if timedOut {
			// A bounded non-answer from the symlink resolution is evidence in
			// its own right: the mount the probe is about to write to did not
			// answer a metadata lookup, so the git subprocess's failure
			// classifies environmental rather than structural, the same way a
			// stat-probe non-answer does. The actual destination is unknown after
			// a timeout — the resolution did not complete, so the lexical
			// filepath.Join of the un-resolved tail is a guess at a filesystem
			// git may never touch: for a relative path containing traversal such
			// as `link/../../wt`, git may have failed before reaching that
			// destination (for example, while validating a branch), and a
			// coincidental ENOSPC on the unrelated lexical filesystem would be
			// added to the timeout chain. Because lostRestoreEnvironmentalClass
			// checks resource errnos before the bounded-deadline signature, that
			// would convert a capped timeout retry into an indefinite errno wait.
			// Preserve the timeout evidence without probing a guessed lexical
			// destination, while still probing independently known locations
			// such as the common directory, and join the typed non-answer so the
			// lost-restore classifier reads the bounded environmental class,
			// not bare structural silence.
			lexical := filepath.Join(repoPath, g.worktreePath)
			out := g.annotateGitWriteError(err)
			return errors.Join(out, fmt.Errorf("symlink resolution of %s: %w", lexical, resolveErr))
		}
		if resolveErr == nil {
			// A final `..` or `.` held in base never reaches resolveSymlinkTail's
			// non-directory check: filepath.Join collapses resolvedParent/.. to
			// its lexical parent (or resolvedParent/. to resolvedParent itself),
			// but the kernel does not traverse `..` or `.` through a
			// non-directory — `cd file/..` and `cd file/.` (or a symlink that
			// resolved to a regular file followed by `..` or `.`) fail with
			// ENOTDIR before writing anywhere. When resolvedParent is a symlink
			// that resolved to a regular file (or a regular file component),
			// filepath.Join would back out to the parent (or keep the file) and
			// the subsequent probe would target a filesystem git never reaches,
			// so a coincidental ENOSPC there would stall the lost-restore loop
			// indefinitely on a structural ENOTDIR failure. Surface ENOTDIR the
			// way the tail walk does so the caller probes only the independently
			// known locations and joins the structural error.
			if base == ".." || base == "." {
				isDir, dErr := tailIsDir(resolvedParent)
				if dErr != nil {
					lexical := filepath.Join(repoPath, g.worktreePath)
					out := g.annotateGitWriteError(err)
					return errors.Join(out, fmt.Errorf("symlink resolution of %s: %w", lexical, dErr))
				}
				if !isDir {
					lexical := filepath.Join(repoPath, g.worktreePath)
					out := g.annotateGitWriteError(err)
					return errors.Join(out, fmt.Errorf("symlink resolution of %s: %w", lexical, &os.PathError{Op: "stat", Path: resolvedParent, Err: syscall.ENOTDIR}))
				}
			}
			worktreePath = filepath.Join(resolvedParent, base)
		} else {
			// A non-timeout resolution error is environmental evidence about the
			// filesystem the probe is about to write to, not a missing path. It
			// covers two shapes: an environmental errno (EACCES, EIO, the
			// bounded ErrProbeTimedOut) returned by a prefix or tail lookup on
			// an existing component, and a dangling-symlink ENOENT the tail
			// walk surfaces (a symlink whose target does not exist — git fails
			// resolving it the way it fails a missing path, but the symlink
			// entry itself is present). In both shapes the actual destination
			// is unknown — the resolution did not follow the path the kernel's
			// chdir does — so a lexical filepath.Join of the un-resolved tail
			// is a guess at a filesystem git may never touch. For a traversal
			// such as `link/../../wt` with a dangling `link`, the lexical join
			// collapses the symlink traversal to the repo's parent and a
			// coincidental ENOSPC there would stall the lost-restore loop
			// indefinitely on a structural dangling-link failure; for a
			// symlinked component the tail walk could not follow, the same
			// hazard applies. Rejoining the tail lexically would also remove a
			// symlinked component like `link/..` and let the subsequent
			// write/stat probes all succeed, misclassifying the original git
			// failure as structural. Probe only the independently known
			// locations (the common directory) and join the resolution error so
			// the lost-restore classifier reads it alongside the git failure
			// rather than dropping it, without write-probing a guessed lexical
			// destination.
			lexical := filepath.Join(repoPath, g.worktreePath)
			out := g.annotateGitWriteError(err)
			return errors.Join(out, fmt.Errorf("symlink resolution of %s: %w", lexical, resolveErr))
		}
	}
	dirs := []string{filepath.Dir(worktreePath)}
	// When the destination's parent does not exist yet (a nested relative
	// worktreePath such as wt/dest, where git creates the missing leading
	// directory via safe_create_leading_directories), a write probe of the
	// parent returns ENOENT rather than the ENOSPC git can encounter creating
	// it. Walk up to the nearest existing ancestor and probe it too, so a
	// full filesystem on the ancestor that git writes the missing parent into
	// surfaces the resource errno the subprocess's opaque ExitError drops. The
	// common dir is probed separately below, but a common dir on another
	// writable filesystem leaves this ancestor as the only probe that can
	// catch the ENOSPC.
	//
	// Bound the complete ancestor search the way resolveSymlinkPrefixBounded
	// bounds the prefix walk: each statSpaceProbe already runs in its own
	// per-dir bounded flight, but without a shared budget a hand-edited
	// relative path with many components whose missing-path stats each return
	// ENOENT just before the per-call timeout could block the synchronous
	// restore poll for roughly components × probeTimeout. Stop walking once
	// the shared budget elapses; the in-flight stat drains on its own per-dir
	// timeout, and the common-dir probe below still covers the independently
	// known location.
	ancestorDeadline := time.Now().Add(symlinkResolveTimeout)
	parentDir := filepath.Dir(worktreePath)
	for {
		if time.Now().After(ancestorDeadline) {
			break
		}
		if st, sErr := statSpaceProbe(parentDir); sErr == nil && st != nil {
			if parentDir != filepath.Dir(worktreePath) {
				dirs = append(dirs, parentDir)
			}
			break
		} else if !os.IsNotExist(sErr) {
			// A non-ENOENT stat error on the ancestor is environmental evidence
			// about the filesystem the missing parent would be created on; the
			// write probe of the parent already in dirs surfaces the same kind
			// of error, and the common dir probe below covers the rest, so stop
			// walking and let the existing probes handle it.
			break
		}
		next := filepath.Dir(parentDir)
		if next == parentDir {
			break
		}
		parentDir = next
	}
	// Probe the existing parent of every creatable component the tail walk
	// advanced past, so a full filesystem on the mount git actually wrote a
	// missing directory into surfaces the resource errno the subprocess's
	// opaque ExitError drops. When a creatable component sits on a mounted
	// symlink target a later `..` escapes (for example `link/missing/../../dest`
	// with `link -> /mnt/x`, where git creates `missing` under `/mnt/x` before
	// the final worktree lands under a different filesystem), the
	// existing-parent walk above (filepath.Dir(worktreePath) upward) never
	// reaches that mount: it ascends from the final destination's parent
	// toward the root, but the creatable parent is a sibling branch the tail
	// walk followed. The tail walk retains each such parent so the probe
	// covers the filesystem git wrote the missing directory into, not just the
	// final parent and the common dir. annotateGitWriteError dedups via its
	// probed map, so a creatable parent that coincides with an already-probed
	// dir is probed once.
	for _, cp := range creatableParents {
		dirs = append(dirs, cp)
	}
	var statErr error
	var lstatErr error
	var readDirErr error
	if st, sErr := statSpaceProbe(worktreePath); sErr == nil && st != nil && st.IsDir() {
		// Stat follows a final symlink, so a destination whose final name is a
		// symlink to an existing directory looks like a directory here. `git
		// worktree add` rejects a destination that already exists when the
		// target is nonempty (writing through the symlink into a nonempty
		// directory) but populates an empty one — even through a symlink, an
		// empty directory is a usable destination git writes into, so a full
		// filesystem there is evidence the lost-restore classifier must read
		// as environmental. Lstat the final component the way the
		// existing-destination check sees it (the un-resolved name) so the
		// probe can split the two cases: a regular (non-symlink) directory is
		// always probed in its own right (a pre-created mount point or
		// partial-attempt debris can sit on a different filesystem than the
		// parent that hosts its name), a symlinked destination whose target is
		// empty is probed (git writes there), and a symlinked destination whose
		// target already holds content is skipped the way a missing one is —
		// freeing that filesystem cannot fix a structural "already exists"
		// rejection. A non-ENOENT lstat error (EIO, ETIMEDOUT, the bounded
		// ErrProbeTimedOut) is environmental evidence about the filesystem
		// the probe is about to write to, not a missing path: a missing
		// destination is the normal pre-creation answer and stays silent, but
		// discarding any other lstat error would leave the opaque git failure
		// with no environmental evidence when the parent/common-directory
		// write probes succeed, charging the structural budget for a mount the
		// lstat did not answer. The same is true of a non-ENOENT read-dir error
		// on a symlinked destination whose target the open could not list.
		if lst, lErr := lstatSpaceProbe(worktreePath); lErr == nil {
			if lst.Mode()&os.ModeSymlink == 0 {
				dirs = append(dirs, worktreePath)
			} else if empty, dErr := isEmptyDirSpaceProbe(worktreePath); dErr == nil {
				// The symlink target is empty — git populates it, so probe the
				// destination the way a regular directory is probed. A
				// nonempty target is skipped: git rejects the add without
				// writing, so a coincidental ENOSPC on the target's filesystem
				// would stall the lost-restore loop indefinitely on a
				// filesystem freeing it cannot fix.
				if empty {
					dirs = append(dirs, worktreePath)
				}
			} else if !os.IsNotExist(dErr) {
				readDirErr = dErr
			}
		} else if !os.IsNotExist(lErr) {
			lstatErr = lErr
		}
	} else if sErr != nil && !os.IsNotExist(sErr) {
		statErr = sErr
	}
	out := g.annotateGitWriteError(err, dirs...)
	if statErr != nil {
		out = errors.Join(out, fmt.Errorf("stat probe destination %s: %w", worktreePath, statErr))
	}
	if lstatErr != nil {
		out = errors.Join(out, fmt.Errorf("lstat probe destination %s: %w", worktreePath, lstatErr))
	}
	if readDirErr != nil {
		out = errors.Join(out, fmt.Errorf("readdir probe destination %s: %w", worktreePath, readDirErr))
	}
	return out
}

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
		return "", true, fmt.Errorf("symlink prefix walk of %s did not answer within %s: %w: %w",
			path, symlinkResolveTimeout, fsprobe.ErrProbeTimedOut, context.DeadlineExceeded), nil
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
// filesystem would be joined to a structural ENOTDIR git failure and stall the
// lost-restore loop indefinitely on a filesystem freeing it cannot fix. The
// bound and non-answer shape match the other tail lookups so a stalled mount
// surfaces the typed non-answer rather than wedging the synchronous restore
// poll.
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
			return "", false, ctx.Err(), nil
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
				// joins the structural error.
				return "", false, &os.PathError{Op: "stat", Path: cur, Err: syscall.ENOTDIR}, nil
			}
			cur = filepath.Dir(cur)
			curIsDir = true
			continue
		}
		next := filepath.Join(cur, part)
		info, err := lstatSpaceProbe(next)
		if err != nil {
			if errors.Is(err, fsprobe.ErrProbeTimedOut) {
				return "", true, err, nil
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
			return "", false, err, nil
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
				return "", true, rErr, nil
			}
			if os.IsNotExist(rErr) {
				return "", false, fmt.Errorf("dangling symlink %s: %w", next, rErr), nil
			}
			// A non-ENOENT, non-timeout resolution error on a symlink is
			// environmental evidence about the filesystem the symlink's target
			// sits on; return it the way the prefix walk returns a non-ENOENT
			// EvalSymlinks error.
			return "", false, rErr, nil
		}
		// Track whether the resolved symlink target is a directory: a `..`
		// after a symlink to a regular file must surface ENOTDIR the way the
		// kernel does, rather than backing out lexically and probing the
		// target's parent filesystem.
		curIsDir, err = tailIsDir(resolved)
		if err != nil {
			if errors.Is(err, fsprobe.ErrProbeTimedOut) {
				return "", true, err, nil
			}
			// A non-timeout lstat on the resolved target is environmental
			// evidence about the filesystem git's child is about to write to;
			// return it the way a non-ENOENT tail lstat is returned so the
			// caller joins it rather than probing a guessed lexical destination.
			return "", false, err, nil
		}
		cur = resolved
	}
	return cur, false, nil, creatableParents
}
