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

	// t.TempDir() lives under a symlinked prefix on macOS (/var/folders ->
	// /private/var/folders), and the probe resolves the longest existing prefix
	// through symlinks the way the kernel's chdir does, so the expected paths
	// must be computed against the resolved launch cwd to match across hosts.
	resolvedLaunchCwd, err := filepath.EvalSymlinks(launchCwd)
	require.NoError(t, err)

	g := &GitWorktree{repoPath: "repo", worktreePath: "wt/dest"}
	_ = g.annotateWorktreeAddError(errors.New("git worktree add failed"))

	resolvedDest := filepath.Join(resolvedLaunchCwd, "repo", "wt", "dest")
	require.Equal(t, resolvedDest, statTarget,
		"the relative worktreePath must stat the launch-cwd-resolved repo-dir-joined destination "+
			"(git's child cwd = resolved repo dir), not the raw relative path the daemon resolves against its post-chdir cwd")
	require.Contains(t, probedDirs, filepath.Join(resolvedLaunchCwd, "repo", "wt"),
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

	// t.TempDir() lives under a symlinked prefix on macOS (/var/folders ->
	// /private/var/folders), and the probe resolves the longest existing
	// prefix through symlinks the way the kernel's chdir does, so the full
	// parent the mock must match is the resolved launch cwd's repo/wt.
	resolvedLaunchCwd, err := filepath.EvalSymlinks(launchCwd)
	require.NoError(t, err)
	fullParent := filepath.Join(resolvedLaunchCwd, "repo", "wt")
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
	// <launchCwd>/repo/../wt = <launchCwd>/wt. Resolve linkRepo the same way the
	// production code does — on macOS t.TempDir() lives under /var/folders,
	// itself a symlink to /private/var/folders, so EvalSymlinks lands under
	// /private/var/... and the expected destination must follow it there.
	resolvedRepo, err := filepath.EvalSymlinks(linkRepo)
	require.NoError(t, err)
	resolvedDest := filepath.Join(filepath.Dir(resolvedRepo), "wt")
	lexicalDest := filepath.Join(launchCwd, "wt")
	require.NotEqual(t, lexicalDest, resolvedDest,
		"the symlinked-repo case must diverge from the lexical join, otherwise the test cannot distinguish the fix from the bug")
	g := &GitWorktree{repoPath: "repo", worktreePath: "../wt"}
	_ = g.annotateWorktreeAddError(errors.New("git worktree add failed"))

	require.Equal(t, resolvedDest, statTarget,
		"the probe must target the symlink-resolved destination git's child writes to "+
			"(%s), not the lexical filepath.Join of the un-resolved repoPath (%s)", resolvedDest, lexicalDest)
	require.Contains(t, probedDirs, filepath.Dir(resolvedDest),
		"the worktree parent dir probed must be the symlink-resolved parent (%s), not the lexical %s",
		filepath.Dir(resolvedDest), lexicalDest)
	for _, d := range probedDirs {
		require.True(t, filepath.IsAbs(d),
			"a probed dir must be absolute (resolved through the symlink), not a raw relative path: %q", d)
	}
}

