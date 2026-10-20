package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
// unlinks its file before writing, so nothing stageable ever exists.
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
	// existing prefix is resolved and the remaining non-existent tail is
	// rejoined lexically.
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
	//
	// The un-joined concatenation preserves `..` in the relative worktreePath
	// so the walk traverses a symlinked component before collapsing it the way
	// git's chdir does; a joined path would lexically clean `..` first and the
	// symlink would never be followed.
	worktreePath := g.worktreePath
	if !filepath.IsAbs(worktreePath) {
		worktreePath = repoPath + string(filepath.Separator) + g.worktreePath
		resolved, timedOut, resolveErr := resolveSymlinkPrefixBounded(worktreePath)
		if timedOut {
			// A bounded non-answer from the symlink resolution is evidence in
			// its own right: the mount the probe is about to write to did not
			// answer a metadata lookup, so the git subprocess's failure
			// classifies environmental rather than structural, the same way a
			// stat-probe non-answer does. Fall back to the lexical join — the
			// bounded write/stat probes below still run and add evidence if
			// they answer — and join the typed non-answer so the lost-restore
			// classifier reads the bounded environmental class, not bare
			// structural silence.
			lexical := filepath.Join(repoPath, g.worktreePath)
			out := g.annotateGitWriteError(err, filepath.Dir(lexical))
			return errors.Join(out, fmt.Errorf("symlink resolution of %s: %w", lexical, resolveErr))
		}
		if resolveErr == nil {
			worktreePath = resolved
		} else {
			// A non-timeout, non-ENOENT resolution error (EACCES, ETIMEDOUT,
			// EIO, ...) is environmental evidence about the filesystem the
			// probe is about to write to, not a missing path. Rejoining the
			// tail lexically would remove a symlinked component like `link/..`
			// and let the subsequent write/stat probes all succeed, so the
			// original git failure would misclassify as structural. Fall back
			// to the lexical join (the bounded probes below still run and add
			// evidence if they answer) and join the resolution error so the
			// lost-restore classifier reads it alongside the git failure
			// rather than dropping it.
			lexical := filepath.Join(repoPath, g.worktreePath)
			out := g.annotateGitWriteError(err, filepath.Dir(lexical))
			return errors.Join(out, fmt.Errorf("symlink resolution of %s: %w", lexical, resolveErr))
		}
	}
	dirs := []string{filepath.Dir(worktreePath)}
	var statErr error
	if st, sErr := statSpaceProbe(worktreePath); sErr == nil && st != nil && st.IsDir() {
		dirs = append(dirs, worktreePath)
	} else if sErr != nil && !os.IsNotExist(sErr) {
		statErr = sErr
	}
	out := g.annotateGitWriteError(err, dirs...)
	if statErr != nil {
		out = errors.Join(out, fmt.Errorf("stat probe destination %s: %w", worktreePath, statErr))
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
// argument against the repoPath the git runner sets as cmd.Dir, then rejoins
// the remaining non-existent tail lexically. filepath.Join only lexically
// cleans `..`, so a worktree argument like `link/../wt` (where `<repo>/link`
// is a symlink to /mnt/dir) would collapse to `<repo>/wt` and probe the wrong
// filesystem; git follows `link` first and writes /mnt/wt. Walking the
// un-joined path component by component — following symlinks at each existing
// prefix with filepath.EvalSymlinks before `..` is collapsed — preserves that
// traversal for the probe. The destination git is about to create may not
// exist yet, so the longest existing prefix is resolved and the non-existent
// tail is rejoined; if no prefix resolves the lexical path is the honest
// answer (the probe then reports the same errno git's child surfaces from the
// missing path).
//
// Each prefix lookup runs through the bounded fsprobe.EvalSymlinks, the same
// per-dir flight shape statSpaceProbe uses — a stalled network/FUSE mount that
// never answers a Readlink or Stat would otherwise wedge the synchronous
// restore poll the probe informs (#5295 review). The whole walk shares a
// single deadline (symlinkResolveTimeout) rather than restarting the per-dir
// bound for every prefix, so a path with many components past the first
// nonexistent entry cannot block for N × probeTimeout. A timed-out lookup
// returns the bounded non-answer (ErrProbeTimedOut + context.DeadlineExceeded)
// so the caller joins it into the annotation rather than silently falling back
// to the lexical path; a non-timeout ENOENT (the normal pre-creation answer)
// walks up to the next shorter prefix, while any other resolution error
// (EACCES, ETIMEDOUT, EIO, ...) is returned — it is environmental evidence
// about the filesystem git's child is about to write to, and rejoining the
// tail lexically would remove a symlinked component like `link/..` and let the
// subsequent probes succeed, misclassifying the original git failure as
// structural.
func resolveSymlinkPrefixBounded(path string) (resolved string, timedOut bool, err error) {
	if path == "" {
		return path, false, nil
	}
	// Run the whole prefix walk under one deadline so a slow filesystem
	// cannot block the restore poll for N × probeTimeout — each prefix
	// EvalSymlinks invocation already runs in its own per-dir bounded flight,
	// but without a shared budget a path with many components past the first
	// nonexistent entry could restart the deadline for every prefix. The
	// goroutine keeps running after the deadline fires (the per-dir flights
	// latch their own timeouts and drain), but the caller returns at the
	// shared bound the way a single timed-out flight does.
	type walkResult struct {
		resolved string
		timedOut bool
		err      error
	}
	done := make(chan walkResult, 1)
	go func() {
		r, to, e := resolveSymlinkPrefixWalk(path)
		done <- walkResult{r, to, e}
	}()
	timer := time.NewTimer(symlinkResolveTimeout)
	defer timer.Stop()
	select {
	case res := <-done:
		return res.resolved, res.timedOut, res.err
	case <-timer.C:
		// The whole walk did not answer within the shared budget — surface
		// the typed non-answer the same way a single timed-out flight does,
		// so the classifier reads the bounded environmental class rather than
		// bare structural silence.
		return "", true, fmt.Errorf("symlink prefix walk of %s did not answer within %s: %w: %w",
			path, symlinkResolveTimeout, fsprobe.ErrProbeTimedOut, context.DeadlineExceeded)
	}
}

// resolveSymlinkPrefixWalk is the unbounded component walk
// resolveSymlinkPrefixBounded runs under a single shared deadline. It walks
// the un-joined path from the longest prefix down, resolving the longest
// existing prefix through symlinks and rejoining the non-existent tail
// lexically.
func resolveSymlinkPrefixWalk(path string) (resolved string, timedOut bool, err error) {
	parts := strings.Split(path, string(filepath.Separator))
	for i := len(parts); i > 0; i-- {
		prefix := strings.Join(parts[:i], string(filepath.Separator))
		if prefix == "" {
			prefix = string(filepath.Separator)
		}
		r, e := evalSymlinksSpaceProbe(prefix)
		if e == nil {
			tail := strings.Join(parts[i:], string(filepath.Separator))
			if tail == "" {
				return r, false, nil
			}
			return filepath.Join(r, tail), false, nil
		}
		if errors.Is(e, fsprobe.ErrProbeTimedOut) {
			return "", true, e
		}
		// A missing path (ENOENT) is the normal pre-creation answer — walk
		// up to the next shorter prefix to find the longest existing one.
		// Any other resolution error (EACCES, ETIMEDOUT, EIO, ...) is
		// environmental evidence about the filesystem git's child is about to
		// write to; rejoining the tail lexically would remove a symlinked
		// component like `link/..` and let the subsequent probes succeed,
		// misclassifying the original git failure as structural. Return it so
		// the caller joins it into the annotation.
		if os.IsNotExist(e) {
			continue
		}
		return "", false, e
	}
	// No prefix resolved through symlinks. The lexical path is the honest
	// answer: the probe reports the same errno git's child surfaces from the
	// missing path.
	return path, false, nil
}
