package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
)

// TestAnnotateWorktreeAddError_DiskFull — the #5295 play-test failure: `git
// worktree add` dies with ENOSPC trapped inside *exec.ExitError stderr text,
// but a data-write probe against the same filesystem fails with a REAL errno.
// The joined error must carry BOTH the git failure and ENOSPC so the
// lost-restore classifier reads it as environmental.
func TestAnnotateWorktreeAddError_DiskFull(t *testing.T) {
	old := writeSpaceProbe
	writeSpaceProbe = func(string) error {
		return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
	}
	defer func() { writeSpaceProbe = old }()

	g := &GitWorktree{worktreePath: filepath.Join(t.TempDir(), "wt")}
	gitErr := fmt.Errorf("git command failed: could not write (%w)", exitErrorWithStderr(t, "No space left on device"))
	out := g.annotateWorktreeAddError(fmt.Errorf("failed to create worktree from branch x: %w", gitErr))
	if !errors.Is(out, syscall.ENOSPC) {
		t.Fatalf("disk-full probe must put ENOSPC in the chain: %v", out)
	}
	if !errors.Is(out, gitErr) {
		t.Fatalf("joined error dropped the original git failure: %v", out)
	}
}

// TestAnnotateWorktreeAddError_FreeSpace — a non-disk git failure on a
// writable filesystem returns UNCHANGED: the real probe succeeds, no errno is
// invented, and the structural classifier sees exactly the error it had.
func TestAnnotateWorktreeAddError_FreeSpace(t *testing.T) {
	dir := t.TempDir()
	g := &GitWorktree{worktreePath: filepath.Join(dir, "wt")}
	gitErr := fmt.Errorf("git command failed: ref already exists (%w)", exitErrorWithStderr(t, "fatal: branch exists"))
	out := g.annotateWorktreeAddError(fmt.Errorf("failed to create worktree from branch x: %w", gitErr))
	if errors.Is(out, syscall.ENOSPC) {
		t.Fatalf("non-disk failure on free space must not acquire ENOSPC: %v", out)
	}
	if !errors.Is(out, gitErr) {
		t.Fatalf("original git failure lost: %v", out)
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, ".af-add-probe-*")); len(matches) != 0 {
		t.Fatalf("successful probe left litter: %v", matches)
	}
}

// TestAnnotateWorktreeAddError_ProbeWriteFails pins the byte-write half: a
// create that succeeds but a WRITE that fails is still evidence (tmpfs inode
// budget vs data budget — a zero-byte file is not proof of space).
func TestAnnotateWorktreeAddError_ProbeWriteFails(t *testing.T) {
	old := writeSpaceProbe
	writeSpaceProbe = func(string) error {
		return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.EDQUOT}
	}
	defer func() { writeSpaceProbe = old }()

	g := &GitWorktree{worktreePath: filepath.Join(t.TempDir(), "wt")}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	if !errors.Is(out, syscall.EDQUOT) {
		t.Fatalf("quota-class probe failure must carry EDQUOT: %v", out)
	}
}

// TestAnnotateWorktreeAddError_NonSpaceProbeFailure — when the probe itself
// fails for a NON-space reason (parent vanished), the joined error carries
// that errno honestly: ENOENT is not in the environmental errno class, so the
// git failure keeps its structural classification.
func TestAnnotateWorktreeAddError_NonSpaceProbeFailure(t *testing.T) {
	old := writeSpaceProbe
	writeSpaceProbe = func(string) error {
		return &os.PathError{Op: "open", Path: ".af-add-probe-x", Err: syscall.ENOENT}
	}
	defer func() { writeSpaceProbe = old }()

	g := &GitWorktree{worktreePath: filepath.Join(t.TempDir(), "wt")}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	if errors.Is(out, syscall.ENOSPC) {
		t.Fatalf("ENOENT probe must not read as ENOSPC: %v", out)
	}
	if !errors.Is(out, syscall.ENOENT) {
		t.Fatalf("joined error should still carry the probe's real errno: %v", out)
	}
}

