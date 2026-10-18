package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// writeSpaceProbeSize bounds the probe write below. A single byte is not
// enough evidence: a filesystem that can still accept stray bytes but not a
// real worktree write — nearly full, or one whose block accounting git hit
// first — would pass the probe while every retry keeps failing, and the
// failure classifies structural until the budget gives up on an environmental
// outage (#5295 review). The worktree metadata writes this probe stands in
// for (gitdir registration, HEAD, config, index) sit in the tens-to-hundreds
// of KB; a real checkout is unbounded, so no fixed size can reproduce git's
// exact demand. One MiB covers the metadata class while staying noise-level
// transient allocation on any disk that is not already exhausted.
const writeSpaceProbeSize = 1 << 20

// writeSpaceProbe is the production probe (and the test seam): create a file
// in dir and WRITE a representative payload to it, then Sync before close so
// the bytes must reach the filesystem's accounting. The write is the point —
// on a tmpfs filled past its size limit a zero-length create still succeeds
// because dentries and inodes live outside the data budget, so only a real
// data write sees ENOSPC. The Sync is the second half — on a
// delayed-allocation filesystem (ext4, xfs) an unsynced write can land in the
// page cache and report success while the filesystem is already out of
// blocks, so a write that only looked cheap could pass while git's real write
// got the ENOSPC the classifier never saw. The file is removed before
// returning either way.
var writeSpaceProbe = func(dir string) error {
	f, err := os.CreateTemp(dir, ".af-add-probe-*")
	if err != nil {
		return err
	}
	name := f.Name()
	// Unlink IMMEDIATELY, while the fd is still open: on POSIX the descriptor
	// stays valid and every write below lands on an inode whose name is
	// already gone, so the probe file is invisible to `git status`/`git add
	// -A` at every instant it exists — inside a tracked worktree included —
	// and no cleanup gap can ever leave it stageable (#5295 review). If the
	// unlink itself fails the file still exists; keep probing and surface
	// that failure at the end.
	rerr := os.Remove(name)
	var werr error
	buf := make([]byte, 64<<10)
	for written := 0; written < writeSpaceProbeSize; {
		chunk := buf
		if writeSpaceProbeSize-written < len(chunk) {
			chunk = chunk[:writeSpaceProbeSize-written]
		}
		n, wErr := f.Write(chunk)
		written += n
		if wErr != nil {
			werr = wErr
			break
		}
	}
	var serr error
	if werr == nil {
		serr = f.Sync()
	}
	cerr := f.Close()
	if rerr != nil {
		// The early unlink failed, so the probe file exists under its name —
		// best-effort retry before reporting.
		_ = os.Remove(name)
		if werr == nil && serr == nil && cerr == nil {
			// The write proved the filesystem writable, so this is not
			// capacity evidence — but a silent leftover probe file is litter
			// (and a staging hazard where a probe target is a tracked tree).
			// Surface it: the caller's join records it in the error, and
			// because classification keys on errnos, an ordinary unlink
			// failure still leaves the git error structural while one
			// carrying a resource errno is honest environmental evidence
			// (#5295 review).
			return fmt.Errorf("probe write verified the filesystem, but removing probe file %s failed: %w", name, rerr)
		}
	}
	if werr != nil {
		return werr
	}
	if serr != nil {
		return serr
	}
	return cerr
}

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
func (g *GitWorktree) annotateWorktreeAddError(err error) error {
	dirs := []string{filepath.Dir(g.worktreePath)}
	if st, statErr := os.Stat(g.worktreePath); statErr == nil && st.IsDir() {
		dirs = append(dirs, g.worktreePath)
	}
	return g.annotateGitWriteError(err, dirs...)
}
