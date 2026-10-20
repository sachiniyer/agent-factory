package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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
	worktreePath := g.worktreePath
	if !filepath.IsAbs(worktreePath) {
		worktreePath = filepath.Join(repoPath, worktreePath)
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