// TestAnnotateGitWriteError_CommonDirProbe — the worktree parent is writable
// but the repository's Git common dir sits on a FULL second filesystem
// (#5295 review): `worktree add` registers under worktrees/<name>/ there, and
// a probe of only the checkout parent would miss it. The common-dir probe's
// errno must still land in the chain.
func TestAnnotateGitWriteError_CommonDirProbe(t *testing.T) {
	old := writeSpaceProbe
	writeSpaceProbe = func(dir string) error {
		if dir == "/common" {
			return &os.PathError{Op: "write", Path: dir + "/.af-add-probe-x", Err: syscall.ENOSPC}
		}
		return nil
	}
	defer func() { writeSpaceProbe = old }()
	oldCommon := gitCommonDir
	gitCommonDir = func(*GitWorktree) (string, error) { return "/common", nil }
	defer func() { gitCommonDir = oldCommon }()

	g := &GitWorktree{worktreePath: filepath.Join(t.TempDir(), "wt")}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed (exit status 128)"))
	if !errors.Is(out, syscall.ENOSPC) {
		t.Fatalf("full common dir must put ENOSPC in the chain even when the worktree parent writes fine: %v", out)
	}
}

// TestGitCommonDir_RelativeRepoPath is the #5295-review regression: a restored
// record can carry a relative repoPath, which runGitLocalCommand resolves
// against daemonLaunchCwd — but a relative --git-common-dir result then has to
// join onto that RESOLVED repo dir, not the bare relative repoPath (which would
// leave the probe path relative to the daemon's post-chdir AF home and probe an
// unrelated filesystem).
func TestGitCommonDir_RelativeRepoPath(t *testing.T) {
	sandboxHome(t)
	root := t.TempDir()
	repoDir := filepath.Join(root, "repo")
	require.NoError(t, exec.Command("git", "init", repoDir).Run())

	old := daemonLaunchCwd
	daemonLaunchCwd = root
	defer func() { daemonLaunchCwd = old }()

	dir, err := gitCommonDir(&GitWorktree{repoPath: "repo"})
	require.NoError(t, err)
	require.True(t, filepath.IsAbs(dir),
		"a relative repoPath must resolve against the launch cwd, not the daemon's post-chdir cwd")
	assert.Equal(t, filepath.Join(repoDir, ".git"), dir)
}

// TestAnnotateGitWriteError_CommonDirUnresolvable — when `rev-parse
// --git-common-dir` itself fails with a resource errno (a fork under
// RLIMIT_NPROC), that failure is evidence too and stays in the chain.
func TestAnnotateGitWriteError_CommonDirUnresolvable(t *testing.T) {
	old := writeSpaceProbe
	writeSpaceProbe = func(string) error { return nil }
	defer func() { writeSpaceProbe = old }()
	oldCommon := gitCommonDir
	gitCommonDir = func(*GitWorktree) (string, error) {
		return "", &os.PathError{Op: "fork/exec", Path: "git", Err: syscall.EAGAIN}
	}
	defer func() { gitCommonDir = oldCommon }()

	g := &GitWorktree{worktreePath: filepath.Join(t.TempDir(), "wt")}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	if !errors.Is(out, syscall.EAGAIN) {
		t.Fatalf("unresolvable common dir must join its own errno: %v", out)
	}
}

// TestAnnotateWorktreeAddError_DestinationStatError — a stat failure on the
// destination that is not plain ENOENT (a dead mount reports ETIMEDOUT; an
// unsearchable parent reports EACCES/ENOTDIR) is probe evidence the git
// subprocess could never carry, and must join the annotated error rather
// than be discarded — otherwise mount stalls charge the structural budget
// (#5295 review).
func TestAnnotateWorktreeAddError_DestinationStatError(t *testing.T) {
	old := writeSpaceProbe
	writeSpaceProbe = func(string) error { return nil }
	defer func() { writeSpaceProbe = old }()
	oldCommon := gitCommonDir
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }
	defer func() { gitCommonDir = oldCommon }()

	// worktreePath sits beneath a regular FILE: os.Stat fails ENOTDIR.
	parent := filepath.Join(t.TempDir(), "a-file")
	require.NoError(t, os.WriteFile(parent, []byte("x"), 0o600))
	g := &GitWorktree{worktreePath: filepath.Join(parent, "wt")}

	out := g.annotateWorktreeAddError(errors.New("git worktree add failed (exit status 128)"))
	require.True(t, errors.Is(out, syscall.ENOTDIR),
		"the destination stat failure must be joined as evidence: %v", out)
}

