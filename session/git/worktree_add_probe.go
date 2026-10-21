package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
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

// writeProbeConcurrency bounds the number of write probes annotateGitWriteError
// runs concurrently. A hand-edited relative worktree path can accumulate many
// distinct creatable parents (repeated `dirN/missing/../../` segments, each
// retained by the tail walk), and starting one goroutine and one 1 MiB write
// probe per parent simultaneously would let all of them target the same mount
// at once — temporarily consuming hundreds of MiB, exhausting file
// descriptors, or leaving hundreds of underlying fsprobe workers stuck after
// their callers time out; the probes could even manufacture ENOSPC on a
// filesystem a structural git failure does not actually write to and stall the
// lost-restore loop on a coincidental resource errno. The per-dir fsprobe
// flights already deduplicate probes of the same path, but a bound on the
// simultaneous fan-out also caps the resource use across distinct paths on the
// same mount while retaining the cross-filesystem concurrency a single
// timed-out probe on one mount does not consume the budget a probe on a
// separate mount needs (writeProbeConcurrency is sized above one so a stalled
// probe on one filesystem does not block a probe on another).
const writeProbeConcurrency = 8

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
	// Each writeSpaceProbe runs in its own per-dir bounded flight, so a
	// hand-edited relative path with many accumulated creatable parents (the
	// tail walker's output) would otherwise block the synchronous restore poll
	// for roughly number of parents × probeTimeout when the metadata lookups
	// answer but writes stall — the degraded-filesystem case these bounds exist
	// to contain. Run the probes concurrently so a stalled write on one
	// filesystem does not consume the whole budget and skip every remaining
	// parent: dirs can contain parents on different filesystems, and a
	// timed-out final parent can precede a creatable parent on a separate full
	// mount whose ENOSPC the lost-restore classifier must read. A sequential
	// loop with a shared deadline could not advance past the first timed-out
	// probe because a single writeSpaceProbe consumes its full probeTimeout
	// (the same duration as symlinkResolveTimeout), so the deadline elapsed
	// before the next parent on a different filesystem was reached. Running
	// the probes concurrently keeps the total within a single probeTimeout
	// rather than the N × probeTimeout a sequential loop would take on a
	// degraded filesystem, and each distinct filesystem gets its own probe
	// within the same bound. The common-dir probe below is a single bounded
	// flight on an independently known location, so it still runs after the
	// concurrent probes on its own per-dir bound. The fan-out is capped by
	// writeProbeConcurrency so a hand-edited path with hundreds of creatable
	// parents cannot start hundreds of simultaneous 1 MiB probes on the same
	// mount (which would manufacture ENOSPC on a filesystem a structural git
	// failure does not write to, or exhaust file descriptors), while still
	// letting probes on distinct filesystems run in parallel so a timed-out
	// probe on one mount does not consume the budget a probe on a separate
	// mount needs.
	// Bound the aggregate wait across semaphore batches the way
	// resolveSymlinkPrefixBounded and the existing-ancestor search bound their
	// walks: acquiring the writeProbeConcurrency semaphore synchronously
	// serializes the probes into batches of writeProbeConcurrency, so on a
	// degraded filesystem where each writeSpaceProbe consumes its full per-dir
	// bound a hand-edited path with many distinct creatable parents would
	// otherwise block for roughly ceil(len(dirs)/writeProbeConcurrency) ×
	// probeTimeout on every retry. The semaphore bounds the fan-out at all
	// times — a hand-edited path with many creatable parents on a stalled mount
	// followed by hundreds of later parents on the same responsive filesystem
	// would otherwise start hundreds of concurrent 1 MiB writes once the first
	// batch exhausts the semaphore and a later deadline fired, manufacturing
	// ENOSPC on a filesystem a structural git failure does not write to or
	// exhausting file descriptors; acquiring the semaphore for every probe (the
	// first batch holds it until its probes drain on their per-dir bound, after
	// which the remaining probes acquire it in batches of writeProbeConcurrency)
	// keeps the fan-out within the bound the steady state caps.
	//
	// Cross-filesystem coverage is preserved: every dir is still probed (a
	// later parent on a separate ENOSPC mount is not discarded by input order),
	// the bound only serializes the probes into batches. The aggregate wait is
	// ceil(len(dirs)/writeProbeConcurrency) × probeTimeout on a degraded
	// filesystem, which is the bound the semaphore defends against in the
	// steady state; the per-dir bound caps each probe, and a later parent's
	// ENOSPC still reaches the chain once its batch acquires the semaphore.
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, writeProbeConcurrency)
	for _, dir := range dirs {
		if dir == "" || probed[dir] {
			continue
		}
		probed[dir] = true
		wg.Add(1)
		sem <- struct{}{}
		go func(dir string) {
			defer wg.Done()
			defer func() { <-sem }()
			if probeErr := writeSpaceProbe(dir); probeErr != nil {
				mu.Lock()
				joined = errors.Join(joined,
					fmt.Errorf("write probe of %s also failed: %w", dir, probeErr))
				mu.Unlock()
			}
		}(dir)
	}
	wg.Wait()
	common, cerr := gitCommonDir(g)
	if cerr != nil {
		joined = errors.Join(joined,
			fmt.Errorf("could not resolve the repository's common dir to probe it: %w", cerr))
	} else if common != "" && !probed[common] {
		probed[common] = true
		if probeErr := writeSpaceProbe(common); probeErr != nil {
			joined = errors.Join(joined,
				fmt.Errorf("write probe of %s also failed: %w", common, probeErr))
		}
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
//
// isWindowsDriveRooted reports whether path is rooted at the current drive
// without a volume name (for example `\wt` or `/wt`). On Windows
// filepath.IsAbs returns false for such a path because it has no volume, but
// the git child resolves it against the current drive rather than the
// repository path, so the probe must not rebase it onto repoPath the way a
// genuinely relative path is rebased — a relative destination probes the
// resolved repo dir while a drive-rooted destination probes the current
// drive's root. On Unix a leading separator makes the path absolute (caught
// by IsAbs) and a leading `\` is a valid filename character, so this only
// matches the Windows current-drive-rooted case.
func isWindowsDriveRooted(path string) bool {
	if runtime.GOOS != "windows" {
		return false
	}
	if filepath.VolumeName(path) != "" {
		// A path with a volume is either absolute (C:\wt, \\server\share) or
		// drive-relative (D:wt), neither of which is rooted at the *current*
		// drive. Returning IsAbs here classified every absolute destination
		// such as C:\wt as current-drive-rooted, so the caller prefixed the
		// repository volume and turned it into an invalid path like D:C:\wt
		// (#5421 review); a drive-relative path is handled by its own branch
		// below. Paths that already carry a volume are not current-drive-rooted
		// and must not be rebased onto the repository volume here.
		return false
	}
	return len(path) > 0 && (path[0] == '\\' || path[0] == '/')
}

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
	// relBase is the relative worktree path the symlink-aware walk rebases
	// onto repoPath. The plain-relative branch uses g.worktreePath directly; a
	// same-drive drive-relative Windows path (D:link\..\wt) carries a volume
	// prefix the rebase must strip (the lexical fallbacks the walk joins into
	// the annotation use relBase so a D: prefix does not appear as a path
	// component).
	relBase := g.worktreePath
	var creatableParents []string
	resolveRelative := false
	if isWindowsDriveRooted(worktreePath) {
		// A Windows current-drive-rooted path (\wt or /wt) resolves against
		// the current drive. The git child runs with cmd.Dir = repoPath (e.g.
		// D:\repo), so it resolves \wt against the repo's drive (D:\wt), while
		// the parent process's Stat and write probes resolve the unchanged \wt
		// against the daemon's current drive (e.g. C:\wt). Qualify it with the
		// resolved repo's volume so the probes target the same drive git's
		// child writes to, instead of the daemon's current drive which the
		// unchanged path would resolve against.
		worktreePath = filepath.VolumeName(repoPath) + worktreePath
	} else if runtime.GOOS == "windows" && filepath.VolumeName(worktreePath) != "" && !filepath.IsAbs(worktreePath) {
		// A Windows drive-relative path such as D:wt has a volume but is not
		// absolute: the git child running with cmd.Dir = repoPath (D:\repo)
		// resolves D:wt to D:\repo\wt on that drive, but a raw concatenation
		// beneath repoPath would probe D:\repo\D:wt — an invalid path whose
		// probes miss the filesystem git actually wrote to, so a full
		// filesystem on D: would be charged to the structural retry budget
		// instead of surfacing ENOSPC (#5421 review). Rebase the relative part
		// onto the repo's directory on the same drive the way git's chdir does.
		// A path whose drive differs from the repo's resolves against the
		// daemon's cwd on that drive, which the parent process cannot observe,
		// so it is left unchanged the same way an un-rebased absolute path is.
		// Windows drive letters are case-insensitive, so compare the
		// volumes with strings.EqualFold: a worktreePath of `d:wt` against a
		// repoPath of `D:\repo` is the same drive, and a case-mismatched
		// spelling must not skip the rebase (the git child resolves `d:wt`
		// against its `D:\repo` working directory, while the parent would
		// otherwise probe the daemon's drive-relative `d:wt`, which may refer
		// to a different directory and filesystem; ENOSPC can consequently be
		// missed or attached spuriously).
		if vol := filepath.VolumeName(worktreePath); strings.EqualFold(vol, filepath.VolumeName(repoPath)) {
			rest := strings.TrimPrefix(worktreePath, vol)
			if strings.ContainsAny(rest, "/\\") {
				// A same-drive path with a separator (for example
				// `D:link\..\wt` or `D:link/sub`) carries traversal the
				// kernel's chdir resolves component by component: the git
				// child running with cmd.Dir = repoPath (`D:\repo`) resolves
				// `D:link\..\wt` to `D:\repo\link\..\wt`, following `link`
				// before `..` collapses it. filepath.Join would lexically
				// collapse it to `D:\repo\wt`, after which it never reaches
				// the symlink-aware walk, so a symlinked `link` targeting
				// another filesystem would leave the probe on the repository
				// filesystem and miss/attach ENOSPC. Rebase onto the repo's
				// directory on the same drive without Joining the `..` away,
				// and run the symlink-aware walk the way a plain relative path
				// does. The lexical fallbacks the walk joins into the
				// annotation use relBase (the volume-stripped path) so a `D:`
				// prefix does not appear as a path component.
				relBase = rest
				resolveRelative = true
			} else {
				worktreePath = filepath.Join(repoPath, rest)
			}
		}
	} else if !filepath.IsAbs(worktreePath) {
		resolveRelative = true
	}
	if resolveRelative {
		worktreePath = repoPath + string(filepath.Separator) + relBase
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
		// On Windows both \ and / are valid path separators, but the trim
		// above recognized only filepath.Separator (\). A restored path
		// written with forward slashes (for example dest/) left the trailing
		// / in place: filepath.Split recognizes it on Windows and returns an
		// empty base, folding the final component into the parent resolution
		// the way a trailing \ does. Trim both separators so the final
		// component is preserved the same way the non-trailing-slash case
		// already defends.
		seps := string(filepath.Separator)
		if seps != "/" {
			seps += "/"
		}
		for len(worktreePath) > len(vol)+1 && strings.ContainsRune(seps, rune(worktreePath[len(worktreePath)-1])) {
			worktreePath = worktreePath[:len(worktreePath)-1]
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
		// Trim both Windows path separators from the parent the way the
		// trailing-destination trim above trims both: on Windows a restored
		// `worktreePath` of `link/.` leaves `parent` ending in `/` when the
		// trim removes only `\`, and the final-dot branch below preserves the
		// un-resolved parent as the probe target. `os.Lstat` follows a final
		// symlink when the path ends in a separator, so a symlink to a nonempty
		// directory would be seen as its target and write-probed — git rejects
		// `link/.` without writing through the symlink, so a coincidental ENOSPC
		// on the target's filesystem would stall the lost-restore loop on a
		// structural "already exists" failure. Trimming both separators keeps the
		// final component the way the non-trailing-slash case does.
		parent = strings.TrimRight(parent, seps)
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
			// not bare structural silence. The creatable parents the tail walk
			// accumulated before the timeout are independently known locations
			// git wrote missing directories into, so probe them too rather than
			// dropping the resource errno the classifier must read.
			lexical := filepath.Join(repoPath, relBase)
			out := g.annotateGitWriteError(err, creatableParents...)
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
					lexical := filepath.Join(repoPath, relBase)
					out := g.annotateGitWriteError(err, creatableParents...)
					return errors.Join(out, fmt.Errorf("symlink resolution of %s: %w", lexical, dErr))
				}
				if !isDir {
					lexical := filepath.Join(repoPath, relBase)
					out := g.annotateGitWriteError(err, creatableParents...)
					return errors.Join(out, fmt.Errorf("symlink resolution of %s: %w", lexical, &os.PathError{Op: "stat", Path: resolvedParent, Err: syscall.ENOTDIR}))
				}
				if base == "." {
					// Preserve a final symlink before the dot: `link/.` with
					// `link` targeting an existing nonempty directory is rejected
					// by `git worktree add` because the destination already
					// exists, without writing through the symlink.
					// filepath.Join(resolvedParent, ".") collapses the dot after
					// resolving `link`, so the later Lstat would see the target
					// as an ordinary directory and the destination-directory
					// probe below would unconditionally write-probe it (the lstat
					// gate skips only a final symlink, not the target it resolved
					// to); a coincidental ENOSPC on the target's filesystem
					// would turn a structural "already exists" failure into an
					// indefinite environmental retry. Keeping the un-resolved
					// parent as the probe target preserves the symlink as the
					// final component the existing-destination check's Lstat
					// sees, so the empty-dir gate below skips a nonempty target
					// the way a non-dot symlinked destination is skipped — `git
					// worktree add` populates an empty target through the symlink
					// but rejects a nonempty one without writing, so a full
					// filesystem on the target the structural failure does not
					// write to must not stall the loop. For a regular directory
					// the un-resolved and resolved paths coincide, so the
					// regular-dir probe is unchanged.
					worktreePath = parent
				} else {
					worktreePath = filepath.Join(resolvedParent, base)
				}
			} else {
				worktreePath = filepath.Join(resolvedParent, base)
			}
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
			// locations (the common directory and the creatable parents the
			// tail walk accumulated before the unresolvable component — git wrote
			// the missing directories that precede it, so a full filesystem on
			// the mount they were created into is the resource errno the
			// classifier must read even when a later component fails) and join
			// the resolution error so the lost-restore classifier reads it
			// alongside the git failure rather than dropping it, without
			// write-probing a guessed lexical destination.
			lexical := filepath.Join(repoPath, relBase)
			out := g.annotateGitWriteError(err, creatableParents...)
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
	// A non-ENOENT stat error on an ancestor (EIO, ETIMEDOUT, the bounded
	// ErrProbeTimedOut, ...) is environmental evidence about the filesystem
	// the missing parent would be created on. The write probe of the parent
	// already in dirs surfaces the same kind of error on the immediate parent,
	// but a coincidental writable parent (ENOENT on the parent itself, a
	// writable common dir) leaves the opaque Git failure with no environmental
	// errno if this branch discards it — the same hazard the destination and
	// symlink-resolution probes join rather than drop. Retain the error and
	// the ancestor it was reported on so the classifier reads it alongside
	// the Git failure the way it reads statErr/lstatErr/readDirErr, instead
	// of charging a full filesystem on the mount the ancestor stat named to
	// the structural retry budget. Walk up to the nearest existing ancestor
	// and probe it too, so a full filesystem on the ancestor that git writes
	// the missing parent into surfaces the resource errno.
	var ancestorStatErr error
	var ancestorStatPath string
	for {
		if time.Now().After(ancestorDeadline) {
			// The shared deadline elapsed before the walk reached an existing
			// ancestor: each stat returned ENOENT just before the per-call
			// timeout, so the loop never set ancestorStatErr or reached the
			// existing ancestor. A writable parent and common dir would leave
			// the opaque Git failure with no environmental errno, charging the
			// structural retry budget for a full filesystem on the mount the
			// missing ancestor git creates the leading directories into — the
			// same hazard a non-ENOENT stat error the existing-ancestor branch
			// joins. Surface the typed non-answer the symlink walk joins on its
			// shared deadline (fsprobe.ErrProbeTimedOut +
			// context.DeadlineExceeded) so the classifier reads the bounded
			// environmental non-answer the way it reads a stat-probe timeout,
			// instead of bare structural silence.
			ancestorStatErr = fmt.Errorf("ancestor stat search of %s did not answer within %s: %w: %w",
				parentDir, symlinkResolveTimeout, fsprobe.ErrProbeTimedOut, context.DeadlineExceeded)
			ancestorStatPath = parentDir
			break
		}
		if st, sErr := statSpaceProbe(parentDir); sErr == nil && st != nil {
			if parentDir != filepath.Dir(worktreePath) {
				dirs = append(dirs, parentDir)
			}
			break
		} else if !os.IsNotExist(sErr) {
			// Stop walking and let the existing probes handle the immediate
			// parent, but retain this error the way the destination and
			// symlink-resolution probes retain theirs — a writable parent and
			// common dir would otherwise leave the opaque Git failure with no
			// environmental evidence, charging the structural budget for a
			// mount the ancestor stat reported on.
			ancestorStatErr = sErr
			ancestorStatPath = parentDir
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
	if ancestorStatErr != nil {
		out = errors.Join(out, fmt.Errorf("stat probe ancestor %s: %w", ancestorStatPath, ancestorStatErr))
	}
	return out
}
