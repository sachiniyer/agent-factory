package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/sachiniyer/agent-factory/internal/fsprobe"
)

// probedDirsMu guards appendProbedDir so the concurrent write-probe loop in
// annotateGitWriteError can call writeSpaceProbe from goroutines without racing
// on a test mock's probedDirs slice (#5421: keep probing other-filesystem
// parents after a timed-out probe).
var probedDirsMu sync.Mutex

func appendProbedDir(dirs *[]string, dir string) {
	probedDirsMu.Lock()
	*dirs = append(*dirs, dir)
	probedDirsMu.Unlock()
}

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
		appendProbedDir(&probedDirs, dir)
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
		appendProbedDir(&probedDirs, dir)
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
		appendProbedDir(&probedDirs, dir)
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
		appendProbedDir(&probedDirs, dir)
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
		appendProbedDir(&probedDirs, dir)
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
		appendProbedDir(&probedDirs, dir)
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

// TestAnnotateWorktreeAddError_RelativeWorktreePathFinalSymlinkSkipsDestinationProbe pins the
// contract the #5421 review added: when a restored relative destination's final component is a
// symlink to an existing nonempty directory, Stat follows the symlink and reports it as a
// directory, but `git worktree add` rejects the add because the destination already exists
// without writing through the symlink. The destination-directory probe must therefore not
// follow the symlink and write-probe the target's filesystem — a coincidental ENOSPC there
// would make the restore retry environmentally even though freeing it cannot fix the
// structural failure. A bounded lstat excludes the final-component symlink the way a missing
// destination is skipped, while a regular (non-symlink) destination directory is still
// probed in its own right.
//
// Deterministic in both directions: against the reverted form (Stat alone, no lstat gate)
// the destination probe targets the symlinked path and the mocked write returns ENOSPC, so
// errors.Is(out, syscall.ENOSPC) is true and the test fails; with the lstat gate the
// symlinked destination is excluded before the write probe and no ENOSPC reaches the chain.
func TestAnnotateWorktreeAddError_RelativeWorktreePathFinalSymlinkSkipsDestinationProbe(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldStat := statSpaceProbe
	defer func() { statSpaceProbe = oldStat }()
	oldLstat := lstatSpaceProbe
	defer func() { lstatSpaceProbe = oldLstat }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()

	launchCwd := t.TempDir()
	oldLaunch := daemonLaunchCwd
	daemonLaunchCwd = launchCwd
	defer func() { daemonLaunchCwd = oldLaunch }()

	// The relative worktree path's final component is a symlink to a real
	// nonempty directory on a distinct filesystem. Git rejects the add
	// because the destination already exists, without writing through the
	// symlink; the destination-directory probe must not follow the symlink
	// and write-probe the target's filesystem, where a coincidental ENOSPC
	// would stall the lost-restore loop indefinitely. The target is nonempty
	// so the empty-dir read does not re-enable the probe for an empty target
	// (an empty target is a usable destination git would populate).
	repo := filepath.Join(launchCwd, "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))
	realTarget := t.TempDir()
	require.NotEqual(t, realTarget, filepath.Join(repo, "dest"))
	// A nonempty target: git rejects the add because the destination already
	// exists, and the empty-dir check must keep the probe off the target's
	// filesystem.
	require.NoError(t, os.WriteFile(filepath.Join(realTarget, "occupied"), []byte("x"), 0o644))
	require.NoError(t, os.Symlink(realTarget, filepath.Join(repo, "dest")))

	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	// Resolve the symlinked prefix the way the production code does, so the
	// expected destination matches across hosts (macOS /var/folders ->
	// /private/var/folders).
	resolvedRepo, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	destPath := filepath.Join(resolvedRepo, "dest")

	// Stat follows the symlink and reports a directory (the existing
	// destination); Lstat reports the symlink itself so the gate excludes it
	// before the write probe targets the symlink's target.
	statSpaceProbe = func(p string) (os.FileInfo, error) { return os.Stat(p) }
	lstatSpaceProbe = func(p string) (os.FileInfo, error) { return os.Lstat(p) }

	var probedDirs []string
	writeSpaceProbe = func(dir string) error {
		appendProbedDir(&probedDirs, dir)
		// The write probe following the symlink into the target's filesystem
		// would see ENOSPC there (the bug); the parent that hosts the
		// destination's name is writable.
		if dir == destPath {
			return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
		}
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{repoPath: "repo", worktreePath: "dest"}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.False(t, errors.Is(out, syscall.ENOSPC),
		"a final-component symlink to a nonempty directory must not be probed through to its "+
			"target — git rejects the add because the destination already exists, so a full target "+
			"filesystem cannot fix the structural failure and must not stall the lost-restore loop: %v", out)
	require.NotContains(t, probedDirs, destPath,
		"the write probe must not target the symlinked destination (%s) — its target's filesystem "+
			"is not one git writes into", destPath)
	require.Contains(t, probedDirs, resolvedRepo,
		"the parent that hosts the destination's name must still be probed")
}

// TestAnnotateWorktreeAddError_DestinationRegularDirIsProbed pins that the lstat gate does
// not exclude a regular (non-symlink) destination directory: a pre-created mount point or
// partial-attempt debris on a different filesystem than the parent must still be probed in
// its own right. Only a final-component symlink is skipped.
func TestAnnotateWorktreeAddError_DestinationRegularDirIsProbed(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldStat := statSpaceProbe
	defer func() { statSpaceProbe = oldStat }()
	oldLstat := lstatSpaceProbe
	defer func() { lstatSpaceProbe = oldLstat }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()

	// The destination already exists as a regular directory on a distinct
	// filesystem than the parent that hosts its name — a pre-created mount
	// point or partial-attempt debris. The probe must still target it so a
	// full filesystem there reaches the chain the way a full parent does.
	parent := t.TempDir()
	dest := filepath.Join(parent, "dest")
	require.NoError(t, os.Mkdir(dest, 0o755))

	statSpaceProbe = func(p string) (os.FileInfo, error) { return os.Stat(p) }
	lstatSpaceProbe = func(p string) (os.FileInfo, error) { return os.Lstat(p) }
	var probedDirs []string
	writeSpaceProbe = func(dir string) error {
		appendProbedDir(&probedDirs, dir)
		if dir == dest {
			return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
		}
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{worktreePath: dest}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.True(t, errors.Is(out, syscall.ENOSPC),
		"a regular destination directory on a full filesystem must still be probed so its ENOSPC "+
			"reaches the chain: %v", out)
	require.Contains(t, probedDirs, dest,
		"a regular (non-symlink) destination directory must still be probed in its own right")
}

// TestAnnotateWorktreeAddError_RelativeWorktreePathFinalSymlinkEmptyTargetIsProbed pins the
// contract the #5421 review added for a final-component symlink to an EMPTY directory: `git
// worktree add` through a symlink to an empty directory succeeds (it populates the empty
// target), unlike the nonempty-target case the skip probe defends against. The destination-
// directory probe must therefore still target the symlink's filesystem so a full filesystem
// there reaches the chain as environmental evidence; the empty-dir read re-enables the probe
// the lstat gate would otherwise drop.
//
// Deterministic in both directions: against the form that skipped every final symlink the
// write probe never targets the symlinked destination and errors.Is(out, syscall.ENOSPC) is
// false, so the test fails; with the empty-target re-enable the probe targets the destination
// and ENOSPC reaches the chain.
func TestAnnotateWorktreeAddError_RelativeWorktreePathFinalSymlinkEmptyTargetIsProbed(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldStat := statSpaceProbe
	defer func() { statSpaceProbe = oldStat }()
	oldLstat := lstatSpaceProbe
	defer func() { lstatSpaceProbe = oldLstat }()
	oldEmpty := isEmptyDirSpaceProbe
	defer func() { isEmptyDirSpaceProbe = oldEmpty }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()

	launchCwd := t.TempDir()
	oldLaunch := daemonLaunchCwd
	daemonLaunchCwd = launchCwd
	defer func() { daemonLaunchCwd = oldLaunch }()

	// The relative worktree path's final component is a symlink to an empty
	// directory on a distinct filesystem. `git worktree add` through a symlink
	// to an empty directory succeeds (it populates the empty target), so the
	// destination-directory probe must still target the symlink's filesystem —
	// a full filesystem there is the real ENOSPC git's write would hit.
	repo := filepath.Join(launchCwd, "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))
	realTarget := t.TempDir()
	require.NotEqual(t, realTarget, filepath.Join(repo, "dest"))
	require.NoError(t, os.Symlink(realTarget, filepath.Join(repo, "dest")))

	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	resolvedRepo, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	destPath := filepath.Join(resolvedRepo, "dest")

	// Stat follows the symlink and reports a directory (the empty target);
	// Lstat reports the symlink itself so the gate reads the target's entries
	// rather than blindly probing every symlinked directory.
	statSpaceProbe = func(p string) (os.FileInfo, error) { return os.Stat(p) }
	lstatSpaceProbe = func(p string) (os.FileInfo, error) { return os.Lstat(p) }
	// The target is empty, so the production empty-dir read re-enables the
	// probe the lstat gate deferred on it.
	isEmptyDirSpaceProbe = fsprobe.IsEmptyDir

	var probedDirs []string
	writeSpaceProbe = func(dir string) error {
		appendProbedDir(&probedDirs, dir)
		if dir == destPath {
			return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
		}
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{repoPath: "repo", worktreePath: "dest"}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.True(t, errors.Is(out, syscall.ENOSPC),
		"a final-component symlink to an empty directory must be probed through to its target — "+
			"git populates an empty destination even through a symlink, so a full target filesystem "+
			"is the real ENOSPC git's write would hit and must reach the chain: %v", out)
	require.Contains(t, probedDirs, destPath,
		"the write probe must target the symlinked destination (%s) — its target's filesystem "+
			"is one git writes into when the target is empty", destPath)
	require.Contains(t, probedDirs, resolvedRepo,
		"the parent that hosts the destination's name must still be probed")
}

// TestAnnotateWorktreeAddError_LstatErrorPreserved pins the contract the #5421 review added:
// when the destination Stat succeeds (a directory) but the bounded Lstat returns a
// non-ENOENT error (EIO, ETIMEDOUT, the bounded ErrProbeTimedOut), the gate must not silently
// discard it. The opaque git failure would otherwise be charged to the structural retry
// budget despite the metadata probe having failed; a non-ENOENT lstat error is environmental
// evidence about the filesystem the probe is about to write to and joins the chain the way a
// non-ENOENT stat error does.
//
// Deterministic in both directions: against the form that discarded lErr the chain carries no
// lstat error, so errors.Is(out, syscall.EIO) is false and the test fails; with the lstat
// error preserved the joined evidence reaches the chain.
func TestAnnotateWorktreeAddError_LstatErrorPreserved(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldStat := statSpaceProbe
	defer func() { statSpaceProbe = oldStat }()
	oldLstat := lstatSpaceProbe
	defer func() { lstatSpaceProbe = oldLstat }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()

	// The destination Stat succeeds as a directory, but the bounded Lstat
	// returns EIO — a metadata failure the gate must join as environmental
	// evidence rather than silently dropping.
	dest := t.TempDir()
	statSpaceProbe = func(p string) (os.FileInfo, error) {
		// Report a directory so the lstat gate runs; Stat follows symlinks
		// but the real directory is a regular (non-symlink) directory here.
		info, err := os.Stat(p)
		require.NoError(t, err)
		require.True(t, info.IsDir())
		return info, nil
	}
	lstatSpaceProbe = func(p string) (os.FileInfo, error) {
		return nil, &os.PathError{Op: "lstat", Path: p, Err: syscall.EIO}
	}
	var probedDirs []string
	writeSpaceProbe = func(dir string) error {
		appendProbedDir(&probedDirs, dir)
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{worktreePath: dest}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.True(t, errors.Is(out, syscall.EIO),
		"a non-ENOENT lstat error (EIO) must be joined into the annotation so the lost-restore "+
			"classifier reads it as environmental evidence rather than silently dropping it: %v", out)
}

// TestAnnotateWorktreeAddError_SymlinkedDestinationReadDirErrorPreserved pins the contract
// the #5421 review added: when the destination Stat succeeds as a directory and the lstat shows
// a symlink (so the empty-dir read runs), a non-ENOENT read-dir error (EIO, ETIMEDOUT, the
// bounded ErrProbeTimedOut) must be preserved. Discarding it would let the parent/common-
// directory write probes succeed and leave the opaque git failure with no environmental
// evidence from the filesystem the probe is about to write to.
//
// Deterministic in both directions: against the form that discarded dErr the chain carries no
// read-dir error, so errors.Is(out, syscall.EIO) is false and the test fails; with the
// read-dir error preserved the joined evidence reaches the chain.
func TestAnnotateWorktreeAddError_SymlinkedDestinationReadDirErrorPreserved(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldStat := statSpaceProbe
	defer func() { statSpaceProbe = oldStat }()
	oldLstat := lstatSpaceProbe
	defer func() { lstatSpaceProbe = oldLstat }()
	oldEmpty := isEmptyDirSpaceProbe
	defer func() { isEmptyDirSpaceProbe = oldEmpty }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()

	launchCwd := t.TempDir()
	oldLaunch := daemonLaunchCwd
	daemonLaunchCwd = launchCwd
	defer func() { daemonLaunchCwd = oldLaunch }()

	repo := filepath.Join(launchCwd, "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))
	realTarget := t.TempDir()
	require.NoError(t, os.Symlink(realTarget, filepath.Join(repo, "dest")))

	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	statSpaceProbe = func(p string) (os.FileInfo, error) { return os.Stat(p) }
	lstatSpaceProbe = func(p string) (os.FileInfo, error) { return os.Lstat(p) }
	// The bounded empty-dir read fails with EIO on a stalled mount rather than
	// returning an empty/nonempty verdict; the gate must preserve that error
	// instead of silently skipping the destination probe.
	isEmptyDirSpaceProbe = func(dir string) (bool, error) {
		return false, &os.PathError{Op: "readdir", Path: dir, Err: syscall.EIO}
	}
	var probedDirs []string
	writeSpaceProbe = func(dir string) error {
		appendProbedDir(&probedDirs, dir)
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{repoPath: "repo", worktreePath: "dest"}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.True(t, errors.Is(out, syscall.EIO),
		"a non-ENOENT read-dir error on a symlinked destination (EIO) must be joined into the "+
			"annotation so the lost-restore classifier reads it as environmental evidence rather "+
			"than silently dropping it: %v", out)
}

// TestAnnotateWorktreeAddError_RelativeWorktreePathFollowsSymlinkAfterCreatableComponent pins the
// contract the #5421 review added: a relative worktree path whose longest existing prefix is the
// repo but whose non-existent tail traverses a creatable component followed by `..` and an
// existing symlink (worktreePath = `missing/../link/../wt` with `link` -> /mnt/dir) must follow
// the symlink after the creatable component the way git's chdir does — git creates `missing`, backs
// out to the repo, follows `link` to its target, backs out to the target's parent, and creates
// `wt` there. A lexical filepath.Join collapses `missing/../link/../wt` to `<repo>/wt` and probes
// the wrong filesystem, so a full /mnt (the real destination's parent) would never reach the chain
// and a full repo filesystem (which git never writes to for this path) could instead. The tail
// walk follows the symlink at each existing component before `..` collapses the path, so the
// probe targets the filesystem git actually writes to.
//
// Deterministic in both directions: against the form that lexically rejoined the tail the probe
// targeted <repo>/wt (the lexical collapse) and missed the real parent entirely, so a write probe
// mocked to fail only on the real parent returned nil and errors.Is(out, syscall.ENOSPC) was
// false; with the tail walk the probe targets the real parent and the mocked ENOSPC reaches the
// chain.
func TestAnnotateWorktreeAddError_RelativeWorktreePathFollowsSymlinkAfterCreatableComponent(t *testing.T) {
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

	// <repo>/link points at a real dir on a distinct location; `missing` does
	// not exist, so the longest existing prefix is the repo and the tail
	// `missing/../link/../wt` must walk past `missing` (creatable), back to the
	// repo, through `link` (symlink), back to the link target's parent, and
	// into `wt` — landing at parent(realTarget)/wt, the same filesystem git
	// writes to. A lexical join would collapse to <repo>/wt and miss it.
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
		appendProbedDir(&probedDirs, dir)
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	// `missing/../link/../wt` relative to a real repo: git creates `missing`,
	// backs out, follows `link`, backs out, and creates `wt` under the link
	// target's parent. The probe must target parent(realTarget)/wt, not the
	// lexical <repo>/wt that filepath.Join(repo, "missing/../link/../wt")
	// would produce. Resolve the link target the way the production code does
	// so the expected paths match across hosts (macOS /var/folders ->
	// /private/var/folders).
	resolvedRealTarget, err := filepath.EvalSymlinks(realTarget)
	require.NoError(t, err)
	expectedDest := filepath.Join(filepath.Dir(resolvedRealTarget), "wt")
	resolvedRepo, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	lexicalDest := filepath.Join(resolvedRepo, "wt")
	require.NotEqual(t, lexicalDest, expectedDest,
		"the creatable-component-then-symlink case must diverge from the lexical join, otherwise the test cannot distinguish the fix from the bug")
	g := &GitWorktree{repoPath: "repo", worktreePath: "missing/../link/../wt"}
	_ = g.annotateWorktreeAddError(errors.New("git worktree add failed"))

	require.Equal(t, expectedDest, statTarget,
		"the probe must target the symlink-resolved destination git's child writes to (%s), not the "+
			"lexical filepath.Join that collapses the creatable component and the symlink (%s)",
		expectedDest, lexicalDest)
	require.Contains(t, probedDirs, filepath.Dir(expectedDest),
		"the worktree parent dir probed must be the symlink-resolved parent (%s), not the lexical %s",
		filepath.Dir(expectedDest), lexicalDest)
	// git creates the missing `missing` component under the repo via
	// safe_create_leading_directories before backing out and following `link`,
	// so the repo is the existing parent of a creatable component the tail
	// walk advanced past and must be probed for ENOSPC the way a full
	// filesystem on the mount git wrote the missing directory into surfaces
	// the resource errno. The destination stat above still targets the
	// symlink-resolved parent (not the lexical <repo>/wt), so probing the repo
	// as a creatable parent does not collapse the traversal to the lexical
	// destination.
	require.Contains(t, probedDirs, resolvedRepo,
		"the existing parent of the creatable `missing` component (the repo, where git creates it "+
			"via safe_create_leading_directories) must be probed for ENOSPC, not just the symlink-"+
			"resolved destination parent")
}

// TestAnnotateWorktreeAddError_RelativeWorktreePathDanglingSymlinkDoesNotCollapseTraversal pins the
// contract the #5421 review added: a relative worktree path that traverses a dangling symlink
// before `..` (link -> /missing, worktreePath = link/../../wt) must not collapse the traversal
// past the dangling symlink. EvalSymlinks returns ENOENT for prefixes containing `link`, and a
// lexical filepath.Join would backtrack to the repo and collapse `link/../..` to the repo's
// parent, probing a filesystem git never reaches — git fails resolving the dangling `link`
// before it touches that filesystem, so a coincidental ENOSPC there would stall the
// lost-restore loop indefinitely on a structural dangling-link failure. The tail walk
// distinguishes a dangling symlink (Lstat succeeds and reports a symlink whose target does not
// exist) from a genuinely absent component, surfaces it as a resolution error, and the caller
// probes only the independently known locations (the common directory) rather than a guessed
// lexical destination.
//
// Deterministic in both directions: against the form that collapsed the dangling symlink the
// probe targeted the lexical parent (the repo's parent) and a write probe mocked to fail only
// there returned ENOSPC, so errors.Is(out, syscall.ENOSPC) was true and the test failed; with
// the tail walk the dangling symlink is surfaced and the lexical parent is never probed, so the
// ENOSPC never reaches the chain.
func TestAnnotateWorktreeAddError_RelativeWorktreePathDanglingSymlinkDoesNotCollapseTraversal(t *testing.T) {
	oldWrite := writeSpaceProbe
	defer func() { writeSpaceProbe = oldWrite }()
	oldCommon := gitCommonDir
	defer func() { gitCommonDir = oldCommon }()

	launchCwd := t.TempDir()
	oldLaunch := daemonLaunchCwd
	daemonLaunchCwd = launchCwd
	defer func() { daemonLaunchCwd = oldLaunch }()

	// <repo>/link is a dangling symlink: its target does not exist, so
	// EvalSymlinks returns ENOENT for any prefix containing `link`. Git fails
	// resolving it before reaching the lexical parent a filepath.Join would
	// collapse `link/../..` to. A coincidental ENOSPC on that lexical parent
	// must not reach the chain — the dangling symlink is a structural failure,
	// not an environmental one.
	repo := filepath.Join(launchCwd, "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))
	require.NoError(t, os.Symlink(filepath.Join(t.TempDir(), "does-not-exist"), filepath.Join(repo, "link")))

	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	resolvedRepo, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	// The lexical parent a filepath.Join(repo, "link/../../wt") would collapse
	// to — the repo's parent — which git never writes to for this path.
	lexicalParent := filepath.Dir(resolvedRepo)

	var probedDirs []string
	writeSpaceProbe = func(dir string) error {
		appendProbedDir(&probedDirs, dir)
		// A coincidental ENOSPC on the lexical parent (the repo's parent) is
		// the bug: the dangling symlink never lets git reach it, so freeing it
		// cannot fix the structural failure and the probe must not target it.
		if dir == lexicalParent {
			return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
		}
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{repoPath: "repo", worktreePath: "link/../../wt"}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.False(t, errors.Is(out, syscall.ENOSPC),
		"a dangling symlink before `..` must not collapse the traversal to the repo's parent — "+
			"git fails resolving the dangling link before reaching that filesystem, so a coincidental "+
			"ENOSPC there must not stall the lost-restore loop on a structural failure: %v", out)
	require.NotContains(t, probedDirs, lexicalParent,
		"the lexical parent the dangling-symlink traversal would collapse to must not be probed — "+
			"git never writes there for this path")
	// The dangling-symlink resolution error is joined so the classifier sees it
	// alongside the git failure rather than the structural silence a dropped
	// resolution would leave.
	require.Contains(t, out.Error(), "symlink resolution of",
		"the dangling-symlink resolution error must be named in the annotation, not dropped: %v", out)
}

// TestAnnotateWorktreeAddError_SymlinkResolutionTimeoutDoesNotProbeLexicalDestination pins the
// contract the #5421 review added: when symlink resolution times out for a relative path containing
// traversal (link/../../wt), the actual destination is unknown and git may have failed before
// reaching it (for example while validating a branch), so the lexical filepath.Join a timeout
// falls back to is a guess at a filesystem git may never touch. A coincidental ENOSPC on that
// unrelated lexical filesystem would be added to the timeout chain, and because
// lostRestoreEnvironmentalClass checks resource errnos before the bounded-deadline signature,
// it would convert a capped timeout retry into an indefinite errno wait. The timeout branch
// must preserve the timeout evidence and probe only the independently known locations (the
// common directory), never the guessed lexical destination.
//
// Deterministic in both directions: against the form that probed filepath.Dir(lexical) on a
// timeout the lexical parent was probed and a write probe mocked to fail only there returned
// ENOSPC, so errors.Is(out, syscall.ENOSPC) was true and the test failed; with the timeout
// branch probing only the common directory the ENOSPC never reaches the chain while the
// bounded non-answer (ErrProbeTimedOut + context.DeadlineExceeded) does.
func TestAnnotateWorktreeAddError_SymlinkResolutionTimeoutDoesNotProbeLexicalDestination(t *testing.T) {
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

	repo := filepath.Join(launchCwd, "repo")
	require.NoError(t, os.Mkdir(repo, 0o755))

	afHome := t.TempDir()
	require.NotEqual(t, launchCwd, afHome)
	t.Chdir(afHome)

	resolvedRepo, err := filepath.EvalSymlinks(repo)
	require.NoError(t, err)
	// The lexical parent a timeout would collapse `link/../../wt` to — the
	// repo's parent — which git may never reach for this path.
	lexicalParent := filepath.Dir(resolvedRepo)

	// A stalled mount never answers the symlink resolution: the bounded flight
	// returns the typed non-answer, and the timeout branch must not probe the
	// lexical destination it would have fallen back to.
	evalSymlinksSpaceProbe = func(string) (string, error) {
		return "", fmt.Errorf("eval-symlinks probe did not answer within bound: %w: %w",
			fsprobe.ErrProbeTimedOut, context.DeadlineExceeded)
	}
	var probedDirs []string
	writeSpaceProbe = func(dir string) error {
		appendProbedDir(&probedDirs, dir)
		// A coincidental ENOSPC on the lexical parent is the bug: the timeout
		// did not resolve the destination, so git may have failed before
		// reaching it and freeing this filesystem cannot fix the failure.
		if dir == lexicalParent {
			return &os.PathError{Op: "write", Path: ".af-add-probe-x", Err: syscall.ENOSPC}
		}
		return nil
	}
	gitCommonDir = func(*GitWorktree) (string, error) { return "", nil }

	g := &GitWorktree{repoPath: "repo", worktreePath: "link/../../wt"}
	out := g.annotateWorktreeAddError(errors.New("git worktree add failed"))
	require.False(t, errors.Is(out, syscall.ENOSPC),
		"a timeout must not probe the guessed lexical destination — git may have failed before "+
			"reaching it, so a coincidental ENOSPC on the unrelated lexical filesystem must not be "+
			"added to the timeout chain and convert a capped retry into an indefinite errno wait: %v", out)
	require.NotContains(t, probedDirs, lexicalParent,
		"the lexical parent a timeout collapses the traversal to must not be probed — the actual "+
			"destination is unknown after a timeout")
	require.True(t, errors.Is(out, fsprobe.ErrProbeTimedOut),
		"the bounded symlink-resolution non-answer must still reach the chain: %v", out)
	require.True(t, errors.Is(out, context.DeadlineExceeded),
		"the sentinel must keep its deadline pair for the classifier: %v", out)
}