// TestAnnotateWorktreeAddError_DestinationAbsentIsSilent — a simply-missing
// destination is the normal pre-creation answer, not evidence: the stat skip
// must not add noise to the chain.
func TestAnnotateWorktreeAddError_DestinationAbsentIsSilent(t *testing.T) {
	old := writeSpaceProbe
	writeSpaceProbe = func(string) error { return nil }
	defer func() { writeSpaceProbe = old }()
	oldCommon := gitCommonDir
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }
	defer func() { gitCommonDir = oldCommon }()

	g := &GitWorktree{worktreePath: filepath.Join(t.TempDir(), "wt")}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.False(t, errors.Is(out, syscall.ENOENT),
		"plain ENOENT is the expected pre-creation state, not evidence: %v", out)
}

// TestAnnotateWorktreeAddError_DestinationStatTimeout is the #5295-review
// regression: the destination stat must be BOUNDED — a mount that never
// answers a metadata lookup would wedge the synchronous restore poll with a
// plain os.Stat. The bounded probe's typed non-answer (ErrProbeTimedOut +
// context.DeadlineExceeded) joins as evidence so the classifier reads the
// bounded environmental class, and the missing answer does NOT add the
// destination to the probe set.
func TestAnnotateWorktreeAddError_DestinationStatTimeout(t *testing.T) {
	old := writeSpaceProbe
	writeSpaceProbe = func(string) error { return nil }
	defer func() { writeSpaceProbe = old }()
	oldStat := statSpaceProbe
	statSpaceProbe = func(string) (os.FileInfo, error) {
		return nil, fmt.Errorf("stat probe of wt did not answer within bound: %w: %w",
			fsprobe.ErrProbeTimedOut, context.DeadlineExceeded)
	}
	defer func() { statSpaceProbe = oldStat }()
	oldCommon := gitCommonDir
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }
	defer func() { gitCommonDir = oldCommon }()

	g := &GitWorktree{worktreePath: filepath.Join(t.TempDir(), "wt")}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.True(t, errors.Is(out, fsprobe.ErrProbeTimedOut),
		"the bounded stat non-answer must reach the chain: %v", out)
	require.True(t, errors.Is(out, context.DeadlineExceeded),
		"the sentinel must keep its deadline pair for the classifier: %v", out)
	require.False(t, errors.Is(out, syscall.ENOENT),
		"a no-answer is not a missing destination: %v", out)
}

// TestGitCommonDir_DeadlinePairsSentinel is the #5295-review regression:
// the bounded `rev-parse --git-common-dir` lookup's deadline must reach the
// classifier as the bounded-probe non-answer — ErrCommonDirProbeNoAnswer +
// context.DeadlineExceeded — because a bare deadline reads as a hung session
// operation and charges the structural budget for a mount stall.
func TestGitCommonDir_DeadlinePairsSentinel(t *testing.T) {
	sandboxHome(t)
	repoRoot := createGitRepo(t)
	stallingGitOnPath(t)
	shortenLocalTimeout(t, 200*time.Millisecond)

	_, err := gitCommonDir(&GitWorktree{repoPath: repoRoot})
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrCommonDirProbeNoAnswer),
		"the bounded lookup's deadline must carry its sentinel: %v", err)
	require.True(t, errors.Is(err, context.DeadlineExceeded),
		"the sentinel must pair with the deadline the classifier requires: %v", err)
}

// TestAnnotateWorktreeAddError_CommonDirProbeNoAnswer — the paired non-answer
// from a stalled common-dir lookup must ride the joined annotate chain so the
// restore classifier sees sentinel+deadline, not a bare deadline.
func TestAnnotateWorktreeAddError_CommonDirProbeNoAnswer(t *testing.T) {
	old := writeSpaceProbe
	writeSpaceProbe = func(string) error { return nil }
	defer func() { writeSpaceProbe = old }()
	oldCommon := gitCommonDir
	gitCommonDir = func(*GitWorktree) (string, error) {
		return "", fmt.Errorf("%w: %w", ErrCommonDirProbeNoAnswer, context.DeadlineExceeded)
	}
	defer func() { gitCommonDir = oldCommon }()

	g := &GitWorktree{worktreePath: filepath.Join(t.TempDir(), "wt")}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.True(t, errors.Is(out, ErrCommonDirProbeNoAnswer),
		"the common-dir non-answer must reach the chain: %v", out)
	require.True(t, errors.Is(out, context.DeadlineExceeded),
		"the sentinel must keep its deadline pair: %v", out)
}