// TestAnnotateWorktreeAddError_RelativeWorktreePathFollowsSymlinkInPathComponent pins the
// symlink traversal the kernel's chdir applies INSIDE a relative worktreePath: a restored
// record's worktreePath like `link/../wt` (where `<repo>/link` is a symlink to /mnt/dir) must
// resolve the symlink before collapsing `..`, the way `git -C <repo> worktree add link/../wt`
// does (it creates /mnt/wt, not `<repo>/wt`). filepath.Join lexically cleans `link/../wt` to
// `wt`, so a probe built from the join would examine `<repo>/wt` — an unrelated filesystem —
// and misclassify ENOSPC or mount failures on the real destination. The resolution walks the
// un-joined path component by component so the symlink is followed before the `..` collapses,
// then rejoins the non-existent tail onto the resolved prefix.
func TestAnnotateWorktreeAddError_RelativeWorktreePathFollowsSymlinkInPathComponent(t *testing.T) {
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

	// <repo>/link points at a real dir on a distinct location; `link/../wt`
	// must follow the link before collapsing `..`, landing at <realTarget>/wt,
	// not the lexical <repo>/wt.
	repo := filepath.Join(launchCwd, "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))
	realTarget := t.TempDir()
	require.NotEqual(t, realTarget, filepath.Join(repo, "wt"))
	link := filepath.Join(repo, "link")
	require.NoError(t, os.Symlink(realTarget, link))

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

	// `link/../wt` relative to a real repo: git follows `link` to realTarget,
	// then `..` to its parent, then creates `wt`. The probe must target the
	// same parent(realTarget)/wt, not the lexical <repo>/wt that
	// filepath.Join(repo, "link/../wt") would produce.
	resolvedRealTarget, err := filepath.EvalSymlinks(realTarget)
	require.NoError(t, err)
	resolvedRepo, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	expectedDest := filepath.Join(filepath.Dir(resolvedRealTarget), "wt")
	lexicalDest := filepath.Join(resolvedRepo, "wt")
	require.NotEqual(t, lexicalDest, expectedDest,
		"the symlink-in-path case must diverge from the lexical join, otherwise the test cannot distinguish the fix from the bug")
	g := &GitWorktree{repoPath: "repo", worktreePath: "link/../wt"}
	_ = g.annotateWorktreeAddError(errors.New("git worktree add failed"))

	require.Equal(t, expectedDest, statTarget,
		"the probe must target the symlink-resolved destination git's child writes to "+
			"(%s), not the lexical filepath.Join of the un-resolved path (%s)", expectedDest, lexicalDest)
	require.Contains(t, probedDirs, filepath.Dir(expectedDest),
		"the worktree parent dir probed must be the symlink-resolved parent (%s), not the lexical %s",
		filepath.Dir(expectedDest), lexicalDest)
}

// TestAnnotateWorktreeAddError_SymlinkResolutionIsBounded pins the #5295-review contract for
// the symlink resolution the worktree probe added: filepath.EvalSymlinks performs unbounded
// metadata lookups, and the annotate path runs inside the synchronous restore poll, so the
// resolution must run through the same bounded flight as the stat probe — a stalled mount
// that never answers must surface the typed non-answer (ErrProbeTimedOut +
// context.DeadlineExceeded) instead of wedging the poll. The bounded non-answer joins the
// chain as evidence so the classifier reads the bounded environmental class, not bare
// structural silence.
func TestAnnotateWorktreeAddError_SymlinkResolutionIsBounded(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldStat := statSpaceProbe
	defer func() { statSpaceProbe = oldStat }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()
	oldEval := evalSymlinksSpaceProbe
	defer func() { evalSymlinksSpaceProbe = oldEval }()

	launchCwd := t.TempDir()
	oldLaunch := daemonLaunchCwd
	daemonLaunchCwd = launchCwd
	defer func() { daemonLaunchCwd = oldLaunch }()

	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	// A stalled mount never answers the symlink resolution: the bounded flight
	// returns the typed non-answer instead of blocking, so the annotate path
	// stays responsive and the environmental evidence joins the chain.
	var evalCalls []string
	evalSymlinksSpaceProbe = func(p string) (string, error) {
		evalCalls = append(evalCalls, p)
		return "", fmt.Errorf("eval-symlinks probe of %s did not answer within bound: %w: %w",
			p, fsprobe.ErrProbeTimedOut, context.DeadlineExceeded)
	}
	writeSpaceProbe = func(string) error { return nil }
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{repoPath: "repo", worktreePath: "wt/dest"}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.True(t, errors.Is(out, fsprobe.ErrProbeTimedOut),
		"the bounded symlink-resolution non-answer must reach the chain: %v", out)
	require.True(t, errors.Is(out, context.DeadlineExceeded),
		"the sentinel must keep its deadline pair for the classifier: %v", out)
	require.NotEmpty(t, evalCalls,
		"the bounded symlink resolution must be attempted, not skipped")
}

// TestAnnotateWorktreeAddError_SymlinkResolutionNonEnoentErrorPreserved pins the contract that
// a non-ENOENT resolution error — an environmental errno the kernel returns from Lstat/Readlink
// on a path git's child is about to write to — is preserved and joined into the annotation rather
// than discarded. Before the fix the walk treated every non-timeout error as a missing path and
// walked up to the next prefix, rejoining the tail lexically; for `link/../wt` with a symlinked
// `link` whose target returns ETIMEDOUT, that lexical rejoin removes `link/..` and the subsequent
// probes can all succeed, misclassifying the original git failure as structural. The fix returns
// the environmental error and joins it so the lost-restore classifier reads it.
func TestAnnotateWorktreeAddError_SymlinkResolutionNonEnoentErrorPreserved(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldStat := statSpaceProbe
	defer func() { statSpaceProbe = oldStat }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()
	oldEval := evalSymlinksSpaceProbe
	defer func() { evalSymlinksSpaceProbe = oldEval }()

	launchCwd := t.TempDir()
	oldLaunch := daemonLaunchCwd
	daemonLaunchCwd = launchCwd
	defer func() { daemonLaunchCwd = oldLaunch }()

	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	// An environmental errno (ETIMEDOUT) from the first prefix resolution is
	// not a missing path and must not be discarded — it joins the chain the
	// way a timed-out resolution does.
	pathErr := &os.PathError{Op: "lstat", Path: filepath.Join(launchCwd, "repo"), Err: syscall.ETIMEDOUT}
	evalSymlinksSpaceProbe = func(p string) (string, error) {
		return "", pathErr
	}
	writeSpaceProbe = func(string) error { return nil }
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{repoPath: "repo", worktreePath: "link/../wt"}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.True(t, errors.Is(out, syscall.ETIMEDOUT),
		"the non-ENOENT resolution error must reach the chain: %v", out)
	require.Contains(t, out.Error(), "symlink resolution of",
		"the resolution failure must be named as a symlink resolution error, not dropped: %v", out)
}

// TestAnnotateWorktreeAddError_RelativeWorktreePathFinalComponentSymlinkPreservesParent pins the
// contract the #5421 review added: when a restored relative destination's final component is a
// symlink to an existing directory, git rejects the add because the destination already exists
// without writing to the symlink target's parent. Resolving the whole destination would make
// filepath.Dir(worktreePath) point at that unrelated parent, and a full filesystem there would
// add ENOSPC the lost-restore loop waits on indefinitely even though freeing it cannot fix the
// structural failure. The parent traversal is resolved separately and the final component is
// preserved for the existing-destination check, so the parent probe targets the directory that
// hosts the destination's name rather than the symlink target's parent.
func TestAnnotateWorktreeAddError_RelativeWorktreePathFinalComponentSymlinkPreservesParent(t *testing.T) {
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

	// The relative worktree path's final component is a symlink to a real
	// directory on a distinct location. Git rejects the add because the
	// destination already exists; the probe must not resolve through the
	// symlink and probe the symlink target's parent, which sits on a
	// filesystem git never writes to.
	repo := filepath.Join(launchCwd, "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))
	realTarget := t.TempDir()
	require.NotEqual(t, realTarget, filepath.Join(repo, "dest"))
	require.NoError(t, os.Symlink(realTarget, filepath.Join(repo, "dest")))

	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	// Resolve the symlinked prefix the way the production code does, so the
	// expected parent matches across hosts (macOS /var/folders ->
	// /private/var/folders).
	resolvedRepo, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	resolvedRealTarget, err := filepath.EvalSymlinks(realTarget)
	require.NoError(t, err)
	symlinkTargetParent := filepath.Dir(resolvedRealTarget)
	require.NotEqual(t, symlinkTargetParent, resolvedRepo,
		"the symlink target's parent must differ from the repo dir, otherwise the test cannot distinguish the fix from the bug")

	// Only the symlink target's parent is full. The parent that hosts the
	// destination's name is writable, so a probe of the correct parent sees
	// no ENOSPC and the git failure stays structural; a probe of the symlink
	// target's parent (the bug) adds ENOSPC and the lost-restore loop would
	// wait indefinitely on a filesystem freeing it cannot fix.
	var statTarget string
	statSpaceProbe = func(p string) (os.FileInfo, error) {
		statTarget = p
		// The destination already exists (the symlink resolves to a real
		// directory), so the existing-destination check sees it as a dir.
		return os.Stat(p)
	}
	var probedDirs []string
	writeSpaceProbe = func(dir string) error {
		probedDirs = append(probedDirs, dir)
		if dir == symlinkTargetParent {
			return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
		}
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{repoPath: "repo", worktreePath: "dest"}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.False(t, errors.Is(out, syscall.ENOSPC),
		"a full filesystem at the symlink target's parent must not add ENOSPC — git rejected the add "+
			"because the destination already exists, so freeing that filesystem cannot fix the structural "+
			"failure and the parent that hosts the destination's name is writable: %v", out)
	require.NotContains(t, probedDirs, symlinkTargetParent,
		"the parent probe must target the directory that hosts the destination's name (%s), not the "+
			"symlink target's parent (%s)", resolvedRepo, symlinkTargetParent)
	require.Equal(t, filepath.Join(resolvedRepo, "dest"), statTarget,
		"the existing-destination check must see the preserved final component (%s), not the resolved "+
			"symlink target (%s)", filepath.Join(resolvedRepo, "dest"), resolvedRealTarget)
	require.Contains(t, probedDirs, resolvedRepo,
		"the parent that hosts the destination's name must be probed")
}

// TestAnnotateWorktreeAddError_RelativeWorktreePathRootRepoPathPreservesRootParent pins the
// contract the #5421 review added for a filesystem-root repoPath: when repoPath resolves to
// "/" (Unix) or a Windows volume root and worktreePath is a relative name like "wt", Split
// returns a root parent ("//") that TrimRight would empty, making the join produce a
// relative "wt" and the stat/write probes run beneath the daemon's AF-home cwd while Git
// creates "/wt". The parent-trim must preserve the root so the probes target the root
// Git actually writes into.
func TestAnnotateWorktreeAddError_RelativeWorktreePathRootRepoPathPreservesRootParent(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldStat := statSpaceProbe
	defer func() { statSpaceProbe = oldStat }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()
	oldEval := evalSymlinksSpaceProbe
	defer func() { evalSymlinksSpaceProbe = oldEval }()

	// Existing prefixes (the root, in this case) resolve to themselves so the
	// bounded walk does not depend on a real root's symlink structure.
	evalSymlinksSpaceProbe = func(p string) (string, error) { return p, nil }

	// The daemon's post-chdir AF home is a distinct temp dir so a raw relative
	// path would resolve there (the bug) while the root-joined path lands at
	// "/wt" (the fix). Without this divergence the two could not be told
	// apart.
	afHome := t.TempDir()
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

	// repoPath is the filesystem root; the relative worktreePath joined onto it
	// produces "//wt", whose Split parent ("//") TrimRight would empty into a
	// relative path.
	g := &GitWorktree{repoPath: "/", worktreePath: "wt"}
	_ = g.annotateWorktreeAddError(errors.New("git worktree add failed"))

	require.Equal(t, "/wt", statTarget,
		"the probe must stat the root-joined destination (/wt), not a relative "+
			"\"wt\" that resolves against the daemon's AF-home cwd")
	require.Contains(t, probedDirs, "/",
		"the worktree parent probed must be the filesystem root (/), not the "+
			"daemon's cwd")
	for _, d := range probedDirs {
		require.True(t, filepath.IsAbs(d),
			"a probed dir must be absolute (root-joined), not a raw relative path: %q", d)
	}
}

// TestAnnotateWorktreeAddError_RelativeWorktreePathTrailingSlashPreservesFinalSymlink pins the
// contract the #5421 review added for a trailing separator on the destination: when the
// restored relative worktreePath ends in a separator ("dest/"), filepath.Split returns an
// empty base, folding the final component into the parent resolution so the "parent-only"
// walk follows a final symlink the existing-destination check must see un-resolved. A `dest`
// symlink to a nonempty directory makes git reject the add without writing to the symlink
// target's parent, but resolving it would let a coincidental ENOSPC there stall the
// lost-restore loop indefinitely. Trimming the trailing separator before Split keeps the
// final component out of the parent walk the same way the non-trailing-slash case already
// defends.
func TestAnnotateWorktreeAddError_RelativeWorktreePathTrailingSlashPreservesFinalSymlink(t *testing.T) {
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

	// The relative worktree path's final component is a symlink to a real
	// directory on a distinct location. Git rejects the add because the
	// destination already exists; the probe must not resolve through the
	// symlink and probe the symlink target's parent, which sits on a
	// filesystem git never writes to. The trailing slash on "dest/" must
	// not bypass that final-component preservation.
	repo := filepath.Join(launchCwd, "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))
	realTarget := t.TempDir()
	require.NotEqual(t, realTarget, filepath.Join(repo, "dest"))
	require.NoError(t, os.Symlink(realTarget, filepath.Join(repo, "dest")))

	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	// Resolve the symlinked prefix the way the production code does, so the
	// expected parent matches across hosts (macOS /var/folders ->
	// /private/var/folders).
	resolvedRepo, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	resolvedRealTarget, err := filepath.EvalSymlinks(realTarget)
	require.NoError(t, err)
	symlinkTargetParent := filepath.Dir(resolvedRealTarget)
	require.NotEqual(t, symlinkTargetParent, resolvedRepo,
		"the symlink target's parent must differ from the repo dir, otherwise the test cannot distinguish the fix from the bug")

	// Only the symlink target's parent is full. The parent that hosts the
	// destination's name is writable, so a probe of the correct parent sees
	// no ENOSPC and the git failure stays structural; a probe of the symlink
	// target's parent (the bug) adds ENOSPC and the lost-restore loop would
	// wait indefinitely on a filesystem freeing it cannot fix.
	var statTarget string
	statSpaceProbe = func(p string) (os.FileInfo, error) {
		statTarget = p
		// The destination already exists (the symlink resolves to a real
		// directory), so the existing-destination check sees it as a dir.
		return os.Stat(p)
	}
	var probedDirs []string
	writeSpaceProbe = func(dir string) error {
		probedDirs = append(probedDirs, dir)
		if dir == symlinkTargetParent {
			return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
		}
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	// worktreePath ends in a separator so filepath.Split yields an empty base
	// and the final "dest" symlink would be folded into the parent resolution
	// without the trailing-separator trim.
	g := &GitWorktree{repoPath: "repo", worktreePath: "dest/"}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.False(t, errors.Is(out, syscall.ENOSPC),
		"a full filesystem at the symlink target's parent must not add ENOSPC — git rejected the add "+
			"because the destination already exists, so freeing that filesystem cannot fix the structural "+
			"failure and the parent that hosts the destination's name is writable: %v", out)
	require.NotContains(t, probedDirs, symlinkTargetParent,
		"the parent probe must target the directory that hosts the destination's name (%s), not the "+
			"symlink target's parent (%s)", resolvedRepo, symlinkTargetParent)
	require.Equal(t, filepath.Join(resolvedRepo, "dest"), statTarget,
		"the existing-destination check must see the preserved final component (%s), not the resolved "+
			"symlink target (%s), even with a trailing slash", filepath.Join(resolvedRepo, "dest"), resolvedRealTarget)
	require.Contains(t, probedDirs, resolvedRepo,
		"the parent that hosts the destination's name must be probed")
}

// The probe machinery itself (real writes, the O_TMPFILE anonymous inode,
// the named fallback, and the bounded flight) is tested in internal/fsprobe.