// TestAnnotateWorktreeAddError_RelativeWorktreePathResolvedAgainstLaunchCwd is the
// worktreePath-side regression that TestGitCommonDir_RelativeRepoPath already pins for
// repoPath: a restored record can carry a RELATIVE worktreePath. The git runner resolves
// the relative `-C repoPath` against daemonLaunchCwd and sets cmd.Dir to that joined dir,
// so a relative worktree argument resolves inside git against that resolved repo dir —
// the file probes must target the same destination, not the raw relative worktreePath (which
// the daemon resolves against its post-chdir AF home and verdicts an unrelated filesystem).
//
// Deterministic in both directions: before the fix the probes received the raw relative
// path and statTarget/probedDirs would be "wt/dest"/"wt"; after the fix they are the
// launch-cwd-resolved repo-dir-joined paths.
func TestAnnotateWorktreeAddError_RelativeWorktreePathResolvedAgainstLaunchCwd(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldStat := statSpaceProbe
	defer func() { statSpaceProbe = oldStat }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()

	launchCwd := t.TempDir()
	oldLaunch := daemonLaunchCwd
	daemonLaunchCwd = launchCwd
	defer func() { daemonLaunchCwd = oldLaunch }()

	// Model the daemon's post-chdir AF home as a separate temp dir, distinct from the
	// launch cwd, so a raw relative path resolves there (the bug) while the resolved
	// path lands under the launch cwd (the fix). Without this divergence the two
	// resolutions would coincide and the assertion could not tell them apart.
	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	var statTarget string
	statSpaceProbe = func(p string) (os.FileInfo, error) {
		statTarget = p
		return nil, os.ErrNotExist // pre-creation normal answer — stays silent in the chain
	}
	var probedDirs []string
	writeSpaceProbe = func(dir string) error {
		probedDirs = append(probedDirs, dir)
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{repoPath: "repo", worktreePath: "wt/dest"}
	_ = g.annotateWorktreeAddError(errors.New("git worktree add failed"))

	resolvedDest := filepath.Join(launchCwd, "repo", "wt", "dest")
	require.Equal(t, resolvedDest, statTarget,
		"the relative worktreePath must stat the launch-cwd-resolved repo-dir-joined destination "+
			"(git's child cwd = resolved repo dir), not the raw relative path the daemon resolves against its post-chdir cwd")
	require.Contains(t, probedDirs, filepath.Join(launchCwd, "repo", "wt"),
		"the worktree parent dir probed must be the resolved <launchCwd>/repo/wt, not the raw relative "+
			"path (`wt`) which resolves against the daemon's post-chdir AF home (%s)", afHome)
	for _, d := range probedDirs {
		require.True(t, filepath.IsAbs(d),
			"a probed dir must be absolute (resolved against the launch cwd), not a raw relative path: %q", d)
	}
}

// TestAnnotateWorktreeAddError_RelativeWorktreePathEnvironmentalClassification proves the
// worktreePath resolution keeps the #5295 fix's contract alive for the relative-path edge
// case the rest of the probe layer already defends (git runner, gitCommonDir, bounded
// removal): a full worktree filesystem at the resolved parent must classify as
// environmental rather than structural — the exact #5295 failure mode "session gives up
// while the disk is full" annotateWorktreeAddError exists to prevent. Before the fix the
// probe targeted the daemon's post-chdir AF home (writable here), missed the real ENOSPC,
// and the error stayed structural — the play-test failure.
func TestAnnotateWorktreeAddError_RelativeWorktreePathEnvironmentalClassification(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldStat := statSpaceProbe
	defer func() { statSpaceProbe = oldStat }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()

	launchCwd := t.TempDir()
	oldLaunch := daemonLaunchCwd
	daemonLaunchCwd = launchCwd
	defer func() { daemonLaunchCwd = oldLaunch }()

	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	fullParent := filepath.Join(launchCwd, "repo", "wt")
	statSpaceProbe = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	writeSpaceProbe = func(dir string) error {
		if dir == fullParent {
			return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
		}
		return nil // AF home and any other dir probe writable — no spurious ENOSPC introduced
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{repoPath: "repo", worktreePath: "wt/dest"}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.True(t, errors.Is(out, syscall.ENOSPC),
		"a full worktree filesystem resolved against the launch cwd must read as environmental "+
			"(ENOSPC in the chain) so lost-restore waits instead of hitting the structural give-up budget: %v", out)
}

// TestAnnotateWorktreeAddError_RelativeWorktreePathStatErrorNamesResolvedPath — the
// stat-probe error message must name the resolved destination so the lost-restore
// classifier's diagnostics point at the filesystem git actually touched, not the raw
// relative path the daemon would resolve against its post-chdir AF home. Before the fix
// the message named `g.worktreePath` verbatim, hiding the resolution gap from anyone
// reading the annotated chain.
func TestAnnotateWorktreeAddError_RelativeWorktreePathStatErrorNamesResolvedPath(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldStat := statSpaceProbe
	defer func() { statSpaceProbe = oldStat }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()

	launchCwd := t.TempDir()
	oldLaunch := daemonLaunchCwd
	daemonLaunchCwd = launchCwd
	defer func() { daemonLaunchCwd = oldLaunch }()

	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	resolved := filepath.Join(launchCwd, "repo", "wt", "dest")
	statSpaceProbe = func(p string) (os.FileInfo, error) {
		return nil, &os.PathError{Op: "stat", Path: p, Err: syscall.EACCES}
	}
	writeSpaceProbe = func(string) error { return nil }
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{repoPath: "repo", worktreePath: "wt/dest"}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.True(t, errors.Is(out, syscall.EACCES),
		"the resolved-path stat failure must join its errno: %v", out)
	require.Contains(t, out.Error(), resolved,
		"the stat-error message must name the resolved destination, not the raw relative worktreePath: %v", out)
	require.NotContains(t, out.Error(), "stat probe destination wt/dest",
		"the stat-error message must not name the raw relative worktreePath: %v", out)
}

// TestAnnotateWorktreeAddError_RelativeWorktreePathFollowsSymlinkedRepo pins the
// symlink-traversal the git runner's cmd.Dir induces: when a restored record's
// repoPath is a symlink and worktreePath is relative with "..", filepath.Join
// lexically cleans "/launch/repo/../wt" to "/launch/wt" — but git, started with
// cmd.Dir = repoPath, follows the symlink (chdir resolves it) and resolves the
// relative worktree argument against the real repo dir, creating /mnt/wt when
// /launch/repo -> /mnt/repo. The probe must target the same real filesystem or
// it misclassifies ENOSPC/mount failures on the worktree's actual location.
// Before the fix the probe examined the lexical /launch/wt and missed the real
// /mnt/wt the way git -C /launch/repo worktree add ../wt confirmed.
func TestAnnotateWorktreeAddError_RelativeWorktreePathFollowsSymlinkedRepo(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldStat := statSpaceProbe
	defer func() { statSpaceProbe = oldStat }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()

	launchCwd := t.TempDir()
	oldLaunch := daemonLaunchCwd
	daemonLaunchCwd = launchCwd
	defer func() { daemonLaunchCwd = oldLaunch }()

	// A real repo dir on a distinct filesystem location, with a symlink inside
	// the launch cwd pointing at it. The probe must resolve through the link.
	realRepo := t.TempDir()
	require.NotEqual(t, realRepo, launchCwd)
	linkRepo := filepath.Join(launchCwd, "repo")
	require.NoError(t, os.Symlink(realRepo, linkRepo))

	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	var statTarget string
	statSpaceProbe = func(p string) (os.FileInfo, error) {
		statTarget = p
		return nil, os.ErrNotExist // pre-creation normal answer — stays silent
	}
	var probedDirs []string
	writeSpaceProbe = func(dir string) error {
		probedDirs = append(probedDirs, dir)
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	// "../wt" relative to a symlinked repo: git follows the link, so the real
	// destination is <realRepo>/../wt = parent(realRepo)/wt, not the lexical
	// <launchCwd>/repo/../wt = <launchCwd>/wt.
	g := &GitWorktree{repoPath: "repo", worktreePath: "../wt"}
	_ = g.annotateWorktreeAddError(errors.New("git worktree add failed"))

	resolvedDest := filepath.Join(filepath.Dir(realRepo), "wt")
	require.Equal(t, resolvedDest, statTarget,
		"the probe must target the symlink-resolved destination git's child writes to "+
			"(%s), not the lexical filepath.Join of the un-resolved repoPath (%s)", resolvedDest, filepath.Join(launchCwd, "wt"))
	require.Contains(t, probedDirs, filepath.Dir(resolvedDest),
		"the worktree parent dir probed must be the symlink-resolved parent (%s), not the lexical %s",
		filepath.Dir(resolvedDest), filepath.Join(launchCwd))
	for _, d := range probedDirs {
		require.True(t, filepath.IsAbs(d),
			"a probed dir must be absolute (resolved through the symlink), not a raw relative path: %q", d)
	}
}

// The probe machinery itself (real writes, the O_TMPFILE anonymous inode,
// the named fallback, and the bounded flight) is tested in internal/fsprobe.
